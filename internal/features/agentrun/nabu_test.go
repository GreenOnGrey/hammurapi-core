package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
)

type bindings map[agent.Scenario]string

func (b bindings) AgentFor(_ context.Context, sc agent.Scenario) string { return b[sc] }

// fakeRuns answers a run start and streams the given events; onStart sees the
// body of the start (to call the worker MCP as the agent would).
func fakeRuns(t *testing.T, evs string, onStart func(in nabu.RunInput)) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/oauth/token":
			_, _ = io.WriteString(w, `{"access_token":"t","expires_in":3600}`)
		case strings.HasSuffix(r.URL.Path, "/runs") && r.URL.Path == "/client/v1/agents/hammurapi-analysis/runs":
			var in nabu.RunInput
			_ = json.NewDecoder(r.Body).Decode(&in)
			onStart(in)
			_ = json.NewEncoder(w).Encode(map[string]string{"runId": "r1", "eventsUrl": srv.URL + "/events", "eventsToken": "ev"})
		case r.URL.Path == "/events":
			if r.Header.Get("Authorization") != "Bearer ev" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, evs)
		default:
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"error":{"code":"agent_not_found","message":"no such agent"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// NB-09…11: a bound scenario runs as a run of the service agent; the worker
// MCP goes as callerMcp with the grant's token; sink results come back.
func TestOnceNabu(t *testing.T) {
	srv := mcp.NewServer()
	srv.Register(mcp.Tool{Name: "save_discovery", Modes: []string{mcp.ModeDiscovery}, InputSchema: map[string]any{"type": "object"},
		Handler: func(_ context.Context, g mcp.Grant, args json.RawMessage) (string, error) {
			return "saved", g.Sink("discovery", args)
		}})
	mcpHTTP := httptest.NewServer(srv)
	defer mcpHTTP.Close()
	evs := "id: 1\nevent: text_delta\ndata: {\"delta\":\"Done\"}\n\n" +
		"id: 2\nevent: usage\ndata: {\"tokensIn\":10,\"tokensOut\":5}\n\n" +
		"id: 3\nevent: completed\ndata: {\"status\":\"succeeded\"}\n\n"
	var started nabu.RunInput
	fake := fakeRuns(t, evs, func(in nabu.RunInput) {
		started = in
		// The agent calls the tool of Hammurapi through callerMcp.
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"save_discovery","arguments":{"value":"v"}}}`
		req, _ := http.NewRequest(http.MethodPost, in.CallerMCP[0].URL, strings.NewReader(body))
		req.Header.Set("Authorization", in.CallerMCP[0].Headers["Authorization"])
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Errorf("callerMcp call: %v %v", err, resp)
		}
		if resp != nil {
			resp.Body.Close()
		}
	})
	r := &Runner{MCP: srv, Nabu: &NabuRuns{Client: nabu.New(fake.URL, "c", "s"), Bindings: bindings{agent.ScenarioIssueAnalysis: "hammurapi-analysis"},
		CallerMCPURL: mcpHTTP.URL}}
	out, err := r.Once(context.Background(), agent.ScenarioIssueAnalysis, mcp.Grant{Mode: mcp.ModeDiscovery, Subject: uuid.New()}, "sys", "analyse")
	if err != nil {
		t.Fatal(err)
	}
	if out.Text != "Done" || out.Usage.TokensIn != 10 || out.Model != "nabu:hammurapi-analysis" {
		t.Fatalf("outcome %+v", out)
	}
	if raw, ok := out.Last("discovery"); !ok || !strings.Contains(string(raw), `"v"`) {
		t.Fatalf("results %v", out.Results)
	}
	if !strings.HasPrefix(started.IdempotencyKey, "scenario:issue_analysis:") || !strings.Contains(started.Input, "analyse") {
		t.Fatalf("start %+v", started)
	}
}

// ERR classes of Nabu keep the behaviour of the workflows: a balance problem
// blocks at once, an overload is retried.
func TestOnceNabuErrors(t *testing.T) {
	for _, c := range []struct {
		class     string
		permanent bool
	}{{"insufficient_balance", true}, {"rate_limit", false}} {
		evs := "id: 1\nevent: error\ndata: {\"errorClass\":\"" + c.class + "\",\"message\":\"m\"}\n\n" +
			"id: 2\nevent: completed\ndata: {\"status\":\"failed\",\"errorClass\":\"" + c.class + "\"}\n\n"
		fake := fakeRuns(t, evs, func(nabu.RunInput) {})
		r := &Runner{MCP: mcp.NewServer(), Nabu: &NabuRuns{Client: nabu.New(fake.URL, "c", "s"),
			Bindings: bindings{agent.ScenarioIssueAnalysis: "hammurapi-analysis"}}}
		_, err := r.Once(context.Background(), agent.ScenarioIssueAnalysis, mcp.Grant{Mode: mcp.ModeDiscovery}, "", "p")
		var le *LLMError
		if !errors.As(err, &le) || string(le.Class) != c.class || isPermanent(err) != c.permanent {
			t.Fatalf("%s: %v", c.class, err)
		}
	}
}

// NA: without Nabu and the operator the scenario is blocked with agent_disabled.
func TestOnceDisabled(t *testing.T) {
	domain.SetAgentDisabled(true)
	defer domain.SetAgentDisabled(false)
	_, err := (&Runner{MCP: mcp.NewServer()}).Once(context.Background(), agent.ScenarioIssueAnalysis, mcp.Grant{}, "", "")
	e, ok := apperr.As(err)
	if !isPermanent(err) || !ok || e.Code != "agent_disabled" {
		t.Fatalf("%v", err)
	}
}

func isPermanent(err error) bool {
	var pe *workflows.PermanentError
	return errors.As(err, &pe)
}

// A refused event stream (a rejected events token) is an error, not an empty
// success; the run is cancelled in Nabu.
func TestOnceNabuEventsRefused(t *testing.T) {
	cancelled := make(chan struct{}, 1)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_, _ = io.WriteString(w, `{"access_token":"t","expires_in":3600}`)
		case "/client/v1/agents/hammurapi-analysis/runs":
			_ = json.NewEncoder(w).Encode(map[string]string{"runId": "r1", "eventsUrl": srv.URL + "/events", "eventsToken": "bad"})
		case "/events":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_token","message":"bad events token"}}`)
		case "/client/v1/runs/r1/cancel":
			cancelled <- struct{}{}
		}
	}))
	defer srv.Close()
	r := &Runner{MCP: mcp.NewServer(), Nabu: &NabuRuns{Client: nabu.New(srv.URL, "c", "s"),
		Bindings: bindings{agent.ScenarioIssueAnalysis: "hammurapi-analysis"}}}
	_, err := r.Once(context.Background(), agent.ScenarioIssueAnalysis, mcp.Grant{Mode: mcp.ModeDiscovery}, "", "p")
	if err == nil || !isPermanent(err) || !strings.Contains(err.Error(), "invalid_token") {
		t.Fatalf("err %v", err)
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("the run was not cancelled")
	}
}
