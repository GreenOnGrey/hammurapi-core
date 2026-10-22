package cycledata

import (
	"fmt"
	"math"
)

// The draft estimate of the work in the Discovery document (FTR.HMR.DISC-0001
// R15–R18, tech §5.1): the work is done by agents with people taking part.

// Stages of the estimate.
var (
	AgentStages  = []string{"spec", "codegen", "checks"}
	PeopleStages = []string{"spec", "approval", "validation", "release"}
	Expertises   = []string{"product", "tech"}
	Confidences  = []string{"high", "medium", "low"}
)

// EstimateAgent is the cost of the runs of agents at a stage.
type EstimateAgent struct {
	Stage  string  `json:"stage"`
	USD    float64 `json:"usd"`
	Tokens int64   `json:"tokens"`
}

// EstimatePeople is the work of people of one expertise at a stage, in person-hours.
type EstimatePeople struct {
	Stage       string  `json:"stage"`
	Expertise   string  `json:"expertise"`
	PersonHours float64 `json:"personHours"`
}

// EstimateBasis is what the estimate rests on: similar released features or
// the judgement of the researcher.
type EstimateBasis struct {
	Features  []string `json:"features"`
	Judgement bool     `json:"judgement"`
}

// Estimate is the draft estimate of the work.
type Estimate struct {
	Agent      []EstimateAgent  `json:"agent"`
	People     []EstimatePeople `json:"people"`
	Days       int              `json:"days"`
	WaitDays   int              `json:"waitDays"`
	Confidence string           `json:"confidence"`
	Basis      EstimateBasis    `json:"basis"`
}

// EstimateTotals is the summary line of an estimate (R17).
type EstimateTotals struct {
	AgentUSD    float64 `json:"agentUsd"`
	AgentTokens int64   `json:"agentTokens"`
	PersonHours float64 `json:"personHours"`
	Days        int     `json:"days"`
	Confidence  string  `json:"confidence"`
}

// Totals sums the estimate; the totals are never stored (tech decision 6).
func (e *Estimate) Totals() *EstimateTotals {
	if e == nil {
		return nil
	}
	t := &EstimateTotals{Days: e.Days, Confidence: e.Confidence}
	for _, a := range e.Agent {
		t.AgentUSD += a.USD
		t.AgentTokens += a.Tokens
	}
	for _, p := range e.People {
		t.PersonHours += p.PersonHours
	}
	t.AgentUSD = math.Round(t.AgentUSD*100) / 100
	return t
}

// EstimateError names the field of an estimate that is wrong.
type EstimateError struct{ Field, Msg string }

func (e *EstimateError) Error() string { return e.Field + ": " + e.Msg }

func in(v string, set []string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// MaxBasisFeatures bounds the features an estimate rests on.
const MaxBasisFeatures = 10

// Validate checks the estimate (tech §5.1, ES-02). The existence of the
// features of the basis is checked by the caller with the database.
func (e *Estimate) Validate() error {
	bad := func(field, format string, a ...any) error { return &EstimateError{field, fmt.Sprintf(format, a...)} }
	seen := map[string]bool{}
	for i, a := range e.Agent {
		f := fmt.Sprintf("agent[%d]", i)
		switch {
		case !in(a.Stage, AgentStages):
			return bad(f+".stage", "one of %v", AgentStages)
		case seen[a.Stage]:
			return bad(f+".stage", "the stage %s is given twice", a.Stage)
		case a.USD < 0 || math.IsNaN(a.USD) || math.IsInf(a.USD, 0):
			return bad(f+".usd", "not negative")
		case a.Tokens < 0:
			return bad(f+".tokens", "not negative")
		}
		seen[a.Stage] = true
	}
	seen = map[string]bool{}
	for i, p := range e.People {
		f := fmt.Sprintf("people[%d]", i)
		switch {
		case !in(p.Stage, PeopleStages):
			return bad(f+".stage", "one of %v", PeopleStages)
		case !in(p.Expertise, Expertises):
			return bad(f+".expertise", "one of %v", Expertises)
		case seen[p.Stage+"/"+p.Expertise]:
			return bad(f, "the stage %s and the expertise %s are given twice", p.Stage, p.Expertise)
		case p.PersonHours < 0 || math.IsNaN(p.PersonHours) || math.Mod(p.PersonHours*2, 1) != 0:
			return bad(f+".personHours", "not negative, in steps of 0.5")
		}
		seen[p.Stage+"/"+p.Expertise] = true
	}
	switch {
	case e.Days < 1 || e.Days > 365:
		return bad("days", "1–365")
	case e.WaitDays < 0 || e.WaitDays > e.Days:
		return bad("waitDays", "0–days")
	case !in(e.Confidence, Confidences):
		return bad("confidence", "one of %v", Confidences)
	case len(e.Basis.Features) > MaxBasisFeatures:
		return bad("basis.features", "at most %d", MaxBasisFeatures)
	}
	if e.Basis.Features == nil {
		e.Basis.Features = []string{}
	}
	if len(e.Basis.Features) == 0 {
		e.Basis.Judgement = true
	}
	return nil
}

// MinBasisForHigh is how many similar features a high confidence needs (R16).
const MinBasisForHigh = 3

// ApplyResearcherRule lowers the confidence of an estimate of the researcher
// to what its basis allows (R16, ES-03, ES-04): high needs three similar
// features, none means low. A manual estimate of an expert keeps its
// confidence (ES-05).
func (e *Estimate) ApplyResearcherRule() {
	n := len(e.Basis.Features)
	switch {
	case n == 0:
		e.Confidence, e.Basis.Judgement = "low", true
	case e.Confidence == "high" && n < MinBasisForHigh:
		e.Confidence = "medium"
	}
}
