package cycledata

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
)

const serviceCols = `s.id, s.key, s.name, s.system_id, d.key || '/' || sy.key, d.key, s.repo, s.owner_ref, s.autonomy, s.deploy_override,
	s.override_from_catalog, s.source::text, s.catalog_ref, s.deleted_in_catalog,
	COALESCE((SELECT array_agg(u.username ORDER BY u.username) FROM service_owners o JOIN users u ON u.id = o.user_id WHERE o.service_id = s.id), '{}'),
	s.check_command, s.check_timeout_sec`

const serviceFrom = ` FROM services s LEFT JOIN systems sy ON sy.id = s.system_id LEFT JOIN domains d ON d.id = sy.domain_id`

func scanService(row interface{ Scan(...any) error }) (*Service, error) {
	var s Service
	var command *string
	var timeout *int
	err := row.Scan(&s.ID, &s.Key, &s.Name, &s.SystemID, &s.System, &s.Domain, &s.Repo, &s.OwnerRef, &s.Autonomy, &s.DeployOverride,
		&s.OverrideFromCatalog, &s.Source, &s.CatalogRef, &s.DeletedInCatalog, &s.Owners, &command, &timeout)
	if command != nil {
		s.Check = &ServiceCheck{Command: *command}
		if timeout != nil {
			s.Check.TimeoutSec = *timeout
		}
	}
	return &s, err
}

// ServiceFilter filters services.
type ServiceFilter struct {
	System string // DOMAIN/SYSTEM
	Domain string
	Query  string
}

// ListServices lists services.
func (d *DB) ListServices(ctx context.Context, f ServiceFilter) ([]Service, error) {
	where := []string{"true"}
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if f.System != "" {
		dk, sk, _ := strings.Cut(f.System, "/")
		where = append(where, "d.key = "+arg(dk)+" AND sy.key = "+arg(sk))
	}
	if f.Domain != "" {
		where = append(where, "d.key = "+arg(f.Domain))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		p := arg("%" + q + "%")
		where = append(where, "(s.key ILIKE "+p+" OR s.name ILIKE "+p+" OR s.repo ILIKE "+p+")")
	}
	rows, err := d.q.Query(ctx, `SELECT `+serviceCols+serviceFrom+` WHERE `+strings.Join(where, " AND ")+` ORDER BY s.key`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Service{}
	for rows.Next() {
		s, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// ServiceByKey loads a service.
func (d *DB) ServiceByKey(ctx context.Context, key string) (*Service, error) {
	s, err := scanService(d.q.QueryRow(ctx, `SELECT `+serviceCols+serviceFrom+` WHERE s.key = $1`, key))
	return s, nf(err)
}

// ServiceByID loads a service.
func (d *DB) ServiceByID(ctx context.Context, id uuid.UUID) (*Service, error) {
	s, err := scanService(d.q.QueryRow(ctx, `SELECT `+serviceCols+serviceFrom+` WHERE s.id = $1`, id))
	return s, nf(err)
}

// ServiceByRepo finds a service by repository (first match).
func (d *DB) ServiceByRepo(ctx context.Context, repo string) (*Service, error) {
	s, err := scanService(d.q.QueryRow(ctx, `SELECT `+serviceCols+serviceFrom+` WHERE lower(s.repo) = lower($1) ORDER BY s.key LIMIT 1`, repo))
	return s, nf(err)
}

// UpsertService creates or updates a service by key.
func (d *DB) UpsertService(ctx context.Context, s *Service) error {
	override := s.DeployOverride
	if len(override) == 0 {
		override = nil
	}
	return d.q.QueryRow(ctx, `INSERT INTO services (key, name, system_id, repo, owner_ref, deploy_override, override_from_catalog, source, catalog_ref, deleted_in_catalog)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,false)
		ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, system_id = EXCLUDED.system_id, repo = EXCLUDED.repo,
			owner_ref = EXCLUDED.owner_ref,
			deploy_override = CASE WHEN EXCLUDED.override_from_catalog OR NOT services.override_from_catalog THEN COALESCE(EXCLUDED.deploy_override, services.deploy_override) ELSE services.deploy_override END,
			override_from_catalog = EXCLUDED.override_from_catalog OR services.override_from_catalog,
			source = EXCLUDED.source, catalog_ref = EXCLUDED.catalog_ref, deleted_in_catalog = false, updated_at = now()
		RETURNING id`, s.Key, s.Name, s.SystemID, s.Repo, s.OwnerRef, override, s.OverrideFromCatalog, s.Source, s.CatalogRef).Scan(&s.ID)
}

// SetAutonomy changes the agent autonomy of a service.
func (d *DB) SetAutonomy(ctx context.Context, id uuid.UUID, level domain.Autonomy) error {
	_, err := d.q.Exec(ctx, `UPDATE services SET autonomy = $2, updated_at = now() WHERE id = $1`, id, level)
	return err
}

// SetCheck stores the readiness check of a service; nil clears it (FTR.HMR.CMN-0008 R1).
func (d *DB) SetCheck(ctx context.Context, id uuid.UUID, c *ServiceCheck) error {
	var command *string
	var timeout *int
	if c != nil {
		command, timeout = &c.Command, &c.TimeoutSec
	}
	_, err := d.q.Exec(ctx, `UPDATE services SET check_command = $2, check_timeout_sec = $3, updated_at = now() WHERE id = $1`, id, command, timeout)
	return err
}

// SetDeployOverride stores the per-service deploy override.
func (d *DB) SetDeployOverride(ctx context.Context, id uuid.UUID, override json.RawMessage) error {
	_, err := d.q.Exec(ctx, `UPDATE services SET deploy_override = $2, updated_at = now() WHERE id = $1`, id, override)
	return err
}

// SetServiceOwners replaces owners (resolved users).
func (d *DB) SetServiceOwners(ctx context.Context, serviceID uuid.UUID, userIDs []uuid.UUID) error {
	if _, err := d.q.Exec(ctx, `DELETE FROM service_owners WHERE service_id = $1`, serviceID); err != nil {
		return err
	}
	for _, u := range userIDs {
		if _, err := d.q.Exec(ctx, `INSERT INTO service_owners (service_id, user_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, serviceID, u); err != nil {
			return err
		}
	}
	return nil
}

// DeleteService removes a service unless it is referenced; then it is marked deleted in catalog.
func (d *DB) DeleteService(ctx context.Context, id uuid.UUID) (removed bool, err error) {
	var refs int
	if err := d.q.QueryRow(ctx, `SELECT (SELECT count(*) FROM feature_services fs JOIN features f ON f.id = fs.feature_id
		WHERE fs.service_id = $1 AND f.phase IN ('spec','codegen','validation','in_release'))`, id).Scan(&refs); err != nil {
		return false, err
	}
	if refs > 0 {
		_, err := d.q.Exec(ctx, `UPDATE services SET deleted_in_catalog = true WHERE id = $1`, id)
		return false, err
	}
	if _, err := d.q.Exec(ctx, `DELETE FROM service_owners WHERE service_id = $1`, id); err != nil {
		return false, err
	}
	tag, err := d.q.Exec(ctx, `DELETE FROM services WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM feature_services WHERE service_id = $1)
		AND NOT EXISTS (SELECT 1 FROM agent_tasks WHERE service_id = $1) AND NOT EXISTS (SELECT 1 FROM pull_requests WHERE service_id = $1)`, id)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		_, err := d.q.Exec(ctx, `UPDATE services SET deleted_in_catalog = true WHERE id = $1`, id)
		return false, err
	}
	return true, nil
}

// ─── Feature services and traceability ───────────────────────────────

// FeatureServices lists services affected by a feature (from the tech spec).
func (d *DB) FeatureServices(ctx context.Context, featureID uuid.UUID) ([]Service, error) {
	rows, err := d.q.Query(ctx, `SELECT `+serviceCols+serviceFrom+` JOIN feature_services fs ON fs.service_id = s.id WHERE fs.feature_id = $1 ORDER BY s.key`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Service{}
	for rows.Next() {
		s, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// SetFeatureServices replaces the services of a feature.
func (d *DB) SetFeatureServices(ctx context.Context, featureID uuid.UUID, ids []uuid.UUID) error {
	if _, err := d.q.Exec(ctx, `DELETE FROM feature_services WHERE feature_id = $1`, featureID); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := d.q.Exec(ctx, `INSERT INTO feature_services (feature_id, service_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, featureID, id); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceRequirements writes the requirement projection of a feature.
func (d *DB) ReplaceRequirements(ctx context.Context, featureID uuid.UUID, reqs []Requirement) error {
	if _, err := d.q.Exec(ctx, `DELETE FROM requirements WHERE feature_id = $1`, featureID); err != nil {
		return err
	}
	for _, r := range reqs {
		if _, err := d.q.Exec(ctx, `INSERT INTO requirements (feature_id, req_id, text) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, featureID, r.ID, r.Text); err != nil {
			return err
		}
	}
	return nil
}

// ReplaceRequirementServices maps requirements to services (from the tech spec).
func (d *DB) ReplaceRequirementServices(ctx context.Context, featureID uuid.UUID, byService map[uuid.UUID][]string) error {
	if _, err := d.q.Exec(ctx, `DELETE FROM requirement_services WHERE feature_id = $1`, featureID); err != nil {
		return err
	}
	for svc, reqs := range byService {
		for _, r := range reqs {
			if _, err := d.q.Exec(ctx, `INSERT INTO requirement_services (feature_id, req_id, service_id)
				SELECT $1, $2, $3 WHERE EXISTS (SELECT 1 FROM requirements WHERE feature_id = $1 AND req_id = $2)
				ON CONFLICT DO NOTHING`, featureID, r, svc); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReplaceTestCases writes the test case projection.
func (d *DB) ReplaceTestCases(ctx context.Context, featureID uuid.UUID, tcs []TestCase) error {
	if _, err := d.q.Exec(ctx, `DELETE FROM test_cases WHERE feature_id = $1`, featureID); err != nil {
		return err
	}
	for _, t := range tcs {
		reqs := t.ReqIDs
		if reqs == nil {
			reqs = []string{}
		}
		svcs := t.Services
		if svcs == nil {
			svcs = []string{}
		}
		if _, err := d.q.Exec(ctx, `INSERT INTO test_cases (feature_id, tc_id, level, req_ids, title, services) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`,
			featureID, t.ID, t.Level, reqs, t.Title, svcs); err != nil {
			return err
		}
	}
	return nil
}

// Requirements lists requirements with their services.
func (d *DB) Requirements(ctx context.Context, featureID uuid.UUID) ([]Requirement, error) {
	rows, err := d.q.Query(ctx, `SELECT r.req_id, r.text,
		COALESCE((SELECT array_agg(s.key ORDER BY s.key) FROM requirement_services rs JOIN services s ON s.id = rs.service_id
		 WHERE rs.feature_id = r.feature_id AND rs.req_id = r.req_id), '{}')
		FROM requirements r WHERE r.feature_id = $1
		ORDER BY substring(r.req_id from '\d+')::int, r.req_id`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Requirement{}
	for rows.Next() {
		var r Requirement
		if err := rows.Scan(&r.ID, &r.Text, &r.Services); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TestCases lists test cases.
func (d *DB) TestCases(ctx context.Context, featureID uuid.UUID) ([]TestCase, error) {
	rows, err := d.q.Query(ctx, `SELECT tc_id, level, req_ids, title, services FROM test_cases WHERE feature_id = $1 ORDER BY tc_id`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TestCase{}
	for rows.Next() {
		var t TestCase
		if err := rows.Scan(&t.ID, &t.Level, &t.ReqIDs, &t.Title, &t.Services); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ─── Pull requests ───────────────────────────────────────────────────

const prCols = `p.id, p.repo, p.number, p.url, p.title, p.branch, p.kind::text, p.feature_id, p.service_id, s.key, p.by_agent, p.state::text,
	p.review::text, p.head_sha, p.ci_status, p.merge_sha, p.merged_by, p.merged_at, p.reverts_pr_id, p.created_at,
	COALESCE((SELECT array_agg(req_id ORDER BY req_id) FROM pr_requirements WHERE pr_id = p.id), '{}')`

const prFrom = ` FROM pull_requests p LEFT JOIN services s ON s.id = p.service_id`

func scanPR(row interface{ Scan(...any) error }) (*PR, error) {
	var p PR
	err := row.Scan(&p.ID, &p.Repo, &p.Number, &p.URL, &p.Title, &p.Branch, &p.Kind, &p.FeatureID, &p.ServiceID, &p.Service, &p.ByAgent,
		&p.State, &p.Review, &p.HeadSHA, &p.CIStatus, &p.MergeSHA, &p.MergedBy, &p.MergedAt, &p.RevertsPRID, &p.CreatedAt, &p.ReqIDs)
	return &p, err
}

func (d *DB) prs(ctx context.Context, where string, args ...any) ([]PR, error) {
	rows, err := d.q.Query(ctx, `SELECT `+prCols+prFrom+` WHERE `+where+` ORDER BY p.created_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PR{}
	for rows.Next() {
		p, err := scanPR(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// UpsertPR creates or updates a PR by (repo, number).
func (d *DB) UpsertPR(ctx context.Context, p *PR) error {
	return d.q.QueryRow(ctx, `INSERT INTO pull_requests (repo, number, url, title, branch, kind, feature_id, service_id, by_agent, state, review, head_sha, reverts_pr_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (repo, number) DO UPDATE SET url = EXCLUDED.url, title = EXCLUDED.title, branch = EXCLUDED.branch,
			head_sha = CASE WHEN EXCLUDED.head_sha <> '' THEN EXCLUDED.head_sha ELSE pull_requests.head_sha END,
			review = CASE WHEN pull_requests.review IN ('approved','changes_requested') THEN pull_requests.review ELSE EXCLUDED.review END
		RETURNING id, created_at`,
		p.Repo, p.Number, p.URL, p.Title, p.Branch, p.Kind, p.FeatureID, p.ServiceID, p.ByAgent, nz(p.State, "open"), nz(p.Review, "none"), p.HeadSHA, p.RevertsPRID).
		Scan(&p.ID, &p.CreatedAt)
}

func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// PRByRepoNumber finds a tracked PR.
func (d *DB) PRByRepoNumber(ctx context.Context, repo string, number int) (*PR, error) {
	p, err := scanPR(d.q.QueryRow(ctx, `SELECT `+prCols+prFrom+` WHERE lower(p.repo) = lower($1) AND p.number = $2`, repo, number))
	return p, nf(err)
}

// PRByID loads a PR.
func (d *DB) PRByID(ctx context.Context, id uuid.UUID) (*PR, error) {
	p, err := scanPR(d.q.QueryRow(ctx, `SELECT `+prCols+prFrom+` WHERE p.id = $1`, id))
	return p, nf(err)
}

// FeaturePRs lists PRs of a feature; kind "" = all.
func (d *DB) FeaturePRs(ctx context.Context, featureID uuid.UUID, kind string) ([]PR, error) {
	if kind == "" {
		return d.prs(ctx, `p.feature_id = $1`, featureID)
	}
	return d.prs(ctx, `p.feature_id = $1 AND p.kind::text = $2`, featureID, kind)
}

// ServicePR returns the open (or merged) service PR of a feature for a service.
func (d *DB) ServicePR(ctx context.Context, featureID, serviceID uuid.UUID) (*PR, error) {
	p, err := scanPR(d.q.QueryRow(ctx, `SELECT `+prCols+prFrom+` WHERE p.feature_id = $1 AND p.service_id = $2 AND p.kind = 'service'
		AND p.state <> 'closed' ORDER BY p.created_at DESC LIMIT 1`, featureID, serviceID))
	return p, nf(err)
}

// PRsByHead finds PRs by repository and head sha (CI results).
func (d *DB) PRsByHead(ctx context.Context, repo, sha, branch string) ([]PR, error) {
	return d.prs(ctx, `lower(p.repo) = lower($1) AND (p.head_sha = $2 OR ($3 <> '' AND p.branch = $3 AND p.state = 'open'))`, repo, sha, branch)
}

// SetPRHead updates the head sha (new commits) and clears CI.
func (d *DB) SetPRHead(ctx context.Context, id uuid.UUID, sha string) error {
	_, err := d.q.Exec(ctx, `UPDATE pull_requests SET head_sha = $2, ci_status = CASE WHEN head_sha = $2 THEN ci_status END WHERE id = $1`, id, sha)
	return err
}

// SetPRTitle keeps the title of the PR as the provider has it (FTR.HMR.DEV-0004 R2).
func (d *DB) SetPRTitle(ctx context.Context, id uuid.UUID, title string) error {
	_, err := d.q.Exec(ctx, `UPDATE pull_requests SET title = $2 WHERE id = $1`, id, title)
	return err
}

// SetPRReview sets the review state.
func (d *DB) SetPRReview(ctx context.Context, id uuid.UUID, review string) error {
	_, err := d.q.Exec(ctx, `UPDATE pull_requests SET review = $2::pr_review WHERE id = $1`, id, review)
	return err
}

// SetPRCI sets the CI status of the current head.
func (d *DB) SetPRCI(ctx context.Context, id uuid.UUID, status string) error {
	_, err := d.q.Exec(ctx, `UPDATE pull_requests SET ci_status = $2 WHERE id = $1`, id, status)
	return err
}

// MarkPRMerged records a merge.
func (d *DB) MarkPRMerged(ctx context.Context, id uuid.UUID, mergeSHA string, by *uuid.UUID) error {
	_, err := d.q.Exec(ctx, `UPDATE pull_requests SET state = 'merged', merge_sha = COALESCE(NULLIF($2,''), merge_sha),
		merged_by = COALESCE($3, merged_by), merged_at = COALESCE(merged_at, now()) WHERE id = $1`, id, mergeSHA, by)
	return err
}

// MarkPRClosed records a close without merge.
func (d *DB) MarkPRClosed(ctx context.Context, id uuid.UUID) error {
	_, err := d.q.Exec(ctx, `UPDATE pull_requests SET state = 'closed' WHERE id = $1 AND state = 'open'`, id)
	return err
}

// SetPRRequirements replaces the requirements a PR implements.
func (d *DB) SetPRRequirements(ctx context.Context, id uuid.UUID, reqs []string) error {
	if _, err := d.q.Exec(ctx, `DELETE FROM pr_requirements WHERE pr_id = $1`, id); err != nil {
		return err
	}
	for _, r := range reqs {
		if _, err := d.q.Exec(ctx, `INSERT INTO pr_requirements (pr_id, req_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`, id, r); err != nil {
			return err
		}
	}
	return nil
}

// ─── Agent tasks ─────────────────────────────────────────────────────

const taskCols = `t.id, t.type::text, t.status::text, t.feature_id, t.release_id, t.service_id, s.key, t.workflow_run_id, t.initiator_id,
	t.token_hash, t.executor_ref, t.input, t.result, t.progress, t.error, t.tokens_in, t.tokens_out, t.created_at, t.started_at, t.finished_at,
	t.error_class, t.nabu_run_id::text`

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.Type, &t.Status, &t.FeatureID, &t.ReleaseID, &t.ServiceID, &t.Service, &t.RunID, &t.InitiatorID,
		&t.TokenHash, &t.ExecutorRef, &t.Input, &t.Result, &t.Progress, &t.Error, &t.TokensIn, &t.TokensOut, &t.CreatedAt, &t.StartedAt, &t.FinishedAt,
		&t.ErrorClass, &t.NabuRunID)
	return &t, err
}

// InsertTask creates an agent task.
func (d *DB) InsertTask(ctx context.Context, t *Task) error {
	if len(t.Input) == 0 {
		t.Input = json.RawMessage("{}")
	}
	return d.q.QueryRow(ctx, `INSERT INTO agent_tasks (id, type, feature_id, release_id, service_id, workflow_run_id, initiator_id, input)
		VALUES (COALESCE($1, gen_random_uuid()), $2,$3,$4,$5,$6,$7,$8) RETURNING id, created_at`,
		nilUUID(t.ID), t.Type, t.FeatureID, t.ReleaseID, t.ServiceID, t.RunID, t.InitiatorID, t.Input).Scan(&t.ID, &t.CreatedAt)
}

func nilUUID(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// TaskByID loads a task.
func (d *DB) TaskByID(ctx context.Context, id uuid.UUID) (*Task, error) {
	t, err := scanTask(d.q.QueryRow(ctx, `SELECT `+taskCols+` FROM agent_tasks t JOIN services s ON s.id = t.service_id WHERE t.id = $1`, id))
	return t, nf(err)
}

// FeatureTasks lists tasks of a feature, newest first.
func (d *DB) FeatureTasks(ctx context.Context, featureID uuid.UUID) ([]Task, error) {
	return d.tasks(ctx, `t.feature_id = $1`, featureID)
}

// ReleaseTasks lists tasks of a release.
func (d *DB) ReleaseTasks(ctx context.Context, releaseID uuid.UUID) ([]Task, error) {
	return d.tasks(ctx, `t.release_id = $1`, releaseID)
}

func (d *DB) tasks(ctx context.Context, where string, args ...any) ([]Task, error) {
	rows, err := d.q.Query(ctx, `SELECT `+taskCols+` FROM agent_tasks t JOIN services s ON s.id = t.service_id WHERE `+where+` ORDER BY t.created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// StartTask marks a task running with a token hash. It fails with a unique
// violation if another task of the service is running.
func (d *DB) StartTask(ctx context.Context, id uuid.UUID, tokenHash []byte, executorRef string) error {
	_, err := d.q.Exec(ctx, `UPDATE agent_tasks SET status = 'running', token_hash = $2, executor_ref = $3, started_at = now(), error = NULL WHERE id = $1`,
		id, tokenHash, executorRef)
	return err
}

// ServiceBusy reports whether another task runs in the service's repository.
func (d *DB) ServiceBusy(ctx context.Context, serviceID, except uuid.UUID) (bool, error) {
	var n int
	err := d.q.QueryRow(ctx, `SELECT count(*) FROM agent_tasks WHERE service_id = $1 AND status = 'running' AND id <> $2`, serviceID, except).Scan(&n)
	return n > 0, err
}

// CountRunning counts running tasks (global parallelism).
func (d *DB) CountRunning(ctx context.Context) (int, error) {
	var n int
	err := d.q.QueryRow(ctx, `SELECT count(*) FROM agent_tasks WHERE status = 'running'`).Scan(&n)
	return n, err
}

// FinishTask stores the result and revokes the token.
func (d *DB) FinishTask(ctx context.Context, id uuid.UUID, status string, result json.RawMessage, errText *string) error {
	if len(result) == 0 {
		result = nil
	}
	_, err := d.q.Exec(ctx, `UPDATE agent_tasks SET status = $2::task_status, result = COALESCE($3, result), error = $4, token_hash = NULL, finished_at = now()
		WHERE id = $1`, id, status, result, errText)
	return err
}

// TaskProgress stores progress and token usage.
func (d *DB) TaskProgress(ctx context.Context, id uuid.UUID, msg string, tokensIn, tokensOut int64) error {
	_, err := d.q.Exec(ctx, `UPDATE agent_tasks SET progress = $2, tokens_in = GREATEST(tokens_in, $3), tokens_out = GREATEST(tokens_out, $4) WHERE id = $1`,
		id, msg, tokensIn, tokensOut)
	return err
}

// MergeTaskReport merges a report from the agent (MCP report_task) into the result.
func (d *DB) MergeTaskReport(ctx context.Context, id uuid.UUID, report json.RawMessage) error {
	_, err := d.q.Exec(ctx, `UPDATE agent_tasks SET result = COALESCE(result, '{}'::jsonb) || jsonb_build_object('report', $2::jsonb) WHERE id = $1`, id, report)
	return err
}

// TaskByTokenHash finds a running task by its token hash.
func (d *DB) TaskByTokenHash(ctx context.Context, hash []byte) (*Task, error) {
	t, err := scanTask(d.q.QueryRow(ctx, `SELECT `+taskCols+` FROM agent_tasks t JOIN services s ON s.id = t.service_id
		WHERE t.token_hash = $1 AND t.status = 'running'`, hash))
	return t, nf(err)
}

// StaleTasks lists running tasks started before a deadline (cleaner, timeouts).
func (d *DB) StaleTasks(ctx context.Context, startedBefore any) ([]Task, error) {
	return d.tasks(ctx, `t.status = 'running' AND t.started_at < $1`, startedBefore)
}

var _ = pgx.ErrNoRows
