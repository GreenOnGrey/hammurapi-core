package gategen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/featuretopics"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// The instructions of experts to the writer (FTR.HMR.DEV-0001 R17, R18): the
// personal agent writes one in the topic of the feature, the answers on the
// page make one too; a run takes every queued instruction of the feature.

// States of an instruction.
const (
	RequestQueued  = "queued"
	RequestRunning = "running"
	RequestDone    = "done"
	RequestFailed  = "failed"
)

// Bounds of an instruction, the same as of the instructions to the researcher.
const (
	MaxInstructions = 4000
	MaxAnswer       = 1000
	MaxBriefAnswers = 10
	HistoryLimit    = 20
)

// Events of the web.
const (
	EventRequestUpdated   = "spec.request.updated"
	EventQuestionsUpdated = "spec.questions.updated"
)

// Answers of a question about skipping a gate: the values are set by the
// server, the model does not decide what an answer does (R6).
const (
	OptionSkip = "Пропустить"
	OptionDo   = "Делать"
)

// SpecRequest is an instruction of an expert to the writer.
type SpecRequest struct {
	ID         uuid.UUID `json:"id"`
	FeatureID  uuid.UUID `json:"-"`
	FeatureKey string    `json:"featureKey"`
	AuthorID   uuid.UUID `json:"-"`
	Author     *string   `json:"author"`
	// ConversationID is the topic of the author; nil for answers given on the
	// page without a topic.
	ConversationID *uuid.UUID         `json:"-"`
	Areas          []string           `json:"areas"`
	SkipAreas      []string           `json:"skipAreas"`
	RestoreAreas   []string           `json:"restoreAreas"`
	Instructions   string             `json:"instructions"`
	Context        *string            `json:"context"`
	Answers        []cycledata.Answer `json:"answers"`
	State          string             `json:"state"`
	Position       *int               `json:"position"`
	// Blocked: the instruction waits for «Retry» of a stopped run (R18).
	Blocked    bool              `json:"blocked"`
	RunID      *uuid.UUID        `json:"-"`
	Commits    map[string]string `json:"commits"`
	Error      *string           `json:"error"`
	CreatedAt  time.Time         `json:"createdAt"`
	FinishedAt *time.Time        `json:"finishedAt"`
}

const reqSelect = `SELECT q.id, q.feature_id, f.unique_id, q.author_id, u.display_name, q.conversation_id, q.areas, q.skip_areas, q.restore_areas,
	q.instructions, q.context, q.answers, q.state, q.run_id, q.commits, q.error, q.created_at, q.finished_at
	FROM spec_requests q JOIN features f ON f.id = q.feature_id LEFT JOIN users u ON u.id = q.author_id `

func listRequests(ctx context.Context, q postgres.Querier, where string, args ...any) ([]SpecRequest, error) {
	rows, err := q.Query(ctx, reqSelect+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SpecRequest
	for rows.Next() {
		var r SpecRequest
		var answers, commits []byte
		if err := rows.Scan(&r.ID, &r.FeatureID, &r.FeatureKey, &r.AuthorID, &r.Author, &r.ConversationID, &r.Areas, &r.SkipAreas, &r.RestoreAreas,
			&r.Instructions, &r.Context, &answers, &r.State, &r.RunID, &commits, &r.Error, &r.CreatedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(answers, &r.Answers)
		if r.Answers == nil {
			r.Answers = []cycledata.Answer{}
		}
		_ = json.Unmarshal(commits, &r.Commits)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRequest reads an instruction with its place in the queue.
func GetRequest(ctx context.Context, q postgres.Querier, id uuid.UUID) (*SpecRequest, error) {
	l, err := listRequests(ctx, q, `WHERE q.id = $1`, id)
	if err != nil {
		return nil, err
	}
	if len(l) == 0 {
		return nil, apperr.NotFound("request_not_found", "instruction not found")
	}
	r := &l[0]
	if r.State == RequestQueued {
		var n int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM spec_requests WHERE feature_id = $1 AND state = 'queued' AND created_at <= $2`,
			r.FeatureID, r.CreatedAt).Scan(&n); err != nil {
			return nil, err
		}
		r.Position = &n
		run, err := workflows.LatestRun(ctx, q, Kind, r.FeatureID)
		if err != nil {
			return nil, err
		}
		r.Blocked = run != nil && run.State == workflows.StateBlocked
	}
	return r, nil
}

// RunRequests are the instructions a run took.
func RunRequests(ctx context.Context, q postgres.Querier, runID uuid.UUID) ([]SpecRequest, error) {
	return listRequests(ctx, q, `WHERE q.run_id = $1 ORDER BY q.created_at`, runID)
}

// History lists the answered instructions of the feature, the latest last.
func History(ctx context.Context, q postgres.Querier, featureID uuid.UUID) ([]SpecRequest, error) {
	l, err := listRequests(ctx, q, `WHERE q.feature_id = $1 AND q.state = 'done' ORDER BY q.created_at DESC LIMIT $2`, featureID, HistoryLimit)
	slices.Reverse(l)
	return l, err
}

func claim(ctx context.Context, tx pgx.Tx, featureID, runID uuid.UUID) ([]SpecRequest, error) {
	if _, err := tx.Exec(ctx, `UPDATE spec_requests SET state = 'running', run_id = $2 WHERE feature_id = $1 AND state = 'queued'`, featureID, runID); err != nil {
		return nil, err
	}
	l, err := RunRequests(ctx, tx, runID)
	for range l {
		metrics.SpecRequests.WithLabelValues(RequestRunning).Inc()
	}
	return l, err
}

func finish(ctx context.Context, tx pgx.Tx, runID uuid.UUID, commits map[domain.Area]string) ([]SpecRequest, error) {
	raw, _ := json.Marshal(commits)
	if commits == nil {
		raw = []byte("{}")
	}
	if _, err := tx.Exec(ctx, `UPDATE spec_requests SET state = 'done', commits = $2, error = NULL, finished_at = now()
		WHERE run_id = $1 AND state = 'running'`, runID, raw); err != nil {
		return nil, err
	}
	l, err := listRequests(ctx, tx, `WHERE q.run_id = $1 AND q.state = 'done' AND q.finished_at = now() ORDER BY q.created_at`, runID)
	for range l {
		metrics.SpecRequests.WithLabelValues(RequestDone).Inc()
	}
	return l, err
}

func fail(ctx context.Context, tx pgx.Tx, runID uuid.UUID, reason string) ([]SpecRequest, error) {
	if _, err := tx.Exec(ctx, `UPDATE spec_requests SET state = 'failed', error = $2, finished_at = now() WHERE run_id = $1 AND state = 'running'`, runID, reason); err != nil {
		return nil, err
	}
	l, err := listRequests(ctx, tx, `WHERE q.run_id = $1 AND q.state = 'failed' AND q.finished_at = now() ORDER BY q.created_at`, runID)
	for range l {
		metrics.SpecRequests.WithLabelValues(RequestFailed).Inc()
	}
	return l, err
}

// failRequests fails the open instructions of a feature that left the phase, without notices.
func failRequests(ctx context.Context, q postgres.Querier, featureID uuid.UUID, reason string) (int64, error) {
	tag, err := q.Exec(ctx, `UPDATE spec_requests SET state = 'failed', error = $2, finished_at = now() WHERE feature_id = $1 AND state IN ('queued','running')`, featureID, reason)
	return tag.RowsAffected(), err
}

// Drain starts a run for the queued instructions when none is active.
func Drain(ctx context.Context, q postgres.Querier, featureID uuid.UUID) error {
	var queued bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM spec_requests WHERE feature_id = $1 AND state = 'queued')`, featureID).Scan(&queued); err != nil {
		return err
	}
	if !queued {
		return nil
	}
	_, err := startForRequests(ctx, q, featureID)
	return err
}

// DrainEffects executes spec.drain.
type DrainEffects struct{ Q postgres.Querier }

// Do starts the next run when instructions wait.
func (e *DrainEffects) Do(ctx context.Context, run workflows.RunRef, _ json.RawMessage) ([]workflows.NewEvent, error) {
	return nil, Drain(ctx, e.Q, run.SubjectID)
}

// startForRequests starts a run unless one is active; inside a transaction the
// attempt runs in a savepoint: the unique violation must not abort it.
func startForRequests(ctx context.Context, q postgres.Querier, featureID uuid.UUID) (bool, error) {
	c := toContext(Request{Mode: ModeWrite})
	start := func(q postgres.Querier) error {
		_, err := workflows.Start(ctx, q, Kind, featureID, nil, "queued", c)
		return err
	}
	var err error
	if tx, ok := q.(pgx.Tx); ok {
		sp, berr := tx.Begin(ctx)
		if berr != nil {
			return false, berr
		}
		if err = start(sp); err != nil {
			_ = sp.Rollback(ctx)
		} else {
			err = sp.Commit(ctx)
		}
	} else {
		err = start(q)
	}
	if errors.Is(err, workflows.ErrActiveRun) {
		return false, nil
	}
	return err == nil, err
}

// newRequest is an instruction to write.
type newRequest struct {
	FeatureID, AuthorID  uuid.UUID
	Conversation         *uuid.UUID
	Areas, Skip, Restore []string
	Instructions         string
	Context              *string
	Answers              []cycledata.Answer
}

func enqueue(ctx context.Context, pool *pgxpool.Pool, nr newRequest) (*SpecRequest, error) {
	data, _ := json.Marshal(nr.Answers)
	empty := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	var r *SpecRequest
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO spec_requests (feature_id, author_id, conversation_id, areas, skip_areas, restore_areas, instructions, context, answers)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`, nr.FeatureID, nr.AuthorID, nr.Conversation, empty(nr.Areas), empty(nr.Skip), empty(nr.Restore),
			nr.Instructions, nr.Context, data).Scan(&id); err != nil {
			return err
		}
		started, err := startForRequests(ctx, tx, nr.FeatureID)
		if err != nil {
			return err
		}
		if r, err = GetRequest(ctx, tx, id); err != nil {
			return err
		}
		if started { // the run of this call takes it at once
			r.State, r.Position = RequestRunning, nil
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	metrics.SpecRequests.WithLabelValues(RequestQueued).Inc()
	return r, nil
}

func requestEvents(reqs []SpecRequest) []events.Event {
	out := make([]events.Event, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, events.Event{Type: EventRequestUpdated, Data: map[string]any{"id": r.ID, "featureKey": r.FeatureKey, "state": r.State, "error": r.Error}})
	}
	return out
}

// RequestRevision is the MCP tool request_spec_revision (R12, R17): the topic
// and the channel come from the signed JWT of Nabu, never from the arguments.
func RequestRevision(ctx context.Context, pool *pgxpool.Pool, store specdata.Store, channel string, p *domain.Principal, g mcp.Grant, b agent.SpecBrief) (*SpecRequest, error) {
	refuse := func(msg string) (*SpecRequest, error) { return nil, &mcp.ToolError{Msg: msg} }
	if g.Channel == "" || g.Channel != channel {
		return refuse("Specification changes are requested from the chat of the feature in Hammurapi")
	}
	f, err := store.FeatureByUniqueID(ctx, strings.TrimSpace(b.FeatureKey))
	if errors.Is(err, specdata.ErrNotFound) {
		return refuse("No feature " + b.FeatureKey)
	}
	if err != nil {
		return nil, err
	}
	if f.Phase != domain.PhaseSpec {
		return refuse(fmt.Sprintf("The feature is in phase %s: its specification is read-only", f.Phase))
	}
	conv, err := featuretopics.ConversationOf(ctx, pool, p.UserID, f.ID)
	if err != nil {
		return nil, err
	}
	if conv == "" || conv != g.Conversation {
		return refuse("Open the Specification tab of " + f.UniqueID + " in Hammurapi to request a change")
	}
	if !p.IsExpertOf(f.DomainKey) {
		return refuse("Only experts of domain " + f.DomainKey + " request changes of a specification")
	}
	instructions, briefContext := strings.TrimSpace(b.Instructions), strings.TrimSpace(b.Context)
	switch {
	case len(b.Areas) == 0 && len(b.Answers) == 0 && len(b.SkipAreas) == 0 && len(b.RestoreAreas) == 0:
		return refuse("areas, answers, skipAreas or restoreAreas are required")
	case utf8.RuneCountInString(instructions) > MaxInstructions:
		return refuse(fmt.Sprintf("instructions: up to %d characters", MaxInstructions))
	case utf8.RuneCountInString(briefContext) > MaxInstructions:
		return refuse(fmt.Sprintf("context: up to %d characters", MaxInstructions))
	case len(b.Answers) > MaxBriefAnswers:
		return refuse(fmt.Sprintf("answers: up to %d", MaxBriefAnswers))
	}
	all, err := store.ActiveGates(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	if msg := checkAreas(f, all, b); msg != "" {
		return refuse(msg)
	}
	nr := newRequest{FeatureID: f.ID, AuthorID: p.UserID, Skip: b.SkipAreas, Restore: b.RestoreAreas, Instructions: instructions}
	for _, a := range b.Areas { // a skipped gate named in areas is brought back (arch §4.6)
		if slices.Contains(f.SkippedAreas, a) {
			nr.Restore = appendNew(nr.Restore, a)
		} else {
			nr.Areas = appendNew(nr.Areas, a)
		}
	}
	for _, x := range b.Answers {
		text, problem := answerText(x.Answer)
		if problem != "" {
			return refuse("answer: " + problem)
		}
		if !x.Source.OfBrief() {
			return refuse("source: user or memory")
		}
		area, err := domain.ParseArea(x.Area)
		if err != nil {
			return refuse("area: " + err.Error())
		}
		qs, err := LoadQuestions(ctx, pool, f.ID, area)
		if err != nil {
			return nil, err
		}
		question, ok := findQuestion(qs.Questions, x.QuestionID)
		if !ok {
			return refuse(unknownQuestion(area, x.QuestionID, qs.Questions))
		}
		if !p.HasExpert(f.DomainKey, audienceOf(area, question)) {
			return refuse(fmt.Sprintf("%s/%s is for the %s experts", area, question.ID, audienceOf(area, question)))
		}
		touches := applyAnswer(&nr, area, question, text, x.AllowTouches)
		nr.Answers = append(nr.Answers, cycledata.Answer{Area: string(area), QuestionID: question.ID, Question: question.Title, Answer: text, Source: x.Source, Touches: touches})
	}
	id, _ := uuid.Parse(conv)
	nr.Conversation = &id
	if briefContext != "" {
		nr.Context = &briefContext
	}
	return enqueue(ctx, pool, nr)
}

// checkAreas tells what is wrong with the gates a brief names, "" — nothing.
func checkAreas(f *specdata.Feature, all []specdata.Gate, b agent.SpecBrief) string {
	for _, a := range append(append(append([]string{}, b.Areas...), b.SkipAreas...), b.RestoreAreas...) {
		if _, err := domain.ParseArea(a); err != nil {
			return "areas: " + err.Error()
		}
	}
	pending := features.PendingGenerated(all)
	stage := slices.Index(stageOrder, stageOf(f))
	for _, a := range b.Areas {
		area := domain.Area(a)
		if area.Generated() && slices.Contains(pending, area) {
			return "tech and qa are written after arch: nothing to change yet"
		}
		// R4: a gate starts in its turn; a feature without stages may get a missing gate (R31)
		if i := slices.Index(stageOrder, a); stage >= 0 && i > stage && !slices.Contains(f.SkippedAreas, a) {
			return a + " has not started yet: the stages go in order"
		}
	}
	for _, a := range append(append([]string{}, b.SkipAreas...), b.RestoreAreas...) {
		if a != string(domain.AreaDesign) && a != string(domain.AreaArch) {
			return "Only design and arch can be skipped"
		}
	}
	for _, a := range b.RestoreAreas {
		if !slices.Contains(f.SkippedAreas, a) {
			return a + " is not skipped"
		}
	}
	return ""
}

// applyAnswer adds to an instruction what an answer changes: its gate; an answer
// about skipping a gate decides it (R6). The gates its question touches change
// only when the expert allowed it (R21): the model names them, a person decides.
// It returns the gates the answer opened.
func applyAnswer(nr *newRequest, area domain.Area, q cycledata.Question, text string, allowTouches bool) []string {
	if q.SkipArea != "" {
		if strings.EqualFold(strings.TrimSpace(text), OptionSkip) {
			nr.Skip = appendNew(nr.Skip, q.SkipArea)
			return nil
		}
		nr.Areas = appendNew(nr.Areas, q.SkipArea)
		return nil
	}
	nr.Areas = appendNew(nr.Areas, string(area))
	if !allowTouches {
		return nil
	}
	for _, t := range q.Touches {
		nr.Areas = appendNew(nr.Areas, t)
	}
	return q.Touches
}

func appendNew(l []string, s string) []string {
	if slices.Contains(l, s) {
		return l
	}
	return append(l, s)
}

func answerText(s string) (string, string) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return s, "required"
	case utf8.RuneCountInString(s) > MaxAnswer:
		return s, fmt.Sprintf("up to %d characters", MaxAnswer)
	}
	return s, ""
}
