package codegen

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Task workflow kind and effects.
const (
	TaskKind = "codegen_task"
	// FTR.HMR.CMN-0007 tech §7.1: code tasks run in the sandbox of Nabu.
	EffectStart    = "code.start"
	EffectStop     = "code.cancel"
	EffectPoll     = "code.poll"
	EffectApply    = "code.apply"
	TaskImplement  = "implement"
	TaskReview     = "address_review"
	TaskUpdatePR   = "update_pr"
	TaskRevert     = "revert"
	TaskCISetup    = "ci_setup"
	startingWindow = 20 * time.Minute // code.start uploads the working copy
	// pollEvery is how often a running task asks Nabu about its run; applyWindow
	// bounds taking the changes and committing them (FTR.HMR.CMN-0007 tech §7.1).
	pollEvery   = 30 * time.Second
	applyWindow = 20 * time.Minute
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

// AutofixAuthor is the author of the review comment of an automatic fix of
// discrepancies found on validation (FTR.HMR.CMN-0008 R21).
const AutofixAuthor = "hammurapi-validation"

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
	metrics.CodeTaskStates.WithLabelValues(s.Type, "queued").Inc()
	return id, nil
}

// Limits of code tasks (CODE_MAX_PARALLEL, CODE_TIMEOUT).
type Limits struct {
	MaxParallel int
	Timeout     time.Duration // 0 — no limit
}

// TaskMachine is the codegen_task workflow: queued → starting → running →
// applying → done · failed · cancelled (FTR.HMR.CMN-0007 tech §7.1). running
// polls the run of Nabu every 30 s. One task per service repository runs at a
// time (unique index), and at most MaxParallel tasks overall.
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
	metrics.CodeTaskStates.WithLabelValues(t.Type, status).Inc()
	if t.StartedAt != nil {
		metrics.CodeTaskDuration.WithLabelValues(t.Type).Observe(time.Since(*t.StartedAt).Seconds())
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
		if err == nil && run.State != "queued" {
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
			t.Status = "running"
			return workflows.Result{State: "running", Context: run.Context, NextRunAt: workflows.At(now.Add(pollEvery)), Notify: taskNotify(t)}, nil
		}
		if ev, ok := workflows.Find(evs, "effect_failed"); ok {
			var f struct{ Error string }
			_ = ev.Decode(&f)
			return m.finish(ctx, tx, run, t, "failed", "the task could not start: "+f.Error)
		}
		if ev, ok := workflows.Find(evs, "task_result"); ok { // done without an agent or refused at once
			return m.result(ctx, tx, run, t, ev)
		}
		if len(evs) == 0 && run.NextRunAt != nil && !now.Before(*run.NextRunAt) {
			return m.finish(ctx, tx, run, t, "failed", "the task did not start in time")
		}
		return workflows.Result{State: run.State, Context: run.Context, NextRunAt: run.NextRunAt}, nil
	case "running":
		if ev, ok := workflows.Find(evs, "run_finished"); ok {
			return workflows.Result{State: "applying", Context: run.Context, NextRunAt: workflows.At(now.Add(applyWindow)),
				Effects: []workflows.Effect{{Type: EffectApply, Payload: json.RawMessage(ev.Payload)}}}, nil
		}
		if ev, ok := workflows.Find(evs, "task_result"); ok {
			return m.result(ctx, tx, run, t, ev)
		}
		if len(evs) == 0 && run.NextRunAt != nil && !now.Before(*run.NextRunAt) {
			if t.StartedAt != nil && m.Limits.Timeout > 0 && now.After(t.StartedAt.Add(m.Limits.Timeout+5*time.Minute)) {
				// RUN-03: CODE_TIMEOUT exceeded.
				res, err := m.finish(ctx, tx, run, t, "failed", "the task exceeded CODE_TIMEOUT")
				if err == nil {
					_, _ = tx.Exec(ctx, `UPDATE agent_tasks SET error_class = 'limit' WHERE id = $1`, t.ID)
					res.Effects = []workflows.Effect{{Type: EffectStop, Payload: map[string]any{"taskId": t.ID, "reason": "timeout"}}}
				}
				return res, err
			}
			return workflows.Result{State: run.State, Context: run.Context, NextRunAt: workflows.At(now.Add(pollEvery)),
				Effects: []workflows.Effect{{Type: EffectPoll, Payload: map[string]any{"taskId": t.ID}}}}, nil
		}
		return workflows.Result{State: run.State, Context: run.Context, NextRunAt: run.NextRunAt}, nil
	case "applying":
		if ev, ok := workflows.Find(evs, "task_result"); ok {
			return m.result(ctx, tx, run, t, ev)
		}
		if ev, ok := workflows.Find(evs, "effect_failed"); ok {
			var f struct{ Error string }
			_ = ev.Decode(&f)
			return m.finish(ctx, tx, run, t, "failed", "the changes could not be applied: "+f.Error)
		}
		if len(evs) == 0 && run.NextRunAt != nil && !now.Before(*run.NextRunAt) {
			return m.finish(ctx, tx, run, t, "failed", "the changes were not applied in time")
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

// ─── Task tokens ─────────────────────────────────────────────────────

// TaskToken returns the token of a task and its hash. It is derived from
// the key and the task, so a repeat of code.start gives the same token as
// the run Nabu returns by the idempotency key (CT-17); the hash is cleared
// when the task ends, which revokes it.
func TaskToken(key []byte, task uuid.UUID) (string, []byte) {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("hammurapi:task-token:" + task.String()))
	tok := "hmt_" + hex.EncodeToString(m.Sum(nil))
	return tok, HashToken(tok)
}

// HashToken hashes a task token for storage.
func HashToken(tok string) []byte {
	h := sha256.Sum256([]byte(tok))
	return h[:]
}
