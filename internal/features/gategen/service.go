package gategen

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/featuretopics"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specchanges"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// The endpoints of the writer: answers on the page, the next stage, retry.

// AnswersInput is the body of POST /features/{uniqueId}/spec/answers.
type AnswersInput struct {
	Area    string        `json:"area"`
	Answers []AnswerInput `json:"answers"`
	Comment string        `json:"comment"`
}

// AnswerInput is the choice of an expert for an open question.
type AnswerInput struct {
	QuestionID string `json:"questionId"`
	Choice     string `json:"choice"`
	Option     *int   `json:"option"`
	Text       string `json:"text"`
	// AllowTouches: the expert allows the gates the question touches to change (R21).
	AllowTouches bool `json:"allowTouches"`
}

// Service serves the specification endpoints of the writer.
type Service struct {
	Pool   *pgxpool.Pool
	Store  specdata.Store
	Events events.Publisher
	// Changes and Tokens align the notes of the product document before the next stage (FTR.HMR.DEV-0002 R6); nil — none.
	Changes *specchanges.Service
	Tokens  git.TokenSource
}

// AnswerQuestions gives the answers of an expert on the page of the feature to
// the writer without the personal agent (R11).
func (s *Service) AnswerQuestions(ctx context.Context, p *domain.Principal, key string, in AnswersInput) (*SpecRequest, error) {
	f, err := features.Load(ctx, s.Store, key)
	if err != nil {
		return nil, err
	}
	if err := features.RequireExpert(p, f.DomainKey); err != nil {
		return nil, err
	}
	if err := features.RequireSpecPhase(f); err != nil {
		return nil, err
	}
	area, err := domain.ParseArea(in.Area)
	if err != nil {
		return nil, apperr.Unprocessable("invalid_answer", err.Error()).With("field", "area")
	}
	invalid := func(i int, field, msg string) error {
		return apperr.Unprocessable("invalid_answer", msg).With("field", fmt.Sprintf("answers[%d].%s", i, field))
	}
	if len(in.Answers) == 0 {
		return nil, apperr.Unprocessable("invalid_answer", "at least one answer is required").With("field", "answers")
	}
	comment := strings.TrimSpace(in.Comment)
	if utf8.RuneCountInString(comment) > MaxInstructions {
		return nil, apperr.Unprocessable("invalid_answer", fmt.Sprintf("comment: up to %d characters", MaxInstructions)).With("field", "comment")
	}
	qs, err := LoadQuestions(ctx, s.Pool, f.ID, area)
	if err != nil {
		return nil, err
	}
	nr := newRequest{FeatureID: f.ID, AuthorID: p.UserID, Instructions: comment}
	seen := map[string]bool{}
	for i, a := range in.Answers {
		question, ok := findQuestion(qs.Questions, a.QuestionID)
		if !ok {
			return nil, apperr.Unprocessable("unknown_question", unknownQuestion(area, a.QuestionID, qs.Questions)).With("field", fmt.Sprintf("answers[%d].questionId", i))
		}
		if seen[question.ID] {
			return nil, invalid(i, "questionId", "a question is answered once")
		}
		seen[question.ID] = true
		kind := audienceOf(area, question)
		if !p.HasExpert(f.DomainKey, kind) {
			return nil, apperr.Forbidden("question_other_expert", "the question is for other experts").With("kind", kind).With("questionId", question.ID)
		}
		var text string
		switch a.Choice {
		case "recommended":
			text = question.Recommended
		case "option":
			if a.Option == nil || *a.Option < 0 || *a.Option >= len(question.Options) {
				return nil, invalid(i, "option", "no such option")
			}
			text = question.Options[*a.Option]
		case "own":
			var problem string
			if text, problem = answerText(a.Text); problem != "" {
				return nil, invalid(i, "text", problem)
			}
		default:
			return nil, invalid(i, "choice", "recommended, option or own")
		}
		touches := applyAnswer(&nr, area, question, text, a.AllowTouches)
		nr.Answers = append(nr.Answers, cycledata.Answer{Area: string(area), QuestionID: question.ID, Question: question.Title, Answer: text, Source: cycledata.AnswerExpert, Touches: touches})
	}
	if c, err := featuretopics.ConversationOf(ctx, s.Pool, p.UserID, f.ID); err != nil {
		return nil, err
	} else if id, err := uuid.Parse(c); err == nil {
		nr.Conversation = &id
	}
	return enqueue(ctx, s.Pool, nr)
}

// legacyStage is the stage a feature without stages has reached: its last
// written gate, "" — none.
func legacyStage(all []specdata.Gate) string {
	for _, a := range []domain.Area{domain.AreaArch, domain.AreaDesign, domain.AreaProduct} {
		if slices.ContainsFunc(all, func(g specdata.Gate) bool { return g.Area == a }) {
			return string(a)
		}
	}
	return ""
}

// Advance is «Next stage» in a domain without approval (R7).
func (s *Service) Advance(ctx context.Context, p *domain.Principal, key string) (string, error) {
	f, err := features.Load(ctx, s.Store, key)
	if err != nil {
		return "", err
	}
	if err := features.RequireExpert(p, f.DomainKey); err != nil {
		return "", err
	}
	if err := features.RequireSpecPhase(f); err != nil {
		return "", err
	}
	if f.ApprovalRequired {
		return "", apperr.Conflict("approval_enabled", "the next stage starts when the gate is approved")
	}
	gen := Generator{Q: s.Pool}
	if run, err := gen.Active(ctx, f.ID); err != nil {
		return "", err
	} else if run != nil {
		return "", features.ErrSpecRunActive
	}
	latest, err := workflows.LatestRun(ctx, s.Pool, Kind, f.ID)
	if err != nil {
		return "", err
	}
	if latest != nil && latest.State == workflows.StateBlocked { // R19: a failed run is repeated, not skipped
		return "", apperr.Conflict("writer_blocked", "the writer stopped: retry it first")
	}
	all, err := s.Store.ActiveGates(ctx, f.ID)
	if err != nil {
		return "", err
	}
	// FTR.HMR.DEV-0002 R3, R6, FTR.HMR.DEV-0003 R14: the notes are aligned before any next stage, the legacy one too
	for _, name := range specchanges.Areas {
		a := domain.Area(name)
		if s.Changes == nil || !slices.ContainsFunc(all, func(g specdata.Gate) bool { return g.Area == a }) {
			continue
		}
		token, err := s.Tokens.Token(ctx, p.UserID)
		if err != nil {
			return "", err
		}
		if err := s.Changes.Ensure(ctx, token, f, string(a)); err != nil {
			return "", auth.MapGitError(err)
		}
	}
	if f.SpecFlow != specdata.FlowWriter {
		stage := legacyStage(all)
		if stage == string(domain.AreaArch) { // R31: only tech and qa are left
			if len(features.PendingGenerated(all)) == 0 {
				return "", apperr.Conflict("no_next_stage", "the specification has no next stage")
			}
			return specdata.StageGenerated, gen.Generate(ctx, f, []domain.Area{domain.AreaTech, domain.AreaQA}, p.UserID)
		}
		if stage == "" {
			return "", apperr.Conflict("gate_not_written", "the gate of the stage is not written: answer the question of the writer or retry")
		}
		if err := gen.adoptStages(ctx, f, stage); err != nil { // R31: the button is the decision of an expert
			return "", err
		}
	}
	stage := stageOf(f)
	if stage == specdata.StageGenerated {
		return "", apperr.Conflict("no_next_stage", "the specification has no next stage")
	}
	// R6: only an answer of an expert skips a gate
	if !slices.ContainsFunc(all, func(g specdata.Gate) bool { return string(g.Area) == stage }) {
		return "", apperr.Conflict("gate_not_written", "the gate of the stage is not written: answer the question of the writer or retry")
	}
	if err := gen.advance(ctx, f, p.UserID); err != nil {
		return "", err
	}
	s.publish(ctx, f.UniqueID)
	return *f.SpecStage, nil
}

// Retry runs a blocked run again: its failed instructions go with the new one (R19).
func (s *Service) Retry(ctx context.Context, p *domain.Principal, key string) error {
	f, err := features.Load(ctx, s.Store, key)
	if err != nil {
		return err
	}
	if err := features.RequireExpert(p, f.DomainKey); err != nil {
		return err
	}
	if err := features.RequireSpecPhase(f); err != nil {
		return err
	}
	run, err := workflows.LatestRun(ctx, s.Pool, Kind, f.ID)
	if err != nil {
		return err
	}
	if run == nil || run.State != workflows.StateBlocked {
		return apperr.Conflict("not_blocked", "the writer is not blocked")
	}
	err = postgres.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE spec_requests SET state = 'queued', run_id = NULL, error = NULL, finished_at = NULL WHERE run_id = $1 AND state IN ('failed','running')`, run.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET state = 'cancelled', next_run_at = NULL, version = version + 1, updated_at = now() WHERE id = $1`, run.ID); err != nil {
			return err
		}
		req := request(run.Context)
		c := contextWith(Request{Mode: req.mode(), Areas: req.Areas, Remove: req.Remove, Initiator: p.UserID, Comment: req.Comment}, pendingOf(run.Context))
		_, err := workflows.Start(ctx, tx, Kind, f.ID, nil, "queued", c)
		return err
	})
	if err != nil {
		return err
	}
	s.publish(ctx, f.UniqueID)
	return nil
}

func (s *Service) publish(ctx context.Context, key string) {
	if s.Events != nil {
		s.Events.Publish(ctx, events.Event{Type: events.FeatureUpdated, Data: map[string]any{"uniqueId": key}})
	}
}

// Routes mounts the endpoints.
func (s *Service) Routes(r chi.Router) {
	r.Get("/features/{uniqueId}/spec/questions", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		f, err := features.Load(r.Context(), s.Store, chi.URLParam(r, "uniqueId"))
		if err != nil {
			return err
		}
		items, err := QuestionsView(r.Context(), s.Pool, p, f)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, map[string]any{"items": items})
		return nil
	}))
	r.Post("/features/{uniqueId}/spec/answers", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in AnswersInput
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		out, err := s.AnswerQuestions(r.Context(), p, chi.URLParam(r, "uniqueId"), in)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusCreated, map[string]any{"requestId": out.ID, "state": out.State, "position": out.Position, "blocked": out.Blocked})
		return nil
	}))
	r.Post("/features/{uniqueId}/spec/advance", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		stage, err := s.Advance(r.Context(), p, chi.URLParam(r, "uniqueId"))
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusAccepted, map[string]any{"stage": stage})
		return nil
	}))
	r.Post("/features/{uniqueId}/spec/retry", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := s.Retry(r.Context(), p, chi.URLParam(r, "uniqueId")); err != nil {
			return err
		}
		w.WriteHeader(http.StatusAccepted)
		return nil
	}))
	r.Get("/spec-requests/{id}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		id, err := httpx.ParamUUID(r, "id")
		if err != nil {
			return err
		}
		out, err := GetRequest(r.Context(), s.Pool, id)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, out)
		return nil
	}))
}
