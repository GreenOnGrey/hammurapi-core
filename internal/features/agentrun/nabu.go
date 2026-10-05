package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
)

// Bindings name the service agent of Nabu bound to a scenario ("" — none).
type Bindings interface {
	AgentFor(ctx context.Context, sc agent.Scenario) string
}

// NabuRuns runs scenarios through service agents of Nabu (FTR.HMR.CMN-0006
// R8, arch §5: the adapter nabuagent): start, events, cancel.
type NabuRuns struct {
	Client   *nabu.Client
	Bindings Bindings
	// CallerMCPURL is the MCP of the worker as Nabu reaches it.
	CallerMCPURL string
}

// agentFor returns the agent bound to the scenario when Nabu is connected.
func (n *NabuRuns) agentFor(ctx context.Context, sc agent.Scenario) string {
	if n == nil || n.Client == nil || n.Bindings == nil {
		return ""
	}
	return n.Bindings.AgentFor(ctx, sc)
}

// errNoBackend: neither Nabu nor the operator serves the scenario.
func (n *NabuRuns) errNoBackend(sc agent.Scenario) error {
	if domain.AgentDisabled() {
		return workflows.Permanent(apperr.AgentDisabled())
	}
	if n != nil && n.Client != nil {
		return workflows.Permanent(fmt.Errorf("%w: bind the scenario %s to a service agent in Administration → Nabu", ErrNoAgent, sc))
	}
	return workflows.Permanent(ErrNoAgent)
}

// onceNabu runs one scenario as a run of a service agent. The worker MCP is
// passed as callerMcp with a token of the grant; errors of Nabu keep the
// classes of FTR.HMR.CMN-0004, so the workflows behave as before.
func (r *Runner) onceNabu(ctx context.Context, agentName string, sc agent.Scenario, g mcp.Grant, system, prompt string) (Outcome, error) {
	out := Outcome{Results: map[string][]json.RawMessage{}, Model: "nabu:" + agentName}
	var mu sync.Mutex
	g.Sink = func(kind string, payload json.RawMessage) error {
		mu.Lock()
		defer mu.Unlock()
		out.Results[kind] = append(out.Results[kind], append(json.RawMessage(nil), payload...))
		return nil
	}
	token := r.MCP.Issue(g)
	defer r.MCP.Revoke(token)
	runCtx, _ := json.Marshal(map[string]any{"scenario": sc, "subject": g.Subject, "contextKey": g.ContextKey, "feature": g.Feature})
	in := nabu.RunInput{
		Input:   system + "\n\n" + prompt,
		Context: runCtx,
		CallerMCP: []nabu.CallerMCP{{Name: "hammurapi", URL: r.Nabu.CallerMCPURL,
			Headers: map[string]string{"Authorization": "Bearer " + token}}},
		IdempotencyKey: "scenario:" + string(sc) + ":" + uuid.NewString(),
	}
	started, err := r.Nabu.Client.StartRun(ctx, agentName, in)
	if err != nil {
		return out, nabuStartErr(agentName, err)
	}
	var b strings.Builder
	var failure *nabuFailure
	err = nabu.ReadEvents(ctx, started.EventsURL, started.EventsToken, func(e nabu.RunEvent) {
		switch e.Type {
		case "text_delta":
			var d struct{ Delta string }
			_ = json.Unmarshal(e.Data, &d)
			mu.Lock()
			b.WriteString(d.Delta)
			mu.Unlock()
		case "usage":
			var u agent.Usage
			if json.Unmarshal(e.Data, &u) == nil {
				out.Usage.Add(u)
			}
		case "error":
			var f nabuFailure
			if json.Unmarshal(e.Data, &f) == nil {
				failure = &f
			}
		case "completed":
			var c struct {
				Status     string `json:"status"`
				ErrorClass string `json:"errorClass"`
				Summary    string `json:"summary"`
			}
			_ = json.Unmarshal(e.Data, &c)
			if c.Status != "" && c.Status != "succeeded" && failure == nil {
				failure = &nabuFailure{Class: agent.ErrorClass(c.ErrorClass), Message: c.Summary}
				if failure.Class == "" {
					failure.Class = agent.ErrAgentCrashed
				}
			}
		}
	})
	mu.Lock()
	out.Text = b.String()
	mu.Unlock()
	if err != nil {
		// The run is not followed any more: stop it in Nabu as well.
		_ = r.Nabu.Client.CancelRun(context.WithoutCancel(ctx), started.RunID)
		if ctx.Err() != nil {
			return out, err
		}
		return out, nabuStartErr(agentName, err)
	}
	if failure != nil {
		le := &LLMError{Class: failure.Class, Connection: "Nabu: " + agentName, Message: failure.Message}
		if failure.Class.Retryable() && failure.Class != agent.ErrContextOverflow {
			return out, le
		}
		return out, workflows.Permanent(le)
	}
	return out, nil
}

type nabuFailure struct {
	Class   agent.ErrorClass `json:"errorClass"`
	Message string           `json:"message"`
}

// nabuStartErr: Nabu not answering is retried; a refusal (agent unknown or
// not allowed, a client without rights, a rejected events token) blocks the
// run with the reason.
func nabuStartErr(agentName string, err error) error {
	if errors.Is(err, nabu.ErrUnavailable) {
		return &LLMError{Class: agent.ErrUnavailable, Connection: "Nabu: " + agentName, Message: err.Error()}
	}
	var ae *nabu.APIError
	if errors.As(err, &ae) {
		if ae.Status == 429 {
			return &LLMError{Class: agent.ErrRateLimit, Connection: "Nabu: " + agentName, Message: ae.Message}
		}
		// Not a model error: the reason of Nabu is shown to experts as is.
		return workflows.Permanent(fmt.Errorf("the run of %s was refused by Nabu: %s: %s", agentName, ae.Code, ae.Message))
	}
	return err
}
