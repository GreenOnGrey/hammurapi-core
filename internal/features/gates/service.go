// Package gates implements gate documents: reading and saving them, diff since
// approval and gate history. Gates are created and removed by the writer (gategen).
package gates

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specchanges"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/markdown"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Generator is the writer of specifications (FTR.HMR.DEV-0001).
type Generator interface {
	// AfterApproval moves the specification on after a gate was approved;
	// reapproved tells that the gate had been approved before with another text.
	AfterApproval(ctx context.Context, f *specdata.Feature, area domain.Area, reapproved bool, all []specdata.Gate, initiator uuid.UUID) error
	// Active is the run in progress, nil — none.
	Active(ctx context.Context, featureID uuid.UUID) (*features.WriterRun, error)
	// OpenQuestions lists the open questions of a gate.
	OpenQuestions(ctx context.Context, featureID uuid.UUID, area domain.Area) ([]cycledata.Question, error)
}

// Service implements gate use cases.
type Service struct {
	store         specdata.Store
	git           git.Provider
	tokens        git.TokenSource
	events        events.Publisher
	gen           Generator
	defaultBranch string
	// Changes keeps the notes in the features a product document changes (FTR.HMR.DEV-0002); nil — none.
	Changes *specchanges.Service
}

// NewService creates the service.
func NewService(store specdata.Store, provider git.Provider, tokens git.TokenSource, ev events.Publisher, gen Generator, defaultBranch string) *Service {
	return &Service{store: store, git: provider, tokens: tokens, events: ev, gen: gen, defaultBranch: defaultBranch}
}

// Errors.
var (
	ErrGateNotFound = apperr.NotFound("gate_not_found", "gate not found")
	ErrGenerated    = apperr.Forbidden("gate_generated", "tech and qa are written by the agent: ask the agent in the chat instead")
)

// RequireEditable checks that a person may change the gates of the feature:
// an expert of its domain, specification phase, and not a generated area.
func RequireEditable(p *domain.Principal, f *specdata.Feature, area domain.Area) error {
	if area.Generated() {
		return ErrGenerated
	}
	if err := features.RequireExpert(p, f.DomainKey); err != nil {
		return err
	}
	return features.RequireSpecPhase(f)
}

func (s *Service) loadGate(ctx context.Context, uniqueID string, area domain.Area) (*specdata.Feature, *specdata.Gate, error) {
	f, err := features.Load(ctx, s.store, uniqueID)
	if err != nil {
		return nil, nil, err
	}
	g, err := s.store.ActiveGate(ctx, f.ID, area)
	if errors.Is(err, specdata.ErrNotFound) {
		return f, nil, ErrGateNotFound
	}
	return f, g, err
}

func specPath(f *specdata.Feature, area domain.Area) string {
	return git.SpecPath(f.DomainKey, f.SystemKey, f.UniqueID, string(area))
}

// readRef is the feature branch until the release is confirmed; afterwards the
// spec PR is merged, the branch may be gone and the content lives in the default
// branch. Indexed features (FTR.HMR.CMN-0005) live only there.
func (s *Service) readRef(f *specdata.Feature) string {
	if f.Phase == domain.PhaseReleased || f.Phase == domain.PhaseIndexed {
		return s.defaultBranch
	}
	return f.Branch
}

// PublishGateUpdated emits gate.updated.
func PublishGateUpdated(ctx context.Context, ev events.Publisher, uniqueID string, g *specdata.Gate) {
	ev.Publish(ctx, events.Event{Type: events.GateUpdated, Data: map[string]any{
		"uniqueId": uniqueID, "area": g.Area, "status": g.Status, "headCommit": g.HeadCommit, "deleted": g.DeletedAt != nil,
	}})
}

// Document is a gate document.
type Document struct {
	Content   string   `json:"content"`
	SHA       string   `json:"sha"`
	Fix       bool     `json:"fix"`
	Generated bool     `json:"generated"`
	Warnings  []string `json:"warnings"`
	// UnnumberedRequirements lists product requirements without an ID (R15).
	UnnumberedRequirements []string          `json:"unnumberedRequirements"`
	Lock                   *specdata.LockDTO `json:"lock"`
	ReadOnly               bool              `json:"readOnly"`
}

// GetDocument reads the gate document from git.
func (s *Service) GetDocument(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area) (*Document, error) {
	f, _, err := s.loadGate(ctx, uniqueID, area)
	if err != nil {
		return nil, err
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	file, err := s.git.GetFile(ctx, token, s.readRef(f), specPath(f, area))
	if errors.Is(err, git.ErrNotFound) {
		return nil, apperr.NotFound("document_not_found", "the document is missing in git")
	}
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	lock, err := s.store.GetLock(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	content := string(file.Content)
	ro := RequireEditable(p, f, area) != nil || (lock != nil && lock.LockedBy != p.UserID)
	if s.gen != nil && !ro { // FTR.HMR.DEV-0001 R25: what the writer writes is read-only
		run, err := s.gen.Active(ctx, f.ID)
		if err != nil {
			return nil, err
		}
		ro = run.Touches(area)
	}
	warn := markdown.CheckSubset(content, f.IsFix())
	if warn == nil {
		warn = []string{}
	}
	unnumbered := []string{}
	if area == domain.AreaProduct {
		if u := markdown.UnnumberedRequirements(content); u != nil {
			unnumbered = u
		}
	}
	return &Document{Content: content, SHA: git.BlobSHA(file.Content), Fix: f.IsFix(), Generated: area.Generated(), Warnings: warn,
		UnnumberedRequirements: unnumbered, Lock: specdata.ToLockDTO(lock), ReadOnly: ro}, nil
}

// SaveInput is the body of PUT document.
type SaveInput struct {
	Content string `json:"content"`
	BaseSHA string `json:"baseSha"`
}

// SaveResult is returned after a save.
type SaveResult struct {
	SHA    string  `json:"sha"`
	Commit *string `json:"commit"`
}

// SaveDocument commits a new version of the gate document on behalf of the
// user. The projection (event
// "edited", reset to draft) is written by the worker from the push webhook.
func (s *Service) SaveDocument(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area, in SaveInput) (*SaveResult, error) {
	f, _, err := s.loadGate(ctx, uniqueID, area)
	if err != nil {
		return nil, err
	}
	if err := RequireEditable(p, f, area); err != nil {
		return nil, err
	}
	if s.gen != nil { // FTR.HMR.DEV-0001 R25: a run in progress owns its documents; the edit it waits for may finish
		run, err := s.gen.Active(ctx, f.ID)
		if err != nil {
			return nil, err
		}
		if run.Touches(area) {
			cur, err := s.store.GetLock(ctx, f.ID)
			if err != nil {
				return nil, err
			}
			if run.State != features.RunQueued || cur == nil || cur.LockedBy != p.UserID {
				return nil, features.ErrSpecRunActive
			}
		}
	}
	l, ok, err := s.store.AcquireLock(ctx, f.ID, p.UserID)
	if err != nil {
		return nil, err
	}
	if !ok {
		name := ""
		if l != nil {
			name = l.LockedByName
		}
		return nil, apperr.Locked("feature_locked", "feature is being edited by "+name).With("userName", name)
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	path := specPath(f, area)
	cur, err := s.git.GetFile(ctx, token, f.Branch, path)
	if err != nil && !errors.Is(err, git.ErrNotFound) {
		return nil, auth.MapGitError(err)
	}
	curSHA := ""
	if cur != nil {
		curSHA = git.BlobSHA(cur.Content)
	}
	if in.BaseSHA != "" && in.BaseSHA != curSHA {
		return nil, apperr.Conflict("stale_document", "the document was changed concurrently, reload it").With("currentSha", curSHA)
	}
	newSHA := git.BlobSHA([]byte(in.Content))
	if newSHA == curSHA {
		return &SaveResult{SHA: curSHA}, nil // no change, no commit (no noise in git)
	}
	changes := []git.FileChange{{Path: path, Content: []byte(in.Content)}}
	var notes []git.FileChange
	if area == domain.AreaProduct && s.Changes != nil {
		// FTR.HMR.DEV-0002 R3, R4: the notes go in the same commit; a wrong row does not stop the save
		if notes, _, err = s.Changes.Align(ctx, token, f, in.Content); err != nil {
			return nil, auth.MapGitError(err)
		}
		changes = append(changes, notes...)
	}
	msg := git.Trailers{Feature: f.UniqueID, Area: string(area)}.Message(fmt.Sprintf("%s: edit %s specification", f.UniqueID, area))
	commit, err := s.git.Commit(ctx, token, f.Branch, msg, changes)
	if errors.Is(err, git.ErrConflict) {
		return nil, apperr.Conflict("stale_document", "the branch changed concurrently, retry")
	}
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	specchanges.Applied(notes)
	return &SaveResult{SHA: newSHA, Commit: &commit}, nil
}

// DiffResult is the diff since the last approval (or since the default branch).
type DiffResult struct {
	Base       string              `json:"base"`
	BaseLabel  string              `json:"baseLabel"` // "approved" | "default_branch"
	Head       string              `json:"head"`
	ApprovedBy *string             `json:"approvedBy"`
	ApprovedAt *time.Time          `json:"approvedAt"`
	BaseURL    string              `json:"baseUrl"`
	HeadURL    string              `json:"headUrl"`
	Lines      []markdown.DiffLine `json:"lines"`
}

// Diff compares the approved version with the current one. In a domain without
// approval (or before the first approval) the base is the default branch.
func (s *Service) Diff(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area) (*DiffResult, error) {
	f, g, err := s.loadGate(ctx, uniqueID, area)
	if err != nil {
		return nil, err
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	path := specPath(f, area)
	ref := s.readRef(f)
	head, err := s.git.LatestCommit(ctx, token, ref, path)
	if err != nil && !errors.Is(err, git.ErrNotFound) {
		return nil, auth.MapGitError(err)
	}
	res := &DiffResult{Head: head, HeadURL: s.git.CommitURL(head)}
	base := s.defaultBranch
	res.BaseLabel = "default_branch"
	if f.ApprovalRequired && g.ApprovedCommit != nil {
		base = *g.ApprovedCommit
		res.BaseLabel = "approved"
		res.ApprovedBy, res.ApprovedAt = g.ApprovedByName, g.ApprovedAt
		res.BaseURL = s.git.CommitURL(base)
	}
	res.Base = base
	oldContent, err := s.content(ctx, token, base, path)
	if err != nil {
		return nil, err
	}
	newContent, err := s.content(ctx, token, ref, path)
	if err != nil {
		return nil, err
	}
	res.Lines = markdown.Diff(oldContent, newContent)
	return res, nil
}

func (s *Service) content(ctx context.Context, token, ref, path string) (string, error) {
	f, err := s.git.GetFile(ctx, token, ref, path)
	if errors.Is(err, git.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", auth.MapGitError(err)
	}
	return string(f.Content), nil
}

// HistoryItem is a gate history entry.
type HistoryItem struct {
	ID        uuid.UUID            `json:"id"`
	Type      domain.GateEventType `json:"type"`
	Actor     *string              `json:"actor"`
	IsAgent   bool                 `json:"isAgent"`
	Commit    *string              `json:"commit"`
	CommitURL *string              `json:"commitUrl"`
	CreatedAt time.Time            `json:"createdAt"`
}

// History lists events of the area, including those of earlier deleted gates.
func (s *Service) History(ctx context.Context, uniqueID string, area domain.Area, page httpx.Page) (httpx.List[HistoryItem], error) {
	f, err := features.Load(ctx, s.store, uniqueID)
	if err != nil {
		return httpx.List[HistoryItem]{}, err
	}
	evs, err := s.store.History(ctx, f.ID, area, page)
	if err != nil {
		return httpx.List[HistoryItem]{}, err
	}
	items := make([]HistoryItem, 0, len(evs))
	for _, e := range evs {
		it := HistoryItem{ID: e.ID, Type: e.Type, Actor: e.ActorName, IsAgent: e.IsAgent, Commit: e.CommitSHA, CreatedAt: e.CreatedAt}
		if e.CommitSHA != nil {
			u := s.git.CommitURL(*e.CommitSHA)
			it.CommitURL = &u
		}
		items = append(items, it)
	}
	return httpx.NewList(items, page.Limit, func(h HistoryItem) (time.Time, string) { return h.CreatedAt, h.ID.String() }), nil
}

// Meta is GET /gates/{area}.
func (s *Service) Meta(ctx context.Context, uniqueID string, area domain.Area) (*specdata.GateDTO, error) {
	_, g, err := s.loadGate(ctx, uniqueID, area)
	if err != nil {
		return nil, err
	}
	d := specdata.ToGateDTO(*g)
	return &d, nil
}

// RecordTransition counts a gate transition metric.
func RecordTransition(area domain.Area, to string) {
	metrics.GateTransitions.WithLabelValues(string(area), to).Inc()
}

// SpecChanges lists the rows of the section «Изменения в других решениях» of the
// product document with their state in the branch (FTR.HMR.DEV-0002 R7).
func (s *Service) SpecChanges(ctx context.Context, p *domain.Principal, uniqueID string) ([]specchanges.Item, error) {
	f, err := features.Load(ctx, s.store, uniqueID)
	if err != nil {
		return nil, err
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	items := []specchanges.Item{}
	if s.Changes == nil {
		return items, nil
	}
	file, err := s.git.GetFile(ctx, token, s.readRef(f), specPath(f, domain.AreaProduct))
	if errors.Is(err, git.ErrNotFound) {
		return items, nil
	}
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	ref := *f
	ref.Branch = s.readRef(f)
	got, err := s.Changes.Items(ctx, token, &ref, string(file.Content))
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	return append(items, got...), nil
}
