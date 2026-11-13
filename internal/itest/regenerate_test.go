//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/codegen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	gmocks "github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// FTR.HMR.DEV-0004 qa REG-04: the regeneration of a done service — who may, when it may, the task it starts
// with the comment of the expert and the feedback of the PR; a PR that cannot be read does not stop it.
func TestRegenerateCode(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "regenerate")
	must(t, err)
	defer drop()
	pool, err := postgres.Connect(ctx, url)
	must(t, err)
	defer pool.Close()
	must(t, postgres.Migrate(ctx, url))

	var user, sys, svc uuid.UUID
	must(t, pool.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, agent_name, agent_tone)
		VALUES ('rg1', 'ira', 'Ira', 'Hammurapi', 'business') RETURNING id`).Scan(&user))
	must(t, pool.QueryRow(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('FMS', 'Fleet') RETURNING id)
		INSERT INTO systems (domain_id, key, name) SELECT id, 'CAR', 'Cars' FROM d RETURNING id`).Scan(&sys))
	must(t, pool.QueryRow(ctx, `INSERT INTO services (key, name, repo, source) VALUES ('pricing', 'pricing', 'fms/pricing', 'manual') RETURNING id`).Scan(&svc))
	store := specdata.NewPG(pool)
	f := &specdata.Feature{UniqueID: "FTR.FMS.CAR-0005", SystemID: sys, Number: 5, Title: "t", Branch: "feature/FTR.FMS.CAR-0005", PRNumber: 1, PRURL: "u",
		Phase: domain.PhaseCodegen, CreatedBy: user}
	must(t, store.InsertFeature(ctx, f))
	_, err = pool.Exec(ctx, `INSERT INTO feature_services (feature_id, service_id) VALUES ($1, $2)`, f.ID, svc)
	must(t, err)
	cd := cycledata.New(pool)
	pr := &cycledata.PR{Repo: "fms/pricing", Number: 15, URL: "u", Title: "t", Branch: "hammurapi/FTR.FMS.CAR-0005/pricing", Kind: "service",
		FeatureID: f.ID, ServiceID: &svc, ByAgent: true, State: "open", Review: "required", HeadSHA: "h1"}
	must(t, cd.UpsertPR(ctx, pr))
	must(t, cd.SetPRCI(ctx, pr.ID, git.CIFailure))

	ctrl := gomock.NewController(t)
	gp, repo := gmocks.NewMockProvider(ctrl), gmocks.NewMockProvider(ctrl)
	gp.EXPECT().ForRepo("fms/pricing").Return(repo).AnyTimes()
	repo.EXPECT().BotToken(gomock.Any()).Return("tok", nil).AnyTimes()
	svcCode := codegen.NewService(store, nopEvents{})
	svcCode.Git = gp
	expert := domain.NewPrincipal(user, "ira", "Ira", false)
	expert.Experts["FMS"] = map[domain.ExpertKind]bool{"technical": true}
	reader := domain.NewPrincipal(uuid.New(), "oleg", "Oleg", false)
	code := func(err error) string {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			return ae.Code
		}
		return ""
	}
	setLast := func(status string) uuid.UUID {
		id, err := codegen.StartTask(ctx, pool, codegen.TaskSpec{Type: codegen.TaskImplement, FeatureID: &f.ID, ServiceID: svc})
		must(t, err)
		_, err = pool.Exec(ctx, `UPDATE agent_tasks SET status = $2 WHERE id = $1`, id, status)
		must(t, err)
		return id
	}

	// nothing generated yet; a failed last task is retried, not regenerated
	if _, err := svcCode.Regenerate(ctx, expert, f.UniqueID, "pricing", ""); code(err) != "regenerate_unavailable" {
		t.Fatalf("REG-04 no task: %v", err)
	}
	setLast("failed")
	if _, err := svcCode.Regenerate(ctx, expert, f.UniqueID, "pricing", ""); code(err) != "regenerate_unavailable" {
		t.Fatalf("REG-04 failed: %v", err)
	}
	setLast("succeeded")
	if _, err := svcCode.Regenerate(ctx, reader, f.UniqueID, "pricing", ""); code(err) != "forbidden" {
		t.Fatalf("REG-04 not an expert: %v", err)
	}
	if _, err := svcCode.Regenerate(ctx, expert, f.UniqueID, "billing", ""); code(err) != "service_not_found" {
		t.Fatalf("REG-04 another service: %v", err)
	}

	// the head failed its CI: the comments and the checks of it
	fb := &git.PRFeedback{Comments: []git.PRComment{{Author: "max", Body: "off by one"}}, Failed: []git.FailedCheck{{Name: "test", Log: "FAIL"}}}
	repo.EXPECT().PRFeedback(gomock.Any(), "tok", 15, "h1").Return(fb, nil)
	r, err := svcCode.Regenerate(ctx, expert, f.UniqueID, "pricing", "  Keep the old endpoint ")
	must(t, err)
	if r.PRNumber != 15 || r.Comments != 1 || r.FailedChecks != 1 || r.FeedbackError {
		t.Fatalf("REG-04 answer %+v", r)
	}
	tk, err := cd.TaskByID(ctx, uuid.MustParse(r.TaskID))
	must(t, err)
	var in codegen.TaskInput
	must(t, json.Unmarshal(tk.Input, &in))
	if tk.Type != codegen.TaskImplement || tk.Status != "queued" || !in.Regenerate || in.Comment != "Keep the old endpoint" || in.PRNumber != 15 ||
		in.Feedback == nil || in.Feedback.Comments[0].Body != "off by one" || codegen.ReasonOf(tk) != "regenerate" {
		t.Fatalf("REG-04 task %+v %+v", tk, in)
	}
	// the task is queued: a second regeneration waits for it
	if _, err := svcCode.Regenerate(ctx, expert, f.UniqueID, "pricing", ""); code(err) != "task_running" {
		t.Fatalf("REG-04 twice: %v", err)
	}

	// the code generation of the feature has finished: the task has no parent
	var parent *uuid.UUID
	must(t, pool.QueryRow(ctx, `SELECT parent_id FROM workflow_runs WHERE subject_id = $1`, tk.ID).Scan(&parent))
	if parent != nil {
		t.Fatalf("REG-04 a parent without a running code generation: %v", parent)
	}

	// green CI: only the comments, without the summaries of the agent; a provider that fails does not stop the regeneration
	_, err = pool.Exec(ctx, `UPDATE agent_tasks SET status = 'succeeded' WHERE id = $1`, tk.ID)
	must(t, err)
	must(t, cd.SetPRCI(ctx, pr.ID, git.CISuccess))
	repo.EXPECT().PRFeedback(gomock.Any(), "tok", 15, "").Return(nil, errors.New("github down"))
	r, err = svcCode.Regenerate(ctx, expert, f.UniqueID, "pricing", "")
	must(t, err)
	if !r.FeedbackError || r.TaskID == "" {
		t.Fatalf("REG-04 without feedback %+v", r)
	}

	// the code generation still waits for its tasks: the regeneration is one of them; the summaries of the agent are dropped
	_, err = pool.Exec(ctx, `UPDATE agent_tasks SET status = 'succeeded'`)
	must(t, err)
	run, err := workflows.Start(ctx, pool, codegen.Kind, f.ID, nil, "tasks_running", map[string]any{})
	must(t, err)
	repo.EXPECT().PRFeedback(gomock.Any(), "tok", 15, "").Return(&git.PRFeedback{Comments: []git.PRComment{
		{Author: "bot", Body: "Hammurapi agent regenerated the code: done"}, {Author: "max", Body: "rename it"}}}, nil)
	r, err = svcCode.Regenerate(ctx, expert, f.UniqueID, "pricing", "")
	must(t, err)
	must(t, pool.QueryRow(ctx, `SELECT parent_id FROM workflow_runs WHERE subject_id = $1`, uuid.MustParse(r.TaskID)).Scan(&parent))
	if parent == nil || *parent != run || r.Comments != 1 {
		t.Fatalf("REG-04 the parent %v, want %v; comments %d", parent, run, r.Comments)
	}
	if _, err := svcCode.Regenerate(ctx, expert, f.UniqueID, "pricing", string(make([]rune, 4001))); code(err) != "comment_too_long" {
		t.Fatalf("REG-04 a long comment: %v", err)
	}

	// released: no regeneration
	_, err = pool.Exec(ctx, `UPDATE features SET phase = 'in_release' WHERE id = $1`, f.ID)
	must(t, err)
	if _, err := svcCode.Regenerate(ctx, expert, f.UniqueID, "pricing", ""); code(err) != "regenerate_unavailable" {
		t.Fatalf("REG-04 in release: %v", err)
	}
}
