//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/codegen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/deploy"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/discovery"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/issues"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/overview"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/releases"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/rollback"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/validation"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	gmocks "github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
)

// ─── Workflow engine ─────────────────────────────────────────────────

type countingMachine struct{ steps atomic.Int32 }

func (*countingMachine) Kind() string { return "catalog_sync" }

func (m *countingMachine) Step(ctx context.Context, tx pgx.Tx, run *workflows.Run, evs []workflows.Event, now time.Time) (workflows.Result, error) {
	m.steps.Add(1)
	if res, ok := workflows.Unblock(run, evs, now); ok {
		return res, nil
	}
	switch run.State {
	case "queued":
		return workflows.Result{State: "working", Effects: []workflows.Effect{{Type: "test.flaky", Payload: map[string]any{}}}}, nil
	case "working":
		if workflows.Has(evs, "effect_failed") {
			return workflows.Block(run, "flaky effect failed"), nil
		}
		if workflows.Has(evs, "ok") {
			return workflows.Result{State: "done"}, nil
		}
	}
	return workflows.Keep(run), nil
}

func drain(t *testing.T, e *workflows.Engine) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 500; i++ {
		worked, err := e.ProcessOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sent, err := e.DispatchOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked && !sent {
			return
		}
	}
	t.Fatal("workflows did not settle")
}

// WF-02 / WF-03: one worker per run; effects retry with backoff and block after the limit.
func TestEngine(t *testing.T) {
	ctx := context.Background()
	m := &countingMachine{}
	var calls atomic.Int32
	newEngine := func() *workflows.Engine {
		e := workflows.New(pool, nopEvents{}, workflows.Config{MaxAttempts: 3, Lease: time.Minute})
		e.Register(m)
		e.Handle("test.flaky", workflows.EffectHandler{Do: func(context.Context, workflows.RunRef, json.RawMessage) ([]workflows.NewEvent, error) {
			calls.Add(1)
			return nil, errors.New("provider down")
		}})
		return e
	}
	subject := uuid.New()
	id, err := workflows.Start(ctx, pool, "catalog_sync", subject, nil, "queued", nil)
	must(t, err)
	_, err = workflows.Start(ctx, pool, "catalog_sync", subject, nil, "queued", nil)
	if !errors.Is(err, workflows.ErrActiveRun) {
		t.Fatalf("second active run: %v", err)
	}
	// Two engines race for the same run: exactly one transition happens.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = newEngine().ProcessOnce(ctx)
		}()
	}
	wg.Wait()
	if m.steps.Load() != 1 {
		t.Fatalf("run processed %d times", m.steps.Load())
	}
	e := newEngine()
	for i := 0; i < 3; i++ {
		// Make the retried effect due right away.
		_, _ = pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() WHERE run_id = $1 AND sent_at IS NULL`, id)
		_, err := e.DispatchOnce(ctx)
		must(t, err)
	}
	drain(t, e)
	run, err := workflows.Load(ctx, pool, id)
	must(t, err)
	if run.State != workflows.StateBlocked || calls.Load() != 3 {
		t.Fatalf("state %s after %d calls", run.State, calls.Load())
	}
	must(t, workflows.Send(ctx, pool, id, "retry", nil))
	drain(t, e)
	run, _ = workflows.Load(ctx, pool, id)
	if run.State != "working" {
		t.Fatalf("retry resumed %s", run.State)
	}
	must(t, workflows.Send(ctx, pool, id, "ok", nil))
	drain(t, e)
	run, _ = workflows.Load(ctx, pool, id)
	if run.State != "done" {
		t.Fatalf("final %s", run.State)
	}
}

// ─── The cycle through the real state machines ───────────────────────

type failingTokens struct{}

func (failingTokens) Token(context.Context, uuid.UUID) (string, error) {
	return "", errors.New("no token in tests")
}

// TestCycle: issue → Discovery → feature → validation → release (merge in the
// plan order, deploy marks, confirmation) and a second release rolled back.
func TestCycle(t *testing.T) {
	ctx := context.Background()
	cd := cycledata.New(pool)
	store := specdata.NewPG(pool)

	// Users and roles: Pavel is a product and technical expert of PAY.
	var pavel uuid.UUID
	must(t, pool.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, language, agent_name, agent_tone)
		VALUES ('p1','pavel','Pavel','en','Codex','business') RETURNING id`).Scan(&pavel))
	must(t, pool.QueryRow(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('PAY','Payments') RETURNING id),
		s AS (INSERT INTO systems (domain_id, key, name) SELECT id, 'BIL', 'Billing' FROM d RETURNING id)
		INSERT INTO domain_experts (domain_id, user_id, kind) SELECT d.id, $1, k FROM d, unnest(ARRAY['product','technical']::expert_kind[]) k
		RETURNING $1::uuid`, pavel).Scan(&pavel))
	p := tu.User("expert:PAY:product", "expert:PAY:technical")
	p.UserID = pavel
	sys, err := store.SystemByKeys(ctx, "PAY", "BIL")
	must(t, err)
	for _, s := range []string{"booking", "pricing"} {
		svc := &cycledata.Service{Key: s, Name: s, SystemID: &sys.ID, Repo: "pay/" + s, Source: "manual"}
		must(t, cd.UpsertService(ctx, svc))
	}

	metricSource(t, "ch", "clickhouse") // FTR.HMR.DISC-0001 R23: the source of a way to measure is configured
	engine := workflows.New(pool, nopEvents{}, workflows.Config{MaxAttempts: 3, Lease: time.Minute})
	engine.Register(discovery.Machine{}, validation.Machine{}, releases.Machine{}, rollback.Machine{},
		codegen.TaskMachine{Limits: codegen.Limits{MaxParallel: 5, Timeout: time.Hour}})
	nop := workflows.EffectHandler{Do: func(context.Context, workflows.RunRef, json.RawMessage) ([]workflows.NewEvent, error) {
		return nil, nil
	}}
	engine.Handle(validation.EffectCheck, nop)
	engine.Handle(deploy.Effect, nop)
	engine.Handle(codegen.EffectStop, nop)
	engine.Handle(discovery.Effect, workflows.EffectHandler{Do: func(context.Context, workflows.RunRef, json.RawMessage) ([]workflows.NewEvent, error) {
		return []workflows.NewEvent{{Type: "discovery_ready", Payload: map[string]any{"content": "# Discovery", "value": "More bookings",
			"measure": map[string]string{"source": "ch", "query": "SELECT 1", "target": "+5%", "window": "14d"}}}}, nil
	}})
	var merges []string
	engine.Handle(releases.EffectMerge, workflows.EffectHandler{Do: func(ctx context.Context, _ workflows.RunRef, raw json.RawMessage) ([]workflows.NewEvent, error) {
		var in releases.MergePayload
		_ = json.Unmarshal(raw, &in)
		pr, err := cd.PRByID(ctx, in.PRID)
		if err != nil {
			return nil, err
		}
		merges = append(merges, pr.Repo)
		sha := "merge-" + pr.Repo
		must(t, cd.MarkPRMerged(ctx, pr.ID, sha, &in.UserID))
		return []workflows.NewEvent{{Type: "merged", Payload: map[string]string{"prId": pr.ID.String(), "sha": sha}}}, nil
	}})
	engine.Handle(releases.EffectMergeSpec, workflows.EffectHandler{Do: func(context.Context, workflows.RunRef, json.RawMessage) ([]workflows.NewEvent, error) {
		return []workflows.NewEvent{{Type: "spec_merged", Payload: map[string]string{}}}, nil
	}})
	engine.Handle(releases.EffectClosePRs, workflows.EffectHandler{Do: func(context.Context, workflows.RunRef, json.RawMessage) ([]workflows.NewEvent, error) {
		return []workflows.NewEvent{{Type: "closed", Payload: map[string]string{}}}, nil
	}})
	engine.Handle(codegen.EffectStart, workflows.EffectHandler{Do: func(ctx context.Context, _ workflows.RunRef, raw json.RawMessage) ([]workflows.NewEvent, error) {
		var in struct {
			TaskID uuid.UUID `json:"taskId"`
		}
		_ = json.Unmarshal(raw, &in)
		_, hash := codegen.NewToken()
		must(t, cd.StartTask(ctx, in.TaskID, hash, "test"))
		return []workflows.NewEvent{{Type: "started", Payload: map[string]string{}}}, nil
	}})

	// ISS-01: an issue gets a key and Discovery starts; DSC-01: verification after Discovery.
	iss := issues.NewService(store, nil, nil, nopEvents{})
	is, err := iss.Create(ctx, p, issues.CreateInput{Type: domain.IssueIdea, Domain: "PAY", Title: "Weekend tariffs", Description: "…"})
	must(t, err)
	if is.Key != "ISS.PAY-0001" {
		t.Fatalf("key %s", is.Key)
	}
	drain(t, engine)
	is, _, err = cd.IssueByKey(ctx, is.Key)
	must(t, err)
	if is.Status != domain.IssueVerification {
		t.Fatalf("issue status %s", is.Status)
	}
	// ISS-04: a moved issue keeps its old key.
	newKey, err := iss.Move(ctx, p, is.Key, "PAY")
	must(t, err)
	if newKey != is.Key {
		t.Fatal("move into the same domain changed the key")
	}
	tu.Code(t, iss.Reject(ctx, p, is.Key, " "), 422, "reason_required")

	// A feature linked to the issue with two services and their PRs (as after codegen).
	mkFeature := func(n int, title string) *specdata.Feature {
		f := &specdata.Feature{UniqueID: domain.FeatureKey("PAY", "BIL", n), SystemID: sys.ID, Number: n, Title: title,
			Branch: fmt.Sprintf("feature/FTR.PAY.BIL-%04d", n), PRNumber: 100 + n, PRURL: "u", Phase: domain.PhaseValidation, CreatedBy: pavel}
		must(t, store.InTx(ctx, func(tx specdata.Store) error {
			if _, err := tx.NextNumber(ctx, sys.ID); err != nil {
				return err
			}
			if err := tx.InsertFeature(ctx, f); err != nil {
				return err
			}
			return tx.LinkIssue(ctx, f.ID, is.ID)
		}))
		var ids []uuid.UUID
		for i, s := range []string{"booking", "pricing"} {
			svc, err := cd.ServiceByKey(ctx, s)
			must(t, err)
			ids = append(ids, svc.ID)
			pr := &cycledata.PR{Repo: svc.Repo, Number: 10*n + i, URL: "u", Title: "t", Branch: git.ServiceBranch(f.UniqueID, s), Kind: "service",
				FeatureID: f.ID, ServiceID: &svc.ID, ByAgent: true, State: "open", Review: "not_required", HeadSHA: fmt.Sprintf("head-%d-%s", n, s)}
			must(t, cd.UpsertPR(ctx, pr))
		}
		must(t, cd.SetFeatureServices(ctx, f.ID, ids))
		must(t, cd.ReplaceRequirements(ctx, f.ID, []cycledata.Requirement{{ID: "R1", Text: "Weekend tariff"}}))
		must(t, cd.ReplaceTestCases(ctx, f.ID, []cycledata.TestCase{{ID: "QA-01", Level: "U", ReqIDs: []string{"R1"}, Title: "tariff"}}))
		must(t, validation.Start(ctx, pool, f.ID))
		return f
	}
	f := mkFeature(1, "Weekend tariffs")
	must(t, cd.SetIssueStatus(ctx, is.ID, domain.IssueAccepted)) // as after acceptance (DSC-09)
	ctrl := gomock.NewController(t)
	gp := gmocks.NewMockProvider(ctrl)
	gp.EXPECT().Repo().Return("org/specs").AnyTimes()
	val := validation.NewService(pool, store, gp, failingTokens{}, nopEvents{})
	drain(t, engine)
	// VAL-03: no CI results yet.
	_, err = val.Sign(ctx, p, f.UniqueID, validation.SignInput{Side: "product"})
	tu.Code(t, err, 409, "validation_incomplete")
	ciFor := func(f *specdata.Feature) {
		prs, err := cd.FeaturePRs(ctx, f.ID, "service")
		must(t, err)
		for _, pr := range prs {
			_, err := cd.InsertCIRun(ctx, pr.Repo, pr.HeadSHA, pr.Branch, "ci", "", []cycledata.TestResult{{TCID: "QA-01", Status: "passed"}})
			must(t, err)
			_, err = workflows.SendToSubject(ctx, pool, validation.Kind, f.ID, "ci_results", map[string]string{})
			must(t, err)
		}
		drain(t, engine)
	}
	ciFor(f)
	sum, err := val.Get(ctx, p, f.UniqueID)
	must(t, err)
	if !sum.CIComplete || sum.Coverage.Passed != 1 || !sum.Permissions.SignProduct {
		t.Fatalf("summary %+v", sum)
	}
	// VAL-07 / VAL-08 / VAL-12: sides by kind; the second signature creates the release.
	_, err = val.Sign(ctx, tu.User("expert:PAY:product"), f.UniqueID, validation.SignInput{Side: "technical"})
	tu.Code(t, err, 403, "forbidden")
	rk, err := val.Sign(ctx, p, f.UniqueID, validation.SignInput{Side: "product"})
	must(t, err)
	if rk != nil {
		t.Fatal("release after one signature")
	}
	rk, err = val.Sign(ctx, p, f.UniqueID, validation.SignInput{Side: "technical"})
	must(t, err)
	if rk == nil || *rk != "RLS.PAY.BIL-0001" {
		t.Fatalf("release key %v", rk)
	}
	drain(t, engine)

	rel := releases.NewService(pool, store, nopEvents{})
	card, err := rel.Get(ctx, p, *rk)
	must(t, err)
	if card.Step != "awaiting_start" || len(card.PRs) != 3 || card.PRs[2].Kind != "spec" || len(card.Issues) != 1 {
		t.Fatalf("release card %+v", card)
	}
	// REL-02: the plan changes only before the merge starts.
	must(t, rel.SetPlan(ctx, p, *rk, []string{"pricing", "booking"}))
	must(t, rel.StartMerge(ctx, p, *rk))
	drain(t, engine)
	tu.Code(t, rel.SetPlan(ctx, p, *rk, []string{"booking", "pricing"}), 409, "release_step_invalid")
	// Deploy is not configured (DEP-07): the release waits for a mark of each service in order (REL-03, REL-10).
	for _, s := range []string{"pricing", "booking"} {
		card, err = rel.Get(ctx, p, *rk)
		must(t, err)
		if card.Current == nil || *card.Current != s || card.Status != "deploying" {
			t.Fatalf("waiting for %s, card %+v", s, card)
		}
		must(t, rel.MarkDeploy(ctx, p, *rk, s, "v1"))
		drain(t, engine)
	}
	if len(merges) != 2 || merges[0] != "pay/pricing" || merges[1] != "pay/booking" {
		t.Fatalf("merge order %v", merges)
	}
	card, err = rel.Get(ctx, p, *rk)
	must(t, err)
	if card.Status != "awaiting_confirmation" || !card.Permissions.Confirm {
		t.Fatalf("before confirmation %+v", card)
	}
	// REL-16: confirmation merges the spec PR; release succeeded, feature released, issue resolved.
	must(t, rel.Confirm(ctx, p, *rk))
	drain(t, engine)
	card, err = rel.Get(ctx, p, *rk)
	must(t, err)
	got, err := store.FeatureByID(ctx, f.ID)
	must(t, err)
	is, _, _ = cd.IssueByKey(ctx, is.Key)
	if card.Status != "succeeded" || got.Phase != domain.PhaseReleased || is.Status != domain.IssueResolved {
		t.Fatalf("after confirmation: %s %s %s", card.Status, got.Phase, is.Status)
	}
	tu.Code(t, rel.Rollback(ctx, p, *rk, "late"), 409, "release_finished") // REL-19

	// Second release: merged booking, then a rollback (RB-01 … RB-06).
	must(t, cd.SetIssueStatus(ctx, is.ID, domain.IssueAccepted))
	f2 := mkFeature(2, "Refunds")
	drain(t, engine)
	ciFor(f2)
	_, err = val.Sign(ctx, p, f2.UniqueID, validation.SignInput{Side: "product"})
	must(t, err)
	rk2, err := val.Sign(ctx, p, f2.UniqueID, validation.SignInput{Side: "technical"})
	must(t, err)
	drain(t, engine)
	must(t, rel.StartMerge(ctx, p, *rk2))
	drain(t, engine)
	must(t, rel.MarkDeploy(ctx, p, *rk2, "booking", "v2"))
	drain(t, engine) // booking released, pricing merged and waiting for its deploy
	tu.Code(t, rel.Rollback(ctx, p, *rk2, ""), 422, "reason_required")
	must(t, rel.Rollback(ctx, p, *rk2, "payment errors grow"))
	drain(t, engine)
	// Two revert tasks in reverse order; the runner is simulated here.
	for i := 0; i < 2; i++ {
		var taskID, runID uuid.UUID
		var svcKey string
		err := pool.QueryRow(ctx, `SELECT t.id, t.workflow_run_id, s.key FROM agent_tasks t JOIN services s ON s.id = t.service_id
			WHERE t.type = 'revert' AND t.status = 'running' ORDER BY t.created_at LIMIT 1`).Scan(&taskID, &runID, &svcKey)
		must(t, err)
		if want := []string{"pricing", "booking"}[i]; svcKey != want {
			t.Fatalf("revert %d is for %s, want %s", i, svcKey, want)
		}
		svc, _ := cd.ServiceByKey(ctx, svcKey)
		rpr := &cycledata.PR{Repo: svc.Repo, Number: 900 + i, URL: "u", Branch: "hammurapi/revert", Kind: "revert", FeatureID: f2.ID,
			ServiceID: &svc.ID, ByAgent: true, State: "open", Review: "not_required", HeadSHA: "r"}
		must(t, cd.UpsertPR(ctx, rpr))
		must(t, cd.FinishTask(ctx, taskID, "succeeded", json.RawMessage(fmt.Sprintf(`{"status":"succeeded","prNumber":%d}`, rpr.Number)), nil))
		must(t, workflows.Send(ctx, pool, runID, "task_result", map[string]any{"status": "succeeded", "prNumber": rpr.Number}))
		drain(t, engine)
		must(t, rel.MarkDeploy(ctx, p, *rk2, svcKey, "revert"))
		drain(t, engine)
	}
	card, err = rel.Get(ctx, p, *rk2)
	must(t, err)
	got, _ = store.FeatureByID(ctx, f2.ID)
	is, _, _ = cd.IssueByKey(ctx, is.Key)
	// RB-06: the issue returned to "new" with a link to the release, and a new Discovery ran (stubbed: back in verification).
	if card.Status != "rolled_back" || got.Phase != domain.PhaseRolledBack || is.RolledBackRelease == nil || *is.RolledBackRelease != *rk2 {
		t.Fatalf("after rollback: release %s feature %s issue %+v", card.Status, got.Phase, is)
	}
	var discoveries int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE kind = 'discovery' AND subject_id = $1`, is.ID).Scan(&discoveries))
	if discoveries != 2 || is.Status != domain.IssueVerification {
		t.Fatalf("Discovery after rollback: %d runs, status %s", discoveries, is.Status)
	}

	// NAV-02 / NAV-04: the General section queries.
	ov := overview.NewService(pool)
	fo, err := ov.Focus(ctx, p)
	must(t, err)
	o, err := ov.Overview(ctx, pavel, "mine")
	must(t, err)
	if len(o.Issues) != 1 || len(fo.Research) != 1 || fo.Research[0].Action != "verify_discovery" {
		t.Fatalf("overview %+v focus %+v", o, fo)
	}
	list, err := cd.ListReleases(ctx, cycledata.ReleaseFilter{UserID: pavel, Domain: "all", Status: "all", Page: httpx.Page{Limit: 10}})
	must(t, err)
	if len(list) != 2 {
		t.Fatalf("releases %d", len(list))
	}
}
