package nabuconn

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
)

func sign(priv ed25519.PrivateKey, c map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": "k1"})
	b, _ := json.Marshal(c)
	u := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(b)
	return u + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, []byte(u)))
}

// NB-05…07: calls of the personal agent run with the rights of the delegated
// user; without the delegation header — 403; without Nabu — 503 agent_disabled.
func TestMCPHandler(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "OKP", "crv": "Ed25519", "kid": "k1",
			"x": base64.RawURLEncoding.EncodeToString(pub)}}})
	}))
	defer jwks.Close()
	srv := mcp.NewServer()
	var gotUser uuid.UUID
	var gotMode string
	srv.Register(mcp.Tool{Name: "whoami", Modes: []string{mcp.ModeNabu}, InputSchema: map[string]any{"type": "object"},
		Handler: func(_ context.Context, g mcp.Grant, _ json.RawMessage) (string, error) {
			gotUser, gotMode = g.UserID, g.Mode
			return "ok", nil
		}})
	ann := uuid.New()
	created := map[string]bool{}
	h := &MCPHandler{Server: srv, Verifier: &nabu.Verifier{Issuer: jwks.URL, Audience: "hammurapi", TTL: time.Minute},
		EnsureUser: func(_ context.Context, email string) (uuid.UUID, error) {
			created[email] = true
			return ann, nil
		}}
	tok := sign(priv, map[string]any{"iss": jwks.URL, "aud": "hammurapi", "exp": time.Now().Unix() + 60, "email": "ann@x.org"})
	call := func(h http.Handler, tok, onBehalf string) *httptest.ResponseRecorder {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`
		r := httptest.NewRequest(http.MethodPost, "/mcp/nabu", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		if onBehalf != "" {
			r.Header.Set(DelegationHeader, onBehalf)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call(h, tok, "Ann@X.org"); w.Code != 200 || !strings.Contains(w.Body.String(), `"ok"`) {
		t.Fatalf("delegated call: %d %s", w.Code, w.Body)
	}
	if gotUser != ann || gotMode != mcp.ModeNabu || !created["ann@x.org"] {
		t.Fatalf("grant %v %s %v", gotUser, gotMode, created)
	}
	if w := call(h, tok, ""); w.Code != http.StatusForbidden {
		t.Fatalf("no delegation: %d", w.Code)
	}
	if w := call(h, tok, "bob@x.org"); w.Code != http.StatusForbidden {
		t.Fatalf("delegation for another user: %d", w.Code)
	}
	if w := call(h, "garbage", "ann@x.org"); w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", w.Code)
	}
	off := &MCPHandler{Server: srv, EnsureUser: h.EnsureUser}
	if w := call(off, tok, "ann@x.org"); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "agent_disabled") {
		t.Fatalf("without Nabu: %d %s", w.Code, w.Body)
	}
}

// fakeNabu serves the token endpoint and /client/v1/me/* and records requests.
type fakeNabu struct {
	*httptest.Server
	seen   chan *http.Request
	bodies chan string
	events chan string
}

func newFakeNabu(t *testing.T) *fakeNabu {
	f := &fakeNabu{seen: make(chan *http.Request, 16), bodies: make(chan string, 16), events: make(chan string, 16)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			_, _ = io.WriteString(w, `{"access_token":"client-token","expires_in":3600}`)
			return
		case "/client/v1/me/events":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case e := <-f.events:
					_, _ = io.WriteString(w, e)
					w.(http.Flusher).Flush()
				}
			}
		}
		b, _ := io.ReadAll(r.Body)
		f.seen <- r.Clone(context.Background())
		f.bodies <- string(b)
		switch {
		case r.URL.Path == "/client/v1/me/conversations/c1/messages" && r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"messageId":"m1"}`)
		case r.URL.Path == "/client/v1/me/agent":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_tone","message":"bad tone"}}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func withUser(uid uuid.UUID, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(httpx.WithPrincipal(r.Context(), domain.NewPrincipal(uid, "ann", "Ann", false))))
	})
}

// NB-02…04: the chat goes to /client/v1/me/* on behalf of the session user;
// errors of Nabu keep their codes; a failure of Nabu is nabu_unavailable.
func TestChatProxy(t *testing.T) {
	f := newFakeNabu(t)
	uid := uuid.New()
	c := &Chat{Client: nabu.New(f.URL, "hammurapi", "secret"), Hub: events.NewHub(),
		Email: func(context.Context, uuid.UUID) string { return "ann@x.org" }}
	r := chi.NewRouter()
	c.Routes(r)
	h := withUser(uid, r)

	body := `{"text":"hi","context":{"type":"feature","key":"FTR.FMS.CAR-0007","area":"tech"}}`
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/chat/conversations/c1/messages", strings.NewReader(body)))
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), "m1") {
		t.Fatalf("message: %d %s", w.Code, w.Body)
	}
	req := <-f.seen
	if req.Header.Get(DelegationHeader) != "ann@x.org" || req.Header.Get("Authorization") != "Bearer client-token" {
		t.Fatalf("headers %v", req.Header)
	}
	if got := <-f.bodies; got != body {
		t.Fatalf("body %s", got)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/agent", strings.NewReader(`{"tone":"x"}`)))
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "invalid_tone") {
		t.Fatalf("error code: %d %s", w.Code, w.Body)
	}
	<-f.seen
	<-f.bodies

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/chat/conversations", nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "nabu_unavailable") {
		t.Fatalf("unavailable: %d %s", w.Code, w.Body)
	}
}

// The events of Nabu reach the tab of the user as nabu.* while it is open.
func TestChatEventsBridge(t *testing.T) {
	f := newFakeNabu(t)
	uid := uuid.New()
	hub := events.NewHub()
	c := &Chat{Client: nabu.New(f.URL, "hammurapi", "secret"), Hub: hub,
		Email: func(context.Context, uuid.UUID) string { return "ann@x.org" }}
	sub := hub.Subscribe(uid)
	defer hub.Unsubscribe(sub)
	release := c.acquire(uid, "ann@x.org")
	wait := func(typ string) events.Event {
		t.Helper()
		for {
			select {
			case e := <-sub.C:
				if e.Type == typ {
					return e
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no %s", typ)
			}
		}
	}
	wait("nabu.connected")
	f.events <- fmt.Sprintf("event: message.delta\ndata: %s\n\n", `{"conversationId":"c1","delta":"Hel"}`)
	e := wait("nabu.message.delta")
	if !strings.Contains(string(e.Data.(json.RawMessage)), "Hel") || e.UserID == nil || *e.UserID != uid {
		t.Fatalf("event %+v", e)
	}
	release()
	c.mu.Lock()
	n := len(c.bridges)
	c.mu.Unlock()
	if n != 0 {
		t.Fatal("the bridge stays after the last tab closed")
	}
}

// The transfer passes the git source of skills only when /agent/skills has skills.
func TestSkillsSource(t *testing.T) {
	for _, snap := range []string{"", "null", "[]"} {
		if sk := skillsSource(json.RawMessage(snap), "org/specs", "main"); sk != nil {
			t.Fatalf("snapshot %q: %v", snap, sk)
		}
	}
	snap := json.RawMessage(`[{"name":"review","scenarios":["codegen"]}]`)
	if sk := skillsSource(snap, "", "main"); sk != nil {
		t.Fatalf("without the repository: %v", sk)
	}
	sk := skillsSource(snap, "org/specs", "main")
	as, _ := sk["assignments"].(map[string][]string)
	if sk["repo"] != "org/specs" || sk["path"] != "agent/skills" || sk["ref"] != "main" || len(as["review"]) != 1 || as["review"][0] != "codegen" {
		t.Fatalf("%v", sk)
	}
}
