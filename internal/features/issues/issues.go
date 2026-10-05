// Package issues implements issues — Ideas and Problems (FTR.HMR.CMN-0002 R1–R9):
// creation (which starts Discovery), the Research list, the issue card,
// verification actions (accept, reject, merge, move, reopen) and the keys
// ISS.<DOMAIN>-NNNN with old keys kept after a move.
package issues

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/discovery"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// MetricTester dry-runs a metric query (metricsources.Service).
type MetricTester interface {
	Test(ctx context.Context, source, query string) (float64, error)
}

// Service implements issue use cases.
type Service struct {
	store    specdata.Store
	features *features.Service
	metrics  MetricTester
	events   events.Publisher
	now      func() time.Time
}

// NewService creates the service.
func NewService(store specdata.Store, fs *features.Service, mt MetricTester, ev events.Publisher) *Service {
	return &Service{store: store, features: fs, metrics: mt, events: ev, now: time.Now}
}

// Errors.
var (
	ErrNotFound     = apperr.NotFound("issue_not_found", "issue not found")
	errReasonNeeded = apperr.Unprocessable("reason_required", "a reason is required")
)

// CreateInput is the body of POST /issues.
type CreateInput struct {
	Type          domain.IssueType `json:"type"`
	Domain        string           `json:"domain"`
	Title         string           `json:"title"`
	Description   string           `json:"description"`
	AttachmentIDs []uuid.UUID      `json:"attachmentIds"`
	// ViaNabu marks an issue the personal agent of Nabu created on behalf of
	// the user (FTR.HMR.CMN-0006 tech §3.6: history "via the Nabu agent").
	ViaNabu bool `json:"-"`
}

func (s *Service) cd() *cycledata.DB { return cycledata.New(s.store.Q()) }

func (s *Service) publish(ctx context.Context, key string, st domain.IssueStatus) {
	s.events.Publish(ctx, events.Event{Type: events.IssueUpdated, Data: map[string]any{"key": key, "status": st}})
	s.events.Publish(ctx, events.Event{Type: events.FocusChanged, Data: map[string]any{"key": key}})
}

// Create creates an issue; any user may create one (R1). Discovery starts at once (R4);
// without the agent the issue goes straight to verification (FTR.HMR.CMN-0006 R9).
func (s *Service) Create(ctx context.Context, p *domain.Principal, in CreateInput) (*cycledata.Issue, error) {
	if in.Type != domain.IssueIdea && in.Type != domain.IssueProblem {
		return nil, apperr.Unprocessable("invalid_type", "type must be idea or problem")
	}
	if strings.TrimSpace(in.Domain) == "" {
		return nil, apperr.Unprocessable("domain_required", "domain is required")
	}
	title := strings.TrimSpace(in.Title)
	if title == "" || len([]rune(title)) > 200 {
		return nil, apperr.Unprocessable("invalid_title", "title is required (up to 200 characters)")
	}
	if len(in.Description) > 50000 {
		return nil, apperr.Unprocessable("invalid_description", "description is too long")
	}
	var created *cycledata.Issue
	err := s.store.InTx(ctx, func(tx specdata.Store) error {
		cd := cycledata.New(tx.Q())
		domainID, _, err := cd.DomainID(ctx, in.Domain)
		if errors.Is(err, cycledata.ErrNotFound) {
			return apperr.Unprocessable("unknown_domain", "domain is not in the dictionary")
		}
		if err != nil {
			return err
		}
		n, err := cd.NextIssueNumber(ctx, domainID)
		if err != nil {
			return err
		}
		is := &cycledata.Issue{Key: domain.IssueKey(in.Domain, n), DomainID: domainID, Domain: in.Domain, Number: n, Type: in.Type,
			Title: title, Description: in.Description, Source: "manual", Status: domain.IssueNew, AuthorID: &p.UserID}
		if err := cd.InsertIssue(ctx, is); err != nil {
			return err
		}
		if len(in.AttachmentIDs) > 0 {
			if err := cd.LinkIssueAttachments(ctx, is.ID, p.UserID, in.AttachmentIDs); err != nil {
				return apperr.Unprocessable("invalid_attachment", err.Error())
			}
		}
		var via map[string]any
		if in.ViaNabu {
			via = map[string]any{"via": "nabu"}
		}
		if err := cd.AddActivity(ctx, "issue", is.ID, "created", &p.UserID, in.ViaNabu, via); err != nil {
			return err
		}
		if err := discovery.Start(ctx, tx.Q(), is.ID, nil); err != nil {
			return err
		}
		created = is
		return nil
	})
	if err != nil {
		return nil, err
	}
	if domain.AgentDisabled() {
		created.Status = domain.IssueVerification
	}
	s.publish(ctx, created.Key, created.Status)
	return created, nil
}

// Card is GET /issues/{key}.
type Card struct {
	*cycledata.Issue
	MovedFrom   []string                    `json:"movedFrom"`
	Features    []string                    `json:"features"`
	Releases    []string                    `json:"releases"`
	Attachments []cycledata.IssueAttachment `json:"attachments"`
	MergedFrom  []string                    `json:"mergedFrom"`
	Activity    []cycledata.Activity        `json:"activity"`
	TokensIn    int64                       `json:"tokensIn"`
	TokensOut   int64                       `json:"tokensOut"`
	Permissions Permissions                 `json:"permissions"`
}

// Permissions are the actions available to the user.
type Permissions struct {
	Verify   bool `json:"verify"` // accept, reject, merge, move
	Reopen   bool `json:"reopen"`
	Discover bool `json:"discover"` // restart Discovery after a failure
}

// Moved is returned when a former key is requested; the handler answers 308.
type Moved struct{ Key string }

func (m *Moved) Error() string { return "moved to " + m.Key }

func (s *Service) load(ctx context.Context, key string) (*cycledata.Issue, bool, error) {
	is, moved, err := s.cd().IssueByKey(ctx, key)
	if errors.Is(err, cycledata.ErrNotFound) {
		return nil, false, ErrNotFound
	}
	return is, moved, err
}

// Get builds the issue card.
func (s *Service) Get(ctx context.Context, p *domain.Principal, key string) (*Card, error) {
	is, moved, err := s.load(ctx, key)
	if err != nil {
		return nil, err
	}
	if moved {
		return nil, &Moved{Key: is.Key}
	}
	cd := s.cd()
	c := &Card{Issue: is}
	if c.MovedFrom, err = cd.IssueAliases(ctx, is.ID); err != nil {
		return nil, err
	}
	if c.Features, err = cd.IssueFeatureKeys(ctx, is.ID); err != nil {
		return nil, err
	}
	if c.Releases, err = cd.IssueReleaseKeys(ctx, is.ID); err != nil {
		return nil, err
	}
	if c.Attachments, err = cd.IssueAttachments(ctx, is.ID); err != nil {
		return nil, err
	}
	rows, err := s.store.Q().Query(ctx, `SELECT key FROM issues WHERE merged_into_id = $1 ORDER BY key`, is.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return nil, err
		}
		c.MergedFrom = append(c.MergedFrom, k)
	}
	rows.Close()
	if c.Activity, err = cd.ActivityOf(ctx, "issue", is.ID); err != nil {
		return nil, err
	}
	if c.TokensIn, c.TokensOut, err = cd.UsageTotals(ctx, "issue_id", is.ID); err != nil {
		return nil, err
	}
	for _, l := range []*[]string{&c.MovedFrom, &c.Features, &c.Releases, &c.MergedFrom} {
		if *l == nil {
			*l = []string{}
		}
	}
	if c.Activity == nil {
		c.Activity = []cycledata.Activity{}
	}
	expert := p.IsExpertOf(is.Domain)
	c.Permissions = Permissions{
		Verify:   expert && is.Status == domain.IssueVerification,
		Reopen:   expert && is.Status == domain.IssueRejected,
		Discover: expert && !domain.AgentDisabled() && (is.Status == domain.IssueNew || is.Status == domain.IssueDiscovery),
	}
	return c, nil
}

// List is GET /issues.
func (s *Service) List(ctx context.Context, f cycledata.IssueFilter) ([]cycledata.Issue, error) {
	return s.cd().ListIssues(ctx, f)
}

func (s *Service) loadForAction(ctx context.Context, p *domain.Principal, key string) (*cycledata.Issue, error) {
	is, _, err := s.load(ctx, key)
	if err != nil {
		return nil, err
	}
	if !p.IsExpertOf(is.Domain) {
		return nil, apperr.Forbidden("forbidden", "expert of domain "+is.Domain+" required").With("domain", is.Domain)
	}
	return is, nil
}

// AcceptInput is the body of POST /issues/{key}/accept.
type AcceptInput struct {
	System        string `json:"system"`
	ParentFeature string `json:"parentFeature"`
	NoFeature     bool   `json:"noFeature"`
}

// Accept turns a verified issue into a feature (R6, R7).
func (s *Service) Accept(ctx context.Context, p *domain.Principal, key string, in AcceptInput) (string, error) {
	is, err := s.loadForAction(ctx, p, key)
	if err != nil {
		return "", err
	}
	if is.Status != domain.IssueVerification {
		return "", apperr.Conflict("not_in_verification", "only an issue in verification can be accepted").With("status", is.Status)
	}
	cd := s.cd()
	doc, err := cd.Discovery(ctx, is.ID)
	if err != nil {
		return "", err
	}
	if missing := discovery.Missing(doc); len(missing) > 0 {
		return "", apperr.Unprocessable("discovery_incomplete", "Discovery has no value or no way to measure it").With("missing", missing)
	}
	if s.metrics != nil {
		if _, err := s.metrics.Test(ctx, doc.Measure.Source, doc.Measure.Query); err != nil {
			return "", err
		}
		if err := cd.MarkMeasureChecked(ctx, is.ID, s.now()); err != nil {
			return "", err
		}
	}
	var sys *specdata.System
	var parent *specdata.Feature
	switch {
	case is.Type == domain.IssueProblem && in.ParentFeature != "" && !in.NoFeature:
		pf, err := s.store.FeatureByUniqueID(ctx, in.ParentFeature)
		if errors.Is(err, specdata.ErrNotFound) || (err == nil && pf.Phase == domain.PhaseDeleted) {
			return "", apperr.Unprocessable("unknown_parent", "the feature with the error is not found")
		}
		if err != nil {
			return "", err
		}
		parent = pf
		if sys, err = s.store.SystemByKeys(ctx, pf.DomainKey, pf.SystemKey); err != nil {
			return "", err
		}
	default:
		if in.System == "" {
			return "", apperr.Unprocessable("system_required", "choose a system of the domain")
		}
		dk, sk, ok := strings.Cut(in.System, "/")
		if !ok {
			dk, sk = is.Domain, in.System
		}
		if dk != is.Domain {
			return "", apperr.Unprocessable("system_of_other_domain", "the system must belong to the issue's domain; move the issue first")
		}
		sys, err = s.store.SystemByKeys(ctx, dk, sk)
		if errors.Is(err, specdata.ErrNotFound) {
			return "", apperr.Unprocessable("unknown_system", "system is not in the dictionary")
		}
		if err != nil {
			return "", err
		}
	}
	var featureKey string
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		tcd := cycledata.New(tx.Q())
		// Issues merged into this one are linked to the feature too (R8).
		ids := []uuid.UUID{is.ID}
		rows, err := tx.Q().Query(ctx, `SELECT id FROM issues WHERE merged_into_id = $1`, is.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		f, err := s.features.CreateFromIssue(ctx, tx, p, features.FromIssue{
			System: sys, Title: is.Title, Parent: parent, IsProblem: is.Type == domain.IssueProblem && parent == nil,
			IssueIDs: ids, Discovery: discoveryMarkdown(is, doc), Measure: doc.Measure,
		})
		if err != nil {
			return err
		}
		if err := tcd.SetIssueStatus(ctx, is.ID, domain.IssueAccepted); err != nil {
			return err
		}
		featureKey = f.UniqueID
		return tcd.AddActivity(ctx, "issue", is.ID, "accepted", &p.UserID, false, map[string]any{"feature": f.UniqueID})
	})
	if err != nil {
		return "", err
	}
	s.publish(ctx, is.Key, domain.IssueAccepted)
	s.events.Publish(ctx, events.Event{Type: events.FeatureUpdated, Data: map[string]any{"uniqueId": featureKey}})
	return featureKey, nil
}

func discoveryMarkdown(is *cycledata.Issue, doc *cycledata.Discovery) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Discovery: %s %s\n\n", is.Key, is.Title)
	b.WriteString(strings.TrimSpace(doc.Content))
	b.WriteString("\n")
	return b.String()
}

// Reject closes an issue with a reason visible to the author (R5).
func (s *Service) Reject(ctx context.Context, p *domain.Principal, key, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errReasonNeeded
	}
	is, err := s.loadForAction(ctx, p, key)
	if err != nil {
		return err
	}
	switch is.Status {
	case domain.IssueNew, domain.IssueDiscovery, domain.IssueVerification:
	default:
		return apperr.Conflict("issue_closed", "the issue cannot be rejected in status "+string(is.Status))
	}
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		cd := cycledata.New(tx.Q())
		if err := discovery.Cancel(ctx, tx.Q(), is.ID); err != nil {
			return err
		}
		if err := cd.RejectIssue(ctx, is.ID, reason); err != nil {
			return err
		}
		return cd.AddActivity(ctx, "issue", is.ID, "rejected", &p.UserID, false, map[string]any{"reason": reason})
	})
	if err != nil {
		return err
	}
	s.publish(ctx, is.Key, domain.IssueRejected)
	return nil
}

// Reopen returns a rejected issue to "new" and runs Discovery again.
func (s *Service) Reopen(ctx context.Context, p *domain.Principal, key string) error {
	is, err := s.loadForAction(ctx, p, key)
	if err != nil {
		return err
	}
	if is.Status != domain.IssueRejected {
		return apperr.Conflict("not_rejected", "only a rejected issue can be reopened")
	}
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		cd := cycledata.New(tx.Q())
		if err := cd.ReopenIssue(ctx, is.ID); err != nil {
			return err
		}
		if err := cd.AddActivity(ctx, "issue", is.ID, "reopened", &p.UserID, false, nil); err != nil {
			return err
		}
		return discovery.Start(ctx, tx.Q(), is.ID, nil)
	})
	if err != nil {
		return err
	}
	s.publish(ctx, is.Key, domain.IssueNew)
	return nil
}

// Rediscover restarts Discovery (after a failure or to refresh it).
func (s *Service) Rediscover(ctx context.Context, p *domain.Principal, key string) error {
	if domain.AgentDisabled() {
		return apperr.AgentDisabled()
	}
	is, err := s.loadForAction(ctx, p, key)
	if err != nil {
		return err
	}
	switch is.Status {
	case domain.IssueNew, domain.IssueDiscovery, domain.IssueVerification:
	default:
		return apperr.Conflict("issue_closed", "Discovery cannot run in status "+string(is.Status))
	}
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		if err := discovery.Cancel(ctx, tx.Q(), is.ID); err != nil {
			return err
		}
		return discovery.Start(ctx, tx.Q(), is.ID, nil)
	})
	if err != nil {
		return err
	}
	s.publish(ctx, is.Key, is.Status)
	return nil
}

// Merge closes a duplicate with a link to the main issue; features of the main
// issue get a link to the duplicate too (R8).
func (s *Service) Merge(ctx context.Context, p *domain.Principal, key, into string) error {
	dup, err := s.loadForAction(ctx, p, key)
	if err != nil {
		return err
	}
	main, _, err := s.load(ctx, into)
	if err != nil {
		return err
	}
	if main.ID == dup.ID {
		return apperr.Unprocessable("merge_into_self", "an issue cannot be merged into itself")
	}
	if main.Status == domain.IssueMerged || main.Status == domain.IssueRejected {
		return apperr.Conflict("main_closed", "the main issue is closed")
	}
	switch dup.Status {
	case domain.IssueNew, domain.IssueDiscovery, domain.IssueVerification:
	default:
		return apperr.Conflict("issue_closed", "the issue cannot be merged in status "+string(dup.Status))
	}
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		cd := cycledata.New(tx.Q())
		if err := discovery.Cancel(ctx, tx.Q(), dup.ID); err != nil {
			return err
		}
		if err := cd.MergeIssue(ctx, dup.ID, main.ID); err != nil {
			return err
		}
		if _, err := tx.Q().Exec(ctx, `INSERT INTO feature_issues (feature_id, issue_id)
			SELECT fi.feature_id, $2 FROM feature_issues fi JOIN features f ON f.id = fi.feature_id
			WHERE fi.issue_id = $1 AND f.phase <> 'deleted' ON CONFLICT DO NOTHING`, main.ID, dup.ID); err != nil {
			return err
		}
		if err := cd.AddActivity(ctx, "issue", dup.ID, "merged", &p.UserID, false, map[string]any{"into": main.Key}); err != nil {
			return err
		}
		return cd.AddActivity(ctx, "issue", main.ID, "merged_from", &p.UserID, false, map[string]any{"issue": dup.Key})
	})
	if err != nil {
		return err
	}
	s.publish(ctx, dup.Key, domain.IssueMerged)
	return nil
}

// Move gives the issue a key in another domain; the old key keeps working (R2).
func (s *Service) Move(ctx context.Context, p *domain.Principal, key, target string) (string, error) {
	is, err := s.loadForAction(ctx, p, key)
	if err != nil {
		return "", err
	}
	if is.Domain == target {
		return is.Key, nil
	}
	switch is.Status {
	case domain.IssueNew, domain.IssueDiscovery, domain.IssueVerification:
	default:
		return "", apperr.Conflict("issue_closed", "the issue cannot be moved in status "+string(is.Status))
	}
	var newKey string
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		cd := cycledata.New(tx.Q())
		domainID, _, err := cd.DomainID(ctx, target)
		if errors.Is(err, cycledata.ErrNotFound) {
			return apperr.Unprocessable("unknown_domain", "domain is not in the dictionary")
		}
		if err != nil {
			return err
		}
		n, err := cd.NextIssueNumber(ctx, domainID)
		if err != nil {
			return err
		}
		newKey = domain.IssueKey(target, n)
		if err := cd.MoveIssue(ctx, is, domainID, newKey, n); err != nil {
			return err
		}
		return cd.AddActivity(ctx, "issue", is.ID, "moved", &p.UserID, false, map[string]any{"from": is.Key, "to": newKey})
	})
	if err != nil {
		return "", err
	}
	s.publish(ctx, newKey, is.Status)
	return newKey, nil
}

// ─── HTTP ────────────────────────────────────────────────────────────

// Routes mounts /api/v1/issues.
func (s *Service) Routes(r chi.Router) {
	r.Get("/issues", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		page, err := httpx.ParsePage(r)
		if err != nil {
			return err
		}
		q := r.URL.Query()
		items, err := s.List(r.Context(), cycledata.IssueFilter{UserID: p.UserID, Domain: q.Get("domain"), Type: q.Get("type"),
			Status: q.Get("status"), Source: q.Get("source"), Query: q.Get("q"), Page: page})
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, httpx.NewList(items, page.Limit, func(i cycledata.Issue) (time.Time, string) { return i.CreatedAt, i.ID.String() }))
		return nil
	}))
	r.Post("/issues", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in CreateInput
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		is, err := s.Create(r.Context(), p, in)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusCreated, map[string]string{"key": is.Key})
		return nil
	}))
	r.Get("/issues/{key}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		c, err := s.Get(r.Context(), p, chi.URLParam(r, "key"))
		var moved *Moved
		if errors.As(err, &moved) {
			w.Header().Set("Location", "/api/v1/issues/"+moved.Key)
			httpx.JSON(w, http.StatusPermanentRedirect, map[string]string{"key": moved.Key})
			return nil
		}
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, c)
		return nil
	}))
	action := func(fn func(r *http.Request, p *domain.Principal, key string) (any, error)) http.Handler {
		return httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			p, err := httpx.MustPrincipal(r)
			if err != nil {
				return err
			}
			out, err := fn(r, p, chi.URLParam(r, "key"))
			if err != nil {
				return err
			}
			if out == nil {
				httpx.NoContent(w)
				return nil
			}
			httpx.JSON(w, 200, out)
			return nil
		})
	}
	r.Method(http.MethodPost, "/issues/{key}/accept", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in AcceptInput
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		k, err := s.Accept(r.Context(), p, chi.URLParam(r, "key"), in)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusCreated, map[string]string{"featureKey": k})
		return nil
	}))
	r.Method(http.MethodPost, "/issues/{key}/reject", action(func(r *http.Request, p *domain.Principal, key string) (any, error) {
		var in struct {
			Reason string `json:"reason"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return nil, err
		}
		return nil, s.Reject(r.Context(), p, key, in.Reason)
	}))
	r.Method(http.MethodPost, "/issues/{key}/reopen", action(func(r *http.Request, p *domain.Principal, key string) (any, error) {
		return nil, s.Reopen(r.Context(), p, key)
	}))
	r.Method(http.MethodPost, "/issues/{key}/discover", action(func(r *http.Request, p *domain.Principal, key string) (any, error) {
		return nil, s.Rediscover(r.Context(), p, key)
	}))
	r.Method(http.MethodPost, "/issues/{key}/merge", action(func(r *http.Request, p *domain.Principal, key string) (any, error) {
		var in struct {
			Into string `json:"into"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return nil, err
		}
		return nil, s.Merge(r.Context(), p, key, in.Into)
	}))
	r.Method(http.MethodPost, "/issues/{key}/move", action(func(r *http.Request, p *domain.Principal, key string) (any, error) {
		var in struct {
			Domain string `json:"domain"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return nil, err
		}
		k, err := s.Move(r.Context(), p, key, in.Domain)
		if err != nil {
			return nil, err
		}
		return map[string]string{"key": k}, nil
	}))
}
