// Package features implements features (solutions, FTR): creation from an
// accepted issue, the feature list and card, deletion before a release exists,
// the flag key, requirements traceability and edit locks.
package features

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/markdown"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Service implements feature use cases.
type Service struct {
	store         specdata.Store
	git           git.Provider
	tokens        git.TokenSource
	events        events.Publisher
	defaultBranch string
}

// NewService creates the service.
func NewService(store specdata.Store, provider git.Provider, tokens git.TokenSource, ev events.Publisher, defaultBranch string) *Service {
	return &Service{store: store, git: provider, tokens: tokens, events: ev, defaultBranch: defaultBranch}
}

// ErrFeatureNotFound is the 404 for unknown features.
var ErrFeatureNotFound = apperr.NotFound("feature_not_found", "feature not found")

// Load returns a live feature: 404 if unknown, 410 if deleted.
func Load(ctx context.Context, store specdata.Store, uniqueID string) (*specdata.Feature, error) {
	f, err := store.FeatureByUniqueID(ctx, uniqueID)
	if errors.Is(err, specdata.ErrNotFound) {
		return nil, ErrFeatureNotFound
	}
	if err != nil {
		return nil, err
	}
	if f.Phase == domain.PhaseDeleted {
		e := apperr.Gone("feature_deleted", "feature was deleted")
		e.With("uniqueId", f.UniqueID)
		if f.DeletedByName != nil {
			e.With("deletedBy", *f.DeletedByName)
		}
		if f.DeletedAt != nil {
			e.With("deletedAt", f.DeletedAt)
		}
		return nil, e
	}
	return f, nil
}

// RequireExpert returns 403 unless the principal is an expert of the feature's domain.
func RequireExpert(p *domain.Principal, domainKey string) error {
	if !p.IsExpertOf(domainKey) {
		return apperr.Forbidden("forbidden", "expert of domain "+domainKey+" required").With("domain", domainKey)
	}
	return nil
}

// RequireSpecPhase returns 409 unless the specification is editable.
func RequireSpecPhase(f *specdata.Feature) error {
	switch f.Phase {
	case domain.PhaseSpec:
		return nil
	case domain.PhaseCodegen:
		return apperr.Conflict("codegen_in_progress", "specifications are read-only during code generation")
	}
	return apperr.Conflict("feature_read_only", "the feature is read-only in phase "+string(f.Phase)).With("phase", f.Phase)
}

// Template reads the rules template of an area from the default branch.
// A missing template falls back to an empty document with a title.
func Template(ctx context.Context, provider git.Provider, token, defaultBranch string, area domain.Area, fix bool) string {
	f, err := provider.GetFile(ctx, token, defaultBranch, git.RulePath(string(area), fix))
	if err != nil {
		slog.WarnContext(ctx, "rules template unavailable, using a blank document", "area", area, "fix", fix, "err", err)
		if fix {
			return "---\nparent: <parent>\n---\n\n# <title>\n"
		}
		return "# <title>\n"
	}
	return string(f.Content)
}

// FromIssue describes a feature created from accepted issues (R7).
type FromIssue struct {
	System    *specdata.System
	Title     string
	Parent    *specdata.Feature // Problem with a known feature → fix feature
	IsProblem bool
	IssueIDs  []uuid.UUID
	Discovery string // markdown copied as discovery.md
	Measure   *cycledata.Measure
}

// MetricTable renders the success metric for the product specification.
func MetricTable(m *cycledata.Measure) string {
	if m == nil {
		return ""
	}
	esc := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "|", `\|`), "\n", " ") }
	return fmt.Sprintf("| Source | Query | Target | Window |\n| --- | --- | --- | --- |\n| %s | `%s` | %s | %s |",
		esc(m.Source), esc(m.Query), esc(m.Target), esc(m.Window))
}

// CreateFromIssue creates a feature: number, branch, product gate from the rules
// template with the success metric, discovery.md and a spec PR. It runs in the
// caller's transaction (the issue acceptance), so the number is not spent if
// anything fails; a failed git call deletes the branch.
func (s *Service) CreateFromIssue(ctx context.Context, tx specdata.Store, p *domain.Principal, in FromIssue) (*specdata.Feature, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return nil, apperr.Unprocessable("invalid_title", "title is required")
	}
	if r := []rune(title); len(r) > 200 {
		title = string(r[:200])
	}
	sys := in.System
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	n, err := tx.NextNumber(ctx, sys.ID)
	if err != nil {
		return nil, err
	}
	uid := domain.FeatureKey(sys.DomainKey, sys.Key, n)
	branch := git.FeatureBranch(uid)
	base, err := s.git.BranchHead(ctx, token, s.defaultBranch)
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	parentID := ""
	if in.Parent != nil {
		parentID = in.Parent.UniqueID
	}
	doc := markdown.RenderTemplate(Template(ctx, s.git, token, s.defaultBranch, domain.AreaProduct, in.Parent != nil), title, parentID)
	if in.Measure != nil {
		doc = markdown.FillSection(doc, "Success metrics", MetricTable(in.Measure), "success metric", "метрики успеха", "erfolgsmetrik", "métricas de éxito", "成功指标")
	}
	if err := s.git.CreateBranch(ctx, token, branch, base); err != nil {
		return nil, auth.MapGitError(err)
	}
	cleanup := func(cause error) error {
		if derr := s.git.DeleteBranch(context.WithoutCancel(ctx), token, branch); derr != nil {
			slog.ErrorContext(ctx, "delete branch after failed create", "branch", branch, "err", derr)
		}
		return auth.MapGitError(cause)
	}
	files := []git.FileChange{{Path: git.SpecPath(sys.DomainKey, sys.Key, uid, string(domain.AreaProduct)), Content: []byte(doc)}}
	if strings.TrimSpace(in.Discovery) != "" {
		files = append(files, git.FileChange{Path: git.DiscoveryPath(sys.DomainKey, sys.Key, uid), Content: []byte(in.Discovery)})
	}
	msg := git.Trailers{Feature: uid, Area: string(domain.AreaProduct)}.Message(fmt.Sprintf("%s: create feature", uid))
	sha, err := s.git.Commit(ctx, token, branch, msg, files)
	if err != nil {
		return nil, cleanup(err)
	}
	body := fmt.Sprintf("Hammurapi feature **%s**: %s\n\nThe specification PR is merged when the release is confirmed.", uid, title)
	if in.Parent != nil {
		body += fmt.Sprintf("\n\nFix of %s.", in.Parent.UniqueID)
	}
	pr, err := s.git.CreatePR(ctx, token, branch, s.defaultBranch, fmt.Sprintf("%s %s", uid, title), body)
	if err != nil {
		return nil, cleanup(err)
	}
	var metric []byte
	if in.Measure != nil {
		metric, _ = json.Marshal(in.Measure)
	}
	f := &specdata.Feature{UniqueID: uid, SystemID: sys.ID, DomainKey: sys.DomainKey, SystemKey: sys.Key,
		ApprovalRequired: sys.ApprovalRequired, Number: n, Title: title, Branch: branch, PRNumber: pr.Number, PRURL: pr.URL,
		Phase: domain.PhaseSpec, IsProblem: in.IsProblem, Metric: metric, CreatedBy: p.UserID, CreatedByName: p.DisplayName}
	if in.Parent != nil {
		f.ParentID, f.ParentUniqueID = &in.Parent.ID, &in.Parent.UniqueID
	}
	if err := tx.InsertFeature(ctx, f); err != nil {
		return nil, err
	}
	g := &specdata.Gate{FeatureID: f.ID, Area: domain.AreaProduct, Status: domain.GateDraft, HeadCommit: sha, CreatedBy: p.UserID}
	if err := tx.InsertGate(ctx, g); err != nil {
		return nil, err
	}
	if err := tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventCreated, ActorID: &p.UserID, CommitSHA: &sha}); err != nil {
		return nil, err
	}
	for _, id := range in.IssueIDs {
		if err := tx.LinkIssue(ctx, f.ID, id); err != nil {
			return nil, err
		}
	}
	if err := cycledata.New(tx.Q()).AddActivity(ctx, "feature", f.ID, "created", &p.UserID, false, map[string]any{"issues": len(in.IssueIDs)}); err != nil {
		return nil, err
	}
	return f, nil
}

// Card is the feature card of GET /features/{key}.
type Card struct {
	UniqueID         string                `json:"uniqueId"`
	Domain           string                `json:"domain"`
	System           string                `json:"system"`
	Title            string                `json:"title"`
	Phase            domain.FeaturePhase   `json:"phase"`
	ApprovalRequired bool                  `json:"approvalRequired"`
	IsProblem        bool                  `json:"isProblem"`
	Imported         bool                  `json:"imported"`
	Branch           string                `json:"branch"`
	PR               PRRef                 `json:"pr"`
	Parent           *string               `json:"parent"`
	Fixes            []specdata.FeatureRef `json:"fixes"`
	Issues           []string              `json:"issues"`
	Services         []ServiceRef          `json:"services"`
	Release          *string               `json:"release"`
	Metric           json.RawMessage       `json:"metric"`
	FlagKey          *string               `json:"flagKey"`
	Gates            []specdata.GateDTO    `json:"gates"`
	PendingGates     []domain.Area         `json:"pendingGates"` // tech/qa not generated yet
	Lock             *specdata.LockDTO     `json:"lock"`
	Workflow         *WorkflowRef          `json:"workflow"`
	TokensIn         int64                 `json:"tokensIn"`
	TokensOut        int64                 `json:"tokensOut"`
	CreatedBy        string                `json:"createdBy"`
	CreatedAt        time.Time             `json:"createdAt"`
	Permissions      Permissions           `json:"permissions"`
}

// ServiceRef is an affected service.
type ServiceRef struct {
	Key      string          `json:"key"`
	Repo     string          `json:"repo"`
	Autonomy domain.Autonomy `json:"autonomy"`
}

// WorkflowRef is the state of the latest gate generation run.
type WorkflowRef struct {
	Kind      string  `json:"kind"`
	State     string  `json:"state"`
	Step      *string `json:"step"`
	LastError *string `json:"lastError"`
}

// PRRef is a PR/MR reference.
type PRRef struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// Permissions tells the UI which actions the current user has.
type Permissions struct {
	Edit       []domain.Area `json:"edit"`
	Submit     []domain.Area `json:"submit"`
	Approve    []domain.Area `json:"approve"`
	DeleteGate []domain.Area `json:"deleteGate"`
	AddGate    []domain.Area `json:"addGate"`
	Regenerate bool          `json:"regenerate"`
	Codegen    bool          `json:"codegen"`
	Delete     bool          `json:"delete"`
	SetFlag    bool          `json:"setFlag"`
}

// Get builds the feature card.
func (s *Service) Get(ctx context.Context, p *domain.Principal, uniqueID string) (*Card, error) {
	f, err := Load(ctx, s.store, uniqueID)
	if err != nil {
		return nil, err
	}
	gates, err := s.store.ActiveGates(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	fixes, err := s.store.Fixes(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	lock, err := s.store.GetLock(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	issues, err := s.store.FeatureIssueKeys(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	if issues == nil {
		issues = []string{}
	}
	c := &Card{UniqueID: f.UniqueID, Domain: f.DomainKey, System: f.SystemKey, Title: f.Title, Phase: f.Phase,
		ApprovalRequired: f.ApprovalRequired, IsProblem: f.IsProblem, Imported: f.Imported, Branch: f.Branch,
		PR: PRRef{Number: f.PRNumber, URL: f.PRURL}, Parent: f.ParentUniqueID, Fixes: fixes, Issues: issues,
		Services: []ServiceRef{}, Metric: f.Metric, FlagKey: f.FlagKey, Gates: specdata.GateDTOs(gates),
		PendingGates: PendingGenerated(gates), Lock: specdata.ToLockDTO(lock), CreatedBy: f.CreatedByName, CreatedAt: f.CreatedAt}
	cd := cycledata.New(s.store.Q())
	svcs, err := cd.FeatureServices(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	for _, sv := range svcs {
		c.Services = append(c.Services, ServiceRef{Key: sv.Key, Repo: sv.Repo, Autonomy: sv.Autonomy})
	}
	r, err := cd.ReleaseByFeature(ctx, f.ID) // nil without a release
	if err != nil {
		return nil, err
	}
	if r != nil {
		c.Release = &r.Key
	}
	var wf WorkflowRef
	err = s.store.Q().QueryRow(ctx, `SELECT kind::text, state, step, last_error FROM workflow_runs
		WHERE subject_id = $1 AND kind = 'gate_generation' ORDER BY created_at DESC LIMIT 1`, f.ID).Scan(&wf.Kind, &wf.State, &wf.Step, &wf.LastError)
	if err == nil {
		c.Workflow = &wf
	}
	if c.TokensIn, c.TokensOut, err = cd.UsageTotals(ctx, "feature_id", f.ID); err != nil {
		return nil, err
	}
	c.Permissions = PermissionsOf(p, f, gates)
	return c, nil
}

// PendingGenerated lists generated areas (tech, qa) without a gate yet.
func PendingGenerated(gates []specdata.Gate) []domain.Area {
	have := map[domain.Area]bool{}
	for _, g := range gates {
		have[g.Area] = true
	}
	out := []domain.Area{}
	for _, a := range []domain.Area{domain.AreaTech, domain.AreaQA} {
		if !have[a] {
			out = append(out, a)
		}
	}
	return out
}

// HumanGatesApproved reports whether every non-generated gate is approved —
// the trigger of tech/qa generation (R12).
func HumanGatesApproved(gates []specdata.Gate) bool {
	n := 0
	for _, g := range gates {
		if g.Generated {
			continue
		}
		n++
		if g.Status != domain.GateApproved {
			return false
		}
	}
	return n > 0
}

// ReadyForCodegen reports whether code generation may start (R16): tech and qa
// exist, and every gate is approved unless the domain does not require approval.
func ReadyForCodegen(f *specdata.Feature, gates []specdata.Gate) bool {
	if len(PendingGenerated(gates)) > 0 {
		return false
	}
	return !f.ApprovalRequired || specdata.AllApproved(gates)
}

// PermissionsOf computes the actions available to the principal (FTR.HMR.CMN-0002 §5).
func PermissionsOf(p *domain.Principal, f *specdata.Feature, gates []specdata.Gate) Permissions {
	perm := Permissions{Edit: []domain.Area{}, Submit: []domain.Area{}, Approve: []domain.Area{}, DeleteGate: []domain.Area{}, AddGate: []domain.Area{}}
	expert := p.IsExpertOf(f.DomainKey)
	perm.Delete = (expert || p.GlobalAdmin) && f.Phase.Active()
	perm.SetFlag = expert && f.Phase != domain.PhaseDeleted && f.Phase != domain.PhaseRolledBack && f.Phase != domain.PhaseReleased &&
		f.Phase != domain.PhaseIndexed
	if f.Phase != domain.PhaseSpec || !expert {
		return perm
	}
	active := map[domain.Area]bool{}
	for _, g := range gates {
		active[g.Area] = true
		if !g.Generated {
			perm.Edit = append(perm.Edit, g.Area)
			if g.Area != domain.AreaProduct {
				perm.DeleteGate = append(perm.DeleteGate, g.Area)
			}
		}
		if f.ApprovalRequired && g.Status == domain.GateDraft {
			perm.Submit = append(perm.Submit, g.Area)
		}
		if f.ApprovalRequired && g.Status == domain.GateInReview && p.CanApprove(f.DomainKey, g.Area) {
			perm.Approve = append(perm.Approve, g.Area)
		}
	}
	for _, a := range []domain.Area{domain.AreaDesign, domain.AreaArch} {
		if !active[a] {
			perm.AddGate = append(perm.AddGate, a)
		}
	}
	// Without the agent nothing is generated (FTR.HMR.CMN-0006 R9).
	perm.Regenerate = !domain.AgentDisabled() && (!f.ApprovalRequired || HumanGatesApproved(gates))
	perm.Codegen = !domain.AgentDisabled() && ReadyForCodegen(f, gates)
	return perm
}

// Delete deletes a feature before a release exists: closes the spec PR and the
// service PRs without merging, cancels running processes and deletes the
// branch. The row is kept (soft delete), the key is never reused; the issues
// return to verification.
func (s *Service) Delete(ctx context.Context, p *domain.Principal, uniqueID, confirm string) error {
	f, err := Load(ctx, s.store, uniqueID)
	if err != nil {
		return err
	}
	if !p.GlobalAdmin {
		if err := RequireExpert(p, f.DomainKey); err != nil {
			return err
		}
	}
	if !f.Phase.Active() {
		return apperr.Conflict("release_exists", "a feature cannot be deleted after its release was created").With("phase", f.Phase)
	}
	if confirm != f.UniqueID {
		return apperr.Unprocessable("confirm_mismatch", "confirmation does not match the feature key")
	}
	if err := LockedByOther(ctx, s.store, f.ID, p.UserID); err != nil {
		return err
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return err
	}
	if err := s.git.ClosePR(ctx, token, f.PRNumber); err != nil && !errors.Is(err, git.ErrNotFound) {
		return auth.MapGitError(err)
	}
	cd := cycledata.New(s.store.Q())
	prs, err := cd.FeaturePRs(ctx, f.ID, "service")
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if pr.State != "open" {
			continue
		}
		rp := s.git.ForRepo(pr.Repo)
		bot, err := rp.BotToken(ctx)
		if err == nil {
			err = rp.ClosePR(ctx, bot, pr.Number)
		}
		if err != nil && !errors.Is(err, git.ErrNotFound) {
			slog.WarnContext(ctx, "close service PR of a deleted feature", "repo", pr.Repo, "pr", pr.Number, "err", err)
		}
	}
	pending := false
	if err := s.git.DeleteBranch(ctx, token, f.Branch); err != nil {
		slog.WarnContext(ctx, "branch deletion failed, cleaner will retry", "branch", f.Branch, "err", err)
		pending = true
	}
	if err := s.store.InTx(ctx, func(tx specdata.Store) error {
		if err := tx.MarkDeleted(ctx, f.ID, p.UserID, pending); err != nil {
			return err
		}
		q := tx.Q()
		if _, err := q.Exec(ctx, `UPDATE workflow_runs SET state = 'cancelled', updated_at = now(), version = version + 1
			WHERE subject_id = $1 AND state NOT IN ('done','succeeded','failed','cancelled','rolled_back')`, f.ID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE workflow_runs SET state = 'cancelled', updated_at = now(), version = version + 1
			WHERE subject_id IN (SELECT id FROM agent_tasks WHERE feature_id = $1) AND state NOT IN ('done','succeeded','failed','cancelled','rolled_back')`, f.ID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE agent_tasks SET status = 'cancelled', finished_at = now(), token_hash = NULL
			WHERE feature_id = $1 AND status IN ('queued','running')`, f.ID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE pull_requests SET state = 'closed' WHERE feature_id = $1 AND state = 'open'`, f.ID); err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE issues SET status = 'verification', updated_at = now()
			WHERE id IN (SELECT issue_id FROM feature_issues WHERE feature_id = $1) AND status = 'accepted'`, f.ID); err != nil {
			return err
		}
		if err := cycledata.New(q).AddActivity(ctx, "feature", f.ID, "deleted", &p.UserID, false, nil); err != nil {
			return err
		}
		return tx.DropLock(ctx, f.ID)
	}); err != nil {
		return err
	}
	s.events.Publish(ctx, events.Event{Type: events.FeatureDeleted, Data: map[string]string{"uniqueId": f.UniqueID}})
	s.events.Publish(ctx, events.Event{Type: events.FocusChanged, Data: map[string]string{"uniqueId": f.UniqueID}})
	return nil
}

// SetFlagKey sets or clears the feature flag key (PATCH /features/{key}).
func (s *Service) SetFlagKey(ctx context.Context, p *domain.Principal, uniqueID string, flag *string) error {
	f, err := Load(ctx, s.store, uniqueID)
	if err != nil {
		return err
	}
	if err := RequireExpert(p, f.DomainKey); err != nil {
		return err
	}
	if flag != nil {
		v := strings.TrimSpace(*flag)
		if len(v) > 200 {
			return apperr.Unprocessable("invalid_flag", "flag key is too long")
		}
		if v == "" {
			flag = nil
		} else {
			flag = &v
		}
	}
	if err := s.store.SetFlagKey(ctx, f.ID, flag); err != nil {
		return err
	}
	s.events.Publish(ctx, events.Event{Type: events.FeatureUpdated, Data: map[string]string{"uniqueId": f.UniqueID}})
	return nil
}

// RequirementRow is a row of GET /features/{key}/requirements.
type RequirementRow struct {
	ID        string             `json:"id"`
	Text      string             `json:"text"`
	Services  []string           `json:"services"`
	TestCases []TestCaseRef      `json:"testCases"`
	PRs       []RequirementPRRef `json:"prs"`
}

// TestCaseRef is a test case covering a requirement.
type TestCaseRef struct {
	ID    string `json:"id"`
	Level string `json:"level"`
	Title string `json:"title"`
}

// RequirementPRRef is a PR closing a requirement.
type RequirementPRRef struct {
	Service *string `json:"service"`
	Number  int     `json:"number"`
	URL     string  `json:"url"`
	ByAgent bool    `json:"byAgent"`
	State   string  `json:"state"`
}

// Requirements builds the traceability matrix requirement → test case → PR (R21).
func (s *Service) Requirements(ctx context.Context, uniqueID string) ([]RequirementRow, error) {
	f, err := Load(ctx, s.store, uniqueID)
	if err != nil {
		return nil, err
	}
	return Matrix(ctx, cycledata.New(s.store.Q()), f.ID)
}

// Matrix builds the traceability matrix of a feature.
func Matrix(ctx context.Context, cd *cycledata.DB, featureID uuid.UUID) ([]RequirementRow, error) {
	reqs, err := cd.Requirements(ctx, featureID)
	if err != nil {
		return nil, err
	}
	tcs, err := cd.TestCases(ctx, featureID)
	if err != nil {
		return nil, err
	}
	prs, err := cd.FeaturePRs(ctx, featureID, "service")
	if err != nil {
		return nil, err
	}
	out := make([]RequirementRow, 0, len(reqs))
	for _, r := range reqs {
		row := RequirementRow{ID: r.ID, Text: r.Text, Services: r.Services, TestCases: []TestCaseRef{}, PRs: []RequirementPRRef{}}
		if row.Services == nil {
			row.Services = []string{}
		}
		for _, tc := range tcs {
			for _, id := range tc.ReqIDs {
				if id == r.ID {
					row.TestCases = append(row.TestCases, TestCaseRef{ID: tc.ID, Level: tc.Level, Title: tc.Title})
					break
				}
			}
		}
		for _, pr := range prs {
			if pr.State == "closed" {
				continue
			}
			for _, id := range pr.ReqIDs {
				if id == r.ID {
					row.PRs = append(row.PRs, RequirementPRRef{Service: pr.Service, Number: pr.Number, URL: pr.URL, ByAgent: pr.ByAgent, State: pr.State})
					break
				}
			}
		}
		out = append(out, row)
	}
	return out, nil
}

// LockedByOther returns 423 if another user holds the feature lock.
func LockedByOther(ctx context.Context, store specdata.Store, featureID, userID uuid.UUID) error {
	l, err := store.GetLock(ctx, featureID)
	if err != nil {
		return err
	}
	if l != nil && l.LockedBy != userID {
		return apperr.Locked("feature_locked", "feature is being edited by "+l.LockedByName).
			With("userName", l.LockedByName).With("lockedAt", l.LockedAt)
	}
	return nil
}

// Lock takes or extends the edit lock.
func (s *Service) Lock(ctx context.Context, p *domain.Principal, uniqueID string) (*specdata.Lock, error) {
	f, err := Load(ctx, s.store, uniqueID)
	if err != nil {
		return nil, err
	}
	if err := RequireSpecPhase(f); err != nil {
		return nil, err
	}
	if err := RequireExpert(p, f.DomainKey); err != nil {
		return nil, err
	}
	l, ok, err := s.store.AcquireLock(ctx, f.ID, p.UserID)
	if err != nil {
		return nil, err
	}
	if !ok {
		e := apperr.Conflict("feature_locked", "feature is being edited by another user")
		if l != nil {
			e.With("userName", l.LockedByName).With("lockedAt", l.LockedAt)
		}
		return nil, e
	}
	return l, nil
}

// Unlock releases the caller's lock.
func (s *Service) Unlock(ctx context.Context, p *domain.Principal, uniqueID string) error {
	f, err := s.store.FeatureByUniqueID(ctx, uniqueID)
	if errors.Is(err, specdata.ErrNotFound) {
		return ErrFeatureNotFound
	}
	if err != nil {
		return err
	}
	return s.store.ReleaseLock(ctx, f.ID, p.UserID)
}

// List returns the Development stage feature list.
func (s *Service) List(ctx context.Context, lf specdata.ListFilter) ([]specdata.ListedFeature, error) {
	return s.store.ListFeatures(ctx, lf)
}
