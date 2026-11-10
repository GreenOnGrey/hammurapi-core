// Package webhooks receives git provider events (api) from the specification
// repository, the service repositories and the catalog files, and applies them
// in the worker (FTR.HMR.CMN-0002 arch §9):
//
//   - specification repository: pushes are projected onto gates (git is the
//     source of truth: an unknown commit touching a gate folder is an edit and
//     returns the gate to draft) and onto the traceability tables;
//   - service repositories: PRs/MRs are linked to features by the branch
//     hammurapi/<key>/<service>, trailers or the key in the title; reviews,
//     merges and tags are delivered to the workflow runs waiting for them;
//   - catalog-info.yaml changes trigger an incremental catalog sync.
package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/trace"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gates"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/kafka"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Envelope is the raw webhook as published to Kafka.
type Envelope struct {
	Provider string            `json:"provider"`
	Headers  map[string]string `json:"headers"`
	Body     json.RawMessage   `json:"body"`
}

var forwardedHeaders = []string{"X-GitHub-Event", "X-GitHub-Delivery", "X-Gitlab-Event", "X-Gitlab-Event-UUID"}

// Receiver handles POST /hooks/v1/git.
type Receiver struct {
	provider git.Provider
	secret   string
	bus      kafka.Publisher
}

// NewReceiver creates the receiver.
func NewReceiver(provider git.Provider, secret string, bus kafka.Publisher) *Receiver {
	return &Receiver{provider: provider, secret: secret, bus: bus}
}

func (h *Receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 25<<20))
	if err != nil {
		metrics.WebhookEvents.WithLabelValues("bad_request").Inc()
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	if !h.provider.VerifyWebhook(r.Header, body, h.secret) {
		metrics.WebhookEvents.WithLabelValues("unauthorized").Inc()
		http.Error(w, "invalid webhook secret", http.StatusUnauthorized)
		return
	}
	if !json.Valid(body) {
		metrics.WebhookEvents.WithLabelValues("bad_request").Inc()
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	// Partition by the feature key when it is visible (branch, title), so the
	// events of one feature are processed in order; otherwise by repository.
	key := domain.FeatureKeyInText.FindString(string(body))
	if key == "" {
		var repo struct {
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
			Project struct {
				PathWithNamespace string `json:"path_with_namespace"`
			} `json:"project"`
		}
		_ = json.Unmarshal(body, &repo)
		key = repo.Repository.FullName + repo.Project.PathWithNamespace
	}
	env := Envelope{Provider: h.provider.Name(), Headers: map[string]string{}, Body: body}
	for _, k := range forwardedHeaders {
		if v := r.Header.Get(k); v != "" {
			env.Headers[k] = v
		}
	}
	msg, _ := json.Marshal(env)
	if err := h.bus.Publish(r.Context(), kafka.TopicGitPush, key, msg); err != nil {
		slog.ErrorContext(r.Context(), "publish webhook", "err", err)
		metrics.WebhookEvents.WithLabelValues("error").Inc()
		httpx.JSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"code": "queue_unavailable", "message": "try again later"}})
		return
	}
	metrics.WebhookEvents.WithLabelValues("accepted").Inc()
	w.WriteHeader(http.StatusAccepted)
}

// Catalog is notified about changed catalog files.
type Catalog interface {
	Changed(ctx context.Context, repo string, paths []string) error
}

// Reviews handles review comments on agent PRs (address_review tasks, R18).
type Reviews interface {
	ReviewComment(ctx context.Context, pr *cycledata.PR, body, author string) error
}

// Processor applies webhook events (worker).
type Processor struct {
	store    specdata.Store
	provider git.Provider
	events   events.Publisher
	catalog  Catalog
	reviews  Reviews
	botLogin string
	// Skills rebuilds the agent skills snapshot on pushes to /agent/ (FTR.HMR.CMN-0004 arch §5).
	Skills SkillsSync
	// Specs updates the specification index on pushes to the default branch (FTR.HMR.CMN-0005 R17).
	Specs SpecPushes
	// Rules checks the templates against the document contract after a push to rules/ (FTR.HMR.DEV-0003 R20).
	Rules RulesPushes
}

// RulesPushes is the check of the rules as the push processor uses it.
type RulesPushes interface {
	RulesPushed(ctx context.Context, branch string, paths []string) error
}

// SpecPushes is the specification index as the push processor uses it.
type SpecPushes interface {
	SpecsPushed(ctx context.Context, push *git.PushEvent) error
}

// SkillsSync is the Agent section as the push processor uses it.
type SkillsSync interface {
	SkillsChanged(ctx context.Context, branch string, paths []string) error
}

// NewProcessor creates the processor.
func NewProcessor(store specdata.Store, provider git.Provider, ev events.Publisher, catalog Catalog, reviews Reviews, botLogin string) *Processor {
	return &Processor{store: store, provider: provider, events: ev, catalog: catalog, reviews: reviews, botLogin: botLogin}
}

// Handle is the Kafka handler.
func (p *Processor) Handle(ctx context.Context, _, value []byte) error {
	var env Envelope
	if err := json.Unmarshal(value, &env); err != nil {
		slog.ErrorContext(ctx, "drop malformed webhook envelope", "err", err)
		return nil
	}
	h := http.Header{}
	for k, v := range env.Headers {
		h.Set(k, v)
	}
	ev, err := p.provider.ParseHook(h, env.Body)
	if err != nil {
		slog.ErrorContext(ctx, "drop unparsable webhook", "err", err)
		return nil
	}
	if ev == nil {
		return nil // an event Hammurapi does not use
	}
	return p.Dispatch(ctx, ev)
}

// Dispatch routes a normalized event.
func (p *Processor) Dispatch(ctx context.Context, ev *git.HookEvent) error {
	switch ev.Kind {
	case "push":
		push := ev.Push
		if push.Repo == "" || strings.EqualFold(push.Repo, p.provider.Repo()) {
			if err := p.Apply(ctx, push); err != nil {
				return err
			}
			var paths []string
			truncated := len(push.Commits) >= 20 // GitHub lists at most 20 commits of a push
			for _, c := range push.Commits {
				paths = append(append(append(paths, c.Added...), c.Modified...), c.Removed...)
				truncated = truncated || len(c.Added)+len(c.Modified)+len(c.Removed) == 0
			}
			if p.Skills != nil {
				if err := p.Skills.SkillsChanged(ctx, push.Branch, paths); err != nil {
					slog.ErrorContext(ctx, "agent skills snapshot", "err", err)
				}
			}
			if p.Rules != nil {
				rulePaths := paths
				if truncated { // the files of the push are not all known: check every template
					rulePaths = append(rulePaths, "rules/")
				}
				if err := p.Rules.RulesPushed(ctx, push.Branch, rulePaths); err != nil {
					slog.ErrorContext(ctx, "rules contract check", "err", err) // the next start of the worker checks again
				}
			}
			if p.Specs != nil {
				if err := p.Specs.SpecsPushed(ctx, push); err != nil {
					slog.ErrorContext(ctx, "specification index", "err", err) // the scheduled check catches up
				}
			}
		} else if err := p.servicePush(ctx, push); err != nil {
			return err
		}
		return p.catalogPush(ctx, push)
	case "tag":
		return p.tag(ctx, ev.Push)
	case "pr":
		return p.pullRequest(ctx, ev.PR)
	case "review":
		return p.review(ctx, ev.Review)
	}
	return nil
}

func (p *Processor) catalogPush(ctx context.Context, push *git.PushEvent) error {
	if p.catalog == nil || push.Branch == "" {
		return nil
	}
	var paths []string
	for _, c := range push.Commits {
		paths = append(append(append(paths, c.Added...), c.Modified...), c.Removed...)
	}
	for _, x := range paths {
		if strings.HasSuffix(x, ".yaml") || strings.HasSuffix(x, ".yml") {
			return p.catalog.Changed(ctx, push.Repo, paths)
		}
	}
	return nil
}

// servicePush updates the head of open PRs of the pushed branch.
func (p *Processor) servicePush(ctx context.Context, push *git.PushEvent) error {
	if push.Branch == "" || push.After == "" {
		return nil
	}
	return p.store.InTx(ctx, func(tx specdata.Store) error {
		cd := cycledata.New(tx.Q())
		prs, err := cd.PRsByHead(ctx, push.Repo, "", push.Branch)
		if err != nil {
			return err
		}
		for _, pr := range prs {
			if pr.State != "open" || pr.HeadSHA == push.After {
				continue
			}
			if err := cd.SetPRHead(ctx, pr.ID, push.After); err != nil {
				return err
			}
			if err := notifyFeature(ctx, tx.Q(), pr.FeatureID, "pr_updated", map[string]any{"prId": pr.ID, "headSha": push.After}); err != nil {
				return err
			}
		}
		return nil
	})
}

// notifyFeature delivers an event to the active runs of a feature.
func notifyFeature(ctx context.Context, q postgres.Querier, featureID uuid.UUID, typ string, payload any) error {
	for _, kind := range []string{"codegen", "validation"} {
		if _, err := workflows.SendToSubject(ctx, q, kind, featureID, typ, payload); err != nil {
			return err
		}
	}
	var releaseID uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM releases WHERE feature_id = $1`, featureID).Scan(&releaseID)
	if postgres.IsNoRows(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, kind := range []string{"release", "rollback"} {
		if _, err := workflows.SendToSubject(ctx, q, kind, releaseID, typ, payload); err != nil {
			return err
		}
	}
	return nil
}

// tag delivers a tag to releases and rollbacks waiting for a deploy signal of
// the service (R28: a tag containing the merge commit counts as the release).
func (p *Processor) tag(ctx context.Context, push *git.PushEvent) error {
	return p.store.InTx(ctx, func(tx specdata.Store) error {
		if ok, err := tx.MarkWebhookProcessed(ctx, push.EventID); err != nil || !ok {
			return err
		}
		svc, err := cycledata.New(tx.Q()).ServiceByRepo(ctx, push.Repo)
		if errors.Is(err, cycledata.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.Q().Query(ctx, `SELECT id FROM workflow_runs WHERE kind IN ('release','rollback')
			AND state NOT IN ('done','succeeded','failed','cancelled','rolled_back')`)
		if err != nil {
			return err
		}
		var runs []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			runs = append(runs, id)
		}
		rows.Close()
		for _, id := range runs {
			if err := workflows.Send(ctx, tx.Q(), id, "tag", map[string]any{"serviceId": svc.ID, "service": svc.Key, "tag": push.Tag, "sha": push.After}); err != nil {
				return err
			}
		}
		return nil
	})
}

// LinkFeature finds the feature of a service PR: branch hammurapi/<key>/…,
// then the key in the branch or the title (R19).
func LinkFeature(branch, title string) string {
	if k := domain.FeatureKeyInText.FindString(branch); k != "" {
		return k
	}
	return domain.FeatureKeyInText.FindString(title)
}

func (p *Processor) pullRequest(ctx context.Context, ev *git.PREvent) error {
	if strings.EqualFold(ev.Repo, p.provider.Repo()) {
		return nil // the spec PR is driven by Hammurapi itself (release confirmation, rollback)
	}
	var notify []events.Event
	err := p.store.InTx(ctx, func(tx specdata.Store) error {
		if ok, err := tx.MarkWebhookProcessed(ctx, ev.EventID); err != nil || !ok {
			return err
		}
		cd := cycledata.New(tx.Q())
		svc, err := cd.ServiceByRepo(ctx, ev.Repo)
		if errors.Is(err, cycledata.ErrNotFound) {
			return nil // not a service of the catalog
		}
		if err != nil {
			return err
		}
		existing, err := cd.PRByRepoNumber(ctx, ev.Repo, ev.Number)
		if err != nil && !errors.Is(err, cycledata.ErrNotFound) {
			return err
		}
		var pr *cycledata.PR
		if existing != nil && err == nil {
			pr = existing
		} else {
			key := LinkFeature(ev.Branch, ev.Title)
			if key == "" {
				return nil
			}
			f, err := tx.FeatureByUniqueID(ctx, key)
			if errors.Is(err, specdata.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if f.Phase == domain.PhaseDeleted || f.Phase == domain.PhaseReleased || f.Phase == domain.PhaseRolledBack {
				return nil
			}
			byAgent := strings.HasPrefix(ev.Branch, "hammurapi/") && (p.botLogin == "" || strings.EqualFold(ev.Author, p.botLogin))
			pr = &cycledata.PR{Repo: ev.Repo, Number: ev.Number, URL: ev.URL, Title: ev.Title, Branch: ev.Branch, Kind: "service",
				FeatureID: f.ID, ServiceID: &svc.ID, ByAgent: byAgent, State: "open", Review: "none", HeadSHA: ev.HeadSHA}
			if svc.Autonomy == domain.AutonomyAutonomous && byAgent {
				pr.Review = "not_required"
			}
			if err := cd.UpsertPR(ctx, pr); err != nil {
				return err
			}
			if !byAgent {
				if err := cd.AddActivity(ctx, "feature", f.ID, "human_pr_linked", nil, false, map[string]any{"repo": ev.Repo, "number": ev.Number, "service": svc.Key}); err != nil {
					return err
				}
			}
		}
		switch ev.Action {
		case "merged":
			if err := cd.MarkPRMerged(ctx, pr.ID, ev.MergeSHA, nil); err != nil {
				return err
			}
			if err := notifyFeature(ctx, tx.Q(), pr.FeatureID, "pr_merged", map[string]any{"prId": pr.ID, "mergeSha": ev.MergeSHA}); err != nil {
				return err
			}
		case "closed":
			if err := cd.MarkPRClosed(ctx, pr.ID); err != nil {
				return err
			}
			if err := notifyFeature(ctx, tx.Q(), pr.FeatureID, "pr_closed", map[string]any{"prId": pr.ID}); err != nil {
				return err
			}
		default: // opened, updated, reopened
			if ev.HeadSHA != "" && ev.HeadSHA != pr.HeadSHA {
				if err := cd.SetPRHead(ctx, pr.ID, ev.HeadSHA); err != nil {
					return err
				}
			}
			if err := notifyFeature(ctx, tx.Q(), pr.FeatureID, "pr_updated", map[string]any{"prId": pr.ID, "headSha": ev.HeadSHA}); err != nil {
				return err
			}
		}
		notify = append(notify, events.Event{Type: events.FeatureUpdated, Data: map[string]any{"prId": pr.ID}})
		return nil
	})
	if err != nil {
		return err
	}
	for _, n := range notify {
		p.events.Publish(ctx, n)
	}
	return nil
}

func (p *Processor) review(ctx context.Context, ev *git.ReviewEvent) error {
	if p.botLogin != "" && strings.EqualFold(ev.Author, p.botLogin) {
		return nil // the agent's own replies
	}
	var comment *cycledata.PR
	err := p.store.InTx(ctx, func(tx specdata.Store) error {
		if ok, err := tx.MarkWebhookProcessed(ctx, ev.EventID); err != nil || !ok {
			return err
		}
		cd := cycledata.New(tx.Q())
		pr, err := cd.PRByRepoNumber(ctx, ev.Repo, ev.Number)
		if errors.Is(err, cycledata.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		switch ev.State {
		case "approved", "changes_requested":
			if pr.Review != "not_required" || ev.State == "changes_requested" {
				if err := cd.SetPRReview(ctx, pr.ID, ev.State); err != nil {
					return err
				}
			}
		}
		if err := notifyFeature(ctx, tx.Q(), pr.FeatureID, "pr_updated", map[string]any{"prId": pr.ID, "review": ev.State}); err != nil {
			return err
		}
		if pr.ByAgent && pr.State == "open" && (ev.State == "changes_requested" || (ev.State == "commented" && strings.TrimSpace(ev.Body) != "")) {
			comment = pr
		}
		return nil
	})
	if err != nil || comment == nil || p.reviews == nil {
		return err
	}
	return p.reviews.ReviewComment(ctx, comment, ev.Body, ev.Author)
}

type pending struct {
	uniqueID         string
	gate             specdata.Gate
	approvalsChanged bool
}

// Apply processes one push event of the specification repository idempotently.
func (p *Processor) Apply(ctx context.Context, ev *git.PushEvent) error {
	uid, ok := git.UniqueIDFromBranch(ev.Branch)
	if !ok {
		// Pushes to the default branch or rules/* branches need no projection:
		// rules are read from git on demand.
		return nil
	}
	var published []pending
	var traced *specdata.Feature
	err := p.store.InTx(ctx, func(tx specdata.Store) error {
		published, traced = nil, nil
		fresh, err := tx.MarkWebhookProcessed(ctx, ev.EventID)
		if err != nil {
			return err
		}
		if !fresh {
			slog.InfoContext(ctx, "duplicate webhook skipped", "event", ev.EventID)
			return nil
		}
		f, err := tx.FeatureByUniqueID(ctx, uid)
		if errors.Is(err, specdata.ErrNotFound) {
			slog.InfoContext(ctx, "push to unknown feature branch ignored", "branch", ev.Branch)
			return nil
		}
		if err != nil {
			return err
		}
		if f.Phase == domain.PhaseDeleted || f.Phase == domain.PhaseReleased || f.Phase == domain.PhaseRolledBack {
			return nil // finished features are not projected
		}
		actor, err := tx.UserIDByUsername(ctx, ev.Actor)
		if err != nil {
			return err
		}
		for _, c := range ev.Commits {
			out, err := p.applyCommit(ctx, tx, f, c, actor)
			if err != nil {
				return err
			}
			published = append(published, out...)
			if touchesTrace(c) {
				traced = f
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if traced != nil {
		if err := p.retrace(ctx, traced); err != nil {
			return err
		}
	}
	metrics.WebhookEvents.WithLabelValues("processed").Inc()
	approvals := false
	for _, x := range published {
		g := x.gate
		gates.PublishGateUpdated(ctx, p.events, x.uniqueID, &g)
		approvals = approvals || x.approvalsChanged
	}
	if approvals {
		p.events.Publish(ctx, events.Event{Type: events.ApprovalsChanged, Data: map[string]string{"uniqueId": uid}})
	}
	return nil
}

func touchesTrace(c git.PushCommit) bool {
	for _, list := range [][]string{c.Added, c.Modified, c.Removed} {
		for _, x := range list {
			if loc, ok := git.ParseSpecPath(x); ok && loc.Rest == "spec.md" &&
				(loc.Area == string(domain.AreaProduct) || loc.Area == string(domain.AreaArch) || loc.Area == string(domain.AreaTech) || loc.Area == string(domain.AreaQA)) {
				return true
			}
		}
	}
	return false
}

// retrace re-projects requirements, services and test cases from the branch.
func (p *Processor) retrace(ctx context.Context, f *specdata.Feature) error {
	if p.provider == nil {
		return nil
	}
	token, err := p.provider.BotToken(ctx)
	if err != nil {
		slog.WarnContext(ctx, "traceability not projected: no bot token for the specification repository", "err", err)
		return nil
	}
	tf := trace.Feature{ID: f.ID, Key: f.UniqueID, DomainKey: f.DomainKey, SystemKey: f.SystemKey, Ref: f.Branch}
	docs, err := trace.Read(ctx, p.provider, token, tf)
	if err != nil {
		return err
	}
	_, err = trace.Project(ctx, p.store.Q(), f.ID, docs)
	return err
}

func (p *Processor) applyCommit(ctx context.Context, tx specdata.Store, f *specdata.Feature, c git.PushCommit, actor *uuid.UUID) ([]pending, error) {
	trailers := git.ParseTrailers(c.Message)
	type touch struct{ specRemoved bool }
	areas := map[domain.Area]*touch{}
	visit := func(path string, removed bool) {
		loc, ok := git.ParseSpecPath(path)
		if !ok {
			if !strings.HasPrefix(path, "rules/") {
				slog.InfoContext(ctx, "path outside specs/<d>/<s>/<id>/<area>/ ignored", "path", path, "commit", c.SHA)
			}
			return
		}
		if loc.Domain != f.DomainKey || loc.System != f.SystemKey || loc.UniqueID != f.UniqueID {
			slog.InfoContext(ctx, "path of another feature ignored", "path", path, "feature", f.UniqueID)
			return
		}
		a := domain.Area(loc.Area)
		if !a.Valid() {
			slog.InfoContext(ctx, "unknown area ignored", "path", path)
			return
		}
		t := areas[a]
		if t == nil {
			t = &touch{}
			areas[a] = t
		}
		if removed && loc.Rest == "spec.md" {
			t.specRemoved = true
		}
	}
	for _, x := range c.Added {
		visit(x, false)
	}
	for _, x := range c.Modified {
		visit(x, false)
	}
	for _, x := range c.Removed {
		visit(x, true)
	}
	var out []pending
	sha := c.SHA
	for _, area := range domain.Areas {
		t, ok := areas[area]
		if !ok {
			continue
		}
		g, err := tx.ActiveGate(ctx, f.ID, area)
		if errors.Is(err, specdata.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if g.HeadCommit == sha {
			continue // a commit Hammurapi made itself (creation, import, deletion)
		}
		if trailers.Generated {
			continue // a commit of the writer is projected by the gate_generation workflow
		}
		wasInReview := g.Status == domain.GateInReview
		if t.specRemoved {
			if area == domain.AreaProduct {
				slog.WarnContext(ctx, "product spec.md removed directly in git; gate kept", "feature", f.UniqueID)
				continue
			}
			now := time.Now()
			g.DeletedAt, g.DeletedBy, g.HeadCommit = &now, actor, sha
			if err := tx.SaveGate(ctx, g); err != nil {
				return nil, err
			}
			if err := tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventDeleted, ActorID: actor, IsAgent: trailers.Agent, CommitSHA: &sha}); err != nil {
				return nil, err
			}
			out = append(out, pending{uniqueID: f.UniqueID, gate: *g, approvalsChanged: wasInReview})
			continue
		}
		g.HeadCommit = sha
		if err := tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventEdited, ActorID: actor, IsAgent: trailers.Agent, CommitSHA: &sha}); err != nil {
			return nil, err
		}
		if g.Status != domain.GateDraft {
			g.Status, g.SubmittedAt = domain.GateDraft, nil
			if err := tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventReset, ActorID: actor, IsAgent: trailers.Agent, CommitSHA: &sha}); err != nil {
				return nil, err
			}
			gates.RecordTransition(area, string(domain.GateDraft))
		}
		if err := tx.SaveGate(ctx, g); err != nil {
			return nil, err
		}
		out = append(out, pending{uniqueID: f.UniqueID, gate: *g, approvalsChanged: wasInReview})
	}
	return out, nil
}
