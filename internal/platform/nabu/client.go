// Package nabu is the client of Nabu, the corporate agent platform
// (FTR.HMR.CMN-0006 arch §4, FTR.NAB.CMN-0001 tech §5): Hammurapi is a service
// client of Nabu. It exchanges its credentials for a short client token,
// acts on behalf of users by delegation (Nabu-On-Behalf-Of), starts runs of
// service agents and reads their events, and verifies the JWTs Nabu signs when
// its personal agents call Hammurapi's MCP.
package nabu

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DelegationHeader carries the email of the user Hammurapi acts for.
const DelegationHeader = "Nabu-On-Behalf-Of"

// Client calls Nabu.
type Client struct {
	URL          string
	ClientID     string
	ClientSecret string
	HTTP         *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New creates a client; it is nil when Nabu is not configured.
func New(baseURL, clientID, secret string) *Client {
	if baseURL == "" {
		return nil
	}
	return &Client{URL: strings.TrimRight(baseURL, "/"), ClientID: clientID, ClientSecret: secret,
		HTTP: &http.Client{Timeout: 0}}
}

// APIError is an error answer of Nabu with its stable code.
type APIError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *APIError) Error() string { return fmt.Sprintf("nabu %d %s: %s", e.Status, e.Code, e.Message) }

// ErrUnavailable means Nabu did not answer (nabu_unavailable).
var ErrUnavailable = errors.New("nabu is unavailable")

// Token returns a valid client token (cached until shortly before expiry).
func (c *Client) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expires) > time.Minute {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.ClientID, c.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	var t struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t)
	if resp.StatusCode/100 != 2 || t.AccessToken == "" {
		if resp.StatusCode >= 500 {
			return "", fmt.Errorf("%w: token %d", ErrUnavailable, resp.StatusCode)
		}
		return "", &APIError{Status: resp.StatusCode, Code: "nabu_auth_failed", Message: "Nabu refused the client credentials: " + t.Error}
	}
	c.token, c.expires = t.AccessToken, time.Now().Add(time.Duration(t.ExpiresIn)*time.Second)
	return c.token, nil
}

// Request performs a request; onBehalf is the email of the delegated user ("" — the client itself).
func (c *Client) Request(ctx context.Context, method, path, onBehalf string, body io.Reader, contentType string) (*http.Response, error) {
	tok, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if onBehalf != "" {
		req.Header.Set(DelegationHeader, onBehalf)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		c.mu.Lock()
		c.token = ""
		c.mu.Unlock()
	}
	return resp, nil
}

// Do sends JSON and decodes the JSON answer into out (nil — ignore).
func (c *Client) Do(ctx context.Context, method, path, onBehalf string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.Request(ctx, method, path, onBehalf, body, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return Decode(resp, out)
}

// Decode turns an answer into out or an *APIError; 5xx are ErrUnavailable.
func Decode(resp *http.Response, out any) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		var e struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		if resp.StatusCode >= 500 && e.Error.Code == "" {
			return fmt.Errorf("%w: %d", ErrUnavailable, resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message, Details: e.Error.Details}
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, out)
}

// ─── service agents (tech §3.3–3.4) ─────────────────────────────────

// Agent is a service agent allowed to the client.
type Agent struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Workspace   string `json:"workspace"`
}

// Agents lists the service agents allowed to Hammurapi.
func (c *Client) Agents(ctx context.Context) ([]Agent, error) {
	var out struct {
		Items []Agent `json:"items"`
	}
	err := c.Do(ctx, http.MethodGet, "/client/v1/agents", "", nil, &out)
	return out.Items, err
}

// CallerMCP is an MCP server passed to a run (Hammurapi's own MCP with the task token).
type CallerMCP struct {
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// RunInput is the body of a run.
type RunInput struct {
	Input          string          `json:"input"`
	Context        json.RawMessage `json:"context,omitempty"`
	CallerMCP      []CallerMCP     `json:"callerMcp,omitempty"`
	Initiator      string          `json:"initiator,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
}

// Started is the answer of a run start.
type Started struct {
	RunID          string `json:"runId"`
	WorkspaceToken string `json:"workspaceToken,omitempty"`
	WorkspaceID    string `json:"workspaceId,omitempty"`
	RelayURL       string `json:"relayUrl,omitempty"`
	EventsURL      string `json:"eventsUrl"`
	EventsToken    string `json:"eventsToken"`
}

// StartRun starts a run of a service agent.
func (c *Client) StartRun(ctx context.Context, agent string, in RunInput) (*Started, error) {
	var out Started
	err := c.Do(ctx, http.MethodPost, "/client/v1/agents/"+url.PathEscape(agent)+"/runs", "", in, &out)
	return &out, err
}

// CancelRun cancels a run.
func (c *Client) CancelRun(ctx context.Context, runID string) error {
	return c.Do(ctx, http.MethodPost, "/client/v1/runs/"+url.PathEscape(runID)+"/cancel", "", nil, nil)
}

// RunEvent is an event of a run (FTR.NAB.CMN-0001 tech §5).
type RunEvent struct {
	Seq  int
	Type string // started, text_delta, tool_call, tool_result, usage, error, completed, cancelled
	Data json.RawMessage
}

// ReadEvents reads the SSE of a run with the events token until completed;
// it resumes after a broken stream with Last-Event-ID. A refusal of the stream
// (a 4xx, e.g. a rejected events token) is returned as is.
func ReadEvents(ctx context.Context, eventsURL, token string, onEvent func(RunEvent)) error {
	last := 0
	for attempt := 0; ; attempt++ {
		done, err := readEventsOnce(ctx, eventsURL, token, &last, onEvent)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if done {
			return err // nil after "completed"; a refusal of Nabu (4xx) otherwise
		}
		if attempt >= 10 {
			return fmt.Errorf("%w: events: %v", ErrUnavailable, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
}

func readEventsOnce(ctx context.Context, eventsURL, token string, last *int, onEvent func(RunEvent)) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eventsURL, nil)
	if err != nil {
		return true, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	if *last > 0 {
		req.Header.Set("Last-Event-ID", strconv.Itoa(*last))
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode < 500 {
			return true, Decode(resp, nil)
		}
		return false, fmt.Errorf("events %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 8<<20)
	var ev RunEvent
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id:"):
			ev.Seq, _ = strconv.Atoi(strings.TrimSpace(line[3:]))
		case strings.HasPrefix(line, "event:"):
			ev.Type = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			ev.Data = json.RawMessage(strings.TrimSpace(line[5:]))
		case line == "":
			if ev.Type != "" {
				if ev.Seq > *last {
					*last = ev.Seq
				}
				onEvent(ev)
				if ev.Type == "completed" {
					return true, nil
				}
			}
			ev = RunEvent{}
		}
	}
	return false, sc.Err()
}

// Import sends the agent settings of Hammurapi to Nabu (R11, tech §4).
func (c *Client) Import(ctx context.Context, body any) (map[string]any, error) {
	var out map[string]any
	err := c.Do(ctx, http.MethodPost, "/client/v1/import/hammurapi-agent", "", body, &out)
	return out, err
}
