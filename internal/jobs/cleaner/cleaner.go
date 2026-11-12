// Package cleaner is the daily maintenance job (CronJob `hammurapi cleaner`):
// expired attachments, sessions, locks, unconfirmed imports and branches whose
// deletion failed when a feature was deleted.
package cleaner

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/features/admin"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/storage"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// ImportTTL is how long an unconfirmed import is kept.
const ImportTTL = 24 * time.Hour

// Cleaner runs the maintenance steps.
type Cleaner struct {
	Pool   *pgxpool.Pool
	S3     storage.Storage
	Store  specdata.Store
	Git    git.Provider
	Tokens git.TokenSource
	Now    func() time.Time
}

// Report summarizes a run.
type Report struct {
	Attachments int
	Sessions    int64
	Locks       int64
	Imports     int
	Branches    int
	// Archives of changes of code tasks no longer needed for «Повторить коммит» (FTR.HMR.DEV-0004 R13).
	Archives int
}

// Run executes every step; a failing step is logged and does not stop the others.
func (c *Cleaner) Run(ctx context.Context) (Report, error) {
	if c.Now == nil {
		c.Now = time.Now
	}
	var rep Report
	var firstErr error
	note := func(step string, err error) {
		if err != nil {
			slog.ErrorContext(ctx, "cleaner step failed", "step", step, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	var err error
	rep.Attachments, err = c.attachments(ctx)
	note("attachments", err)
	rep.Sessions, err = c.exec(ctx, `DELETE FROM user_sessions WHERE expires_at < $1`)
	note("sessions", err)
	rep.Locks, err = c.exec(ctx, `DELETE FROM feature_locks WHERE expires_at < $1`)
	note("locks", err)
	rep.Imports, err = c.imports(ctx)
	note("imports", err)
	rep.Branches, err = c.branches(ctx)
	note("branches", err)
	rep.Archives, err = c.archives(ctx)
	note("archives", err)
	slog.InfoContext(ctx, "cleaner finished", "attachments", rep.Attachments, "sessions", rep.Sessions,
		"locks", rep.Locks, "imports", rep.Imports, "branches", rep.Branches, "archives", rep.Archives)
	return rep, firstErr
}

func (c *Cleaner) exec(ctx context.Context, sql string) (int64, error) {
	tag, err := c.Pool.Exec(ctx, sql, c.Now())
	return tag.RowsAffected(), err
}

// attachments deletes files older than the retention setting (object + row).
func (c *Cleaner) attachments(ctx context.Context) (int, error) {
	days, err := admin.RetentionDays(ctx, c.Pool)
	if err != nil {
		return 0, err
	}
	cutoff := c.Now().Add(-time.Duration(days) * 24 * time.Hour)
	n := 0
	for {
		rows, err := c.Pool.Query(ctx, `SELECT id, s3_key FROM attachments WHERE created_at < $1 LIMIT 500`, cutoff)
		if err != nil {
			return n, err
		}
		type item struct {
			id  uuid.UUID
			key string
		}
		var batch []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.key); err != nil {
				rows.Close()
				return n, err
			}
			batch = append(batch, it)
		}
		rows.Close()
		if len(batch) == 0 {
			return n, rows.Err()
		}
		for _, it := range batch {
			if err := c.S3.Delete(ctx, it.key); err != nil {
				return n, err
			}
			if _, err := c.Pool.Exec(ctx, `DELETE FROM attachments WHERE id = $1`, it.id); err != nil {
				return n, err
			}
			n++
		}
	}
}

// imports removes unconfirmed jobs older than a day with their archives.
func (c *Cleaner) imports(ctx context.Context) (int, error) {
	rows, err := c.Pool.Query(ctx, `SELECT id, s3_key FROM imports WHERE status = 'validated' AND created_at < $1`, c.Now().Add(-ImportTTL))
	if err != nil {
		return 0, err
	}
	type item struct {
		id  uuid.UUID
		key string
	}
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.key); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, it)
	}
	rows.Close()
	n := 0
	for _, it := range items {
		if err := c.S3.Delete(ctx, it.key); err != nil {
			slog.WarnContext(ctx, "delete import archive", "key", it.key, "err", err)
		}
		if _, err := c.Pool.Exec(ctx, `DELETE FROM imports WHERE id = $1`, it.id); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// branches retries branch deletion of deleted features with the deleting user's token.
func (c *Cleaner) branches(ctx context.Context) (int, error) {
	fs, err := c.Store.FeaturesPendingCleanup(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range fs {
		if f.DeletedBy == nil {
			continue
		}
		token, err := c.Tokens.Token(ctx, *f.DeletedBy)
		if err != nil {
			slog.WarnContext(ctx, "no token to delete branch", "feature", f.UniqueID, "err", err)
			continue
		}
		if err := c.Git.DeleteBranch(ctx, token, f.Branch); err != nil {
			slog.WarnContext(ctx, "branch deletion failed again", "feature", f.UniqueID, "err", err)
			continue
		}
		if err := c.Store.SetBranchCleanupPending(ctx, f.ID, false); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// archives deletes the kept changes of code tasks that no longer need them
// (FTR.HMR.DEV-0004 R13, arch §4.5): a task that succeeded, a task replaced by
// a newer one of its service and feature, or a kept term that has passed.
func (c *Cleaner) archives(ctx context.Context) (int, error) {
	rows, err := c.Pool.Query(ctx, `SELECT t.id, t.changes_key FROM agent_tasks t
		WHERE t.changes_key IS NOT NULL AND t.status NOT IN ('queued', 'running') AND (
		  t.status = 'succeeded' OR t.changes_until < $1 OR EXISTS (
		    SELECT 1 FROM agent_tasks n WHERE n.feature_id = t.feature_id AND n.service_id = t.service_id AND n.created_at > t.created_at))`, c.Now())
	if err != nil {
		return 0, err
	}
	type kept struct {
		id  uuid.UUID
		key string
	}
	var all []kept
	for rows.Next() {
		var k kept
		if err := rows.Scan(&k.id, &k.key); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, k := range all {
		if c.S3 != nil {
			if err := c.S3.Delete(ctx, k.key); err != nil {
				return n, err
			}
		}
		if _, err := c.Pool.Exec(ctx, `UPDATE agent_tasks SET changes_key = NULL, changes_until = NULL WHERE id = $1`, k.id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
