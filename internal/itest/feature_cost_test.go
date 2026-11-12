//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/codegen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/usage"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// FTR.HMR.DEV-0004 qa COST-01, COST-03, COST-07, COST-09, COST-10, COST-18, COST-20, CKN-03:
// the attribution of runs and GET /api/v1/features/{id}/cost.
func TestFeatureCost(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "featurecost")
	must(t, err)
	defer drop()
	db, err := postgres.Connect(ctx, url)
	must(t, err)
	defer db.Close()
	must(t, postgres.Migrate(ctx, url))

	var user, feature, svc uuid.UUID
	must(t, db.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, agent_name, agent_tone)
		VALUES ('fc1', 'ira', 'Ira', 'Hammurapi', 'business') RETURNING id`).Scan(&user))
	must(t, db.QueryRow(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('FMS', 'Fleet') RETURNING id),
		s AS (INSERT INTO systems (domain_id, key, name) SELECT id, 'CAR', 'Cars' FROM d RETURNING id)
		INSERT INTO features (unique_id, system_id, number, title, branch_name, pr_number, pr_url, phase, created_by)
		SELECT 'FTR.FMS.CAR-0005', id, 5, 'Скидка', 'feature/FTR.FMS.CAR-0005', 1, 'http://pr', 'codegen', $1 FROM s RETURNING id`, user).Scan(&feature))
	must(t, db.QueryRow(ctx, `INSERT INTO services (key, name, repo, source) VALUES ('booking', 'booking', 'fms/booking', 'manual') RETURNING id`).Scan(&svc))
	run := uuid.New()
	task := func(status string, input string) uuid.UUID {
		var id uuid.UUID
		must(t, db.QueryRow(ctx, `INSERT INTO agent_tasks (type, status, feature_id, service_id, workflow_run_id, input, nabu_run_id)
			VALUES ('implement', $1, $2, $3, gen_random_uuid(), $4, $5) RETURNING id`, status, feature, svc, input, run).Scan(&id))
		return id
	}

	// COST-18, COST-20: a row of a code task written before the attribution gets the service and the run of its task
	failed := task("failed", `{}`)
	_, err = db.Exec(ctx, `INSERT INTO agent_usage (context, scenario, model, feature_id, task_id, tokens_in, tokens_out, cost_usd)
		VALUES ('codegen', 'codegen', 'nabu', $1, $2, 100, 10, 35.18)`, feature, failed)
	must(t, err)
	_, err = db.Exec(ctx, `UPDATE agent_usage u SET service_id = t.service_id, nabu_run_id = t.nabu_run_id
		FROM agent_tasks t WHERE u.task_id = t.id AND u.service_id IS NULL`) // what migration 31 does to the old rows
	must(t, err)

	us := &usage.Service{Pool: db}
	f := false
	// COST-01: the writer generates tech, then repeats it; a run for tech and qa at once
	must(t, us.Add(ctx, usage.Record{Context: "gate", Scenario: agent.ScenarioGateGeneration, FeatureID: &feature, Areas: []string{"tech"},
		Reason: "generate", RunID: &run, Usage: agent.Usage{TokensIn: 10, CostUSD: 1.22}}))
	must(t, us.Add(ctx, usage.Record{Context: "gate", Scenario: agent.ScenarioGateGeneration, FeatureID: &feature, Areas: []string{"tech", "qa"},
		Reason: "regenerate", Usage: agent.Usage{TokensIn: 120, CacheRead: 1800, CostUSD: 3}}))
	// CKN-03: a run on a model without a price
	must(t, us.Add(ctx, usage.Record{Context: "gate", Scenario: agent.ScenarioGateGeneration, FeatureID: &feature, Areas: []string{"tech"},
		Reason: "request", Usage: agent.Usage{TokensIn: 40, CostKnown: &f}}))
	// COST-03: the retry of the failed task; its reason comes from the input of the task
	retry := task("succeeded", `{"retryOf":"`+failed.String()+`"}`)
	tk, err := cycledata.New(db).TaskByID(ctx, retry)
	must(t, err)
	if r := codegen.ReasonOf(tk); r != "retry" {
		t.Fatalf("COST-03 reason %q", r)
	}

	r := chi.NewRouter()
	as := func(p *domain.Principal) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			r.ServeHTTP(w, req.WithContext(httpx.WithPrincipal(req.Context(), p)))
		})
	}
	us.CostRoutes(r)
	reader := domain.NewPrincipal(user, "ira", "Ira", false)
	expert := domain.NewPrincipal(user, "ira", "Ira", false)
	expert.Experts["FMS"] = map[domain.ExpertKind]bool{"product": true}
	get := func(p *domain.Principal, path string) (int, usage.FeatureCost) {
		srv := httptest.NewServer(as(p))
		defer srv.Close()
		resp, err := http.Get(srv.URL + path)
		must(t, err)
		defer resp.Body.Close()
		var out usage.FeatureCost
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	code, c := get(reader, "/features/FTR.FMS.CAR-0005/cost?detail=area:tech")
	if code != 200 || c.CanViewLog {
		t.Fatalf("COST-10 a reader sees the cost, not the log: %d %v", code, c.CanViewLog)
	}
	if c.Total.Runs != 4 || c.Total.UnknownRuns != 1 || !near(c.Total.CostUSD, 1.22+3+35.18) {
		t.Fatalf("COST-07 total %+v", c.Total)
	}
	tech := c.Items[0]
	if tech.Key == nil || *tech.Key != "tech" || tech.Runs != 3 || len(tech.List) != 3 || !near(tech.CostUSD, 1.22+1.5) {
		t.Fatalf("COST-01 tech %+v", tech)
	}
	reasons := []string{}
	for _, run := range tech.List {
		reasons = append(reasons, *run.Reason)
	}
	if fmtList(reasons) != "[request regenerate generate]" || tech.List[0].CostUSD != nil {
		t.Fatalf("COST-01, CKN-03 runs %v", reasons)
	}
	if svc := c.Items[2]; svc.Kind != usage.CostService || *svc.Key != "booking" || !near(svc.CostUSD, 35.18) {
		t.Fatalf("COST-20 the old row of the code task %+v", svc)
	}
	if _, c := get(expert, "/features/FTR.FMS.CAR-0005/cost"); !c.CanViewLog || c.Items[0].List != nil {
		t.Fatalf("an expert sees the log; no list without detail: %+v", c)
	}
	if code, _ := get(reader, "/features/FTR.FMS.CAR-0404/cost"); code != 404 {
		t.Fatalf("COST-10 an unknown feature: %d", code)
	}
	// COST-09: the usage of administrators counts the same rows
	var sum float64
	must(t, db.QueryRow(ctx, `SELECT COALESCE(sum(cost_usd), 0)::float8 FROM agent_usage WHERE feature_id = $1 AND cost_known`, feature).Scan(&sum))
	if !near(sum, c.Total.CostUSD) {
		t.Fatalf("COST-09 %v != %v", sum, c.Total.CostUSD)
	}
}

func near(a, b float64) bool { return a-b < 1e-6 && b-a < 1e-6 }

func fmtList(xs []string) string { return "[" + strings.Join(xs, " ") + "]" }
