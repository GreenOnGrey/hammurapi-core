// Package approvals implements "submit for approval", "approve" and the
// "awaiting your approval" section of the home page.
package approvals

import (
	"context"
	"errors"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gates"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/markdown"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Pending is a gate awaiting approval.
type Pending struct {
	UniqueID    string      `json:"uniqueId"`
	Title       string      `json:"title"`
	Domain      string      `json:"domain"`
	System      string      `json:"system"`
	Area        domain.Area `json:"area"`
	SubmittedBy *string     `json:"submittedBy"`
	SubmittedAt time.Time   `json:"submittedAt"`
	GateID      string      `json:"-"`
}

// Lister reads the approvals queue.
type Lister interface {
	Pending(ctx context.Context, userID string, page httpx.Page) ([]Pending, error)
}

// Service implements approval use cases.
type Service struct {
	store  specdata.Store
	list   Lister
	git    git.Provider
	tokens git.TokenSource
	events events.Publisher
	gen    gates.Generator
}

// NewService creates the service.
func NewService(store specdata.Store, list Lister, provider git.Provider, tokens git.TokenSource, ev events.Publisher, gen gates.Generator) *Service {
	return &Service{store: store, list: list, git: provider, tokens: tokens, events: ev, gen: gen}
}

var (
	errApprovalDisabled = apperr.Conflict("approval_disabled", "approval is disabled for this domain")
	errGateNotFound     = apperr.NotFound("gate_not_found", "gate not found")
)

func (s *Service) load(ctx context.Context, uniqueID string, area domain.Area) (*specdata.Feature, *specdata.Gate, error) {
	f, err := features.Load(ctx, s.store, uniqueID)
	if err != nil {
		return nil, nil, err
	}
	g, err := s.store.ActiveGate(ctx, f.ID, area)
	if errors.Is(err, specdata.ErrNotFound) {
		return nil, nil, errGateNotFound
	}
	return f, g, err
}

// Submit moves a draft gate to in_review. Any expert of the domain submits,
// including the generated tech and qa. Product requirements without an ID
// return 409 requirements_without_id with their list unless force is set (R15).
func (s *Service) Submit(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area, force bool) (*specdata.Gate, error) {
	f, g, err := s.load(ctx, uniqueID, area)
	if err != nil {
		return nil, err
	}
	if err := features.RequireExpert(p, f.DomainKey); err != nil {
		return nil, err
	}
	if err := features.RequireSpecPhase(f); err != nil {
		return nil, err
	}
	if !f.ApprovalRequired {
		return nil, errApprovalDisabled
	}
	if g.Status != domain.GateDraft {
		return nil, apperr.Conflict("not_draft", "only a draft can be submitted for approval")
	}
	if area == domain.AreaProduct && !force {
		token, err := s.tokens.Token(ctx, p.UserID)
		if err != nil {
			return nil, err
		}
		doc, err := s.git.GetFile(ctx, token, f.Branch, git.SpecPath(f.DomainKey, f.SystemKey, f.UniqueID, string(area)))
		if err != nil && !errors.Is(err, git.ErrNotFound) {
			return nil, auth.MapGitError(err)
		}
		if doc != nil {
			if u := markdown.UnnumberedRequirements(string(doc.Content)); len(u) > 0 {
				return nil, apperr.Conflict("requirements_without_id", "some requirements have no ID; ask the agent to number them or submit anyway").
					With("requirements", u)
			}
		}
	}
	now := time.Now()
	g.Status, g.SubmittedAt = domain.GateInReview, &now
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		if err := tx.SaveGate(ctx, g); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventSubmitted, ActorID: &p.UserID})
	})
	if err != nil {
		return nil, err
	}
	gates.RecordTransition(area, string(domain.GateInReview))
	gates.PublishGateUpdated(ctx, s.events, f.UniqueID, g)
	s.events.Publish(ctx, events.Event{Type: events.ApprovalsChanged, Data: map[string]string{"uniqueId": f.UniqueID}})
	return g, nil
}

// Approve approves an in_review gate: product and design by a product expert,
// arch, tech and qa by a technical expert of the feature's domain. Approval is
// strictly sequential in area order, and it is rejected when git has a newer
// commit of the document than the projection knows. When every human gate is
// approved, the agent (re)generates tech and qa (R12, R13).
func (s *Service) Approve(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area) (*specdata.Gate, error) {
	f, g, err := s.load(ctx, uniqueID, area)
	if err != nil {
		return nil, err
	}
	if !p.CanApprove(f.DomainKey, area) {
		return nil, apperr.Forbidden("forbidden", string(area.ApproverKind())+" expert of domain "+f.DomainKey+" required").
			With("kind", area.ApproverKind()).With("domain", f.DomainKey)
	}
	if err := features.RequireSpecPhase(f); err != nil {
		return nil, err
	}
	if !f.ApprovalRequired {
		return nil, errApprovalDisabled
	}
	if g.Status != domain.GateInReview {
		return nil, apperr.Conflict("not_in_review", "only a gate awaiting approval can be approved")
	}
	all, err := s.store.ActiveGates(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	for _, other := range all {
		if other.Area.Index() < area.Index() && other.Status != domain.GateApproved {
			return nil, apperr.Conflict("previous_not_approved", "earlier gates must be approved first").With("area", other.Area)
		}
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	head, err := s.git.LatestCommit(ctx, token, f.Branch, git.SpecDir(f.DomainKey, f.SystemKey, f.UniqueID, string(area)))
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	if head != g.HeadCommit {
		return nil, apperr.Conflict("document_changed", "the document changed, reload the page")
	}
	now := time.Now()
	g.Status, g.ApprovedCommit, g.ApprovedBy, g.ApprovedAt = domain.GateApproved, &head, &p.UserID, &now
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		if err := tx.SaveGate(ctx, g); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventApproved, ActorID: &p.UserID, CommitSHA: &head})
	})
	if err != nil {
		return nil, err
	}
	gates.RecordTransition(area, string(domain.GateApproved))
	gates.PublishGateUpdated(ctx, s.events, f.UniqueID, g)
	s.events.Publish(ctx, events.Event{Type: events.ApprovalsChanged, Data: map[string]string{"uniqueId": f.UniqueID}})
	s.events.Publish(ctx, events.Event{Type: events.FocusChanged, Data: map[string]string{"uniqueId": f.UniqueID}})
	if !area.Generated() && s.gen != nil && !domain.AgentDisabled() {
		for i := range all {
			if all[i].Area == area {
				all[i].Status = domain.GateApproved
			}
		}
		if features.HumanGatesApproved(all) {
			if err := s.gen.Generate(ctx, f, []domain.Area{domain.AreaTech, domain.AreaQA}, p.UserID, ""); err != nil {
				return nil, err
			}
		}
	}
	return g, nil
}

// List returns the approvals queue; empty for users who are not experts.
func (s *Service) List(ctx context.Context, p *domain.Principal, page httpx.Page) (httpx.List[Pending], error) {
	if !p.IsAnyExpert() {
		return httpx.List[Pending]{Items: []Pending{}}, nil
	}
	items, err := s.list.Pending(ctx, p.UserID.String(), page)
	if err != nil {
		return httpx.List[Pending]{}, err
	}
	return httpx.NewList(items, page.Limit, func(x Pending) (time.Time, string) { return x.SubmittedAt, x.GateID }), nil
}
