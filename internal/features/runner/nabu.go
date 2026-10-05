package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/relay"
)

// runAgentNabu performs the task with the service agent of Nabu
// (FTR.HMR.CMN-0006 tech §3.4–3.5): api starts the run, the runner connects
// its workspace server to the relay of Nabu (no inbound port) and reads the
// events of the run with the events token.
func runAgentNabu(ctx context.Context, c *client, cfg Config, d *Description, root string) (string, agent.Usage, error) {
	var usage agent.Usage
	prompt := Prompt(d)
	var run AgentRun
	if err := c.call(ctx, http.MethodPost, "/internal/v1/tasks/"+cfg.TaskID+"/agent-run", map[string]any{"input": prompt}, &run); err != nil {
		return "", usage, fmt.Errorf("agent run: %w", err)
	}
	tok := make([]byte, 24)
	_, _ = rand.Read(tok)
	ws := &Workspace{Root: root, Token: hex.EncodeToString(tok), Env: CommandEnv(root)}

	rctx, rcancel := context.WithCancel(ctx)
	defer rcancel()
	relayErr := make(chan error, 1)
	connected := make(chan struct{})
	var once sync.Once
	rc := &relay.Client{URL: run.RelayURL, Token: run.WorkspaceToken, WorkspaceID: run.WorkspaceID, Kind: "external",
		Handler: ws.Handler(), HandlerToken: ws.Token, GiveUp: 60 * time.Second,
		OnConnected: func() { once.Do(func() { close(connected) }) }}
	go func() { relayErr <- rc.Run(rctx) }()
	select {
	case <-connected:
	case err := <-relayErr:
		return "", usage, fmt.Errorf("relay: %w", err)
	case <-time.After(60 * time.Second):
		return "", usage, errors.New("relay: could not connect within 60 seconds")
	case <-ctx.Done():
		return "", usage, ctx.Err()
	}

	var mu sync.Mutex
	var answer strings.Builder
	last := ""
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				mu.Lock()
				msg, out := last, usage.TokensOut
				mu.Unlock()
				if msg == "" {
					msg = "working"
				}
				_ = c.call(ctx, http.MethodPost, "/internal/v1/tasks/"+cfg.TaskID+"/progress", Progress{Message: msg, TokensOut: out}, nil)
			}
		}
	}()
	var failure struct {
		Class   agent.ErrorClass `json:"errorClass"`
		Message string           `json:"message"`
	}
	status := ""
	evCtx, evCancel := context.WithCancel(ctx)
	defer evCancel()
	go func() {
		// The run cannot finish without its workspace: a lost relay ends the wait.
		if err := <-relayErr; err != nil {
			slog.Error("relay channel lost", "err", err)
			evCancel()
		}
	}()
	err := nabu.ReadEvents(evCtx, run.EventsURL, run.EventsToken, func(e nabu.RunEvent) {
		mu.Lock()
		defer mu.Unlock()
		switch e.Type {
		case "text_delta":
			var x struct{ Delta string }
			_ = json.Unmarshal(e.Data, &x)
			answer.WriteString(x.Delta)
		case "tool_call":
			var x struct{ Name string }
			_ = json.Unmarshal(e.Data, &x)
			last = x.Name
		case "usage":
			var u agent.Usage
			if json.Unmarshal(e.Data, &u) == nil {
				usage.Add(u)
			}
		case "error":
			_ = json.Unmarshal(e.Data, &failure)
		case "completed":
			var x struct {
				Status, Summary, ErrorClass string
			}
			_ = json.Unmarshal(e.Data, &x)
			status = x.Status
			if failure.Message == "" {
				failure.Message = x.Summary
			}
			if failure.Class == "" {
				failure.Class = agent.ErrorClass(x.ErrorClass)
			}
		}
	})
	mu.Lock()
	defer mu.Unlock()
	out := answer.String()
	switch {
	case err != nil && ctx.Err() == nil && evCtx.Err() != nil:
		return out, usage, errors.New("the workspace lost its connection to Nabu")
	case err != nil:
		return out, usage, err
	case status != "succeeded":
		if failure.Class != "" {
			return out, usage, fmt.Errorf("%s: %s", failure.Class, failure.Message)
		}
		return out, usage, fmt.Errorf("the run of Nabu ended %s: %s", status, failure.Message)
	}
	return out, usage, nil
}
