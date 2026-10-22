package discovery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/issuetopics"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// The instructions of experts to the researcher (FTR.HMR.DISC-0001 R7–R13,
// arch §4.2–4.3): the personal agent of an expert writes one in the topic of
// the issue; a run of Discovery takes every queued instruction of the issue.

// States of an instruction.
const (
	RequestQueued  = "queued"
	RequestRunning = "running"
	RequestDone    = "done"
	RequestFailed  = "failed"
)

// MaxInstructions bounds the text of an instruction.
const MaxInstructions = 4000

// Channel is the channel of Nabu of the messages written in Hammurapi: Nabu
// names it after the service client (NABU_CLIENT_NAME).
func Channel(clientName string) string { return "client:" + clientName }

// EventRequestUpdated tells the web the state of an instruction (tech §3.2).
const EventRequestUpdated = "discovery.request.updated"

// Request is an instruction of an expert to the researcher.
type Request struct {
	ID             uuid.UUID  `json:"id"`
	IssueID        uuid.UUID  `json:"-"`
	IssueKey       string     `json:"issueKey"`
	AuthorID       uuid.UUID  `json:"-"`
	Author         *string    `json:"author"`
	ConversationID uuid.UUID  `json:"-"`
	Instructions   string     `json:"instructions"`
	State          string     `json:"state"`
	Position       *int       `json:"position"`
	RunID          *uuid.UUID `json:"-"`
	Revision       *int       `json:"revision"`
	Error          *string    `json:"error"`
	CreatedAt      time.Time  `json:"createdAt"`
	FinishedAt     *time.Time `json:"finishedAt"`
}

const reqCols = `q.id, q.issue_id, i.key, q.author_id, u.display_name, q.conversation_id, q.instructions, q.state, q.run_id, q.revision,
	q.error, q.created_at, q.finished_at`

const reqFrom = ` FROM discovery_requests q JOIN issues i ON i.id = q.issue_id LEFT JOIN users u ON u.id = q.author_id`

func listRequests(ctx context.Context, q postgres.Querier, sql string, args ...any) ([]Request, error) {
	rows, err := q.Query(ctx, `SELECT `+reqCols+reqFrom+` `+sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var r Request
		if err := rows.Scan(&r.ID, &r.IssueID, &r.IssueKey, &r.AuthorID, &r.Author, &r.ConversationID, &r.Instructions, &r.State, &r.RunID,
			&r.Revision, &r.Error, &r.CreatedAt, &r.FinishedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRequest reads an instruction with its place in the queue.
func GetRequest(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Request, error) {
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
		if err := q.QueryRow(ctx, `SELECT count(*) FROM discovery_requests WHERE issue_id = $1 AND state = 'queued' AND created_at <= $2`,
			r.IssueID, r.CreatedAt).Scan(&n); err != nil {
			return nil, err
		}
		r.Position = &n
	}
	return r, nil
}

// RunRequests are the instructions a run of Discovery took.
func RunRequests(ctx context.Context, q postgres.Querier, runID uuid.UUID) ([]Request, error) {
	return listRequests(ctx, q, `WHERE q.run_id = $1 ORDER BY q.created_at`, runID)
}

// HistoryLimit is how many answered instructions the researcher sees (tech §5.2).
const HistoryLimit = 20

// History lists the answered instructions of the issue, the latest last.
func History(ctx context.Context, q postgres.Querier, issueID uuid.UUID) ([]Request, error) {
	l, err := listRequests(ctx, q, `WHERE q.issue_id = $1 AND q.state = 'done' ORDER BY q.created_at DESC LIMIT $2`, issueID, HistoryLimit)
	for i, j := 0, len(l)-1; i < j; i, j = i+1, j-1 {
		l[i], l[j] = l[j], l[i]
	}
	return l, err
}

// claim gives every queued instruction of the issue to the run.
func claim(ctx context.Context, tx pgx.Tx, issueID, runID uuid.UUID) ([]Request, error) {
	if _, err := tx.Exec(ctx, `UPDATE discovery_requests SET state = 'running', run_id = $2 WHERE issue_id = $1 AND state = 'queued'`,
		issueID, runID); err != nil {
		return nil, err
	}
	l, err := RunRequests(ctx, tx, runID)
	for range l {
		metrics.DiscoveryRequests.WithLabelValues(RequestRunning).Inc()
	}
	return l, err
}

// finish answers the instructions of the run with the revision.
func finish(ctx context.Context, tx pgx.Tx, runID uuid.UUID, revision int) ([]Request, error) {
	if _, err := tx.Exec(ctx, `UPDATE discovery_requests SET state = 'done', revision = $2, error = NULL, finished_at = now()
		WHERE run_id = $1 AND state = 'running'`, runID, revision); err != nil {
		return nil, err
	}
	l, err := RunRequests(ctx, tx, runID)
	for _, r := range l {
		metrics.DiscoveryRequests.WithLabelValues(RequestDone).Inc()
		metrics.DiscoveryRequestDuration.Observe(time.Since(r.CreatedAt).Seconds())
	}
	return l, err
}

// fail marks the instructions of the run failed with the reason.
func fail(ctx context.Context, tx pgx.Tx, runID uuid.UUID, reason string) ([]Request, error) {
	if _, err := tx.Exec(ctx, `UPDATE discovery_requests SET state = 'failed', error = $2, finished_at = now()
		WHERE run_id = $1 AND state = 'running'`, runID, reason); err != nil {
		return nil, err
	}
	l, err := RunRequests(ctx, tx, runID)
	for range l {
		metrics.DiscoveryRequests.WithLabelValues(RequestFailed).Inc()
	}
	return l, err
}

// requeue puts the instructions a run took back into the queue: the run was
// cancelled by a manual edit or is retried (R13, RQ-16).
func requeue(ctx context.Context, q postgres.Querier, issueID uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE discovery_requests q SET state = 'queued', run_id = NULL, error = NULL, finished_at = NULL
		WHERE q.issue_id = $1 AND (q.state = 'running'
			OR q.state = 'failed' AND q.run_id IN (SELECT id FROM workflow_runs WHERE kind = 'discovery' AND subject_id = $1 AND state = 'blocked'))`, issueID)
	return err
}

// cancelRuns cancels the active run of Discovery of the issue.
func cancelRuns(ctx context.Context, q postgres.Querier, issueID uuid.UUID) error {
	_, err := q.Exec(ctx, `UPDATE workflow_runs SET state = 'cancelled', next_run_at = NULL, version = version + 1, updated_at = now()
		WHERE kind = 'discovery' AND subject_id = $1 AND state NOT IN ('done','succeeded','failed','cancelled','rolled_back')`, issueID)
	return err
}

// Drain starts a run for the queued instructions of the issue when no run is
// active (R11): after a run ends, after a manual edit.
func Drain(ctx context.Context, q postgres.Querier, issueID uuid.UUID) error {
	var queued bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM discovery_requests WHERE issue_id = $1 AND state = 'queued')`, issueID).Scan(&queued); err != nil {
		return err
	}
	if !queued {
		return nil
	}
	_, err := startRun(ctx, q, issueID)
	return err
}

// startRun starts a run for the instructions unless one is active; started
// tells which. Inside a transaction the attempt runs in a savepoint: the
// unique violation of an active run must not abort the transaction.
func startRun(ctx context.Context, q postgres.Querier, issueID uuid.UUID) (started bool, err error) {
	start := func(q postgres.Querier) error {
		_, err := workflows.Start(ctx, q, Kind, issueID, nil, "queued", map[string]any{"requests": true})
		return err
	}
	if tx, ok := q.(pgx.Tx); ok {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return false, err
		}
		if err = start(sp); err != nil {
			_ = sp.Rollback(ctx)
		} else {
			err = sp.Commit(ctx)
		}
		if errors.Is(err, workflows.ErrActiveRun) {
			return false, nil
		}
		return err == nil, err
	}
	err = start(q)
	if errors.Is(err, workflows.ErrActiveRun) {
		return false, nil
	}
	return err == nil, err
}

// Blocked reports whether the Discovery of the issue waits for «Retry».
func Blocked(ctx context.Context, q postgres.Querier, issueID uuid.UUID) bool {
	var b bool
	_ = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workflow_runs WHERE kind = 'discovery' AND subject_id = $1 AND state = 'blocked')`, issueID).Scan(&b)
	return b
}

// Restart runs Discovery again (after a failure or to refresh it): the
// instructions of the active or failed run go with the new run (R13, RQ-16).
func Restart(ctx context.Context, q postgres.Querier, issueID uuid.UUID) error {
	if err := requeue(ctx, q, issueID); err != nil {
		return err
	}
	if err := cancelRuns(ctx, q, issueID); err != nil {
		return err
	}
	return Start(ctx, q, issueID, nil)
}

// RequestRevision is the MCP tool request_discovery_revision (R6–R8, tech
// §4.2): the topic and the channel come from the signed JWT of Nabu, never
// from the arguments.
// channel is the channel of Hammurapi in Nabu (Channel of NABU_CLIENT_NAME).
func RequestRevision(ctx context.Context, pool *pgxpool.Pool, channel string, p *domain.Principal, g mcp.Grant, issueKey, instructions string) (*Request, error) {
	var q postgres.Querier = pool
	refuse := func(msg string) (*Request, error) { return nil, &mcp.ToolError{Msg: msg} }
	if g.Channel == "" || g.Channel != channel {
		return refuse("Revisions are requested from the chat of the issue in Hammurapi")
	}
	is, _, err := cycledata.New(q).IssueByKey(ctx, strings.TrimSpace(issueKey))
	if errors.Is(err, cycledata.ErrNotFound) {
		return refuse("No issue " + issueKey)
	}
	if err != nil {
		return nil, err
	}
	conv, err := issuetopics.ConversationOf(ctx, q, p.UserID, is.ID)
	if err != nil {
		return nil, err
	}
	if conv == "" || conv != g.Conversation {
		return refuse("Open the chat of the issue " + is.Key + " in Hammurapi to request a revision")
	}
	if !p.IsExpertOf(is.Domain) {
		return refuse("Only experts of domain " + is.Domain + " request revisions of Discovery")
	}
	switch is.Status {
	case domain.IssueNew, domain.IssueDiscovery, domain.IssueVerification:
	default:
		return refuse(fmt.Sprintf("The issue is %s: Discovery is not revised any more", is.Status))
	}
	instructions = strings.TrimSpace(instructions)
	if instructions == "" || utf8.RuneCountInString(instructions) > MaxInstructions {
		return refuse(fmt.Sprintf("instructions: 1–%d characters", MaxInstructions))
	}
	convID, _ := uuid.Parse(conv)
	var r *Request
	err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO discovery_requests (issue_id, author_id, conversation_id, instructions) VALUES ($1,$2,$3,$4) RETURNING id`,
			is.ID, p.UserID, convID, instructions).Scan(&id); err != nil {
			return err
		}
		started, err := startRun(ctx, tx, is.ID)
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
	metrics.DiscoveryRequests.WithLabelValues(RequestQueued).Inc()
	return r, nil
}

// requestEvents tell the web the new states of the instructions.
func requestEvents(reqs []Request) []events.Event {
	out := make([]events.Event, 0, len(reqs))
	for _, r := range reqs {
		out = append(out, events.Event{Type: EventRequestUpdated, Data: map[string]any{"id": r.ID, "issueKey": r.IssueKey, "state": r.State,
			"revision": r.Revision, "error": r.Error}})
	}
	return out
}
