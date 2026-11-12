// Package events delivers server-sent events to browser clients.
//
// Every api and worker instance publishes events through Postgres NOTIFY; every
// api pod LISTENs and fans them out to its locally connected SSE clients. This
// way an event produced by the worker (gate.updated) or by another api pod
// (approvals.changed) reaches all users regardless of which pod they are on.
// Agent token streams are delivered locally: the pod that runs the chat request
// reads the operator's stream and the user's SSE stream lives there too
// (sticky sessions).
package events

//go:generate go tool mockgen -destination=mocks/publisher.go -package=mocks . Publisher

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event types.
const (
	AgentToken       = "agent.token"
	AgentToolCall    = "agent.tool_call"
	AgentDone        = "agent.done"
	AgentError       = "agent.error"
	GateUpdated      = "gate.updated"
	FeatureDeleted   = "feature.deleted"
	ApprovalsChanged = "approvals.changed"
	ImportProgress   = "import.progress"

	// FTR.HMR.CMN-0002
	IssueUpdated      = "issue.updated"
	DiscoveryProgress = "discovery.progress"
	FeatureUpdated    = "feature.updated"
	TaskProgress      = "task.progress"
	TaskStep          = "task.step" // FTR.HMR.DEV-0004 R8: the steps of Hammurapi after the agent are written
	ValidationUpdated = "validation.updated"
	ReleaseUpdated    = "release.updated"
	ReleaseBlocked    = "release.blocked"
	FocusChanged      = "focus.changed"
)

const channel = "hammurapi_events"

// Event is one SSE message. UserID limits delivery to one user; nil broadcasts.
type Event struct {
	Type   string     `json:"type"`
	UserID *uuid.UUID `json:"userId,omitempty"`
	Data   any        `json:"data"`
}

// Publisher publishes events to all pods.
type Publisher interface {
	Publish(ctx context.Context, e Event)
}

// PGPublisher publishes via pg_notify.
type PGPublisher struct{ pool *pgxpool.Pool }

// NewPGPublisher creates a publisher.
func NewPGPublisher(pool *pgxpool.Pool) *PGPublisher { return &PGPublisher{pool: pool} }

// Publish sends the event; failures are logged, never returned: events are
// a UI refresh signal, the source of truth is the database.
func (p *PGPublisher) Publish(ctx context.Context, e Event) {
	b, err := json.Marshal(e)
	if err != nil {
		slog.ErrorContext(ctx, "encode event", "err", err)
		return
	}
	if len(b) > 7900 {
		slog.WarnContext(ctx, "event too large for NOTIFY, dropping data", "type", e.Type)
		b, _ = json.Marshal(Event{Type: e.Type, UserID: e.UserID})
	}
	if _, err := p.pool.Exec(context.WithoutCancel(ctx), "SELECT pg_notify($1, $2)", channel, string(b)); err != nil {
		slog.ErrorContext(ctx, "publish event", "err", err, "type", e.Type)
	}
}

// Hub fans events out to local SSE subscribers.
type Hub struct {
	mu   sync.RWMutex
	subs map[*Subscriber]struct{}
}

// Subscriber is one SSE connection.
type Subscriber struct {
	UserID uuid.UUID
	C      chan Event
}

// NewHub creates a hub.
func NewHub() *Hub { return &Hub{subs: map[*Subscriber]struct{}{}} }

// Subscribe registers a connection.
func (h *Hub) Subscribe(userID uuid.UUID) *Subscriber {
	s := &Subscriber{UserID: userID, C: make(chan Event, 256)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Unsubscribe removes a connection.
func (h *Hub) Unsubscribe(s *Subscriber) {
	h.mu.Lock()
	delete(h.subs, s)
	h.mu.Unlock()
}

// Deliver sends to matching local subscribers; slow clients drop events.
func (h *Hub) Deliver(e Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs {
		if e.UserID != nil && *e.UserID != s.UserID {
			continue
		}
		select {
		case s.C <- e:
		default:
		}
	}
}

// Publish implements Publisher for local-only delivery (agent streams).
func (h *Hub) Publish(_ context.Context, e Event) { h.Deliver(e) }

// Listen consumes NOTIFY messages into the hub until ctx is done, reconnecting on failure.
func (h *Hub) Listen(ctx context.Context, pool *pgxpool.Pool) {
	for ctx.Err() == nil {
		if err := h.listenOnce(ctx, pool); err != nil && ctx.Err() == nil {
			slog.Error("event listener failed, reconnecting", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (h *Hub) listenOnce(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var e Event
		if err := json.Unmarshal([]byte(n.Payload), &e); err != nil {
			continue
		}
		h.Deliver(e)
	}
}
