package cycledata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// NextIssueNumber increments the domain counter; the row stays locked until the transaction ends.
func (d *DB) NextIssueNumber(ctx context.Context, domainID uuid.UUID) (int, error) {
	var n int
	err := d.q.QueryRow(ctx, `UPDATE domains SET last_issue_number = last_issue_number + 1 WHERE id = $1 RETURNING last_issue_number`, domainID).Scan(&n)
	return n, nf(err)
}

// DomainID resolves a domain key.
func (d *DB) DomainID(ctx context.Context, key string) (uuid.UUID, bool, error) {
	var id uuid.UUID
	var approval bool
	err := d.q.QueryRow(ctx, `SELECT id, approval_required FROM domains WHERE key = $1`, key).Scan(&id, &approval)
	return id, approval, nf(err)
}

// InsertIssue creates an issue.
func (d *DB) InsertIssue(ctx context.Context, is *Issue) error {
	if len(is.SourceRef) == 0 {
		is.SourceRef = json.RawMessage("null")
	}
	return d.q.QueryRow(ctx, `INSERT INTO issues (key, domain_id, number, type, title, description, source, source_ref, status, author_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id, created_at, updated_at`,
		is.Key, is.DomainID, is.Number, is.Type, is.Title, is.Description, is.Source, is.SourceRef, is.Status, is.AuthorID).
		Scan(&is.ID, &is.CreatedAt, &is.UpdatedAt)
}

const issueCols = `i.id, i.key, i.domain_id, d.key, i.number, i.type, i.title, i.description, i.source::text, i.source_ref,
	i.status, i.author_id, u.display_name, m.key, i.reject_reason, i.rolled_back_release_id, r.key, i.created_at, i.updated_at`

const issueFrom = ` FROM issues i JOIN domains d ON d.id = i.domain_id LEFT JOIN users u ON u.id = i.author_id
	LEFT JOIN issues m ON m.id = i.merged_into_id LEFT JOIN releases r ON r.id = i.rolled_back_release_id`

func scanIssue(row interface{ Scan(...any) error }) (*Issue, error) {
	var is Issue
	err := row.Scan(&is.ID, &is.Key, &is.DomainID, &is.Domain, &is.Number, &is.Type, &is.Title, &is.Description, &is.Source,
		&is.SourceRef, &is.Status, &is.AuthorID, &is.Author, &is.MergedInto, &is.RejectReason, &is.RolledBackReleaseID,
		&is.RolledBackRelease, &is.CreatedAt, &is.UpdatedAt)
	return &is, err
}

// IssueByKey finds an issue by its key or a former key (after a move). moved is
// true when key is an alias.
func (d *DB) IssueByKey(ctx context.Context, key string) (is *Issue, moved bool, err error) {
	is, err = scanIssue(d.q.QueryRow(ctx, `SELECT `+issueCols+issueFrom+` WHERE i.key = $1`, key))
	if err == nil {
		return is, false, nil
	}
	if !postgres.IsNoRows(err) {
		return nil, false, err
	}
	is, err = scanIssue(d.q.QueryRow(ctx, `SELECT `+issueCols+issueFrom+` JOIN issue_aliases a ON a.issue_id = i.id WHERE a.alias_key = $1`, key))
	if err != nil {
		return nil, false, nf(err)
	}
	return is, true, nil
}

// IssueByID loads an issue.
func (d *DB) IssueByID(ctx context.Context, id uuid.UUID) (*Issue, error) {
	is, err := scanIssue(d.q.QueryRow(ctx, `SELECT `+issueCols+issueFrom+` WHERE i.id = $1`, id))
	return is, nf(err)
}

// SetIssueStatus changes the status.
func (d *DB) SetIssueStatus(ctx context.Context, id uuid.UUID, st domain.IssueStatus) error {
	_, err := d.q.Exec(ctx, `UPDATE issues SET status = $2, updated_at = now() WHERE id = $1`, id, st)
	return err
}

// RejectIssue marks an issue rejected.
func (d *DB) RejectIssue(ctx context.Context, id uuid.UUID, reason string) error {
	_, err := d.q.Exec(ctx, `UPDATE issues SET status = 'rejected', reject_reason = $2, updated_at = now() WHERE id = $1`, id, reason)
	return err
}

// ReopenIssue returns a rejected issue to "new".
func (d *DB) ReopenIssue(ctx context.Context, id uuid.UUID) error {
	_, err := d.q.Exec(ctx, `UPDATE issues SET status = 'new', reject_reason = NULL, updated_at = now() WHERE id = $1`, id)
	return err
}

// MergeIssue marks dup as merged into main.
func (d *DB) MergeIssue(ctx context.Context, dup, main uuid.UUID) error {
	_, err := d.q.Exec(ctx, `UPDATE issues SET status = 'merged', merged_into_id = $2, updated_at = now() WHERE id = $1`, dup, main)
	return err
}

// ReturnIssueAfterRollback sets the issue back to "new" with a link to the release.
func (d *DB) ReturnIssueAfterRollback(ctx context.Context, id, releaseID uuid.UUID) error {
	_, err := d.q.Exec(ctx, `UPDATE issues SET status = 'new', rolled_back_release_id = $2, updated_at = now() WHERE id = $1`, id, releaseID)
	return err
}

// MoveIssue gives the issue a new key in another domain and keeps the old key as an alias.
func (d *DB) MoveIssue(ctx context.Context, is *Issue, domainID uuid.UUID, newKey string, number int) error {
	if _, err := d.q.Exec(ctx, `INSERT INTO issue_aliases (alias_key, issue_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, is.Key, is.ID); err != nil {
		return err
	}
	_, err := d.q.Exec(ctx, `UPDATE issues SET key = $2, domain_id = $3, number = $4, updated_at = now() WHERE id = $1`, is.ID, newKey, domainID, number)
	return err
}

// IssueAliases lists former keys.
func (d *DB) IssueAliases(ctx context.Context, id uuid.UUID) ([]string, error) {
	return d.strings(ctx, `SELECT alias_key FROM issue_aliases WHERE issue_id = $1 ORDER BY alias_key`, id)
}

// IssueFeatureKeys lists features created from (or linked to) the issue.
func (d *DB) IssueFeatureKeys(ctx context.Context, id uuid.UUID) ([]string, error) {
	return d.strings(ctx, `SELECT f.unique_id FROM feature_issues fi JOIN features f ON f.id = fi.feature_id
		WHERE fi.issue_id = $1 AND f.phase <> 'deleted' ORDER BY f.created_at`, id)
}

// IssueReleaseKeys lists releases of the issue's features.
func (d *DB) IssueReleaseKeys(ctx context.Context, id uuid.UUID) ([]string, error) {
	return d.strings(ctx, `SELECT r.key FROM feature_issues fi JOIN releases r ON r.feature_id = fi.feature_id
		WHERE fi.issue_id = $1 ORDER BY r.created_at`, id)
}

// FeatureIssueIDs lists issue ids of a feature.
func (d *DB) FeatureIssueIDs(ctx context.Context, featureID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.q.Query(ctx, `SELECT issue_id FROM feature_issues WHERE feature_id = $1`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// LinkIssueAttachments attaches uploaded files to an issue.
func (d *DB) LinkIssueAttachments(ctx context.Context, issueID, userID uuid.UUID, ids []uuid.UUID) error {
	for _, a := range ids {
		tag, err := d.q.Exec(ctx, `INSERT INTO issue_attachments (issue_id, attachment_id)
			SELECT $1, id FROM attachments WHERE id = $2 AND user_id = $3 ON CONFLICT DO NOTHING`, issueID, a, userID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("attachment %s not found", a)
		}
	}
	return nil
}

// IssueAttachment is a file of an issue.
type IssueAttachment struct {
	ID       uuid.UUID `json:"id"`
	FileName string    `json:"fileName"`
	MimeType string    `json:"mimeType"`
}

// IssueAttachments lists files of an issue.
func (d *DB) IssueAttachments(ctx context.Context, issueID uuid.UUID) ([]IssueAttachment, error) {
	rows, err := d.q.Query(ctx, `SELECT a.id, a.file_name, a.mime_type FROM issue_attachments ia JOIN attachments a ON a.id = ia.attachment_id
		WHERE ia.issue_id = $1 ORDER BY a.created_at`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IssueAttachment{}
	for rows.Next() {
		var a IssueAttachment
		if err := rows.Scan(&a.ID, &a.FileName, &a.MimeType); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// IssueFilter filters the Research list.
type IssueFilter struct {
	UserID uuid.UUID
	Domain string // mine | all | KEY
	Type   string
	Status string // open | accepted | resolved | closed | all | a concrete status
	Source string
	Query  string
	Page   httpx.Page
	// Sort orders by the estimate (FTR.HMR.DISC-0001 R17): agentUsd,
	// personHours or days, "-" first for the descending order; issues without
	// an estimate are last both ways. A sorted list has no cursor.
	Sort string
}

// IssueSorts are the orders of the Research list by the estimate.
var IssueSorts = map[string]string{
	"agentUsd":    `(SELECT sum((a->>'usd')::numeric) FROM jsonb_array_elements(dd.estimate->'agent') a)`,
	"personHours": `(SELECT sum((p->>'personHours')::numeric) FROM jsonb_array_elements(dd.estimate->'people') p)`,
	"days":        `(dd.estimate->>'days')::int`,
}

// ListIssues lists issues, newest first.
func (d *DB) ListIssues(ctx context.Context, f IssueFilter) ([]Issue, error) {
	where := []string{"true"}
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	switch f.Domain {
	case "", "all":
	case "mine":
		where = append(where, "i.domain_id IN (SELECT domain_id FROM user_domains WHERE user_id = "+arg(f.UserID)+")")
	default:
		where = append(where, "d.key = "+arg(f.Domain))
	}
	if f.Type == "idea" || f.Type == "problem" {
		where = append(where, "i.type = "+arg(f.Type)+"::issue_type")
	}
	switch f.Status {
	case "", "open":
		where = append(where, "i.status IN ('new','discovery','verification')")
	case "accepted":
		where = append(where, "i.status = 'accepted'")
	case "resolved":
		where = append(where, "i.status = 'resolved'")
	case "closed":
		where = append(where, "i.status IN ('rejected','merged')")
	case "all":
	default:
		where = append(where, "i.status::text = "+arg(f.Status))
	}
	if f.Source != "" {
		where = append(where, "i.source::text = "+arg(f.Source))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		p := arg("%" + q + "%")
		where = append(where, "(i.title ILIKE "+p+" OR i.key ILIKE "+p+")")
	}
	order := `i.created_at DESC, i.id::text DESC`
	if f.Sort != "" {
		expr, ok := IssueSorts[strings.TrimPrefix(f.Sort, "-")]
		if !ok {
			return nil, ErrInvalidSort
		}
		dir := "ASC"
		if strings.HasPrefix(f.Sort, "-") {
			dir = "DESC"
		}
		order = expr + " " + dir + " NULLS LAST, " + order
	} else if c := f.Page.Cursor; c != nil {
		where = append(where, "(i.created_at, i.id::text) < ("+arg(c.T)+", "+arg(c.ID)+")")
	}
	limit := f.Page.Limit
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.q.Query(ctx, `SELECT `+issueCols+`, dd.estimate`+issueFrom+` LEFT JOIN discovery_docs dd ON dd.issue_id = i.id WHERE `+
		strings.Join(where, " AND ")+` ORDER BY `+order+` LIMIT `+arg(limit+1), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Issue
	for rows.Next() {
		var is Issue
		var estimate []byte
		if err := rows.Scan(&is.ID, &is.Key, &is.DomainID, &is.Domain, &is.Number, &is.Type, &is.Title, &is.Description, &is.Source,
			&is.SourceRef, &is.Status, &is.AuthorID, &is.Author, &is.MergedInto, &is.RejectReason, &is.RolledBackReleaseID,
			&is.RolledBackRelease, &is.CreatedAt, &is.UpdatedAt, &estimate); err != nil {
			return nil, err
		}
		is.Estimate = decodeEstimate(estimate).Totals()
		out = append(out, is)
	}
	return out, rows.Err()
}

// ErrInvalidSort is an unknown order of the Research list (ES-14).
var ErrInvalidSort = errors.New("invalid sort")

// ─── Discovery ───────────────────────────────────────────────────────

// Discovery loads the Discovery document (nil if not written yet).
func (d *DB) Discovery(ctx context.Context, issueID uuid.UUID) (*Discovery, error) {
	var doc Discovery
	var measure, similar, systems, services, estimate []byte
	err := d.q.QueryRow(ctx, `SELECT dd.issue_id, dd.content, dd.value_text, dd.measure, dd.measure_checked_at, dd.similar_items, dd.systems,
		dd.services, f.unique_id, dd.revision, dd.updated_at, dd.estimate, dd.change_summary
		FROM discovery_docs dd LEFT JOIN features f ON f.id = dd.problem_feature_id WHERE dd.issue_id = $1`, issueID).
		Scan(&doc.IssueID, &doc.Content, &doc.Value, &measure, &doc.MeasureCheckedAt, &similar, &systems, &services,
			&doc.ProblemFeature, &doc.Revision, &doc.UpdatedAt, &estimate, &doc.ChangeSummary)
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(measure) > 0 && string(measure) != "null" {
		doc.Measure = &Measure{}
		_ = json.Unmarshal(measure, doc.Measure)
	}
	doc.Estimate = decodeEstimate(estimate)
	doc.Content = OpenQuestionsLast(doc.Content)
	_ = json.Unmarshal(similar, &doc.Similar)
	_ = json.Unmarshal(systems, &doc.Systems)
	_ = json.Unmarshal(services, &doc.Services)
	if doc.Similar == nil {
		doc.Similar = []Similar{}
	}
	if doc.Systems == nil {
		doc.Systems = []string{}
	}
	if doc.Services == nil {
		doc.Services = []string{}
	}
	return &doc, nil
}

// SaveDiscovery writes a new revision of the Discovery document.
func (d *DB) SaveDiscovery(ctx context.Context, doc *Discovery, problemFeatureID *uuid.UUID, isAgent bool, actor *uuid.UUID) error {
	doc.Content = OpenQuestionsLast(doc.Content)
	measure, _ := json.Marshal(doc.Measure)
	if doc.Measure == nil {
		measure = []byte("null")
	}
	nz := func(v any) []byte {
		b, _ := json.Marshal(v)
		if string(b) == "null" {
			return []byte("[]")
		}
		return b
	}
	var estimate []byte
	if doc.Estimate != nil {
		estimate, _ = json.Marshal(doc.Estimate)
	}
	err := d.q.QueryRow(ctx, `INSERT INTO discovery_docs (issue_id, content, value_text, measure, similar_items, systems, services, problem_feature_id,
			estimate, change_summary, revision, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,1,now())
		ON CONFLICT (issue_id) DO UPDATE SET content = EXCLUDED.content, value_text = EXCLUDED.value_text, measure = EXCLUDED.measure,
			similar_items = EXCLUDED.similar_items, systems = EXCLUDED.systems, services = EXCLUDED.services,
			problem_feature_id = EXCLUDED.problem_feature_id, measure_checked_at = CASE WHEN discovery_docs.measure = EXCLUDED.measure
				THEN discovery_docs.measure_checked_at END,
			estimate = EXCLUDED.estimate, change_summary = EXCLUDED.change_summary,
			revision = discovery_docs.revision + 1, updated_at = now()
		RETURNING revision, updated_at`,
		doc.IssueID, doc.Content, doc.Value, measure, nz(doc.Similar), nz(doc.Systems), nz(doc.Services), problemFeatureID,
		estimate, doc.ChangeSummary).
		Scan(&doc.Revision, &doc.UpdatedAt)
	if err != nil {
		return err
	}
	ids := doc.RequestIDs
	if ids == nil {
		ids = []uuid.UUID{}
	}
	_, err = d.q.Exec(ctx, `INSERT INTO discovery_revisions (issue_id, revision, content, is_agent, actor_id, estimate, change_summary, request_ids)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		doc.IssueID, doc.Revision, doc.Content, isAgent, actor, estimate, doc.ChangeSummary, ids)
	return err
}

func decodeEstimate(b []byte) *Estimate {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var e Estimate
	if json.Unmarshal(b, &e) != nil {
		return nil
	}
	if e.Basis.Features == nil {
		e.Basis.Features = []string{}
	}
	return &e
}

// MarkMeasureChecked records a successful dry run of the metric query.
func (d *DB) MarkMeasureChecked(ctx context.Context, issueID uuid.UUID, at time.Time) error {
	_, err := d.q.Exec(ctx, `UPDATE discovery_docs SET measure_checked_at = $2 WHERE issue_id = $1`, issueID, at)
	return err
}

// Revisions lists Discovery revisions, newest first.
func (d *DB) Revisions(ctx context.Context, issueID uuid.UUID) ([]Revision, error) {
	rows, err := d.q.Query(ctx, `SELECT r.revision, r.content, r.is_agent, u.display_name, r.created_at, r.estimate, r.change_summary,
		COALESCE((SELECT jsonb_agg(jsonb_build_object('id', q.id, 'author', a.display_name, 'instructions', left(q.instructions, 80)) ORDER BY q.created_at)
			FROM discovery_requests q LEFT JOIN users a ON a.id = q.author_id WHERE q.id = ANY(r.request_ids)), '[]')
		FROM discovery_revisions r
		LEFT JOIN users u ON u.id = r.actor_id WHERE r.issue_id = $1 ORDER BY r.revision DESC LIMIT 100`, issueID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Revision{}
	for rows.Next() {
		var r Revision
		var estimate, reqs []byte
		if err := rows.Scan(&r.Revision, &r.Content, &r.IsAgent, &r.Actor, &r.CreatedAt, &estimate, &r.ChangeSummary, &reqs); err != nil {
			return nil, err
		}
		r.Estimate = decodeEstimate(estimate)
		r.Content = OpenQuestionsLast(r.Content)
		_ = json.Unmarshal(reqs, &r.Requests)
		if r.Requests == nil {
			r.Requests = []RevReq{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) strings(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := d.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
