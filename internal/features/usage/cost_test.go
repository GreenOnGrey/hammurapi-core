package usage

import (
	"math"
	"testing"
	"time"

	"github.com/google/uuid"
)

func ptr[T any](v T) *T { return &v }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// FTR.HMR.DEV-0004 qa COST-02, COST-07, COST-08, COST-21, CKN-03: the items of the cost of a feature in the order of the stages.
func TestGroupCost(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	run := uuid.New()
	rows := []costRow{
		// a run of the writer for tech and qa for $3.00 (R4a)
		{id: uuid.New(), runID: &run, at: now.Add(-time.Hour), reason: ptr("regenerate"), costKnown: true, areas: []string{"tech", "qa"},
			context: "gate", tokens: Tokens{In: 120, Out: 36, CacheRead: 1800, CacheWrite: 24}, cost: 3},
		{id: uuid.New(), at: now.Add(-2 * time.Hour), reason: ptr("generate"), costKnown: true, areas: []string{"tech"}, context: "gate", cost: 1.22},
		// a run on a model without a price (R6)
		{id: uuid.New(), at: now.Add(-3 * time.Hour), reason: ptr("request"), costKnown: false, areas: []string{"tech"}, context: "gate",
			tokens: Tokens{In: 40}},
		// a failed code task and its retry (R5)
		{id: uuid.New(), at: now.Add(-4 * time.Hour), reason: ptr("first"), costKnown: true, service: ptr("nabu-core"), scenario: ptr("codegen"),
			context: "codegen", cost: 35.18, taskStatus: ptr("failed")},
		{id: uuid.New(), at: now.Add(-time.Minute), costKnown: true, service: ptr("nabu-core"), cost: 6.02, running: true},
		// the check and a run of the writer before the attribution
		{id: uuid.New(), at: now.Add(-5 * time.Hour), costKnown: true, scenario: ptr("conformance_check"), context: "check", cost: 1.61},
		{id: uuid.New(), at: now.AddDate(0, -2, 0), costKnown: true, context: "gate", cost: 2},
		// the analysis of an issue of the feature
		{id: uuid.New(), at: now.AddDate(0, -3, 0), costKnown: true, scenario: ptr("issue_analysis"), context: "discovery", cost: 0.4},
	}
	got := groupCost(rows, "area:tech", now)

	if !near(got.Total.CostUSD, 3+1.22+35.18+6.02+1.61+2+0.4) || got.Total.Runs != 8 || got.Total.UnknownRuns != 1 || got.Total.Running != 1 {
		t.Fatalf("COST-07 total %+v", got.Total)
	}
	var kinds []string
	var sum float64
	for _, it := range got.Items {
		k := it.Kind
		if it.Key != nil {
			k += ":" + *it.Key
		}
		kinds = append(kinds, k)
		sum += it.CostUSD
	}
	if want := "[research area:tech area:qa legacy service:nabu-core checks]"; fmtList(kinds) != want {
		t.Fatalf("items %v, want %s", kinds, want)
	}
	if !near(sum, got.Total.CostUSD) {
		t.Fatalf("COST-07 the items sum to %v, the total is %v", sum, got.Total.CostUSD)
	}
	tech := got.Items[1]
	if !near(tech.CostUSD, 1.5+1.22) || tech.Runs != 3 || tech.UnknownRuns != 1 || len(tech.List) != 3 {
		t.Fatalf("COST-02 tech %+v", tech)
	}
	shared := tech.List[0]
	if shared.Shared == nil || !near(*shared.CostUSD, 1.5) || !near(*shared.Shared.CostUSD, 3) || !near(shared.Tokens.CacheRead, 900) ||
		!shared.LogAvailable || shared.RunID == nil {
		t.Fatalf("COST-02 the shared run %+v", shared)
	}
	if unknown := tech.List[2]; unknown.CostUSD != nil || unknown.CostKnown {
		t.Fatalf("CKN-03 a run without a price %+v", unknown)
	}
	if svc := got.Items[4]; svc.Runs != 2 || svc.Running != 1 || !near(svc.CostUSD, 41.2) || svc.List != nil {
		t.Fatalf("COST-03 nabu-core %+v", svc)
	}
	// the detail of a service: a failed run keeps its status; an old run has no log
	d := groupCost(rows, "service:nabu-core", now).Items[4].List
	if len(d) != 2 || d[0].Status != "running" || d[1].Status != "failed" || d[1].LogAvailable {
		t.Fatalf("COST-03 runs %+v", d)
	}
	if l := groupCost(rows, "legacy", now).Items[3].List; len(l) != 1 || l[0].LogAvailable {
		t.Fatalf("COST-08 legacy %+v", l)
	}
	if r := groupCost(rows, "research", now).Items[0]; r.Kind != CostResearch || r.Runs != 1 || !near(r.CostUSD, 0.4) || len(r.List) != 1 {
		t.Fatalf("COST-21 research %+v", r)
	}
}

func fmtList(xs []string) string {
	s := "["
	for i, x := range xs {
		if i > 0 {
			s += " "
		}
		s += x
	}
	return s + "]"
}
