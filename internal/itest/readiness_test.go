//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/codegen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/services"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/validation"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
)

// FTR.HMR.CMN-0008 CHK-01…CHK-04: the command of a service.
func TestServiceCheck(t *testing.T) {
	ctx := context.Background()
	cd := cycledata.New(pool)
	store := specdata.NewPG(pool)
	var uid uuid.UUID
	must(t, pool.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, language, agent_name, agent_tone)
		VALUES ('chk1','chk1','Chk','en','A','business') RETURNING id`).Scan(&uid))
	must(t, pool.QueryRow(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('CHK','Check') RETURNING id)
		INSERT INTO systems (domain_id, key, name) SELECT id, 'SVC', 'Svc' FROM d RETURNING $1::uuid`, uid).Scan(&uid))
	sys, err := store.SystemByKeys(ctx, "CHK", "SVC")
	must(t, err)
	must(t, cd.UpsertService(ctx, &cycledata.Service{Key: "chk-svc", Name: "s", SystemID: &sys.ID, Repo: "chk/svc", Source: "manual"}))
	svcs := services.NewService(pool, nil)

	owner := tu.User()
	owner.UserID, owner.OwnedServices = uid, map[string]bool{"chk-svc": true}
	// CHK-01
	must(t, svcs.SetCheck(ctx, owner, "chk-svc", services.CheckInput{Command: " npm test ", TimeoutSec: 900}))
	s, err := cd.ServiceByKey(ctx, "chk-svc")
	must(t, err)
	if s.Check == nil || s.Check.Command != "npm test" || s.Check.TimeoutSec != 900 {
		t.Fatalf("CHK-01: %+v", s.Check)
	}
	var history int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM activity WHERE subject_id = $1 AND type = 'check_changed'`, s.ID).Scan(&history))
	if history != 1 {
		t.Fatalf("CHK-02: history %d", history)
	}
	// CHK-02: a stranger is refused, the command stays.
	tu.Code(t, svcs.SetCheck(ctx, tu.User(), "chk-svc", services.CheckInput{Command: "true"}), 403, "forbidden")
	// CHK-03: the sync of the catalog does not touch it.
	must(t, cd.UpsertService(ctx, &cycledata.Service{Key: "chk-svc", Name: "renamed", SystemID: &sys.ID, Repo: "chk/svc", Source: "manual"}))
	if s, _ = cd.ServiceByKey(ctx, "chk-svc"); s.Check == nil || s.Check.Command != "npm test" || s.Name != "renamed" {
		t.Fatalf("CHK-03: %+v", s)
	}
	// CHK-04
	tu.Code(t, svcs.SetCheck(ctx, owner, "chk-svc", services.CheckInput{Command: "make", TimeoutSec: 5}), 422, "invalid_check")
	// An empty command clears the check; the default timeout.
	must(t, svcs.SetCheck(ctx, owner, "chk-svc", services.CheckInput{}))
	if s, _ = cd.ServiceByKey(ctx, "chk-svc"); s.Check != nil {
		t.Fatalf("clear: %+v", s.Check)
	}
	must(t, svcs.SetCheck(ctx, owner, "chk-svc", services.CheckInput{Command: "make"}))
	if s, _ = cd.ServiceByKey(ctx, "chk-svc"); s.Check.TimeoutSec != 600 {
		t.Fatalf("default: %+v", s.Check)
	}
}

// FTR.HMR.CMN-0008 CHK-50…CHK-57: the automatic fix of discrepancies, on the
// engine of workflows with the agent check replaced by a script.
func TestValidationAutofix(t *testing.T) {
	ctx := context.Background()
	cd := cycledata.New(pool)
	store := specdata.NewPG(pool)
	var uid uuid.UUID
	must(t, pool.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, language, agent_name, agent_tone)
		VALUES ('af1','af1','Af','en','A','business') RETURNING id`).Scan(&uid))
	must(t, pool.QueryRow(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('AFX','Autofix') RETURNING id)
		INSERT INTO systems (domain_id, key, name) SELECT id, 'BIL', 'Bil' FROM d RETURNING $1::uuid`, uid).Scan(&uid))
	sys, err := store.SystemByKeys(ctx, "AFX", "BIL")
	must(t, err)
	var svcIDs []uuid.UUID
	for _, s := range []string{"af-auto", "af-pr"} {
		must(t, cd.UpsertService(ctx, &cycledata.Service{Key: s, Name: s, SystemID: &sys.ID, Repo: "afx/" + s, Source: "manual"}))
		svc, err := cd.ServiceByKey(ctx, s)
		must(t, err)
		svcIDs = append(svcIDs, svc.ID)
	}
	must(t, cd.SetAutonomy(ctx, svcIDs[0], domain.AutonomyAutonomous))

	// What the agent check finds, round after round.
	var findings [][]map[string]any
	checks := 0
	engine := workflows.New(pool, nopEvents{}, workflows.Config{MaxAttempts: 3, Lease: time.Minute})
	engine.Register(validation.Machine{}, codegen.TaskMachine{Limits: codegen.Limits{MaxParallel: 5, Timeout: time.Hour}})
	engine.Handle(validation.EffectCheck, workflows.EffectHandler{Do: func(context.Context, workflows.RunRef, json.RawMessage) ([]workflows.NewEvent, error) {
		var prs []map[string]any
		if checks < len(findings) {
			prs = findings[checks]
		}
		checks++
		return []workflows.NewEvent{{Type: "check_done", Payload: map[string]any{"prs": prs}}}, nil
	}})
	nop := workflows.EffectHandler{Do: func(context.Context, workflows.RunRef, json.RawMessage) ([]workflows.NewEvent, error) {
		return nil, nil
	}}
	engine.Handle(codegen.EffectStart, nop)
	engine.Handle(codegen.EffectPoll, nop)
	engine.Handle(codegen.EffectStop, nop)

	n := 0
	feature := func(byAgent bool) (*specdata.Feature, *cycledata.PR) {
		n++
		f := &specdata.Feature{UniqueID: domain.FeatureKey("AFX", "BIL", n), SystemID: sys.ID, Number: n, Title: "t",
			Branch: fmt.Sprintf("feature/AFX-%d", n), PRNumber: 500 + n, PRURL: "u", Phase: domain.PhaseValidation, CreatedBy: uid}
		must(t, store.InTx(ctx, func(tx specdata.Store) error {
			if _, err := tx.NextNumber(ctx, sys.ID); err != nil {
				return err
			}
			return tx.InsertFeature(ctx, f)
		}))
		pr := &cycledata.PR{Repo: "afx/af-auto", Number: 600 + n, URL: "u", Title: "t", Branch: git.ServiceBranch(f.UniqueID, "af-auto"), Kind: "service",
			FeatureID: f.ID, ServiceID: &svcIDs[0], ByAgent: byAgent, State: "open", Review: "not_required", HeadSHA: "h"}
		must(t, cd.UpsertPR(ctx, pr))
		return f, pr
	}
	found := func(pr *cycledata.PR) []map[string]any {
		return []map[string]any{{"id": pr.ID, "number": pr.Number, "serviceId": svcIDs[0], "text": "R1: no retry"}}
	}
	tasks := func(f *specdata.Feature) (ids []uuid.UUID) {
		rows, err := pool.Query(ctx, `SELECT id FROM agent_tasks WHERE feature_id = $1 AND type = 'address_review' ORDER BY created_at`, f.ID)
		must(t, err)
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			must(t, rows.Scan(&id))
			ids = append(ids, id)
		}
		return ids
	}
	finish := func(f *specdata.Feature, id uuid.UUID) {
		_, _ = pool.Exec(ctx, `UPDATE agent_tasks SET status = 'succeeded' WHERE id = $1`, id)
		_, err := workflows.SendToSubject(ctx, pool, validation.Kind, f.ID, "autofix_finished", map[string]string{"taskId": id.String()})
		must(t, err)
		drain(t, engine)
	}
	review := func(pr *cycledata.PR) string {
		cur, err := cd.PRByID(ctx, pr.ID)
		must(t, err)
		return cur.Review
	}

	// CHK-50, CHK-53: one task with the text; fixed — the second check finds nothing.
	f, pr := feature(true)
	checks, findings = 0, [][]map[string]any{found(pr), nil}
	must(t, validation.Start(ctx, pool, f.ID))
	drain(t, engine)
	ids := tasks(f)
	if len(ids) != 1 || checks != 1 {
		t.Fatalf("CHK-50: tasks %d, checks %d", len(ids), checks)
	}
	task, err := cd.TaskByID(ctx, ids[0])
	must(t, err)
	var in codegen.TaskInput
	must(t, json.Unmarshal(task.Input, &in))
	if in.PRNumber != pr.Number || in.ReviewAuthor != codegen.AutofixAuthor || in.Comment == "" {
		t.Fatalf("CHK-50: %+v", in)
	}
	finish(f, ids[0])
	if checks != 2 || len(tasks(f)) != 1 || review(pr) != "not_required" {
		t.Fatalf("CHK-53: checks %d, tasks %d, review %s", checks, len(tasks(f)), review(pr))
	}

	// CHK-54, CHK-57: not fixed — no new task; the autonomous PR needs a person.
	f, pr = feature(true)
	checks, findings = 0, [][]map[string]any{found(pr), found(pr)}
	must(t, validation.Start(ctx, pool, f.ID))
	drain(t, engine)
	ids = tasks(f)
	finish(f, ids[0])
	if checks != 2 || len(tasks(f)) != 1 || review(pr) != "required" {
		t.Fatalf("CHK-54: checks %d, tasks %d, review %s", checks, len(tasks(f)), review(pr))
	}

	// CHK-56: a new round of validation may fix once more.
	must(t, cd.SetPRReview(ctx, pr.ID, "not_required"))
	_, err = workflows.SendToSubject(ctx, pool, validation.Kind, f.ID, "returned", map[string]string{"target": "code"})
	must(t, err)
	drain(t, engine)
	checks, findings = 0, [][]map[string]any{found(pr)}
	must(t, validation.Start(ctx, pool, f.ID))
	drain(t, engine)
	if len(tasks(f)) != 2 {
		t.Fatalf("CHK-56: tasks %d", len(tasks(f)))
	}

	// CHK-52: an active fix of the service: no second task, a person sees it.
	f, pr = feature(true)
	_, err = codegen.StartTask(ctx, pool, codegen.TaskSpec{Type: codegen.TaskReview, FeatureID: &f.ID, ServiceID: svcIDs[0], Input: codegen.TaskInput{PRNumber: pr.Number, Comment: "human"}})
	must(t, err)
	checks, findings = 0, [][]map[string]any{found(pr)}
	must(t, validation.Start(ctx, pool, f.ID))
	drain(t, engine)
	if len(tasks(f)) != 1 || review(pr) != "required" {
		t.Fatalf("CHK-52: tasks %d, review %s", len(tasks(f)), review(pr))
	}
}
