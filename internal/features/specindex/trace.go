package specindex

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/features/trace"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// traceAreas are the documents a projection reads.
var traceAreas = []string{"product", "arch", "tech", "qa"}

// projectIndexed projects the requirements, services and test cases of indexed
// features from their documents of the default branch, the same projection as
// of a feature in work (FTR.HMR.SPEC-0002 R15, tech §3.6).
//
// The fingerprint of a projection names the blobs of its documents and the keys
// of the catalog, so a feature is projected again when a document changes, is
// added or removed, or when the catalog gets a service it names; the first check
// after the upgrade projects every indexed feature. A failed feature is logged
// and retried by the next check: it does not fail the check of the repository.
func (s *Service) projectIndexed(ctx context.Context) error {
	// only the features whose fingerprint changed; the markdown is read for them alone
	rows, err := s.pool.Query(ctx, `
		WITH catalog AS (SELECT COALESCE(string_agg(key, ',' ORDER BY key), '') AS keys FROM services),
		fp AS (SELECT f.id, f.unique_id, COALESCE(f.trace_source, '') AS have,
			COALESCE((SELECT string_agg(sd.area || ':' || sd.blob_sha, ';' ORDER BY sd.area) FROM spec_documents sd
				WHERE sd.feature_key = f.unique_id AND sd.area = ANY($1)), '') || '|' || (SELECT keys FROM catalog) AS want
			FROM features f WHERE f.phase = 'indexed' AND f.deleted_at IS NULL)
		SELECT id, unique_id, want FROM fp WHERE want <> have ORDER BY unique_id`, traceAreas)
	if err != nil {
		return err
	}
	type todo struct {
		id     uuid.UUID
		key    string
		source string
	}
	var changed []todo
	for rows.Next() {
		var t todo
		if err := rows.Scan(&t.id, &t.key, &t.source); err != nil {
			rows.Close()
			return err
		}
		changed = append(changed, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range changed {
		if err := s.projectFeature(ctx, t.id, t.key, t.source); err != nil {
			slog.WarnContext(ctx, "indexed feature not projected", "feature", t.key, "err", err)
		}
	}
	return nil
}

// projectFeature projects one indexed feature when its fingerprint changed. A
// document missing in the index is projected as empty, so a removed qa clears
// the test cases. The phase is checked again under the lock of the row: a
// feature taken into work keeps the projection of its branch.
func (s *Service) projectFeature(ctx context.Context, id uuid.UUID, key, source string) error {
	return postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var have string
		err := tx.QueryRow(ctx, `SELECT COALESCE(trace_source, '') FROM features WHERE id = $1 AND phase = 'indexed' FOR UPDATE`, id).Scan(&have)
		if postgres.IsNoRows(err) || (err == nil && have == source) {
			return nil
		}
		if err != nil {
			return err
		}
		md := map[string]string{}
		rows, err := tx.Query(ctx, `SELECT area, markdown FROM spec_documents WHERE feature_key = $1 AND area = ANY($2)`, key, traceAreas)
		if err != nil {
			return err
		}
		for rows.Next() {
			var a, m string
			if err := rows.Scan(&a, &m); err != nil {
				rows.Close()
				return err
			}
			md[a] = m
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		product, arch, tech, qa := md["product"], md["arch"], md["tech"], md["qa"]
		res, err := trace.Project(ctx, tx, id, trace.Docs{Product: &product, Arch: &arch, Tech: &tech, QA: &qa})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE features SET trace_source = $2 WHERE id = $1`, id, source); err != nil {
			return err
		}
		slog.InfoContext(ctx, "indexed feature projected", "feature", key, "requirements", res.Requirements, "testCases", res.TestCases, "services", res.Services)
		return nil
	})
}
