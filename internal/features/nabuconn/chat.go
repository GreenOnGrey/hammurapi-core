package nabuconn

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
)

// EventPrefix prefixes the events of Nabu in /api/v1/events; chat.* stays with
// the built-in chat of PLT.HMR-0004 while it lives.
const EventPrefix = "nabu."

// Chat is the window to the personal agent of the user in Nabu (R5, R6,
// tech §3.2): requests go to /client/v1/me/* on behalf of the session user.
type Chat struct {
	Client *nabu.Client
	Hub    *events.Hub
	// Email returns the email of a user ("" — none: the chat is unavailable).
	Email func(ctx context.Context, userID uuid.UUID) string

	mu      sync.Mutex
	bridges map[uuid.UUID]*bridge
}

type bridge struct {
	refs   int
	cancel context.CancelFunc
}

func (c *Chat) email(r *http.Request) (string, error) {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return "", err
	}
	e := c.Email(r.Context(), p.UserID)
	if e == "" {
		return "", apperr.Conflict("email_required", "the user has no email: the agent of Nabu is reached by email")
	}
	return e, nil
}

// forward relays the request to Nabu and its answer back as is: errors of
// Nabu keep their codes; Nabu not answering is nabu_unavailable.
func (c *Chat) forward(w http.ResponseWriter, r *http.Request, path string) error {
	email, err := c.email(r)
	if err != nil {
		return err
	}
	if q := r.URL.RawQuery; q != "" {
		path += "?" + q
	}
	var body io.Reader
	if r.Body != nil && r.Method != http.MethodGet {
		body = http.MaxBytesReader(w, r.Body, 64<<20)
	}
	resp, err := c.Client.Request(r.Context(), r.Method, path, email, body, r.Header.Get("Content-Type"))
	if err != nil {
		return MapErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 && resp.StatusCode != http.StatusServiceUnavailable {
		return ErrUnavailable()
	}
	for _, h := range []string{"Content-Type", "Content-Disposition", "Content-Length", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
	return nil
}

// Routes mounts the chat of Nabu under /api/v1 (tech §3.2).
func (c *Chat) Routes(r chi.Router) {
	to := func(build func(r *http.Request) string) http.HandlerFunc {
		return httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			return c.forward(w, r, build(r))
		})
	}
	fixed := func(p string) func(*http.Request) string { return func(*http.Request) string { return p } }
	conv := func(suffix string) func(*http.Request) string {
		return func(r *http.Request) string {
			return "/client/v1/me/conversations/" + chi.URLParam(r, "id") + suffix
		}
	}
	r.Get("/agent", to(fixed("/client/v1/me/agent")))
	r.Patch("/agent", to(fixed("/client/v1/me/agent")))
	r.Get("/chat/conversations", to(fixed("/client/v1/me/conversations")))
	r.Post("/chat/conversations", to(fixed("/client/v1/me/conversations")))
	r.Patch("/chat/conversations/{id}", to(conv("")))
	r.Get("/chat/conversations/{id}/messages", to(conv("/messages")))
	r.Post("/chat/conversations/{id}/messages", to(conv("/messages")))
	r.Post("/chat/messages/{id}/retry", to(func(r *http.Request) string {
		return "/client/v1/me/messages/" + chi.URLParam(r, "id") + "/retry"
	}))
	r.Post("/chat/attachments", to(fixed("/client/v1/me/attachments")))
	r.Get("/chat/attachments/{id}", to(func(r *http.Request) string {
		return "/client/v1/me/attachments/" + chi.URLParam(r, "id")
	}))
}

// Events wraps the SSE of Hammurapi: while the user has an open tab on this
// pod, one stream of /client/v1/me/events is relayed into it as nabu.* events.
func (c *Chat) Events(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := httpx.PrincipalFrom(r.Context()); p != nil {
			if email := c.Email(r.Context(), p.UserID); email != "" {
				release := c.acquire(p.UserID, email)
				defer release()
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (c *Chat) acquire(uid uuid.UUID, email string) func() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bridges == nil {
		c.bridges = map[uuid.UUID]*bridge{}
	}
	b := c.bridges[uid]
	if b == nil {
		ctx, cancel := context.WithCancel(context.Background())
		b = &bridge{cancel: cancel}
		c.bridges[uid] = b
		go c.relay(ctx, uid, email)
	}
	b.refs++
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		b.refs--
		if b.refs == 0 {
			b.cancel()
			delete(c.bridges, uid)
		}
	}
}

// relay reads the user stream of Nabu until ctx ends, reconnecting with a
// growing delay (up to 30 s).
func (c *Chat) relay(ctx context.Context, uid uuid.UUID, email string) {
	delay := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.relayOnce(ctx, uid, email)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			delay = time.Second
		}
		slog.DebugContext(ctx, "nabu events stream ended", "err", err)
		c.Hub.Deliver(events.Event{Type: EventPrefix + "disconnected", UserID: &uid, Data: map[string]any{}})
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 30*time.Second)
	}
}

func (c *Chat) relayOnce(ctx context.Context, uid uuid.UUID, email string) error {
	req, err := c.Client.Request(ctx, http.MethodGet, "/client/v1/me/events", email, nil, "")
	if err != nil {
		return err
	}
	defer req.Body.Close()
	if req.StatusCode != http.StatusOK {
		return nabu.Decode(req, nil)
	}
	c.Hub.Deliver(events.Event{Type: EventPrefix + "connected", UserID: &uid, Data: map[string]any{}})
	sc := bufio.NewScanner(req.Body)
	sc.Buffer(nil, 8<<20)
	var typ string
	var data json.RawMessage
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			typ = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = json.RawMessage(strings.TrimSpace(line[5:]))
		case line == "":
			if typ != "" && json.Valid(data) {
				c.Hub.Deliver(events.Event{Type: EventPrefix + typ, UserID: &uid, Data: data})
			}
			typ, data = "", nil
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return io.EOF
}
