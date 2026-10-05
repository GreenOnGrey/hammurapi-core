// Package rollback implements the rollback of a release (FTR.HMR.CMN-0002 R32–R34,
// arch §11.5): for every merged service PR in reverse order a revert PR from
// the bot, its merge with the token of the expert who started the rollback and
// a redeploy; then the feature flag is awaited off (Hammurapi never switches
// flags), the specification PR is closed without merging, and the issues go
// back to "new" with a fresh Discovery.
package rollback

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
	"github.com/GreenOnGrey/hammurapi-core/internal/features/discovery"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/releases"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Steps: revert → revert_wait → merge → merge_wait → deploy → deploy_wait (per
// merged service, reverse order) → flags → flags_wait → close → close_wait → done.
const (
	stepStart      = ""
	stepRevert     = "revert"
	stepRevertWait = "revert_wait"
	stepMerge      = "merge"
	stepMergeWait  = "merge_wait"
	stepDeploy     = "deploy"
	stepDeployWait = "deploy_wait"
	stepFlags      = "flags"
	stepFlagsWait  = "flags_wait"
	stepClose      = "close"
	stepCloseWait  = "close_wait"
)

// Machine is the rollback workflow.
type Machine struct{}

// Kind implements workflows.Machine.
func (Machine) Kind() string { return releases.RollbackKind }

type view struct {
	Reason      string   `json:"reason"`
	Initiator   string   `json:"initiator"`
	Services    []string `json:"services"` // merged services, reverse merge order
	Idx         int      `json:"idx"`
	RevertPRID  string   `json:"revertPrId,omitempty"`
	MergeSHA    string   `json:"mergeSha,omitempty"`
	DeployRunID string   `json:"deployRunId,omitempty"`
	RetryStep   string   `json:"retryStep,omitempty"`
	Planned     bool     `json:"planned"`
}

func load(c map[string]any) view {
	raw, _ := json.Marshal(c)
	var v view
	_ = json.Unmarshal(raw, &v)
	return v
}

func (v view) ctx() map[string]any {
	raw, _ := json.Marshal(v)
	var c map[string]any
	_ = json.Unmarshal(raw, &c)
	return c
}

// Step implements workflows.Machine.
func (m Machine) Step(ctx context.Context, tx pgx.Tx, run *workflows.Run, evs []workflows.Event, now time.Time) (workflows.Result, error) {
	cd := cycledata.New(tx)
	store := specdata.NewPGTx(nil, tx)
	rel, err := cd.ReleaseByID(ctx, run.SubjectID)
	if err != nil {
		return workflows.Result{}, err
	}
	f, err := store.FeatureByID(ctx, rel.FeatureID)
	if err != nil {
		return workflows.Result{}, err
	}
	v := load(run.Context)
	initiator, _ := uuid.Parse(v.Initiator)
	notify := []events.Event{{Type: events.ReleaseUpdated, Data: map[string]any{"key": rel.Key, "status": "rolling_back"}}}
	block := func(reason, retry string) (workflows.Result, error) {
		v.RetryStep = retry
		msg := reason
		if err := cd.SetReleaseBlocked(ctx, rel.ID, &msg); err != nil {
			return workflows.Result{}, err
		}
		return workflows.Result{State: workflows.StateBlocked, Step: run.Step, Context: v.ctx(), Error: reason,
			Notify: []events.Event{{Type: events.ReleaseBlocked, Data: map[string]any{"key": rel.Key, "reason": reason}},
				{Type: events.FocusChanged, Data: map[string]any{"key": rel.Key}}}}, nil
	}
	step := run.Step
	if run.State == workflows.StateBlocked {
		switch {
		case workflows.Has(evs, "retry_mark"):
			step = stepDeployWait
		case workflows.Has(evs, "retry"):
			step = v.RetryStep
		default:
			return workflows.Result{State: run.State, Step: run.Step, Context: run.Context, Error: derefString(run.LastError)}, nil
		}
		v.RetryStep = ""
		if err := cd.SetReleaseBlocked(ctx, rel.ID, nil); err != nil {
			return workflows.Result{}, err
		}
		evs = nil
	}
	prs, err := cd.ReleasePRs(ctx, rel.ID)
	if err != nil {
		return workflows.Result{}, err
	}
	if !v.Planned {
		// Merged service PRs in reverse merge order (RB-02); none before the first merge (RB-07).
		for i := len(prs) - 1; i >= 0; i-- {
			if prs[i].Kind == "service" && prs[i].State == "merged" && prs[i].Service != nil {
				v.Services = append(v.Services, *prs[i].Service)
			}
		}
		v.Planned = true
		step = stepRevert
		if len(v.Services) == 0 {
			step = stepFlags
		}
	}
	svcAt := func() (*cycledata.Service, *cycledata.PR, error) {
		if v.Idx >= len(v.Services) {
			return nil, nil, errors.New("no current service")
		}
		svc, err := cd.ServiceByKey(ctx, v.Services[v.Idx])
		if err != nil {
			return nil, nil, err
		}
		return svc, releases.ServicePR(prs, svc.Key), nil
	}
	var effects []workflows.Effect
	for guard := 0; guard < 20; guard++ {
		switch step {
		case stepRevert:
			svc, pr, err := svcAt()
			if err != nil || pr == nil || pr.MergeSHA == nil {
				return block(fmt.Sprintf("the merged PR of %s is unknown", v.Services[v.Idx]), stepRevert)
			}
			if domain.AgentDisabled() {
				return block(fmt.Sprintf("the agent is not connected: revert PR %d of %s by hand", pr.Number, svc.Key), stepRevert)
			}
			if _, err := codegen.StartTask(ctx, tx, codegen.TaskSpec{Type: codegen.TaskRevert, FeatureID: &f.ID, ReleaseID: &rel.ID, ServiceID: svc.ID,
				Initiator: &initiator, Parent: &run.ID, Input: codegen.TaskInput{PRNumber: pr.Number, MergeSHA: *pr.MergeSHA, RevertPRID: pr.ID.String(),
					Release: rel.Key, Reason: v.Reason}}); err != nil {
				return workflows.Result{}, err
			}
			return workflows.Result{State: "reverting", Step: stepRevertWait, Context: v.ctx(), Notify: notify}, nil
		case stepRevertWait:
			if ev, ok := workflows.Find(evs, "task_failed"); ok {
				var p struct {
					Error string `json:"error"`
				}
				_ = ev.Decode(&p)
				return block("the revert PR could not be prepared: "+p.Error, stepRevert)
			}
			ev, ok := workflows.Find(evs, "task_done")
			if !ok {
				return workflows.Result{State: "reverting", Step: step, Context: v.ctx()}, nil
			}
			var p struct {
				Result struct {
					PRNumber int `json:"prNumber"`
				} `json:"result"`
			}
			_ = ev.Decode(&p)
			svc, _, err := svcAt()
			if err != nil {
				return workflows.Result{}, err
			}
			rpr, err := cd.PRByRepoNumber(ctx, svc.Repo, p.Result.PRNumber)
			if err != nil {
				return block(fmt.Sprintf("the revert PR of %s is not tracked", svc.Key), stepRevert)
			}
			v.RevertPRID = rpr.ID.String()
			if err := cd.AddReleasePR(ctx, rel.ID, rpr.ID, 100+v.Idx); err != nil {
				return workflows.Result{}, err
			}
			evs = nil
			step = stepMerge
		case stepMerge:
			id, _ := uuid.Parse(v.RevertPRID)
			effects = append(effects, workflows.Effect{Type: releases.EffectMerge, Key: v.RevertPRID, Payload: releases.MergePayload{PRID: id, UserID: initiator}})
			return workflows.Result{State: "merging_reverts", Step: stepMergeWait, Context: v.ctx(), Effects: effects, Notify: notify}, nil
		case stepMergeWait:
			merged := ""
			for _, ev := range evs {
				var p struct {
					PRID     string `json:"prId"`
					SHA      string `json:"sha"`
					MergeSHA string `json:"mergeSha"`
					Reason   string `json:"reason"`
					Error    string `json:"error"`
				}
				_ = ev.Decode(&p)
				switch ev.Type {
				case "merged", "pr_merged":
					if p.PRID == v.RevertPRID {
						merged = p.SHA + p.MergeSHA
					}
				case "needs_update":
					return block("the revert PR does not merge cleanly: "+p.Reason, stepMerge)
				case "merge_failed":
					return block("the provider refused to merge the revert PR: "+p.Reason, stepMerge)
				case "effect_failed":
					return block("merge of the revert PR failed: "+p.Error, stepMerge)
				}
			}
			if merged == "" {
				return workflows.Result{State: "merging_reverts", Step: step, Context: v.ctx()}, nil
			}
			v.MergeSHA = merged
			evs = nil
			step = stepDeploy
		case stepDeploy:
			svc, _, err := svcAt()
			if err != nil {
				return workflows.Result{}, err
			}
			id, effs, deadline, err := releases.Deploy(ctx, tx, rel, svc, v.MergeSHA, true, now) // RB-03
			if err != nil {
				return workflows.Result{}, err
			}
			v.DeployRunID = id.String()
			effects = append(effects, effs...)
			return workflows.Result{State: "redeploying", Step: stepDeployWait, Context: v.ctx(), Effects: effects, NextRunAt: deadline,
				Notify: append(notify, events.Event{Type: events.FocusChanged, Data: map[string]any{"key": rel.Key}})}, nil
		case stepDeployWait:
			svc, _, err := svcAt()
			if err != nil {
				return workflows.Result{}, err
			}
			id, _ := uuid.Parse(v.DeployRunID)
			outcome, reason, effs, err := releases.WaitDeploy(ctx, tx, id, svc, v.MergeSHA, evs, run.NextRunAt, now)
			if err != nil {
				return workflows.Result{}, err
			}
			effects = append(effects, effs...)
			switch outcome {
			case releases.DeployWaiting:
				return workflows.Result{State: "redeploying", Step: step, Context: v.ctx(), Effects: effects, NextRunAt: run.NextRunAt}, nil
			case releases.DeployFailed:
				res, err := block(reason, stepDeploy)
				res.Effects = effects
				return res, err
			}
			v.Idx++
			v.RevertPRID, v.MergeSHA, v.DeployRunID = "", "", ""
			evs = nil
			step = stepRevert
			if v.Idx >= len(v.Services) {
				step = stepFlags
			}
		case stepFlags:
			need, err := releases.FlagsRequired(ctx, tx, f)
			if err != nil {
				return workflows.Result{}, err
			}
			if need {
				state, _, err := cd.LastFlagState(ctx, *f.FlagKey)
				if err != nil {
					return workflows.Result{}, err
				}
				if state == "on" {
					// RB-04: wait for "off" from the flag service or a manual mark.
					return workflows.Result{State: "disabling_flags", Step: stepFlagsWait, Context: v.ctx(), Effects: effects,
						Notify: append(notify, events.Event{Type: events.FocusChanged, Data: map[string]any{"key": rel.Key}})}, nil
				}
			}
			step = stepClose
		case stepFlagsWait:
			if !workflows.Has(evs, "flag_off") {
				return workflows.Result{State: "disabling_flags", Step: step, Context: v.ctx()}, nil
			}
			evs = nil
			step = stepClose
		case stepClose:
			effects = append(effects, workflows.Effect{Type: releases.EffectClosePRs, Payload: map[string]any{"featureId": f.ID, "userId": initiator}})
			return workflows.Result{State: "closing_spec_pr", Step: stepCloseWait, Context: v.ctx(), Effects: effects, Notify: notify}, nil
		case stepCloseWait:
			if ev, ok := workflows.Find(evs, "effect_failed"); ok {
				var p struct {
					Error string `json:"error"`
				}
				_ = ev.Decode(&p)
				return block("the PRs could not be closed: "+p.Error, stepClose)
			}
			if !workflows.Has(evs, "closed") {
				return workflows.Result{State: "closing_spec_pr", Step: step, Context: v.ctx()}, nil
			}
			if err := Finish(ctx, tx, rel, f, v.Reason, initiator); err != nil {
				return workflows.Result{}, err
			}
			if run.ParentID != nil {
				if err := workflows.Send(ctx, tx, *run.ParentID, "rollback_done", map[string]any{}); err != nil {
					return workflows.Result{}, err
				}
			}
			return workflows.Result{State: "done", Step: "rolled_back", Context: v.ctx(), Notify: []events.Event{
				{Type: events.ReleaseUpdated, Data: map[string]any{"key": rel.Key, "status": "rolled_back"}},
				{Type: events.FeatureUpdated, Data: map[string]any{"uniqueId": f.UniqueID, "phase": domain.PhaseRolledBack}},
				{Type: events.FocusChanged, Data: map[string]any{"key": rel.Key}}}}, nil
		case stepStart:
			step = stepRevert
		default:
			return workflows.Keep(run), nil
		}
	}
	return workflows.Result{}, errors.New("rollback workflow did not settle")
}

// Finish applies the statuses of a finished rollback (RB-06): release and
// feature rolled back, issues back to "new" with a link to the release and a
// new Discovery that takes the reason into account.
func Finish(ctx context.Context, tx pgx.Tx, rel *cycledata.Release, f *specdata.Feature, reason string, by uuid.UUID) error {
	cd := cycledata.New(tx)
	if err := cd.FinishRollback(ctx, rel.ID); err != nil {
		return err
	}
	if err := specdata.NewPGTx(nil, tx).SetPhase(ctx, f.ID, domain.PhaseRolledBack); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE pull_requests SET state = 'closed' WHERE feature_id = $1 AND kind = 'spec'`, f.ID); err != nil {
		return err
	}
	ids, err := cd.FeatureIssueIDs(ctx, f.ID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := cd.ReturnIssueAfterRollback(ctx, id, rel.ID); err != nil {
			return err
		}
		if err := cd.AddActivity(ctx, "issue", id, "returned_after_rollback", &by, false, map[string]any{"release": rel.Key, "reason": reason}); err != nil {
			return err
		}
		if err := discovery.Start(ctx, tx, id, map[string]any{"rolledBackRelease": rel.Key, "rollbackReason": reason}); err != nil {
			return err
		}
	}
	return cd.AddActivity(ctx, "release", rel.ID, "rolled_back", &by, false, map[string]any{"reason": reason})
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
