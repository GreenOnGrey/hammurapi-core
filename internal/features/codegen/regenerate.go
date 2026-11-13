package codegen

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// RegenerateCommentMax bounds the comment of an expert to a regeneration (FTR.HMR.DEV-0004 R18).
const RegenerateCommentMax = 4000

// regenerateComments is how many of the latest comments of the PR the agent gets (R19).
const regenerateComments = 40

// botPrefix starts the comments the agent of Hammurapi leaves on a PR: its own summaries are not feedback.
const botPrefix = "Hammurapi agent"

// Regenerated is the answer of a regeneration: the task and what the PR said to it.
type Regenerated struct {
	TaskID       string `json:"taskId"`
	Service      string `json:"service"`
	PRNumber     int    `json:"prNumber,omitempty"`
	Comments     int    `json:"comments"`
	FailedChecks int    `json:"failedChecks"`
	// FeedbackError: the PR was not read; the agent works without its comments and checks.
	FeedbackError bool `json:"feedbackError,omitempty"`
}

// Regenerate runs the agent of a service again after its last task succeeded (FTR.HMR.DEV-0004 R18–R20):
// on the branch of the service, with the comments and the failed checks of its PR and the comment of
// the expert. From the Implementation tab and from the chat (request_code_regeneration).
func (s *Service) Regenerate(ctx context.Context, p *domain.Principal, key, service, comment string) (*Regenerated, error) {
	f, err := features.Load(ctx, s.store, key)
	if err != nil {
		return nil, err
	}
	if err := features.RequireExpert(p, f.DomainKey); err != nil {
		return nil, err
	}
	comment = strings.TrimSpace(comment)
	if utf8.RuneCountInString(comment) > RegenerateCommentMax {
		return nil, apperr.Unprocessable("comment_too_long", "the comment is longer than 4000 characters")
	}
	if f.Phase != domain.PhaseCodegen && f.Phase != domain.PhaseValidation {
		return nil, apperr.Conflict("regenerate_unavailable", "the code is regenerated only on code generation and validation")
	}
	cd := cycledata.New(s.store.Q())
	svcs, err := cd.FeatureServices(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	var svc *cycledata.Service
	for i := range svcs {
		if svcs[i].Key == service {
			svc = &svcs[i]
		}
	}
	if svc == nil {
		return nil, apperr.NotFound("service_not_found", "the service is not a service of the feature")
	}
	if err := regenerable(ctx, cd, f.ID, svc.ID); err != nil {
		return nil, err
	}
	out := &Regenerated{Service: svc.Key}
	in := TaskInput{Regenerate: true, Comment: comment, Autonomy: string(svc.Autonomy)}
	pr, err := cd.ServicePR(ctx, f.ID, svc.ID)
	if err != nil && !errors.Is(err, cycledata.ErrNotFound) {
		return nil, err // without the number of the PR the agent would open a second one
	}
	if err == nil && pr.State == "open" {
		out.PRNumber, in.PRNumber = pr.Number, pr.Number
		if fb, err := s.feedback(ctx, svc.Repo, pr); err != nil {
			// the agent still works: the requirements and the comment of the expert are there
			slog.WarnContext(ctx, "the feedback of the PR is not read", "repo", svc.Repo, "pr", pr.Number, "err", err)
			out.FeedbackError = true
		} else if fb != nil {
			in.Feedback = fb
			out.Comments, out.FailedChecks = len(fb.Comments), len(fb.Failed)
		}
	}
	err = s.store.InTx(ctx, func(tx specdata.Store) error {
		// the check again inside the transaction: two clicks start one task
		if _, err := tx.Q().Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('hammurapi.regenerate:' || $1::text || ':' || $2::text))`, f.ID, svc.ID); err != nil {
			return err
		}
		if err := regenerable(ctx, cycledata.New(tx.Q()), f.ID, svc.ID); err != nil {
			return err
		}
		// while the code generation of the feature still waits for its tasks, the regeneration is one of them:
		// the feature moves to validation only after it (R20 of FTR.HMR.CMN-0002)
		var parent *uuid.UUID
		if run, err := workflows.LatestRun(ctx, tx.Q(), Kind, f.ID); err != nil {
			return err
		} else if run != nil && (run.State == "tasks_running" || run.State == "blocked") {
			parent = &run.ID
		}
		id, err := StartTask(ctx, tx.Q(), TaskSpec{Type: TaskImplement, FeatureID: &f.ID, ServiceID: svc.ID, Initiator: &p.UserID, Input: in, Parent: parent})
		out.TaskID = id.String()
		return err
	})
	if err != nil {
		return nil, err
	}
	if s.events != nil {
		s.events.Publish(ctx, events.Event{Type: events.FeatureUpdated, Data: map[string]any{"uniqueId": f.UniqueID, "phase": f.Phase}})
	}
	return out, nil
}

// regenerable: the last task of the service succeeded and nothing of it is queued or running.
func regenerable(ctx context.Context, cd *cycledata.DB, featureID, serviceID uuid.UUID) error {
	tasks, err := cd.FeatureTasks(ctx, featureID)
	if err != nil {
		return err
	}
	for _, t := range tasks { // the newest first
		if t.ServiceID != serviceID {
			continue
		}
		switch t.Status {
		case "succeeded":
			return nil
		case "queued", "running":
			return apperr.Conflict("task_running", "a task of the service is running")
		}
		return apperr.Conflict("regenerate_unavailable", "the last task of the service did not succeed: retry it")
	}
	return apperr.Conflict("regenerate_unavailable", "the code of the service was not generated yet")
}

// feedback reads the comments of the PR and the checks of its head that failed (R19).
func (s *Service) feedback(ctx context.Context, repo string, pr *cycledata.PR) (*git.PRFeedback, error) {
	if s.Git == nil {
		return nil, nil
	}
	p := s.Git.ForRepo(repo)
	tok, err := p.BotToken(ctx)
	if err != nil {
		return nil, err
	}
	sha := pr.HeadSHA
	if pr.CIStatus != nil && *pr.CIStatus != git.CIFailure {
		sha = "" // the checks are green or not done: only the comments
	}
	fb, err := p.PRFeedback(ctx, tok, pr.Number, sha)
	if err != nil || fb == nil {
		return fb, err
	}
	// the summaries of the agent itself are not feedback; the latest comments of people and tools are
	var keep []git.PRComment
	for _, c := range fb.Comments {
		if !strings.HasPrefix(c.Body, botPrefix) {
			keep = append(keep, c)
		}
	}
	if len(keep) > regenerateComments {
		keep = keep[len(keep)-regenerateComments:]
	}
	fb.Comments = keep
	return fb, nil
}

// regenerateRoute mounts POST /features/{key}/codegen/services/{service}/regenerate.
func (s *Service) regenerateRoute(r chi.Router) {
	r.Post("/features/{key}/codegen/services/{service}/regenerate", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var body struct {
			Comment string `json:"comment"`
		}
		if err := httpx.Decode(r, &body); err != nil {
			return err
		}
		out, err := s.Regenerate(r.Context(), p, chi.URLParam(r, "key"), chi.URLParam(r, "service"), body.Comment)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusAccepted, out)
		return nil
	}))
}
