package codegen

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/executor"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Task workflow kind and effects.
const (
	TaskKind       = "codegen_task"
	EffectStart    = "runner.start"
	EffectStop     = "runner.stop"
	TaskImplement  = "implement"
	TaskReview     = "address_review"
	TaskUpdatePR   = "update_pr"
	TaskRevert     = "revert"
	TaskCISetup    = "ci_setup"
	startingWindow = 10 * time.Minute
)

// TaskInput is the input of a runner task (agent_tasks.input).
type TaskInput struct {
	Comment  string `json:"comment,omitempty"`
	PRNumber int    `json:"prNumber,omitempty"`
	// Revert: the merged PR to revert.
	RevertPRID   string `json:"revertPrId,omitempty"`
	MergeSHA     string `json:"mergeSha,omitempty"`
	Release      string `json:"release,omitempty"`
	Autonomy     string `json:"autonomy,omitempty"`
	Reason       string `json:"reason,omitempty"`
	ReviewAuthor string `json:"reviewAuthor,omitempty"`
}

// TaskSpec describes a task to create.
type TaskSpec struct {
	Type      string
	FeatureID *uuid.UUID
	ReleaseID *uuid.UUID
	ServiceID uuid.UUID
	Initiator *uuid.UUID
	Input     TaskInput
	Parent    *uuid.UUID // parent workflow run notified with task_done / task_failed
}

// StartTask creates an agent task and its codegen_task run.
func StartTask(ctx context.Context, q postgres.Querier, s TaskSpec) (uuid.UUID, error) {
	if domain.AgentDisabled() {
		return uuid.Nil, apperr.AgentDisabled()
	}
	id := uuid.New()
	runID, err := workflows.Start(ctx, q, TaskKind, id, s.Parent, "queued", map[string]any{"type": s.Type})
	if err != nil {
		return uuid.Nil, err
	}
	in, _ := json.Marshal(s.Input)
	t := &cycledata.Task{ID: id, Type: s.Type, FeatureID: s.FeatureID, ReleaseID: s.ReleaseID, ServiceID: s.ServiceID, RunID: runID,
		InitiatorID: s.Initiator, Input: in}
	if err := cycledata.New(q).InsertTask(ctx, t); err != nil {
		return uuid.Nil, err
	}
	metrics.RunnerTasks.WithLabelValues(s.Type, "queued").Inc()
	return id, nil
}

// Limits of runner tasks (RUNNER_* settings).
type Limits struct {
	MaxParallel int
	Timeout     time.Duration
}

// TaskMachine is the codegen_task workflow: queued → starting → running →
// done · failed · cancelled. One task per service repository runs at a time
// (unique index), and at most MaxParallel tasks overall.
type TaskMachine struct{ Limits Limits }

// Kind implements workflows.Machine.
func (TaskMachine) Kind() string { return TaskKind }

func taskNotify(t *cycledata.Task) []events.Event {
	data := map[string]any{"taskId": t.ID, "status": t.Status, "service": t.Service, "type": t.Type}
	return []events.Event{{Type: events.TaskProgress, Data: data}, {Type: events.FeatureUpdated, Data: data}}
}

func (m TaskMachine) finish(ctx context.Context, tx pgx.Tx, run *workflows.Run, t *cycledata.Task, state string, errText string) (workflows.Result, error) {
	cd := cycledata.New(tx)
	status := map[string]string{"done": "succeeded", "failed": "failed", "cancelled": "cancelled"}[state]
	if t.Status == "queued" || t.Status == "running" {
		var e *string
		if errText != "" {
			e = &errText
		}
		if err := cd.FinishTask(ctx, t.ID, status, nil, e); err != nil {
			return workflows.Result{}, err
		}
	}
	t.Status = status
	metrics.RunnerTasks.WithLabelValues(t.Type, status).Inc()
	if t.StartedAt != nil {
		metrics.RunnerTaskDuration.WithLabelValues(t.Type).Observe(time.Since(*t.StartedAt).Seconds())
	}
	if run.ParentID != nil {
		typ := "task_done"
		if state != "done" {
			typ = "task_failed"
		}
		var result map[string]any
		_ = json.Unmarshal(t.Result, &result)
		if err := workflows.Send(ctx, tx, *run.ParentID, typ, map[string]any{"taskId": t.ID, "serviceId": t.ServiceID,
			"service": t.Service, "type": t.Type, "error": errText, "result": result}); err != nil {
			return workflows.Result{}, err
		}
	}
	return workflows.Result{State: state, Context: run.Context, Error: errText, Notify: taskNotify(t)}, nil
}

// Step implements workflows.Machine.
func (m TaskMachine) Step(ctx context.Context, tx pgx.Tx, run *workflows.Run, evs []workflows.Event, now time.Time) (workflows.Result, error) {
	cd := cycledata.New(tx)
	t, err := cd.TaskByID(ctx, run.SubjectID)
	if errors.Is(err, cycledata.ErrNotFound) {
		// The task row is inserted right after the run, in the same transaction.
		return workflows.Result{State: run.State, Context: run.Context, NextRunAt: workflows.At(now.Add(time.Second))}, nil
	}
	if err != nil {
		return workflows.Result{}, err
	}
	if workflows.Has(evs, "cancel") && run.State != "done" {
		res, err := m.finish(ctx, tx, run, t, "cancelled", "cancelled")
		if err == nil && t.ExecutorRef != nil {
			res.Effects = []workflows.Effect{{Type: EffectStop, Payload: map[string]any{"taskId": t.ID, "reason": "cancelled"}}}
		}
		return res, err
	}
	switch run.State {
	case "queued":
		max := m.Limits.MaxParallel
		if max <= 0 {
			max = 10
		}
		n, err := cd.CountRunning(ctx)
		if err != nil {
			return workflows.Result{}, err
		}
		busy, err := cd.ServiceBusy(ctx, t.ServiceID, t.ID)
		if err != nil {
			return workflows.Result{}, err
		}
		if n >= max || busy {
			// RUN-05: wait for the repository (one task per service) or a free slot.
			return workflows.Result{State: "queued", Context: run.Context, NextRunAt: workflows.At(now.Add(10 * time.Second))}, nil
		}
		return workflows.Result{State: "starting", Context: run.Context, NextRunAt: workflows.At(now.Add(startingWindow)),
			Effects: []workflows.Effect{{Type: EffectStart, Payload: map[string]any{"taskId": t.ID}}}, Notify: taskNotify(t)}, nil
	case "starting":
		if workflows.Has(evs, "started") {
			timeout := m.Limits.Timeout
			if timeout <= 0 {
				timeout = 2 * time.Hour
			}
			t.Status = "running"
			return workflows.Result{State: "running", Context: run.Context, NextRunAt: workflows.At(now.Add(timeout + 5*time.Minute)), Notify: taskNotify(t)}, nil
		}
		if ev, ok := workflows.Find(evs, "effect_failed"); ok {
			var f struct{ Error string }
			_ = ev.Decode(&f)
			return m.finish(ctx, tx, run, t, "failed", "the task could not start: "+f.Error)
		}
		if ev, ok := workflows.Find(evs, "task_result"); ok { // a fast task may report before "started"
			return m.result(ctx, tx, run, t, ev)
		}
		if len(evs) == 0 && run.NextRunAt != nil && !now.Before(*run.NextRunAt) {
			return m.finish(ctx, tx, run, t, "failed", "the task did not start in time")
		}
		return workflows.Result{State: run.State, Context: run.Context, NextRunAt: run.NextRunAt}, nil
	case "running":
		if ev, ok := workflows.Find(evs, "task_result"); ok {
			return m.result(ctx, tx, run, t, ev)
		}
		if len(evs) == 0 && run.NextRunAt != nil && !now.Before(*run.NextRunAt) {
			// RUN-03: RUNNER_TIMEOUT exceeded.
			res, err := m.finish(ctx, tx, run, t, "failed", "the task exceeded RUNNER_TIMEOUT")
			if err == nil {
				res.Effects = []workflows.Effect{{Type: EffectStop, Payload: map[string]any{"taskId": t.ID, "reason": "timeout"}}}
			}
			return res, err
		}
		return workflows.Result{State: run.State, Context: run.Context, NextRunAt: run.NextRunAt}, nil
	}
	return workflows.Keep(run), nil
}

func (m TaskMachine) result(ctx context.Context, tx pgx.Tx, run *workflows.Run, t *cycledata.Task, ev workflows.Event) (workflows.Result, error) {
	var r struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	_ = ev.Decode(&r)
	t, err := cycledata.New(tx).TaskByID(ctx, t.ID) // reload the stored result
	if err != nil {
		return workflows.Result{}, err
	}
	if r.Status == "succeeded" {
		return m.finish(ctx, tx, run, t, "done", "")
	}
	if r.Error == "" {
		r.Error = "the task failed"
	}
	return m.finish(ctx, tx, run, t, "failed", r.Error)
}

// ─── Runner effects ─────────────────────────────────────────────────

// RunnerEffects start and stop runner processes (worker).
type RunnerEffects struct {
	Q           postgres.Querier
	Executor    executor.Executor
	InternalURL string
}

// NewToken returns a one-time task token and its hash.
func NewToken() (string, []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := "hmt_" + hex.EncodeToString(b)
	return tok, HashToken(tok)
}

// HashToken hashes a task token for storage.
func HashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}

// Start implements runner.start.
func (e *RunnerEffects) Start(ctx context.Context, run workflows.RunRef, payload json.RawMessage) ([]workflows.NewEvent, error) {
	var in struct {
		TaskID uuid.UUID `json:"taskId"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	cd := cycledata.New(e.Q)
	t, err := cd.TaskByID(ctx, in.TaskID)
	if err != nil {
		return nil, err
	}
	if t.Status != "queued" && t.Status != "running" {
		return nil, nil // cancelled meanwhile
	}
	tok, hash := NewToken()
	if err := cd.StartTask(ctx, t.ID, hash, ""); err != nil {
		if postgres.IsUniqueViolation(err) {
			return nil, fmt.Errorf("another task runs in service %s", t.Service)
		}
		return nil, err
	}
	ref, err := e.Executor.Start(ctx, executor.Task{ID: t.ID.String(), Token: tok, InternalURL: e.InternalURL})
	if err != nil {
		_, _ = e.Q.Exec(ctx, `UPDATE agent_tasks SET status = 'queued', token_hash = NULL, started_at = NULL WHERE id = $1`, t.ID)
		return nil, err
	}
	if _, err := e.Q.Exec(ctx, `UPDATE agent_tasks SET executor_ref = $2 WHERE id = $1`, t.ID, ref); err != nil {
		return nil, err
	}
	metrics.RunnerTasks.WithLabelValues(t.Type, "running").Inc()
	return []workflows.NewEvent{{Type: "started", Payload: map[string]string{"ref": ref}}}, nil
}

// Stop implements runner.stop.
func (e *RunnerEffects) Stop(ctx context.Context, run workflows.RunRef, payload json.RawMessage) ([]workflows.NewEvent, error) {
	var in struct {
		TaskID uuid.UUID `json:"taskId"`
		Reason string    `json:"reason"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	t, err := cycledata.New(e.Q).TaskByID(ctx, in.TaskID)
	if err != nil {
		return nil, err
	}
	if t.ExecutorRef != nil && *t.ExecutorRef != "" {
		if err := e.Executor.Stop(ctx, *t.ExecutorRef); err != nil && !errors.Is(err, executor.ErrNotFound) {
			return nil, err
		}
	}
	_, err = e.Q.Exec(ctx, `UPDATE agent_tasks SET token_hash = NULL WHERE id = $1`, t.ID)
	return nil, err
}
