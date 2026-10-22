package cycledata

import (
	"errors"
	"testing"
)

func validEstimate() *Estimate {
	return &Estimate{
		Agent:  []EstimateAgent{{Stage: "spec", USD: 9, Tokens: 1_100_000}, {Stage: "codegen", USD: 26.004, Tokens: 3_400_000}},
		People: []EstimatePeople{{Stage: "spec", Expertise: "product", PersonHours: 3}, {Stage: "validation", Expertise: "tech", PersonHours: 2.5}},
		Days:   9, WaitDays: 4, Confidence: "medium", Basis: EstimateBasis{Features: []string{"FTR.FMS.CAR-0003"}},
	}
}

// FTR.HMR.DISC-0001 ES-02: the estimate names the wrong field.
func TestEstimateValidate(t *testing.T) {
	if err := validEstimate().Validate(); err != nil {
		t.Fatal(err)
	}
	for field, change := range map[string]func(e *Estimate){
		"agent[0].stage":        func(e *Estimate) { e.Agent[0].Stage = "deploy" },
		"agent[1].stage":        func(e *Estimate) { e.Agent[1].Stage = "spec" },
		"agent[0].usd":          func(e *Estimate) { e.Agent[0].USD = -1 },
		"people[0].expertise":   func(e *Estimate) { e.People[0].Expertise = "design" },
		"people[1].personHours": func(e *Estimate) { e.People[1].PersonHours = 1.3 },
		"people[1]":             func(e *Estimate) { e.People[1] = e.People[0] },
		"days":                  func(e *Estimate) { e.Days = 0 },
		"waitDays":              func(e *Estimate) { e.WaitDays = 10 },
		"confidence":            func(e *Estimate) { e.Confidence = "sure" },
	} {
		e := validEstimate()
		change(e)
		var ee *EstimateError
		if err := e.Validate(); !errors.As(err, &ee) || ee.Field != field {
			t.Fatalf("ES-02 %s: %v", field, err)
		}
	}
	e := validEstimate()
	e.Basis.Features = nil
	if err := e.Validate(); err != nil || !e.Basis.Judgement || e.Basis.Features == nil {
		t.Fatalf("an empty basis is a judgement: %+v %v", e.Basis, err)
	}
}

// ES-03, ES-04: the rule of the confidence of the researcher (R16).
func TestEstimateResearcherRule(t *testing.T) {
	e := validEstimate()
	e.Confidence = "high"
	e.Basis.Features = []string{"A", "B"}
	e.ApplyResearcherRule()
	if e.Confidence != "medium" {
		t.Fatalf("ES-03: %s", e.Confidence)
	}
	e.Confidence, e.Basis.Features = "high", []string{"A", "B", "C"}
	e.ApplyResearcherRule()
	if e.Confidence != "high" {
		t.Fatalf("three features keep high: %s", e.Confidence)
	}
	e.Confidence, e.Basis.Features = "medium", []string{}
	e.ApplyResearcherRule()
	if e.Confidence != "low" || !e.Basis.Judgement {
		t.Fatalf("ES-04: %+v", e)
	}
}

// R17: the totals of the summary line.
func TestEstimateTotals(t *testing.T) {
	tt := validEstimate().Totals()
	if tt.AgentUSD != 35 || tt.AgentTokens != 4_500_000 || tt.PersonHours != 5.5 || tt.Days != 9 || tt.Confidence != "medium" {
		t.Fatalf("%+v", tt)
	}
	var none *Estimate
	if none.Totals() != nil {
		t.Fatal("no estimate — no totals")
	}
}
