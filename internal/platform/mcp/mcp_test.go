package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
)

func call(t *testing.T, s *Server, token, method string, params any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func server() (*Server, *int) {
	s := NewServer()
	calls := 0
	s.Register(
		Tool{Name: "read_spec", Modes: []string{"general", "spec"}, InputSchema: map[string]any{"type": "object"},
			Handler: func(context.Context, Grant, json.RawMessage) (string, error) { return "doc", nil }},
		Tool{Name: "edit_spec", Modes: []string{"spec"}, InputSchema: map[string]any{"type": "object"},
			Handler: func(context.Context, Grant, json.RawMessage) (string, error) { calls++; return "ok", nil }},
	)
	return s, &calls
}

func TestInvalidToken(t *testing.T) {
	s, _ := server()
	code, _ := call(t, s, "nope", "tools/list", nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("code %d", code)
	}
}

// ACP-04 / CHAT-02: edit_spec is neither listed nor callable in general mode.
func TestEditUnavailableInGeneralMode(t *testing.T) {
	s, calls := server()
	tok := s.Issue(Grant{UserID: uuid.New(), Mode: "general"})
	_, out := call(t, s, tok, "tools/list", map[string]any{})
	if strings.Contains(mustJSON(out), "edit_spec") {
		t.Fatal("edit_spec listed in general mode")
	}
	_, out = call(t, s, tok, "tools/call", map[string]any{"name": "edit_spec", "arguments": map[string]any{}})
	res := out["result"].(map[string]any)
	if res["isError"] != true || *calls != 0 {
		t.Fatalf("edit_spec executed in general mode: %v", out)
	}
	// Switching the session to spec mode updates the token's rights.
	s.Update(tok, Grant{Mode: "spec", ContextType: "feature", Feature: "FTR.FMS.CAR-0005", Expert: true})
	_, out = call(t, s, tok, "tools/call", map[string]any{"name": "edit_spec", "arguments": map[string]any{}})
	if out["result"].(map[string]any)["isError"] != false || *calls != 1 {
		t.Fatalf("got %v", out)
	}
	s.Revoke(tok)
	if code, _ := call(t, s, tok, "tools/list", nil); code != http.StatusUnauthorized {
		t.Fatal("revoked token accepted")
	}
}

// FTR.HMR.DEV-0001 RT-01: a run of the writer submits only its areas.
func TestWriterScope(t *testing.T) {
	w := &WriterScope{Mode: "write", Areas: []domain.Area{domain.AreaArch}}
	if !w.Allows(domain.AreaArch) || w.Allows(domain.AreaProduct) {
		t.Fatal("scope")
	}
	var none *WriterScope
	if none.Allows(domain.AreaArch) {
		t.Fatal("no scope allows nothing")
	}
}

func TestResolveTaskToken(t *testing.T) {
	s, _ := server()
	s.Resolve = func(_ context.Context, tok string) (Grant, bool) { return Grant{Mode: "general"}, tok == "task-token" }
	if code, _ := call(t, s, "task-token", "tools/list", nil); code != http.StatusOK {
		t.Fatalf("resolved token rejected: %d", code)
	}
	if code, _ := call(t, s, "other", "tools/list", nil); code != http.StatusUnauthorized {
		t.Fatal("unknown token accepted")
	}
}

func TestNotification(t *testing.T) {
	s, _ := server()
	tok := s.Issue(Grant{Mode: "general"})
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code %d", rec.Code)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
