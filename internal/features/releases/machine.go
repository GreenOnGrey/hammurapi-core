package releases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/codegen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/deploy"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Kind is the release workflow kind; RollbackKind the rollback workflow.
const (
	Kind         = "release"
	RollbackKind = "rollback"
)

// Steps of the release workflow (release status in brackets):
//
//	awaiting_start (merging) → merge → merge_wait [→ update_wait] → deploy → deploy_wait (deploying)
//	  … for every service in the plan order …
//	→ flags_wait (enabling_flags, only with feature flags) → awaiting_confirmation → confirm_wait → succeeded
//	any step before succeeded: rollback → rolling_back → rolled_back
const (
	stepAwaitingStart = "awaiting_start"
	stepMerge         = "merge"
	stepMergeWait     = "merge_wait"
	stepUpdateWait    = "update_wait"
	stepDeploy        = "deploy"
	stepDeployWait    = "deploy_wait"
	stepFlags         = "flags"
	stepFlagsWait     = "flags_wait"
	stepAwaitConfirm  = "awaiting_confirmation"
	stepConfirm       = "confirm"
	stepConfirmWait   = "confirm_wait"
)

// StatusOf maps a step to the release status.
func StatusOf(step string) string {
	switch step {
	case stepDeploy, stepDeployWait:
		return "deploying"
	case stepFlags, stepFlagsWait:
		return "enabling_flags"
	case stepAwaitConfirm, stepConfirm, stepConfirmWait:
		return "awaiting_confirmation"
	}
	return "merging"
}

// Machine is the release workflow.
type Machine struct{}

// Kind implements workflows.Machine.
func (Machine) Kind() string { return Kind }

// view is the mutable part of the run context.
type view struct {
	Order       []string          `json:"order"`
	Idx         int               `json:"idx"`
	Initiator   string            `json:"initiator,omitempty"`
	Confirmer   string            `json:"confirmer,omitempty"`
	Merged      map[string]string `json:"merged,omitempty"` // service → merge sha
	DeployRunID string            `json:"deployRunId,omitempty"`
	RetryStep   string            `json:"retryStep,omitempty"`
	BlockedFrom string            `json:"blockedFrom,omitempty"`
}

func load(c map[string]any) view {
	raw, _ := json.Marshal(c)
	var v view
	_ = json.Unmarshal(raw, &v)
	if v.Merged == nil {
		v.Merged = map[string]string{}
	}
	return v
}

func (v view) ctx() map[string]any {
	raw, _ := json.Marshal(v)
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	return c
}

func releaseEvent(rel *cycledata.Release, status string) events.Event {
	return events.Event{Type: events.ReleaseUpdated, Data: map[string]any{"key": rel.Key, "status": status}}
}

// ServicePR returns the release PR of a service.
func ServicePR(prs []cycledata.PR, service string) *cycledata.PR {
	for i := range prs {
		if prs[i].Service != nil && *prs[i].Service == service && prs[i].Kind == "service" {
			return &prs[i]
		}
	}
	return nil
}

// Deploy creates the deploy run of a service and, when the environment is
// configured, the trigger effect (DEP-07: otherwise a tag or a manual mark).
func Deploy(ctx context.Context, tx pgx.Tx, rel *cycledata.Release, svc *cycledata.Service, ref string, rollback bool, now time.Time) (uuid.UUID, []workflows.Effect, *time.Time, error) {
	cd := cycledata.New(tx)
	run := &cycledata.DeployRun{Environment: "production", ServiceID: svc.ID, ReleaseID: &rel.ID, Ref: ref, IsRollback: rollback}
	if err := cd.InsertDeployRun(ctx, run); err != nil {
		return uuid.Nil, nil, nil, err
	}
	set, ok, err := deploy.Load(ctx, tx, "production")
	if err != nil {
		return uuid.Nil, nil, nil, err
	}
	if !ok {
		return run.ID, nil, nil, nil // waits for a tag or a manual mark, no timeout
	}
	deadline := now.Add(set.Timeout())
	return run.ID, []workflows.Effect{{Type: deploy.Effect, Key: run.ID.String(), Payload: deploy.TriggerPayload{DeployRunID: run.ID}}}, &deadline, nil
}

// DeployOutcome is the result of waiting for a deploy.
type DeployOutcome int

// Deploy outcomes.
const (
	DeployWaiting DeployOutcome = iota
	DeploySucceeded
	DeployFailed
)

// WaitDeploy handles deploy_result, tag and timeout events for the current
// deploy run; effects are provider.check_tag calls for tags.
func WaitDeploy(ctx context.Context, tx pgx.Tx, runID uuid.UUID, svc *cycledata.Service, mergeSHA string, evs []workflows.Event, deadline *time.Time, now time.Time) (DeployOutcome, string, []workflows.Effect, error) {
	cd := cycledata.New(tx)
	var effects []workflows.Effect
	for _, ev := range evs {
		switch ev.Type {
		case "tag":
			var t struct {
				ServiceID uuid.UUID `json:"serviceId"`
				Tag       string    `json:"tag"`
				SHA       string    `json:"sha"`
			}
			_ = ev.Decode(&t)
			if t.ServiceID == svc.ID && mergeSHA != "" && t.SHA != "" {
				effects = append(effects, workflows.Effect{Type: EffectCheckTag, Key: "tag:" + t.Tag, Payload: map[string]any{
					"deployRunId": runID, "repo": svc.Repo, "mergeSha": mergeSHA, "tagSha": t.SHA, "tag": t.Tag}})
			}
		}
	}
	run, err := cd.DeployRunByID(ctx, runID)
	if err != nil {
		return DeployWaiting, "", nil, err
	}
	switch run.Status {
	case "success":
		return DeploySucceeded, "", effects, nil
	case "failure":
		reason := fmt.Sprintf("the deploy of %s failed", svc.Key)
		if run.RunURL != nil {
			reason += ": " + *run.RunURL
		}
		return DeployFailed, reason, effects, nil
	case "timeout":
		return DeployFailed, fmt.Sprintf("no deploy result for %s", svc.Key), effects, nil
	}
	if deadline != nil && !now.Before(*deadline) && len(evs) == 0 {
		// DEP-05: no result within the environment's timeout.
		e := "no deploy result"
		if _, err := cd.UpdateDeployRun(ctx, runID, "timeout", "pipeline", nil, nil, &e, nil); err != nil {
			return DeployWaiting, "", nil, err
		}
		return DeployFailed, fmt.Sprintf("no deploy result for %s", svc.Key), effects, nil
	}
	return DeployWaiting, "", effects, nil
}

// FlagsRequired reports whether the release waits for the feature flag (R29).
func FlagsRequired(ctx context.Context, tx pgx.Tx, f *specdata.Feature) (bool, error) {
	if f.FlagKey == nil || *f.FlagKey == "" {
		return false, nil
	}
	var fs cycledata.FlagsSetting
	if _, err := cycledata.New(tx).Setting(ctx, "feature_flags", &fs); err != nil {
		return false, err
	}
	return fs.Enabled, nil
}

// Step implements workflows.Machine.
func (m Machine) Step(ctx context.Context, tx pgx.Tx, run *workflows.Run, evs []workflows.Event, now time.Time) (workflows.Result, error) {
	cd := cycledata.New(tx)
	store := specdata.NewPGTx(nil, tx)
	rel, err := cd.ReleaseByID(ctx, run.SubjectID)
	if err != nil {
		return workflows.Result{}, err
	}
	v := load(run.Context)

	// Rollback from any step before confirmation (R32).
	if run.State == "rolling_back" {
		if workflows.Has(evs, "rollback_done") {
			return workflows.Result{State: "rolled_back", Context: run.Context, Notify: []events.Event{releaseEvent(rel, "rolled_back")}}, nil
		}
		return workflows.Keep(run), nil
	}
	if ev, ok := workflows.Find(evs, "rollback"); ok {
		var in struct {
			Reason string    `json:"reason"`
			UserID uuid.UUID `json:"userId"`
		}
		_ = ev.Decode(&in)
		if err := cd.StartRollback(ctx, rel.ID, in.UserID, in.Reason); err != nil {
			return workflows.Result{}, err
		}
		if _, err := workflows.Start(ctx, tx, RollbackKind, rel.ID, &run.ID, "reverting",
			map[string]any{"reason": in.Reason, "initiator": in.UserID.String()}); err != nil && !errors.Is(err, workflows.ErrActiveRun) {
			return workflows.Result{}, err
		}
		if err := cd.AddActivity(ctx, "release", rel.ID, "rollback_started", &in.UserID, false, map[string]any{"reason": in.Reason}); err != nil {
			return workflows.Result{}, err
		}
		metrics.ReleaseRollbacks.Inc()
		return workflows.Result{State: "rolling_back", Step: run.Step, Context: run.Context,
			Notify: []events.Event{releaseEvent(rel, "rolling_back"), {Type: events.FocusChanged, Data: map[string]any{"key": rel.Key}}}}, nil
	}

	step := run.Step
	if step == "" {
		step = stepAwaitingStart
	}
	block := func(reason, retry string) (workflows.Result, error) {
		v.RetryStep = retry
		msg := reason
		if err := cd.SetReleaseStatus(ctx, rel.ID, StatusOf(step), &msg); err != nil {
			return workflows.Result{}, err
		}
		return workflows.Result{State: workflows.StateBlocked, Step: step, Context: v.ctx(), Error: reason,
			Notify: []events.Event{{Type: events.ReleaseBlocked, Data: map[string]any{"key": rel.Key, "reason": reason}},
				{Type: events.FocusChanged, Data: map[string]any{"key": rel.Key}}}}, nil
	}
	if run.State == workflows.StateBlocked {
		if !workflows.Has(evs, "retry") && !workflows.Has(evs, "retry_mark") {
			return workflows.Result{State: run.State, Step: run.Step, Context: run.Context, Error: derefString(run.LastError)}, nil
		}
		step = v.RetryStep
		if workflows.Has(evs, "retry_mark") {
			step = stepDeployWait // a manual mark after a failed deploy counts as the release
		}
		if step == "" {
			step = stepMerge
		}
		v.RetryStep = ""
		if err := cd.SetReleaseStatus(ctx, rel.ID, StatusOf(step), nil); err != nil {
			return workflows.Result{}, err
		}
		evs = nil
	}

	f, err := store.FeatureByID(ctx, rel.FeatureID)
	if err != nil {
		return workflows.Result{}, err
	}
	prs, err := cd.ReleasePRs(ctx, rel.ID)
	if err != nil {
		return workflows.Result{}, err
	}
	var effects []workflows.Effect
	var next *time.Time
	notify := []events.Event{}
	current := func() (*cycledata.Service, *cycledata.PR, error) {
		if v.Idx >= len(v.Order) {
			return nil, nil, errors.New("no current service")
		}
		svc, err := cd.ServiceByKey(ctx, v.Order[v.Idx])
		if err != nil {
			return nil, nil, err
		}
		pr := ServicePR(prs, svc.Key)
		if pr == nil {
			return svc, nil, fmt.Errorf("the release has no PR of %s", svc.Key)
		}
		return svc, pr, nil
	}

	for guard := 0; guard < 20; guard++ {
		switch step {
		case stepAwaitingStart:
			ev, ok := workflows.Find(evs, "merge_started")
			if !ok {
				return workflows.Result{State: "merging", Step: step, Context: v.ctx(), Notify: notify}, nil
			}
			var in struct {
				UserID uuid.UUID `json:"userId"`
			}
			_ = ev.Decode(&in)
			v.Initiator, v.Idx = in.UserID.String(), 0
			v.Order = rel.Plan.Order
			if err := cd.StartMerge(ctx, rel.ID, in.UserID); err != nil {
				return workflows.Result{}, err
			}
			if err := cd.AddActivity(ctx, "release", rel.ID, "merge_started", &in.UserID, false, map[string]any{"order": v.Order}); err != nil {
				return workflows.Result{}, err
			}
			evs = nil
			step = stepMerge
			if len(v.Order) == 0 {
				step = stepFlags
			}
		case stepMerge:
			svc, pr, err := current()
			if err != nil {
				return block(err.Error(), stepMerge)
			}
			if pr.State == "merged" && pr.MergeSHA != nil {
				v.Merged[svc.Key] = *pr.MergeSHA
				step = stepDeploy
				continue
			}
			initiator, _ := uuid.Parse(v.Initiator)
			effects = append(effects, workflows.Effect{Type: EffectMerge, Key: pr.ID.String(), Payload: MergePayload{PRID: pr.ID, UserID: initiator}})
			if err := cd.SetReleaseStatus(ctx, rel.ID, "merging", nil); err != nil {
				return workflows.Result{}, err
			}
			notify = append(notify, releaseEvent(rel, "merging"))
			return workflows.Result{State: "merging", Step: stepMergeWait, Context: v.ctx(), Effects: effects, Notify: notify}, nil
		case stepMergeWait:
			svc, pr, err := current()
			if err != nil {
				return block(err.Error(), stepMerge)
			}
			merged := ""
			for _, ev := range evs {
				var p struct {
					PRID     uuid.UUID `json:"prId"`
					SHA      string    `json:"sha"`
					MergeSHA string    `json:"mergeSha"`
					Reason   string    `json:"reason"`
					Error    string    `json:"error"`
				}
				_ = ev.Decode(&p)
				switch ev.Type {
				case "merged", "pr_merged":
					if p.PRID == pr.ID {
						merged = p.SHA + p.MergeSHA
					}
				case "needs_update":
					if p.PRID == pr.ID {
						if domain.AgentDisabled() {
							return block(fmt.Sprintf("the agent is not connected: update the PR of %s by hand (%s)", svc.Key, p.Reason), stepMerge)
						}
						initiator, _ := uuid.Parse(v.Initiator)
						if _, err := codegen.StartTask(ctx, tx, codegen.TaskSpec{Type: codegen.TaskUpdatePR, FeatureID: &rel.FeatureID, ReleaseID: &rel.ID,
							ServiceID: svc.ID, Initiator: &initiator, Input: codegen.TaskInput{PRNumber: pr.Number, Release: rel.Key, Reason: p.Reason},
							Parent: &run.ID}); err != nil {
							return workflows.Result{}, err
						}
						if err := cd.AddActivity(ctx, "release", rel.ID, "update_pr", nil, true, map[string]any{"service": svc.Key, "reason": p.Reason}); err != nil {
							return workflows.Result{}, err
						}
						return workflows.Result{State: "merging", Step: stepUpdateWait, Context: v.ctx(), Notify: []events.Event{releaseEvent(rel, "merging")}}, nil
					}
				case "merge_failed":
					return block(fmt.Sprintf("the provider refused to merge %s: %s", svc.Key, p.Reason), stepMerge)
				case "effect_failed":
					return block("merge failed: "+p.Error, stepMerge)
				}
			}
			if merged == "" {
				return workflows.Result{State: "merging", Step: step, Context: v.ctx(), Notify: notify}, nil
			}
			v.Merged[svc.Key] = merged
			initiator, _ := uuid.Parse(v.Initiator)
			if err := cd.AddActivity(ctx, "release", rel.ID, "pr_merged", &initiator, false, map[string]any{"service": svc.Key, "pr": pr.Number}); err != nil {
				return workflows.Result{}, err
			}
			evs = nil
			step = stepDeploy
		case stepUpdateWait:
			if ev, ok := workflows.Find(evs, "task_failed"); ok {
				var p struct {
					Error string `json:"error"`
				}
				_ = ev.Decode(&p)
				return block("the agent could not update the PR: "+p.Error, stepMerge)
			}
			if !workflows.Has(evs, "task_done") {
				return workflows.Result{State: "merging", Step: step, Context: v.ctx()}, nil
			}
			evs = nil
			step = stepMerge
		case stepDeploy:
			svc, _, err := current()
			if err != nil {
				return block(err.Error(), stepDeploy)
			}
			id, effs, deadline, err := Deploy(ctx, tx, rel, svc, v.Merged[svc.Key], false, now)
			if err != nil {
				return workflows.Result{}, err
			}
			v.DeployRunID = id.String()
			if err := cd.SetReleaseStatus(ctx, rel.ID, "deploying", nil); err != nil {
				return workflows.Result{}, err
			}
			effects = append(effects, effs...)
			next = deadline
			notify = append(notify, releaseEvent(rel, "deploying"), events.Event{Type: events.FocusChanged, Data: map[string]any{"key": rel.Key}})
			return workflows.Result{State: "deploying", Step: stepDeployWait, Context: v.ctx(), Effects: effects, NextRunAt: next, Notify: notify}, nil
		case stepDeployWait:
			svc, _, err := current()
			if err != nil {
				return block(err.Error(), stepDeploy)
			}
			id, _ := uuid.Parse(v.DeployRunID)
			outcome, reason, effs, err := WaitDeploy(ctx, tx, id, svc, v.Merged[svc.Key], evs, run.NextRunAt, now)
			if err != nil {
				return workflows.Result{}, err
			}
			effects = append(effects, effs...)
			switch outcome {
			case DeployWaiting:
				return workflows.Result{State: "deploying", Step: step, Context: v.ctx(), Effects: effects, NextRunAt: run.NextRunAt}, nil
			case DeployFailed:
				res, err := block(reason, stepDeploy)
				res.Effects = effects
				return res, err
			}
			if err := cd.AddActivity(ctx, "release", rel.ID, "service_released", nil, false, map[string]any{"service": svc.Key}); err != nil {
				return workflows.Result{}, err
			}
			v.Idx++
			v.DeployRunID = ""
			evs = nil
			if v.Idx < len(v.Order) {
				step = stepMerge
			} else {
				step = stepFlags
			}
		case stepFlags:
			need, err := FlagsRequired(ctx, tx, f)
			if err != nil {
				return workflows.Result{}, err
			}
			if need {
				state, _, err := cd.LastFlagState(ctx, *f.FlagKey)
				if err != nil {
					return workflows.Result{}, err
				}
				if state != "on" {
					if err := cd.SetReleaseStatus(ctx, rel.ID, "enabling_flags", nil); err != nil {
						return workflows.Result{}, err
					}
					notify = append(notify, releaseEvent(rel, "enabling_flags"), events.Event{Type: events.FocusChanged, Data: map[string]any{"key": rel.Key}})
					return workflows.Result{State: "enabling_flags", Step: stepFlagsWait, Context: v.ctx(), Effects: effects, Notify: notify}, nil
				}
			}
			// R30 (metric evaluation) is the next release of the feature: confirmation follows right after the deploy.
			step = stepAwaitConfirm
			if err := cd.SetReleaseStatus(ctx, rel.ID, "awaiting_confirmation", nil); err != nil {
				return workflows.Result{}, err
			}
			notify = append(notify, releaseEvent(rel, "awaiting_confirmation"), events.Event{Type: events.FocusChanged, Data: map[string]any{"key": rel.Key}})
			return workflows.Result{State: "awaiting_confirmation", Step: step, Context: v.ctx(), Effects: effects, Notify: notify}, nil
		case stepFlagsWait:
			if !workflows.Has(evs, "flag_on") {
				return workflows.Result{State: "enabling_flags", Step: step, Context: v.ctx()}, nil
			}
			evs = nil
			step = stepFlags
		case stepAwaitConfirm:
			ev, ok := workflows.Find(evs, "confirmed")
			if !ok {
				return workflows.Result{State: "awaiting_confirmation", Step: step, Context: v.ctx(), Notify: notify}, nil
			}
			var in struct {
				UserID uuid.UUID `json:"userId"`
			}
			_ = ev.Decode(&in)
			v.Confirmer = in.UserID.String()
			step = stepConfirm
		case stepConfirm:
			confirmer, _ := uuid.Parse(v.Confirmer)
			effects = append(effects, workflows.Effect{Type: EffectMergeSpec, Payload: map[string]any{"featureId": f.ID, "userId": confirmer}})
			return workflows.Result{State: "awaiting_confirmation", Step: stepConfirmWait, Context: v.ctx(), Effects: effects}, nil
		case stepConfirmWait:
			if ev, ok := workflows.Find(evs, "merge_failed"); ok {
				var p struct {
					Reason string `json:"reason"`
				}
				_ = ev.Decode(&p)
				return block("the specification PR could not be merged: "+p.Reason, stepConfirm)
			}
			if ev, ok := workflows.Find(evs, "effect_failed"); ok {
				var p struct {
					Error string `json:"error"`
				}
				_ = ev.Decode(&p)
				return block("the specification PR could not be merged: "+p.Error, stepConfirm)
			}
			if !workflows.Has(evs, "spec_merged") {
				return workflows.Result{State: "awaiting_confirmation", Step: step, Context: v.ctx()}, nil
			}
			confirmer, _ := uuid.Parse(v.Confirmer)
			if err := Succeed(ctx, tx, rel, f, confirmer); err != nil {
				return workflows.Result{}, err
			}
			return workflows.Result{State: "succeeded", Step: "succeeded", Context: v.ctx(),
				Notify: []events.Event{releaseEvent(rel, "succeeded"), {Type: events.FeatureUpdated, Data: map[string]any{"uniqueId": f.UniqueID}},
					{Type: events.FocusChanged, Data: map[string]any{"key": rel.Key}}}}, nil
		default:
			return workflows.Keep(run), nil
		}
	}
	return workflows.Result{}, errors.New("release workflow did not settle")
}

// Succeed finishes a confirmed release (R31): release "succeeded", feature
// "released", issues "resolved", the spec PR recorded as merged.
func Succeed(ctx context.Context, tx pgx.Tx, rel *cycledata.Release, f *specdata.Feature, confirmer uuid.UUID) error {
	cd := cycledata.New(tx)
	if err := cd.ConfirmRelease(ctx, rel.ID, confirmer); err != nil {
		return err
	}
	if err := specdata.NewPGTx(nil, tx).SetPhase(ctx, f.ID, domain.PhaseReleased); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE pull_requests SET state = 'merged', merged_at = now(), merged_by = $2 WHERE feature_id = $1 AND kind = 'spec'`, f.ID, confirmer); err != nil {
		return err
	}
	ids, err := cd.FeatureIssueIDs(ctx, f.ID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		var st string
		if err := tx.QueryRow(ctx, `SELECT status::text FROM issues WHERE id = $1`, id).Scan(&st); err != nil {
			return err
		}
		if st == string(domain.IssueAccepted) {
			if err := cd.SetIssueStatus(ctx, id, domain.IssueResolved); err != nil {
				return err
			}
		}
	}
	return cd.AddActivity(ctx, "release", rel.ID, "confirmed", &confirmer, false, nil)
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
