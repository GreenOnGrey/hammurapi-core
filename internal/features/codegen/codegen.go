// Package codegen implements code generation (FTR.HMR.CMN-0002 R16–R21): starting
// it when the gates are approved, the codegen workflow that splits the feature
// into runner tasks per service (planning → tasks_running → done · blocked),
// the codegen_task workflow of one runner task, the Implementation tab and
// review comments on agent PRs. Nothing is merged during development.
package codegen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Kind is the codegen workflow kind.
const Kind = "codegen"

// ValidationStarter starts the validation workflow when all PRs are ready (R20).
type ValidationStarter func(ctx context.Context, q postgres.Querier, featureID uuid.UUID) error

// Start starts code generation of a feature: mode "implement" (first run or
// after a return to the specification) or "rework" (return to code, R24).
func Start(ctx context.Context, q postgres.Querier, featureID uuid.UUID, initiator uuid.UUID, mode, comment string) error {
	if domain.AgentDisabled() {
		return apperr.AgentDisabled()
	}
	_, err := workflows.Start(ctx, q, Kind, featureID, nil, "planning", map[string]any{"initiator": initiator.String(), "mode": mode, "comment": comment})
	if errors.Is(err, workflows.ErrActiveRun) {
		return apperr.Conflict("codegen_in_progress", "code generation is already running")
	}
	return err
}

// Machine is the codegen workflow.
type Machine struct{ StartValidation ValidationStarter }

// Kind implements workflows.Machine.
func (Machine) Kind() string { return Kind }

func featureEvent(f *specdata.Feature) events.Event {
	return events.Event{Type: events.FeatureUpdated, Data: map[string]any{"uniqueId": f.UniqueID, "phase": f.Phase}}
}

// Step implements workflows.Machine.
func (m Machine) Step(ctx context.Context, tx pgx.Tx, run *workflows.Run, evs []workflows.Event, now time.Time) (workflows.Result, error) {
	store := specdata.NewPGTx(nil, tx)
	f, err := store.FeatureByID(ctx, run.SubjectID)
	if err != nil {
		return workflows.Result{}, err
	}
	if f.Phase != domain.PhaseCodegen {
		return workflows.Result{State: "cancelled"}, nil
	}
	if res, ok := workflows.Unblock(run, evs, now); ok {
		return res, nil
	}
	cd := cycledata.New(tx)
	switch run.State {
	case "planning":
		svcs, err := cd.FeatureServices(ctx, f.ID)
		if err != nil {
			return workflows.Result{}, err
		}
		if len(svcs) == 0 {
			return workflows.Block(run, "the tech specification names no services of the catalog"), nil
		}
		initiator, _ := uuid.Parse(workflows.Str(run.Context, "initiator"))
		mode := workflows.Str(run.Context, "mode")
		for _, s := range svcs {
			typ := TaskImplement
			in := TaskInput{Autonomy: string(s.Autonomy), Comment: workflows.Str(run.Context, "comment")}
			pr, err := cd.ServicePR(ctx, f.ID, s.ID)
			if err != nil && !errors.Is(err, cycledata.ErrNotFound) {
				return workflows.Result{}, err
			}
			if pr != nil && err == nil && pr.State == "open" {
				in.PRNumber = pr.Number
				if mode == "rework" {
					typ = TaskReview
				}
			}
			ini := initiator
			if _, err := StartTask(ctx, tx, TaskSpec{Type: typ, FeatureID: &f.ID, ServiceID: s.ID, Initiator: &ini, Input: in, Parent: &run.ID}); err != nil {
				return workflows.Result{}, err
			}
		}
		if err := cd.AddActivity(ctx, "feature", f.ID, "codegen_started", &initiator, false, map[string]any{"services": len(svcs), "mode": mode}); err != nil {
			return workflows.Result{}, err
		}
		return workflows.Result{State: "tasks_running", Context: run.Context, Notify: []events.Event{featureEvent(f)}}, nil
	case "tasks_running":
		return m.evaluate(ctx, tx, run, f)
	}
	return workflows.Keep(run), nil
}

// evaluate checks the latest task of every service and the PRs (R20).
func (m Machine) evaluate(ctx context.Context, tx pgx.Tx, run *workflows.Run, f *specdata.Feature) (workflows.Result, error) {
	cd := cycledata.New(tx)
	rows, err := tx.Query(ctx, `SELECT DISTINCT ON (t.service_id) t.service_id, s.key, t.status::text, COALESCE(t.error,'')
		FROM agent_tasks t JOIN workflow_runs r ON r.subject_id = t.id JOIN services s ON s.id = t.service_id
		WHERE r.parent_id = $1 ORDER BY t.service_id, t.created_at DESC`, run.ID)
	if err != nil {
		return workflows.Result{}, err
	}
	type st struct{ key, status, err string }
	var tasks []st
	for rows.Next() {
		var id uuid.UUID
		var x st
		if err := rows.Scan(&id, &x.key, &x.status, &x.err); err != nil {
			rows.Close()
			return workflows.Result{}, err
		}
		tasks = append(tasks, x)
	}
	rows.Close()
	for _, t := range tasks {
		switch t.status {
		case "failed", "cancelled":
			return workflows.Block(run, fmt.Sprintf("the task for %s failed: %s", t.key, t.err)), nil
		case "queued", "running":
			return workflows.Result{State: run.State, Context: run.Context}, nil
		}
	}
	svcs, err := cd.FeatureServices(ctx, f.ID)
	if err != nil {
		return workflows.Result{}, err
	}
	var waiting []string
	for _, s := range svcs {
		pr, err := cd.ServicePR(ctx, f.ID, s.ID)
		if errors.Is(err, cycledata.ErrNotFound) || (err == nil && pr.State != "open") {
			waiting = append(waiting, s.Key) // "Plan" level: a person opens the PR (CG-05)
			continue
		}
		if err != nil {
			return workflows.Result{}, err
		}
	}
	if len(waiting) > 0 {
		c := run.Context
		c["waitingHumanPr"] = waiting
		return workflows.Result{State: run.State, Step: "waiting_human_pr", Context: c, Notify: []events.Event{featureEvent(f)}}, nil
	}
	if err := specdata.NewPGTx(nil, tx).SetPhase(ctx, f.ID, domain.PhaseValidation); err != nil {
		return workflows.Result{}, err
	}
	f.Phase = domain.PhaseValidation
	if err := cd.AddActivity(ctx, "feature", f.ID, "prs_ready", nil, true, nil); err != nil {
		return workflows.Result{}, err
	}
	if m.StartValidation != nil {
		if err := m.StartValidation(ctx, tx, f.ID); err != nil {
			return workflows.Result{}, err
		}
	}
	return workflows.Result{State: "done", Step: "prs_ready", Context: run.Context,
		Notify: []events.Event{featureEvent(f), {Type: events.FocusChanged, Data: map[string]any{"uniqueId": f.UniqueID}}}}, nil
}

// ─── API ─────────────────────────────────────────────────────────────

// Service implements the codegen use cases.
type Service struct {
	store  specdata.Store
	events events.Publisher
}

// NewService creates the service.
func NewService(store specdata.Store, ev events.Publisher) *Service {
	return &Service{store: store, events: ev}
}

// PlanItem is a service in the codegen plan.
type PlanItem struct {
	Service      string          `json:"service"`
	Repo         string          `json:"repo"`
	Autonomy     domain.Autonomy `json:"autonomy"`
	Requirements []string        `json:"requirements"`
}

// Plan is the response of POST /codegen.
type Plan struct {
	Services []PlanItem `json:"services"`
}

func (s *Service) plan(ctx context.Context, cd *cycledata.DB, featureID uuid.UUID) (*Plan, error) {
	svcs, err := cd.FeatureServices(ctx, featureID)
	if err != nil {
		return nil, err
	}
	reqs, err := cd.Requirements(ctx, featureID)
	if err != nil {
		return nil, err
	}
	p := &Plan{Services: []PlanItem{}}
	for _, sv := range svcs {
		it := PlanItem{Service: sv.Key, Repo: sv.Repo, Autonomy: sv.Autonomy, Requirements: []string{}}
		for _, r := range reqs {
			for _, x := range r.Services {
				if x == sv.Key {
					it.Requirements = append(it.Requirements, r.ID)
				}
			}
		}
		p.Services = append(p.Services, it)
	}
	return p, nil
}

// StartCodegen is POST /features/{key}/codegen (R16).
func (s *Service) StartCodegen(ctx context.Context, p *domain.Principal, key string) (*Plan, error) {
	if domain.AgentDisabled() {
		return nil, apperr.AgentDisabled()
	}
	f, err := features.Load(ctx, s.store, key)
	if err != nil {
		return nil, err
	}
	if err := features.RequireExpert(p, f.DomainKey); err != nil {
		return nil, err
	}
	if f.Phase == domain.PhaseCodegen {
		return nil, apperr.Conflict("codegen_in_progress", "code generation is already running")
	}
	if f.Phase != domain.PhaseSpec {
		return nil, apperr.Conflict("feature_read_only", "code generation starts from the specification phase").With("phase", f.Phase)
	}
	gs, err := s.store.ActiveGates(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	if !features.ReadyForCodegen(f, gs) {
		return nil, apperr.Conflict("gates_not_approved", "all gates, including the generated tech and qa, must be approved first").
			With("pending", features.PendingGenerated(gs))
	}
	var plan *Plan
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		cd := cycledata.New(tx.Q())
		var err error
		if plan, err = s.plan(ctx, cd, f.ID); err != nil {
			return err
		}
		if len(plan.Services) == 0 {
			return apperr.Unprocessable("no_services", "the tech specification names no services of the catalog")
		}
		if err := tx.SetPhase(ctx, f.ID, domain.PhaseCodegen); err != nil {
			return err
		}
		if err := tx.DropLock(ctx, f.ID); err != nil {
			return err
		}
		return Start(ctx, tx.Q(), f.ID, p.UserID, "implement", "")
	})
	if err != nil {
		return nil, err
	}
	s.events.Publish(ctx, events.Event{Type: events.FeatureUpdated, Data: map[string]any{"uniqueId": f.UniqueID, "phase": domain.PhaseCodegen}})
	return plan, nil
}

// ServiceImpl is one service of the Implementation tab (R21).
type ServiceImpl struct {
	Service  string           `json:"service"`
	Repo     string           `json:"repo"`
	Autonomy domain.Autonomy  `json:"autonomy"`
	Tasks    []cycledata.Task `json:"tasks"`
	PR       *cycledata.PR    `json:"pr"`
	Plan     *string          `json:"plan"`
}

// Implementation is GET /features/{key}/implementation.
type Implementation struct {
	Phase     domain.FeaturePhase       `json:"phase"`
	Workflow  *WorkflowRef              `json:"workflow"`
	Services  []ServiceImpl             `json:"services"`
	Matrix    []features.RequirementRow `json:"matrix"`
	HumanPRs  []cycledata.PR            `json:"humanPrs"`
	TokensIn  int64                     `json:"tokensIn"`
	TokensOut int64                     `json:"tokensOut"`
	CanStart  bool                      `json:"canStart"`
}

// WorkflowRef is a workflow state.
type WorkflowRef struct {
	State     string  `json:"state"`
	Step      string  `json:"step"`
	LastError *string `json:"lastError"`
}

// Implementation builds the Implementation tab.
func (s *Service) Implementation(ctx context.Context, p *domain.Principal, key string) (*Implementation, error) {
	f, err := features.Load(ctx, s.store, key)
	if err != nil {
		return nil, err
	}
	cd := cycledata.New(s.store.Q())
	out := &Implementation{Phase: f.Phase, Services: []ServiceImpl{}, HumanPRs: []cycledata.PR{}}
	if run, err := workflows.LatestRun(ctx, s.store.Q(), Kind, f.ID); err != nil {
		return nil, err
	} else if run != nil {
		out.Workflow = &WorkflowRef{State: run.State, Step: run.Step, LastError: run.LastError}
	}
	svcs, err := cd.FeatureServices(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	tasks, err := cd.FeatureTasks(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	prs, err := cd.FeaturePRs(ctx, f.ID, "service")
	if err != nil {
		return nil, err
	}
	for _, sv := range svcs {
		si := ServiceImpl{Service: sv.Key, Repo: sv.Repo, Autonomy: sv.Autonomy, Tasks: []cycledata.Task{}}
		for _, t := range tasks {
			if t.ServiceID == sv.ID {
				t.Input = nil // internal
				si.Tasks = append(si.Tasks, t)
				if si.Plan == nil && len(t.Result) > 0 {
					var r struct {
						Plan string `json:"plan"`
					}
					if json.Unmarshal(t.Result, &r) == nil && r.Plan != "" {
						si.Plan = &r.Plan
					}
				}
			}
		}
		for i := range prs {
			if prs[i].ServiceID != nil && *prs[i].ServiceID == sv.ID && prs[i].State != "closed" {
				pr := prs[i]
				si.PR = &pr
				break
			}
		}
		out.Services = append(out.Services, si)
	}
	for _, pr := range prs {
		if !pr.ByAgent {
			out.HumanPRs = append(out.HumanPRs, pr)
		}
	}
	if out.Matrix, err = features.Matrix(ctx, cd, f.ID); err != nil {
		return nil, err
	}
	if out.TokensIn, out.TokensOut, err = cd.UsageTotals(ctx, "feature_id", f.ID); err != nil {
		return nil, err
	}
	var tin, tout int64
	for _, t := range tasks {
		tin += t.TokensIn
		tout += t.TokensOut
	}
	out.TokensIn += tin
	out.TokensOut += tout
	if f.Phase == domain.PhaseSpec && p.IsExpertOf(f.DomainKey) {
		gs, err := s.store.ActiveGates(ctx, f.ID)
		if err != nil {
			return nil, err
		}
		out.CanStart = features.ReadyForCodegen(f, gs)
	}
	return out, nil
}

// RetryTask re-runs a failed task (POST …/codegen/tasks/{taskId}/retry).
func (s *Service) RetryTask(ctx context.Context, p *domain.Principal, key string, taskID uuid.UUID) error {
	f, err := features.Load(ctx, s.store, key)
	if err != nil {
		return err
	}
	if err := features.RequireExpert(p, f.DomainKey); err != nil {
		return err
	}
	return s.store.InTx(ctx, func(tx specdata.Store) error {
		cd := cycledata.New(tx.Q())
		t, err := cd.TaskByID(ctx, taskID)
		if errors.Is(err, cycledata.ErrNotFound) || (err == nil && (t.FeatureID == nil || *t.FeatureID != f.ID)) {
			return apperr.NotFound("task_not_found", "task not found")
		}
		if err != nil {
			return err
		}
		if t.Status != "failed" && t.Status != "cancelled" {
			return apperr.Conflict("task_not_failed", "only a failed task can be retried")
		}
		run, err := workflows.Load(ctx, tx.Q(), t.RunID)
		if err != nil {
			return err
		}
		var in TaskInput
		_ = json.Unmarshal(t.Input, &in)
		if _, err := StartTask(ctx, tx.Q(), TaskSpec{Type: t.Type, FeatureID: t.FeatureID, ReleaseID: t.ReleaseID, ServiceID: t.ServiceID,
			Initiator: &p.UserID, Input: in, Parent: run.ParentID}); err != nil {
			return err
		}
		if run.ParentID != nil {
			return workflows.Send(ctx, tx.Q(), *run.ParentID, "retry", map[string]any{"taskId": taskID})
		}
		return nil
	})
}

// ReviewComment starts an address_review task for a review comment on an
// agent PR (R18), unless one is already queued for the PR.
func (s *Service) ReviewComment(ctx context.Context, pr *cycledata.PR, body, author string) error {
	if pr.ServiceID == nil || domain.AgentDisabled() {
		return nil
	}
	return s.store.InTx(ctx, func(tx specdata.Store) error {
		f, err := tx.FeatureByID(ctx, pr.FeatureID)
		if err != nil {
			return err
		}
		if f.Phase != domain.PhaseCodegen && f.Phase != domain.PhaseValidation {
			return nil
		}
		var n int
		if err := tx.Q().QueryRow(ctx, `SELECT count(*) FROM agent_tasks WHERE feature_id = $1 AND service_id = $2 AND type = 'address_review'
			AND status IN ('queued','running')`, f.ID, *pr.ServiceID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		initiator, _ := cycledata.New(tx.Q()).UserByUsername(ctx, author)
		_, err = StartTask(ctx, tx.Q(), TaskSpec{Type: TaskReview, FeatureID: &f.ID, ServiceID: *pr.ServiceID, Initiator: initiator,
			Input: TaskInput{PRNumber: pr.Number, Comment: body, ReviewAuthor: author}})
		return err
	})
}

// Routes mounts the codegen endpoints.
func (s *Service) Routes(r chi.Router) {
	r.Post("/features/{key}/codegen", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		plan, err := s.StartCodegen(r.Context(), p, chi.URLParam(r, "key"))
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusAccepted, plan)
		return nil
	}))
	r.Get("/features/{key}/implementation", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		out, err := s.Implementation(r.Context(), p, chi.URLParam(r, "key"))
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, out)
		return nil
	}))
	r.Post("/features/{key}/codegen/tasks/{taskId}/retry", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		id, err := httpx.ParamUUID(r, "taskId")
		if err != nil {
			return err
		}
		if err := s.RetryTask(r.Context(), p, chi.URLParam(r, "key"), id); err != nil {
			return err
		}
		w.WriteHeader(http.StatusAccepted)
		return nil
	}))
}
