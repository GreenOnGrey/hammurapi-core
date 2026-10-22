// Package workflows is the engine of long-running processes (FTR.HMR.CMN-0002 arch §5):
// state machines persisted in Postgres with a transactional outbox.
//
//   - A run (workflow_runs) belongs to a subject (issue, feature, task, release)
//     and has a state. Only one active run of a kind exists per subject.
//   - A Machine is a transition function: it receives the run and its new events
//     (or a timer tick) and returns the next state plus effects.
//   - The transition and the effects are written in one transaction; effects go
//     to the outbox with an idempotency key run:step:attempt:key.
//   - A dispatcher executes effects (provider calls, agent sessions, runner
//     tasks, deploy triggers) with exponential backoff. Effect results come
//     back to the run as events. After WORKFLOW_MAX_ATTEMPTS a failing effect
//     sends "effect_failed", and machines move to the blocked state, which
//     shows up in "In focus" for an expert.
//   - Runs are picked with FOR UPDATE SKIP LOCKED, so several workers never
//     process the same run concurrently.
package workflows

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Terminal states: runs in these states are never processed again.
var Terminal = map[string]bool{"done": true, "succeeded": true, "failed": true, "cancelled": true, "rolled_back": true}

const terminalSQL = `('done', 'succeeded', 'failed', 'cancelled', 'rolled_back')`

// StateBlocked is the shared "needs an expert" state.
const StateBlocked = "blocked"

// ErrActiveRun means the subject already has an active run of this kind.
var ErrActiveRun = errors.New("workflows: active run exists")

// Run is a workflow run.
type Run struct {
	ID        uuid.UUID
	Kind      string
	SubjectID uuid.UUID
	ParentID  *uuid.UUID
	State     string
	Step      string
	Context   map[string]any
	Attempt   int
	NextRunAt *time.Time
	LastError *string
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Event is a workflow event.
type Event struct {
	ID      uuid.UUID
	Type    string
	Payload json.RawMessage
}

// Decode unmarshals the payload.
func (e Event) Decode(v any) error {
	if len(e.Payload) == 0 {
		return nil
	}
	return json.Unmarshal(e.Payload, v)
}

// Effect is a side effect executed by the dispatcher after the transition commits.
type Effect struct {
	Type    string
	Payload any
	// Key distinguishes several effects of one step (e.g. per service).
	Key string
}

// Result is the outcome of a machine step.
type Result struct {
	State   string
	Step    string
	Context map[string]any // replaces the run context when non-nil
	// NextRunAt schedules a timer (timeouts, polling); nil means "wait for events".
	NextRunAt *time.Time
	Effects   []Effect
	// Error is shown to experts (blocked reason); cleared when empty.
	Error string
	// Notify is published to SSE after the transaction commits.
	Notify []events.Event
}

// Machine is a state machine of one kind.
type Machine interface {
	Kind() string
	// Step computes the transition. It runs inside the run's transaction and may
	// write domain rows through tx. It must be deterministic with respect to
	// the database state and events: effects do the external work.
	Step(ctx context.Context, tx pgx.Tx, run *Run, evs []Event, now time.Time) (Result, error)
}

// EffectHandler executes an effect. Returned events are delivered to the run.
type EffectHandler struct {
	Lease time.Duration // how long the effect may run before another dispatcher retries it
	Do    func(ctx context.Context, run RunRef, payload json.RawMessage) ([]NewEvent, error)
}

// RunRef identifies the run of an effect.
type RunRef struct {
	ID        uuid.UUID
	Kind      string
	SubjectID uuid.UUID
}

// NewEvent is an event to deliver.
type NewEvent struct {
	Type    string
	Payload any
}

// Engine runs machines and dispatches effects.
type Engine struct {
	pool        *pgxpool.Pool
	machines    map[string]Machine
	effects     map[string]EffectHandler
	events      events.Publisher
	maxAttempts int
	lease       time.Duration
	owner       string
	Now         func() time.Time
}

// Config configures the engine.
type Config struct {
	MaxAttempts int
	Lease       time.Duration
}

// New creates an engine.
func New(pool *pgxpool.Pool, ev events.Publisher, cfg Config) *Engine {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 8
	}
	if cfg.Lease <= 0 {
		cfg.Lease = 2 * time.Minute
	}
	host, _ := os.Hostname()
	return &Engine{pool: pool, machines: map[string]Machine{}, effects: map[string]EffectHandler{}, events: ev,
		maxAttempts: cfg.MaxAttempts, lease: cfg.Lease, owner: fmt.Sprintf("%s:%d", host, os.Getpid()), Now: time.Now}
}

// Register adds machines.
func (e *Engine) Register(ms ...Machine) {
	for _, m := range ms {
		e.machines[m.Kind()] = m
	}
}

// Handle registers an effect handler.
func (e *Engine) Handle(effect string, h EffectHandler) { e.effects[effect] = h }

// ─── Starting runs and sending events (usable from api and worker) ─────

// Start creates a run due immediately. It fails with ErrActiveRun if the subject
// already has an active run of this kind.
func Start(ctx context.Context, q postgres.Querier, kind string, subject uuid.UUID, parent *uuid.UUID, state string, context map[string]any) (uuid.UUID, error) {
	if context == nil {
		context = map[string]any{}
	}
	raw, _ := json.Marshal(context)
	var id uuid.UUID
	err := q.QueryRow(ctx, `INSERT INTO workflow_runs (kind, subject_id, parent_id, state, context, next_run_at)
		VALUES ($1,$2,$3,$4,$5, now()) RETURNING id`, kind, subject, parent, state, raw).Scan(&id)
	if postgres.IsUniqueViolation(err) {
		return uuid.Nil, ErrActiveRun
	}
	return id, err
}

// Send delivers an event to a run.
func Send(ctx context.Context, q postgres.Querier, runID uuid.UUID, typ string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if payload == nil {
		raw = []byte("{}")
	}
	_, err = q.Exec(ctx, `INSERT INTO workflow_events (run_id, type, payload) VALUES ($1,$2,$3)`, runID, typ, raw)
	return err
}

// SendToSubject delivers an event to the active run of a kind for a subject.
// It returns false when no active run exists.
func SendToSubject(ctx context.Context, q postgres.Querier, kind string, subject uuid.UUID, typ string, payload any) (bool, error) {
	id, err := ActiveRunID(ctx, q, kind, subject)
	if err != nil || id == uuid.Nil {
		return false, err
	}
	return true, Send(ctx, q, id, typ, payload)
}

// ActiveRunID returns the active run of a kind for a subject, or uuid.Nil.
func ActiveRunID(ctx context.Context, q postgres.Querier, kind string, subject uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := q.QueryRow(ctx, `SELECT id FROM workflow_runs WHERE kind = $1 AND subject_id = $2 AND state NOT IN `+terminalSQL, kind, subject).Scan(&id)
	if postgres.IsNoRows(err) {
		return uuid.Nil, nil
	}
	return id, err
}

// Load reads a run.
func Load(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Run, error) {
	return scanRun(q.QueryRow(ctx, `SELECT `+runCols+` FROM workflow_runs WHERE id = $1`, id))
}

// LatestRun returns the most recent run of a kind for a subject (active or not).
func LatestRun(ctx context.Context, q postgres.Querier, kind string, subject uuid.UUID) (*Run, error) {
	r, err := scanRun(q.QueryRow(ctx, `SELECT `+runCols+` FROM workflow_runs WHERE kind = $1 AND subject_id = $2
		ORDER BY created_at DESC LIMIT 1`, kind, subject))
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	return r, err
}

const runCols = `id, kind, subject_id, parent_id, state, COALESCE(step,''), context, attempt, next_run_at, last_error, version, created_at, updated_at`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	var raw []byte
	if err := row.Scan(&r.ID, &r.Kind, &r.SubjectID, &r.ParentID, &r.State, &r.Step, &raw, &r.Attempt, &r.NextRunAt,
		&r.LastError, &r.Version, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	r.Context = map[string]any{}
	_ = json.Unmarshal(raw, &r.Context)
	return &r, nil
}

// ─── Worker loop ─────────────────────────────────────────────────────

// Run processes due runs and dispatches effects until ctx is done.
func (e *Engine) Run(ctx context.Context) error {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	gauge := time.NewTicker(30 * time.Second)
	defer gauge.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-gauge.C:
			e.exportGauge(ctx)
		case <-t.C:
			for i := 0; i < 50; i++ { // drain bursts
				worked, err := e.ProcessOnce(ctx)
				if err != nil && ctx.Err() == nil {
					slog.ErrorContext(ctx, "workflow processing failed", "err", err)
				}
				sent, derr := e.DispatchOnce(ctx)
				if derr != nil && ctx.Err() == nil {
					slog.ErrorContext(ctx, "outbox dispatch failed", "err", derr)
				}
				if !worked && !sent {
					break
				}
			}
		}
	}
}

// ProcessOnce handles one due run. It reports whether a run was processed.
func (e *Engine) ProcessOnce(ctx context.Context) (bool, error) {
	var run *Run
	var evs []Event
	var res Result
	var stepErr error
	start := time.Now()
	err := postgres.InTx(ctx, e.pool, func(tx pgx.Tx) error {
		var err error
		run, err = scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM workflow_runs r
			WHERE state NOT IN `+terminalSQL+`
			  AND (next_run_at <= now() OR EXISTS (SELECT 1 FROM workflow_events ev WHERE ev.run_id = r.id AND ev.processed_at IS NULL))
			ORDER BY next_run_at NULLS LAST LIMIT 1 FOR UPDATE SKIP LOCKED`))
		if postgres.IsNoRows(err) {
			run = nil
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, type, payload FROM workflow_events WHERE run_id = $1 AND processed_at IS NULL ORDER BY created_at, id`, run.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ev Event
			if err := rows.Scan(&ev.ID, &ev.Type, &ev.Payload); err != nil {
				rows.Close()
				return err
			}
			evs = append(evs, ev)
		}
		rows.Close()
		m, ok := e.machines[run.Kind]
		if !ok {
			return fmt.Errorf("no machine for kind %s", run.Kind)
		}
		// The machine writes domain rows in a savepoint, so a failing step
		// rolls back its own writes but the retry bookkeeping still commits.
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		res, stepErr = m.Step(ctx, sp, run, evs, e.Now())
		if stepErr != nil {
			_ = sp.Rollback(ctx)
			return e.retry(ctx, tx, run, stepErr)
		}
		if err := sp.Commit(ctx); err != nil {
			return err
		}
		return e.apply(ctx, tx, run, evs, res)
	})
	if err != nil || run == nil {
		return run != nil, err
	}
	metrics.WorkflowTransitionDuration.WithLabelValues(run.Kind, run.State).Observe(time.Since(start).Seconds())
	if stepErr == nil {
		for _, n := range res.Notify {
			e.events.Publish(ctx, n)
		}
		if res.State == StateBlocked && run.State != StateBlocked {
			metrics.WorkflowBlocked.WithLabelValues(run.Kind, res.Error).Inc()
		}
	}
	return true, nil
}

// retry records a failed step: exponential backoff, blocked after the limit.
func (e *Engine) retry(ctx context.Context, tx pgx.Tx, run *Run, cause error) error {
	attempt := run.Attempt + 1
	msg := cause.Error()
	slog.WarnContext(ctx, "workflow step failed", "run", run.ID, "kind", run.Kind, "state", run.State, "attempt", attempt, "err", cause)
	if attempt >= e.maxAttempts {
		metrics.WorkflowBlocked.WithLabelValues(run.Kind, "step_failed").Inc()
		_, err := tx.Exec(ctx, `UPDATE workflow_runs SET state = $2, attempt = $3, last_error = $4, next_run_at = NULL,
			context = context || jsonb_build_object('blockedFrom', $5::text), version = version + 1, updated_at = now() WHERE id = $1`,
			run.ID, StateBlocked, attempt, msg, run.State)
		return err
	}
	next := e.Now().Add(Backoff(attempt))
	_, err := tx.Exec(ctx, `UPDATE workflow_runs SET attempt = $2, last_error = $3, next_run_at = $4, version = version + 1, updated_at = now() WHERE id = $1`,
		run.ID, attempt, msg, next)
	return err
}

// Backoff is the exponential retry delay: 2s, 4s, 8s … capped at 10 minutes.
func Backoff(attempt int) time.Duration {
	d := time.Duration(math.Pow(2, float64(attempt))) * time.Second
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d
}

func (e *Engine) apply(ctx context.Context, tx pgx.Tx, run *Run, evs []Event, res Result) error {
	state := res.State
	if state == "" {
		state = run.State
	}
	step := res.Step
	ctxJSON := []byte(nil)
	if res.Context != nil {
		ctxJSON, _ = json.Marshal(res.Context)
	}
	var lastErr *string
	if res.Error != "" {
		lastErr = &res.Error
	}
	attempt := run.Attempt
	if state != run.State || step != run.Step {
		attempt = 0 // a new step starts a new attempt sequence
	}
	tag, err := tx.Exec(ctx, `UPDATE workflow_runs SET state = $2, step = NULLIF($3,''), context = COALESCE($4::jsonb, context),
		next_run_at = $5, last_error = $6, attempt = $7, locked_by = $8, version = version + 1, updated_at = now()
		WHERE id = $1 AND version = $9`,
		run.ID, state, step, ctxJSON, res.NextRunAt, lastErr, attempt, e.owner, run.Version)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("workflows: concurrent update of run") // optimistic lock
	}
	if len(evs) > 0 {
		ids := make([]uuid.UUID, len(evs))
		for i, ev := range evs {
			ids[i] = ev.ID
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_events SET processed_at = now() WHERE id = ANY($1)`, ids); err != nil {
			return err
		}
	}
	stepKey := step
	if stepKey == "" {
		stepKey = state
	}
	for _, ef := range res.Effects {
		raw, err := json.Marshal(ef.Payload)
		if err != nil {
			return err
		}
		key := fmt.Sprintf("%s:%s:%d:%s", run.ID, stepKey, run.Version, ef.Key)
		if _, err := tx.Exec(ctx, `INSERT INTO outbox (run_id, effect, payload, idempotency_key) VALUES ($1,$2,$3,$4)
			ON CONFLICT (idempotency_key) DO NOTHING`, run.ID, ef.Type, raw, key); err != nil {
			return err
		}
	}
	return nil
}

// DispatchOnce executes one due outbox effect.
func (e *Engine) DispatchOnce(ctx context.Context) (bool, error) {
	var id, runID uuid.UUID
	var effect string
	var payload []byte
	var attempts int
	// Claim with a lease: the row is committed as "in progress until now()+lease".
	err := e.pool.QueryRow(ctx, `UPDATE outbox SET next_attempt_at = now() + interval '10 minutes', attempts = attempts + 1
		WHERE id = (SELECT id FROM outbox WHERE sent_at IS NULL AND failed_at IS NULL AND next_attempt_at <= now()
		            ORDER BY next_attempt_at LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING id, run_id, effect, payload, attempts`).Scan(&id, &runID, &effect, &payload, &attempts)
	if postgres.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	h, ok := e.effects[effect]
	if !ok {
		_, err := e.pool.Exec(ctx, `UPDATE outbox SET failed_at = now(), last_error = 'no handler' WHERE id = $1`, id)
		return true, err
	}
	lease := h.Lease
	if lease <= 0 {
		lease = e.lease
	}
	if _, err := e.pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() + $2::interval WHERE id = $1`, id, fmt.Sprintf("%d seconds", int(lease.Seconds()))); err != nil {
		return true, err
	}
	var ref RunRef
	if err := e.pool.QueryRow(ctx, `SELECT id, kind, subject_id FROM workflow_runs WHERE id = $1`, runID).Scan(&ref.ID, &ref.Kind, &ref.SubjectID); err != nil {
		return true, err
	}
	ectx, cancel := context.WithTimeout(ctx, lease)
	out, derr := h.Do(ectx, ref, payload)
	cancel()
	return true, postgres.InTx(ctx, e.pool, func(tx pgx.Tx) error {
		if derr != nil {
			slog.WarnContext(ctx, "effect failed", "effect", effect, "run", runID, "attempt", attempts, "err", derr)
			var perm *PermanentError
			if attempts >= e.maxAttempts || errors.As(derr, &perm) {
				if _, err := tx.Exec(ctx, `UPDATE outbox SET failed_at = now(), last_error = $2 WHERE id = $1`, id, derr.Error()); err != nil {
					return err
				}
				metrics.EffectsFailed.WithLabelValues(effect).Inc()
				return Send(ctx, tx, runID, "effect_failed", map[string]string{"effect": effect, "error": derr.Error()})
			}
			_, err := tx.Exec(ctx, `UPDATE outbox SET last_error = $2, next_attempt_at = now() + $3::interval WHERE id = $1`,
				id, derr.Error(), fmt.Sprintf("%d seconds", int(Backoff(attempts).Seconds())))
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE outbox SET sent_at = now(), last_error = NULL WHERE id = $1`, id); err != nil {
			return err
		}
		for _, ev := range out {
			if err := Send(ctx, tx, runID, ev.Type, ev.Payload); err != nil {
				return err
			}
		}
		return nil
	})
}

func (e *Engine) exportGauge(ctx context.Context) {
	rows, err := e.pool.Query(ctx, `SELECT kind::text, state, count(*) FROM workflow_runs WHERE state NOT IN `+terminalSQL+` GROUP BY 1, 2`)
	if err != nil {
		return
	}
	defer rows.Close()
	metrics.WorkflowRuns.Reset()
	for rows.Next() {
		var kind, state string
		var n float64
		if rows.Scan(&kind, &state, &n) == nil {
			metrics.WorkflowRuns.WithLabelValues(kind, state).Set(n)
		}
	}
}

// ─── Helpers for machines ────────────────────────────────────────────

// Has reports whether an event of a type is present.
func Has(evs []Event, typ string) bool {
	for _, ev := range evs {
		if ev.Type == typ {
			return true
		}
	}
	return false
}

// Find returns the last event of a type.
func Find(evs []Event, typ string) (Event, bool) {
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == typ {
			return evs[i], true
		}
	}
	return Event{}, false
}

// At returns a pointer to t.
func At(t time.Time) *time.Time { return &t }

// Keep continues the current state without scheduling.
func Keep(run *Run) Result {
	return Result{State: run.State, Step: run.Step, Context: run.Context, NextRunAt: nil}
}

// Str reads a string from a run context.
func Str(ctx map[string]any, k string) string {
	s, _ := ctx[k].(string)
	return s
}

// Int reads an int from a run context (JSON numbers are float64).
func Int(ctx map[string]any, k string) int {
	switch v := ctx[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// Strs reads a []string from a run context.
func Strs(ctx map[string]any, k string) []string {
	switch v := ctx[k].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Unblock handles the shared blocked state: a "retry" event returns the run to
// the state it was blocked in. ok is false when the run is not blocked.
func Unblock(run *Run, evs []Event, now time.Time) (Result, bool) {
	if run.State != StateBlocked {
		return Result{}, false
	}
	if !Has(evs, "retry") {
		return Result{State: StateBlocked, Step: run.Step, Context: run.Context, Error: derefErr(run.LastError)}, true
	}
	from := Str(run.Context, "blockedFrom")
	if from == "" {
		from = "queued"
	}
	ctx := run.Context
	delete(ctx, "blockedFrom")
	return Result{State: from, Step: run.Step, Context: ctx, NextRunAt: At(now)}, true
}

// Block moves the run to blocked, remembering where to resume.
func Block(run *Run, reason string) Result {
	ctx := run.Context
	if ctx == nil {
		ctx = map[string]any{}
	}
	if run.State != StateBlocked {
		ctx["blockedFrom"] = run.State
	}
	return Result{State: StateBlocked, Step: run.Step, Context: ctx, Error: reason}
}

func derefErr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// PermanentError is an effect failure that retrying cannot fix (for example
// an LLM connection without balance, FTR.HMR.CMN-0004 R20): the run gets
// effect_failed at once instead of after WORKFLOW_MAX_ATTEMPTS.
type PermanentError struct{ Err error }

func (e *PermanentError) Error() string { return e.Err.Error() }
func (e *PermanentError) Unwrap() error { return e.Err }

// Permanent marks err as permanent.
func Permanent(err error) error { return &PermanentError{Err: err} }
