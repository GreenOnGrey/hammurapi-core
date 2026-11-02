package specdata

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// PG implements Store on Postgres.
type PG struct {
	pool *pgxpool.Pool
	q    postgres.Querier
}

// NewPG creates a store on the pool.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool, q: pool} }

// NewPGTx wraps an open transaction (e.g. of a workflow transition) as a store.
func NewPGTx(pool *pgxpool.Pool, tx pgx.Tx) *PG { return &PG{pool: pool, q: tx} }

// Q implements Store.
func (s *PG) Q() postgres.Querier { return s.q }

// InTx implements Store. Nested calls reuse the outer transaction.
func (s *PG) InTx(ctx context.Context, fn func(Store) error) error {
	if _, ok := s.q.(pgx.Tx); ok {
		return fn(s)
	}
	return postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(&PG{pool: s.pool, q: tx})
	})
}

func notFound(err error) error {
	if postgres.IsNoRows(err) {
		return ErrNotFound
	}
	return err
}

func (s *PG) SystemByKeys(ctx context.Context, domainKey, systemKey string) (*System, error) {
	var sys System
	err := s.q.QueryRow(ctx, `
		SELECT s.id, d.id, d.key, s.key, s.name, d.approval_required
		FROM systems s JOIN domains d ON d.id = s.domain_id
		WHERE d.key = $1 AND s.key = $2`, domainKey, systemKey).
		Scan(&sys.ID, &sys.DomainID, &sys.DomainKey, &sys.Key, &sys.Name, &sys.ApprovalRequired)
	if err != nil {
		return nil, notFound(err)
	}
	return &sys, nil
}

func (s *PG) NextNumber(ctx context.Context, systemID uuid.UUID) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, `UPDATE systems SET last_number = last_number + 1 WHERE id = $1 RETURNING last_number`, systemID).Scan(&n)
	return n, notFound(err)
}

func (s *PG) PeekNumber(ctx context.Context, systemID uuid.UUID) (int, error) {
	var n int
	err := s.q.QueryRow(ctx, `SELECT last_number FROM systems WHERE id = $1`, systemID).Scan(&n)
	return n, notFound(err)
}

const featureCols = `
	f.id, f.unique_id, f.system_id, d.key, s.key, d.approval_required, f.number, f.title,
	f.branch_name, f.pr_number, f.pr_url, f.phase, f.is_problem, f.imported, f.metric, f.flag_key,
	f.parent_id, p.unique_id, COALESCE(f.created_by, '00000000-0000-0000-0000-000000000000'), COALESCE(cu.display_name, ''), f.created_at,
	f.deleted_by, du.display_name, f.deleted_at, f.branch_cleanup_pending, f.spec_flow, f.spec_stage, f.skipped_areas`

const featureFrom = `
	FROM features f
	JOIN systems s ON s.id = f.system_id
	JOIN domains d ON d.id = s.domain_id
	LEFT JOIN users cu ON cu.id = f.created_by -- indexed features have no author
	LEFT JOIN features p ON p.id = f.parent_id
	LEFT JOIN users du ON du.id = f.deleted_by`

func scanFeature(row pgx.Row, extra ...any) (*Feature, error) {
	var f Feature
	dest := []any{&f.ID, &f.UniqueID, &f.SystemID, &f.DomainKey, &f.SystemKey, &f.ApprovalRequired, &f.Number, &f.Title,
		&f.Branch, &f.PRNumber, &f.PRURL, &f.Phase, &f.IsProblem, &f.Imported, &f.Metric, &f.FlagKey,
		&f.ParentID, &f.ParentUniqueID, &f.CreatedBy, &f.CreatedByName, &f.CreatedAt,
		&f.DeletedBy, &f.DeletedByName, &f.DeletedAt, &f.BranchCleanupPending, &f.SpecFlow, &f.SpecStage, &f.SkippedAreas}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *PG) FeatureByUniqueID(ctx context.Context, uid string) (*Feature, error) {
	f, err := scanFeature(s.q.QueryRow(ctx, `SELECT `+featureCols+featureFrom+` WHERE f.unique_id = $1`, uid))
	return f, notFound(err)
}

func (s *PG) FeatureByID(ctx context.Context, id uuid.UUID) (*Feature, error) {
	f, err := scanFeature(s.q.QueryRow(ctx, `SELECT `+featureCols+featureFrom+` WHERE f.id = $1`, id))
	return f, notFound(err)
}

func (s *PG) InsertFeature(ctx context.Context, f *Feature) error {
	if f.Phase == "" {
		f.Phase = domain.PhaseSpec
	}
	if f.SpecFlow == "" {
		f.SpecFlow = FlowLegacy
	}
	return s.q.QueryRow(ctx, `
		INSERT INTO features (unique_id, system_id, number, title, branch_name, pr_number, pr_url, phase,
			is_problem, imported, metric, flag_key, parent_id, created_by, spec_flow, spec_stage)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id, created_at`,
		f.UniqueID, f.SystemID, f.Number, f.Title, f.Branch, f.PRNumber, f.PRURL, f.Phase,
		f.IsProblem, f.Imported, f.Metric, f.FlagKey, f.ParentID, f.CreatedBy, f.SpecFlow, f.SpecStage).
		Scan(&f.ID, &f.CreatedAt)
}

func (s *PG) SetPhase(ctx context.Context, id uuid.UUID, phase domain.FeaturePhase) error {
	_, err := s.q.Exec(ctx, `UPDATE features SET phase = $2 WHERE id = $1`, id, phase)
	return err
}

func (s *PG) SetFlagKey(ctx context.Context, id uuid.UUID, flag *string) error {
	_, err := s.q.Exec(ctx, `UPDATE features SET flag_key = $2 WHERE id = $1`, id, flag)
	return err
}

func (s *PG) LinkIssue(ctx context.Context, featureID, issueID uuid.UUID) error {
	_, err := s.q.Exec(ctx, `INSERT INTO feature_issues (feature_id, issue_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, featureID, issueID)
	return err
}

func (s *PG) FeatureIssueKeys(ctx context.Context, featureID uuid.UUID) ([]string, error) {
	rows, err := s.q.Query(ctx, `SELECT i.key FROM feature_issues fi JOIN issues i ON i.id = fi.issue_id
		WHERE fi.feature_id = $1 ORDER BY i.key`, featureID)
	if err != nil {
		return nil, err
	}
	keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if keys == nil {
		keys = []string{}
	}
	return keys, err
}

func (s *PG) MarkDeleted(ctx context.Context, id, by uuid.UUID, cleanupPending bool) error {
	_, err := s.q.Exec(ctx, `UPDATE features SET phase = 'deleted', deleted_by = $2, deleted_at = now(),
		branch_cleanup_pending = $3 WHERE id = $1`, id, by, cleanupPending)
	return err
}

func (s *PG) SetBranchCleanupPending(ctx context.Context, id uuid.UUID, pending bool) error {
	_, err := s.q.Exec(ctx, `UPDATE features SET branch_cleanup_pending = $2 WHERE id = $1`, id, pending)
	return err
}

func (s *PG) FeaturesPendingCleanup(ctx context.Context) ([]Feature, error) {
	rows, err := s.q.Query(ctx, `SELECT `+featureCols+featureFrom+` WHERE f.branch_cleanup_pending LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feature
	for rows.Next() {
		f, err := scanFeature(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	return out, rows.Err()
}

func (s *PG) Fixes(ctx context.Context, parentID uuid.UUID) ([]FeatureRef, error) {
	rows, err := s.q.Query(ctx, `SELECT unique_id, title, phase FROM features
		WHERE parent_id = $1 AND phase <> 'deleted' ORDER BY number`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FeatureRef{}
	for rows.Next() {
		var r FeatureRef
		if err := rows.Scan(&r.UniqueID, &r.Title, &r.Phase); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *PG) ListFeatures(ctx context.Context, lf ListFilter) ([]ListedFeature, error) {
	// Indexed features (FTR.HMR.CMN-0005 R4) are read in the navigator, not in the cycle lists.
	where := []string{"f.phase NOT IN ('deleted', 'indexed')"}
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	switch lf.Domain {
	case "", "mine":
		where = append(where, "d.id IN (SELECT domain_id FROM user_domains WHERE user_id = "+arg(lf.UserID)+")")
	case "all":
	default:
		where = append(where, "d.key = "+arg(lf.Domain))
	}
	switch lf.Status {
	case "", "active":
		where = append(where, "f.phase IN ('spec','codegen','validation')")
	case "released":
		where = append(where, "f.phase = 'released'")
	case "rolled_back":
		where = append(where, "f.phase = 'rolled_back'")
	}
	switch lf.Phase {
	case "spec", "codegen", "validation":
		where = append(where, "f.phase = "+arg(lf.Phase)+"::feature_phase")
	}
	if q := strings.TrimSpace(lf.Query); q != "" {
		p := arg("%" + q + "%")
		where = append(where, "(f.title ILIKE "+p+" OR f.unique_id ILIKE "+p+")")
	}
	if c := lf.Page.Cursor; c != nil {
		where = append(where, "(f.created_at, f.id::text) < ("+arg(c.T)+", "+arg(c.ID)+")")
	}
	sql := `SELECT ` + featureCols + featureFrom + ` WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY f.created_at DESC, f.id::text DESC LIMIT ` + arg(lf.Page.Limit+1)
	rows, err := s.q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	var out []ListedFeature
	ids := []uuid.UUID{}
	for rows.Next() {
		f, err := scanFeature(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, ListedFeature{Feature: *f})
		ids = append(ids, f.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return out, nil
	}
	gates, err := s.gatesWhere(ctx, `g.feature_id = ANY($1) AND g.deleted_at IS NULL`, ids)
	if err != nil {
		return nil, err
	}
	byFeature := map[uuid.UUID][]Gate{}
	for _, g := range gates {
		byFeature[g.FeatureID] = append(byFeature[g.FeatureID], g)
	}
	irows, err := s.q.Query(ctx, `SELECT fi.feature_id, i.key FROM feature_issues fi JOIN issues i ON i.id = fi.issue_id
		WHERE fi.feature_id = ANY($1) ORDER BY i.key`, ids)
	if err != nil {
		return nil, err
	}
	issues := map[uuid.UUID][]string{}
	for irows.Next() {
		var fid uuid.UUID
		var key string
		if err := irows.Scan(&fid, &key); err != nil {
			irows.Close()
			return nil, err
		}
		issues[fid] = append(issues[fid], key)
	}
	irows.Close()
	for i := range out {
		out[i].Gates = byFeature[out[i].ID]
		out[i].Issues = issues[out[i].ID]
	}
	return out, irows.Err()
}

const gateCols = `g.id, g.feature_id, g.area, g.status, g.generated, g.head_commit, g.submitted_at, g.approved_commit,
	g.approved_by, au.display_name, g.approved_at, g.created_by, g.created_at, g.deleted_by, g.deleted_at`

func (s *PG) gatesWhere(ctx context.Context, cond string, args ...any) ([]Gate, error) {
	rows, err := s.q.Query(ctx, `SELECT `+gateCols+` FROM gates g LEFT JOIN users au ON au.id = g.approved_by
		WHERE `+cond+` ORDER BY g.area`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Gate
	for rows.Next() {
		var g Gate
		if err := rows.Scan(&g.ID, &g.FeatureID, &g.Area, &g.Status, &g.Generated, &g.HeadCommit, &g.SubmittedAt, &g.ApprovedCommit,
			&g.ApprovedBy, &g.ApprovedByName, &g.ApprovedAt, &g.CreatedBy, &g.CreatedAt, &g.DeletedBy, &g.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *PG) ActiveGates(ctx context.Context, featureID uuid.UUID) ([]Gate, error) {
	return s.gatesWhere(ctx, `g.feature_id = $1 AND g.deleted_at IS NULL`, featureID)
}

func (s *PG) ActiveGate(ctx context.Context, featureID uuid.UUID, area domain.Area) (*Gate, error) {
	gs, err := s.gatesWhere(ctx, `g.feature_id = $1 AND g.area = $2 AND g.deleted_at IS NULL`, featureID, area)
	if err != nil {
		return nil, err
	}
	if len(gs) == 0 {
		return nil, ErrNotFound
	}
	return &gs[0], nil
}

func (s *PG) InsertGate(ctx context.Context, g *Gate) error {
	return s.q.QueryRow(ctx, `INSERT INTO gates (feature_id, area, status, generated, head_commit, submitted_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id, created_at`,
		g.FeatureID, g.Area, g.Status, g.Generated, g.HeadCommit, g.SubmittedAt, g.CreatedBy).Scan(&g.ID, &g.CreatedAt)
}

func (s *PG) SaveGate(ctx context.Context, g *Gate) error {
	_, err := s.q.Exec(ctx, `UPDATE gates SET status = $2, head_commit = $3, submitted_at = $4, approved_commit = $5,
		approved_by = $6, approved_at = $7, deleted_by = $8, deleted_at = $9 WHERE id = $1`,
		g.ID, g.Status, g.HeadCommit, g.SubmittedAt, g.ApprovedCommit, g.ApprovedBy, g.ApprovedAt, g.DeletedBy, g.DeletedAt)
	return err
}

func (s *PG) InsertEvent(ctx context.Context, e *GateEvent) error {
	return s.q.QueryRow(ctx, `INSERT INTO gate_events (gate_id, event_type, actor_id, is_agent, commit_sha)
		VALUES ($1,$2,$3,$4,$5) RETURNING id, created_at`,
		e.GateID, e.Type, e.ActorID, e.IsAgent, e.CommitSHA).Scan(&e.ID, &e.CreatedAt)
}

func (s *PG) History(ctx context.Context, featureID uuid.UUID, area domain.Area, page httpx.Page) ([]GateEvent, error) {
	args := []any{featureID, area, page.Limit + 1}
	cond := ""
	if c := page.Cursor; c != nil {
		args = append(args, c.T, c.ID)
		cond = ` AND (e.created_at, e.id::text) < ($4, $5)`
	}
	rows, err := s.q.Query(ctx, `
		SELECT e.id, e.gate_id, g.area, e.event_type, e.actor_id, u.display_name, e.is_agent, e.commit_sha, e.created_at
		FROM gate_events e
		JOIN gates g ON g.id = e.gate_id
		LEFT JOIN users u ON u.id = e.actor_id
		WHERE g.feature_id = $1 AND g.area = $2`+cond+`
		ORDER BY e.created_at DESC, e.id::text DESC LIMIT $3`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GateEvent
	for rows.Next() {
		var e GateEvent
		if err := rows.Scan(&e.ID, &e.GateID, &e.Area, &e.Type, &e.ActorID, &e.ActorName, &e.IsAgent, &e.CommitSHA, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *PG) LastSubmitter(ctx context.Context, gateID uuid.UUID) (*uuid.UUID, *string, error) {
	var id *uuid.UUID
	var name *string
	err := s.q.QueryRow(ctx, `SELECT e.actor_id, u.display_name FROM gate_events e LEFT JOIN users u ON u.id = e.actor_id
		WHERE e.gate_id = $1 AND e.event_type = 'submitted' ORDER BY e.created_at DESC LIMIT 1`, gateID).Scan(&id, &name)
	if postgres.IsNoRows(err) {
		return nil, nil, nil
	}
	return id, name, err
}

func (s *PG) GetLock(ctx context.Context, featureID uuid.UUID) (*Lock, error) {
	var l Lock
	err := s.q.QueryRow(ctx, `SELECT l.feature_id, l.locked_by, u.display_name, l.locked_at, l.expires_at
		FROM feature_locks l JOIN users u ON u.id = l.locked_by
		WHERE l.feature_id = $1 AND l.expires_at > now()`, featureID).
		Scan(&l.FeatureID, &l.LockedBy, &l.LockedByName, &l.LockedAt, &l.ExpiresAt)
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

func (s *PG) AcquireLock(ctx context.Context, featureID, userID uuid.UUID) (*Lock, bool, error) {
	tag, err := s.q.Exec(ctx, `
		INSERT INTO feature_locks (feature_id, locked_by, locked_at, expires_at)
		VALUES ($1, $2, now(), now() + $3::interval)
		ON CONFLICT (feature_id) DO UPDATE SET
			locked_at = CASE WHEN feature_locks.locked_by = EXCLUDED.locked_by AND feature_locks.expires_at > now()
				THEN feature_locks.locked_at ELSE now() END,
			locked_by = EXCLUDED.locked_by,
			expires_at = EXCLUDED.expires_at
		WHERE feature_locks.locked_by = EXCLUDED.locked_by OR feature_locks.expires_at <= now()`,
		featureID, userID, fmt.Sprintf("%d seconds", int(LockTTL.Seconds())))
	if err != nil {
		return nil, false, err
	}
	l, err := s.GetLock(ctx, featureID)
	if err != nil {
		return nil, false, err
	}
	return l, tag.RowsAffected() > 0 && l != nil && l.LockedBy == userID, nil
}

func (s *PG) ReleaseLock(ctx context.Context, featureID, userID uuid.UUID) error {
	_, err := s.q.Exec(ctx, `DELETE FROM feature_locks WHERE feature_id = $1 AND locked_by = $2`, featureID, userID)
	return err
}

func (s *PG) DropLock(ctx context.Context, featureID uuid.UUID) error {
	_, err := s.q.Exec(ctx, `DELETE FROM feature_locks WHERE feature_id = $1`, featureID)
	return err
}

func (s *PG) UserIDByUsername(ctx context.Context, username string) (*uuid.UUID, error) {
	var id uuid.UUID
	err := s.q.QueryRow(ctx, `SELECT id FROM users WHERE username = $1`, username).Scan(&id)
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func (s *PG) MarkWebhookProcessed(ctx context.Context, eventID string) (bool, error) {
	tag, err := s.q.Exec(ctx, `INSERT INTO processed_webhook_events (provider_event_id) VALUES ($1) ON CONFLICT DO NOTHING`, eventID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

func (s *PG) SetImportItem(ctx context.Context, importID uuid.UUID, archiveID, status string, featureID *uuid.UUID, errText *string) error {
	_, err := s.q.Exec(ctx, `UPDATE import_items SET status = $3, feature_id = $4, error = $5 WHERE import_id = $1 AND archive_id = $2`,
		importID, archiveID, status, featureID, errText)
	return err
}
