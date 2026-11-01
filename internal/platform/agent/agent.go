// Package agent holds what is left of the agent in Hammurapi after it moved to
// Nabu (FTR.HMR.CMN-0007 R3): the scenarios, the classes of LLM errors and the
// usage of a run.
package agent

import "fmt"

// Scenario is what the agent is doing (tech spec §1).
type Scenario string

// Scenarios of FTR.HMR.CMN-0004 §4.
const (
	ScenarioChat             Scenario = "chat"
	ScenarioIssueAnalysis    Scenario = "issue_analysis"
	ScenarioGateGeneration   Scenario = "gate_generation"
	ScenarioConformanceCheck Scenario = "conformance_check"
	ScenarioCodegen          Scenario = "codegen"
	ScenarioReviewUpdate     Scenario = "review_update"
	ScenarioRollbackRevert   Scenario = "rollback_revert"
)

// Scenarios lists every scenario in the order of the H-SDLC stages.
var Scenarios = []Scenario{ScenarioChat, ScenarioIssueAnalysis, ScenarioGateGeneration, ScenarioConformanceCheck,
	ScenarioCodegen, ScenarioReviewUpdate, ScenarioRollbackRevert}

// Valid reports whether s is a known scenario.
func (s Scenario) Valid() bool {
	for _, x := range Scenarios {
		if x == s {
			return true
		}
	}
	return false
}

// HasCode reports whether the scenario changes code: its service agent works
// on the working copy in the sandbox of Nabu (FTR.HMR.CMN-0007 R5).
func (s Scenario) HasCode() bool {
	return s == ScenarioCodegen || s == ScenarioReviewUpdate || s == ScenarioRollbackRevert
}

// ParseScenario validates a scenario name.
func ParseScenario(s string) (Scenario, error) {
	if !Scenario(s).Valid() {
		return "", fmt.Errorf("unknown scenario %q", s)
	}
	return Scenario(s), nil
}

// ErrorClass is the class of an LLM failure (arch §8).
type ErrorClass string

// Error classes.
const (
	ErrInsufficientBalance ErrorClass = "insufficient_balance"
	ErrAuth                ErrorClass = "auth"
	ErrRateLimit           ErrorClass = "rate_limit"
	ErrUnavailable         ErrorClass = "unavailable"
	ErrBadRequest          ErrorClass = "bad_request"
	ErrContextOverflow     ErrorClass = "context_overflow"
	ErrAgentCrashed        ErrorClass = "agent_crashed"
)

// Retryable reports whether the user may retry later without an
// administrator fixing the connection.
func (c ErrorClass) Retryable() bool {
	return c == ErrRateLimit || c == ErrUnavailable || c == ErrAgentCrashed || c == ErrContextOverflow
}

// ConnectionProblem reports whether the class marks the connection as broken
// (shown to global administrators in "In focus").
func (c ErrorClass) ConnectionProblem() bool { return c == ErrInsufficientBalance || c == ErrAuth }

// Usage is token use and cost.
type Usage struct {
	TokensIn   int64   `json:"tokensIn,omitempty"`
	TokensOut  int64   `json:"tokensOut,omitempty"`
	CacheRead  int64   `json:"cacheRead,omitempty"`
	CacheWrite int64   `json:"cacheWrite,omitempty"`
	CostUSD    float64 `json:"costUsd,omitempty"`
}

// Add accumulates u2 into u.
func (u *Usage) Add(u2 Usage) {
	u.TokensIn += u2.TokensIn
	u.TokensOut += u2.TokensOut
	u.CacheRead += u2.CacheRead
	u.CacheWrite += u2.CacheWrite
	u.CostUSD += u2.CostUSD
}

// IsZero reports whether nothing was used.
func (u Usage) IsZero() bool {
	return u.TokensIn == 0 && u.TokensOut == 0 && u.CacheRead == 0 && u.CacheWrite == 0 && u.CostUSD == 0
}
