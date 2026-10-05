// Package mcp is a minimal Model Context Protocol server (Streamable HTTP with
// JSON responses) through which the agent reaches Hammurapi tools. Every agent
// session gets its own bearer token; the token's Grant decides what the agent
// may do on behalf of the user.
package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
)

// ProtocolVersion is the MCP revision implemented.
const ProtocolVersion = "2025-06-18"

// Chat modes and agent contexts (FTR.HMR.CMN-0002 arch §6). The mode selects
// which tools are offered.
const (
	ModeGeneral   = "general"   // chat: questions across all specifications
	ModeSpec      = "spec"      // chat: working on an issue, feature or release
	ModeDiscovery = "discovery" // worker: Discovery of an issue
	ModeGenerate  = "generate"  // worker: generation of tech and qa
	ModeCheck     = "check"     // worker: code vs specification check
	ModeTask      = "task"      // runner: code generation task
	// ModeNabu: the personal agent of the user in Nabu calls Hammurapi on
	// behalf of the user (FTR.HMR.CMN-0006 R7).
	ModeNabu = "nabu"
)

// Grant is the permission scope of an MCP token.
type Grant struct {
	UserID uuid.UUID
	// Mode is a chat mode or an agent context.
	Mode string
	// ContextType is issue, feature or release; ContextKey its key.
	ContextType string
	ContextKey  string
	// Feature is the feature key when the context is a feature (or the feature of a task).
	Feature string
	// Area is the currently open area, if any.
	Area domain.Area
	// Expert: the user is an expert of the context's domain and may edit.
	Expert bool
	// Subject is the issue, feature, release or task id of worker and runner contexts.
	Subject uuid.UUID
	// Sink receives structured results of worker contexts (save_discovery,
	// submit_gate, report_discrepancy); nil in the chat.
	Sink func(kind string, payload json.RawMessage) error
}

// CanEditArea reports whether the grant allows edit_spec in area a: the chat
// in a feature context, an expert, and a gate the agent does not generate.
func (g Grant) CanEditArea(a domain.Area) bool {
	return g.Mode == ModeSpec && g.ContextType == "feature" && g.Feature != "" && g.Expert && !a.Generated()
}

// CanEditDiscovery reports whether the grant allows edit_discovery.
func (g Grant) CanEditDiscovery() bool {
	return g.Mode == ModeSpec && g.ContextType == "issue" && g.ContextKey != "" && g.Expert
}

// Tool is an MCP tool.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Modes lists the chat modes in which the tool is offered.
	Modes []string
	// ReadOnly marks a tool that changes nothing (annotation readOnlyHint).
	ReadOnly bool
	Handler  func(ctx context.Context, g Grant, args json.RawMessage) (string, error)
}

func (t Tool) availableIn(mode string) bool {
	for _, m := range t.Modes {
		if m == mode {
			return true
		}
	}
	return false
}

// ToolError is a tool failure reported to the agent as isError content.
type ToolError struct{ Msg string }

func (e *ToolError) Error() string { return e.Msg }

// Server holds tools and tokens.
type Server struct {
	mu     sync.RWMutex
	grants map[string]Grant
	tools  []Tool
	// Resolve looks up tokens not issued by this server (runner task tokens).
	Resolve func(ctx context.Context, token string) (Grant, bool)
}

// NewServer creates a server.
func NewServer() *Server { return &Server{grants: map[string]Grant{}} }

// Register adds tools.
func (s *Server) Register(tools ...Tool) { s.tools = append(s.tools, tools...) }

// Issue creates a token for a grant.
func (s *Server) Issue(g Grant) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.grants[tok] = g
	s.mu.Unlock()
	return tok
}

// Update replaces the grant of a token (mode or feature switch).
func (s *Server) Update(tok string, g Grant) {
	s.mu.Lock()
	if _, ok := s.grants[tok]; ok {
		s.grants[tok] = g
	}
	s.mu.Unlock()
}

// Revoke deletes a token.
func (s *Server) Revoke(tok string) {
	s.mu.Lock()
	delete(s.grants, tok)
	s.mu.Unlock()
}

func (s *Server) grant(tok string) (Grant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.grants[tok]
	return g, ok
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// ServeHTTP handles POST /mcp.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	g, ok := s.grant(tok)
	if !ok && s.Resolve != nil && tok != "" {
		g, ok = s.Resolve(r.Context(), tok)
	}
	if !ok {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	s.Serve(w, r, g)
}

// Serve handles one MCP request with a grant the caller has already
// authenticated (calls from Nabu, FTR.HMR.CMN-0006 tech §3.6).
func (s *Server) Serve(w http.ResponseWriter, r *http.Request, g Grant) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, nil, nil, &rpcErr{Code: -32700, Message: "parse error"})
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted) // notification
		return
	}
	res, rerr := s.dispatch(r.Context(), g, req)
	writeRPC(w, req.ID, res, rerr)
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, e *rpcErr) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		resp["error"] = e
	} else {
		resp["result"] = result
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) dispatch(ctx context.Context, g Grant, req request) (any, *rpcErr) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := p.ProtocolVersion
		if v == "" {
			v = ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": "hammurapi", "version": "1.0.0"},
			"instructions":    "Hammurapi specification tools. Use search_specs and read_spec to research; edit_spec to change a gate document (spec mode only). Deleting specifications is not possible through tools: the user deletes them in the Hammurapi UI.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		list := []map[string]any{}
		for _, t := range s.tools {
			if t.availableIn(g.Mode) {
				item := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
				if t.ReadOnly {
					item["annotations"] = map[string]any{"readOnlyHint": true}
				}
				list = append(list, item)
			}
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcErr{Code: -32602, Message: "invalid params"}
		}
		for _, t := range s.tools {
			if t.Name != p.Name {
				continue
			}
			if !t.availableIn(g.Mode) {
				return toolResult(fmt.Sprintf("Tool %s is not available in %s mode. Ask the user to switch the chat to specification mode.", t.Name, g.Mode), true), nil
			}
			out, err := t.Handler(ctx, g, p.Arguments)
			if err != nil {
				slog.InfoContext(ctx, "mcp tool refused", "tool", t.Name, "err", err)
				return toolResult(err.Error(), true), nil
			}
			return toolResult(out, false), nil
		}
		return nil, &rpcErr{Code: -32602, Message: "unknown tool " + p.Name}
	default:
		return nil, &rpcErr{Code: -32601, Message: "method not found"}
	}
}

func toolResult(text string, isErr bool) map[string]any {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "isError": isErr}
}
