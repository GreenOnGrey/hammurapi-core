//go:build integration

package itest

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/GreenOnGrey/hammurapi-core/migrations"
)

// migDB returns a fresh database at the given migration version.
func migDB(t *testing.T, version int64) *sql.DB {
	t.Helper()
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "mig")
	must(t, err)
	cfg, err := pgx.ParseConfig(url)
	must(t, err)
	db := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { db.Close(); drop() })
	goose.SetBaseFS(migrations.FS)
	must(t, goose.SetDialect("postgres"))
	must(t, goose.UpToContext(ctx, db, ".", version))
	return db
}

func count(t *testing.T, db *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	must(t, db.QueryRow(q, args...).Scan(&n))
	return n
}

func columns(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`SELECT column_name FROM information_schema.columns WHERE table_name = $1`, table)
	must(t, err)
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var c string
		must(t, rows.Scan(&c))
		out[c] = true
	}
	return out
}

// seedMVP fills the MVP schema (version 1) with a user, chat messages, an
// attachment of a message and usage of FTR.HMR.CMN-0002.
func seedMVP(t *testing.T, db *sql.DB, messages int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO users (id, provider_uid, username, display_name, agent_name, agent_tone) VALUES ('00000000-0000-0000-0000-000000000001', '1', 'anna', 'Anna', 'Hammurapi', 'business')`)
	must(t, err)
	for i := 0; i < messages; i++ {
		_, err = db.Exec(`INSERT INTO chat_messages (user_id, role, mode, content) VALUES ('00000000-0000-0000-0000-000000000001', 'user', 'general', $1)`, "message")
		must(t, err)
	}
	_, err = db.Exec(`INSERT INTO attachments (user_id, message_id, file_name, mime_type, size_bytes, s3_key)
		SELECT user_id, id, 'a.txt', 'text/plain', 1, 'attachments/a' FROM chat_messages LIMIT 1`)
	must(t, err)
	_, err = db.Exec(`INSERT INTO agent_usage (context, model, tokens_in, tokens_out) VALUES ('discovery', 'old-model', 100, 20)`)
	must(t, err)
}

// MIG-01, MIG-02, MIG-06: from the production schema with data to the new one.
func TestMigrationsFromMVP(t *testing.T) {
	db := migDB(t, 1)
	seedMVP(t, db, 25)
	must(t, goose.UpContext(context.Background(), db, "."))

	if n := count(t, db, `SELECT count(*) FROM chat_messages`); n != 0 {
		t.Fatalf("chat history kept: %d messages", n)
	}
	if n := count(t, db, `SELECT count(*) FROM attachments WHERE message_id IS NULL`); n != 1 {
		t.Fatalf("attachment link not cleared: %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM users`); n != 1 {
		t.Fatal("users changed")
	}
	// MIG-02: the existing agent_usage keeps its data and gets the new columns.
	if n := count(t, db, `SELECT count(*) FROM agent_usage WHERE model = 'old-model' AND tokens_in = 100 AND cost_usd = 0`); n != 1 {
		t.Fatal("agent_usage data lost")
	}
	cols := columns(t, db, "agent_usage")
	for _, c := range []string{"scenario", "connection_id", "cache_read", "cache_write", "cost_usd", "chat_session_id", "user_id", "model", "task_id"} {
		if !cols[c] {
			t.Errorf("agent_usage.%s missing", c)
		}
	}
	for _, c := range []string{"model", "connection_id", "error_class", "retry_of"} {
		if !columns(t, db, "chat_messages")[c] {
			t.Errorf("chat_messages.%s missing", c)
		}
	}
	// The MVP table of ACP sessions stays (contract migration of the next release).
	if !columns(t, db, "agent_sessions")["session_id"] || !columns(t, db, "pi_sessions")["snapshot_key"] {
		t.Fatal("session tables")
	}
	// CON-09 at the database level: a connection used by a scenario cannot be deleted.
	_, err := db.Exec(`INSERT INTO llm_connections (id, name, type, base_url, models, api_key_enc, api_key_last4)
		VALUES ('00000000-0000-0000-0000-0000000000c1', 'DeepSeek', 'deepseek', 'https://api.deepseek.com', '[]', '\x00', 'a1b2')`)
	must(t, err)
	_, err = db.Exec(`INSERT INTO agent_scenario_models (scenario, connection_id, model) VALUES ('codegen', '00000000-0000-0000-0000-0000000000c1', 'deepseek-v4-pro')`)
	must(t, err)
	if _, err := db.Exec(`DELETE FROM llm_connections WHERE id = '00000000-0000-0000-0000-0000000000c1'`); err == nil {
		t.Fatal("deleted a connection in use")
	}
	if _, err := db.Exec(`INSERT INTO mcp_servers (name, url) VALUES ('hammurapi', 'http://x')`); err == nil {
		t.Fatal("the built-in name was accepted")
	}

	// MIG-08: migrating again changes nothing.
	before, err := goose.GetDBVersion(db)
	must(t, err)
	must(t, goose.UpContext(context.Background(), db, "."))
	after, err := goose.GetDBVersion(db)
	must(t, err)
	if before != after || before != 15 {
		t.Fatalf("version %d → %d", before, after)
	}
}

// MIG-03: without agent_usage (a schema before FTR.HMR.CMN-0002) the table is created.
func TestAgentUsageCreatedWhenMissing(t *testing.T) {
	db := migDB(t, 4)
	_, err := db.Exec(`DROP TABLE agent_usage`)
	must(t, err)
	must(t, goose.UpToContext(context.Background(), db, ".", 5))
	cols := columns(t, db, "agent_usage")
	if !cols["context"] || !cols["scenario"] || !cols["cost_usd"] {
		t.Fatalf("columns %v", cols)
	}
}

// MIG-04: rolling back the expand migrations restores the schema; agent_usage stays.
func TestMigrationsRollback(t *testing.T) {
	db := migDB(t, 8)
	must(t, goose.DownToContext(context.Background(), db, ".", 1))
	for _, table := range []string{"llm_connections", "agent_scenario_models", "agent_default_model", "mcp_servers",
		"agent_skill_snapshots", "agent_config_changes", "pi_sessions"} {
		if n := count(t, db, `SELECT count(*) FROM information_schema.tables WHERE table_name = $1`, table); n != 0 {
			t.Errorf("%s left after rollback", table)
		}
	}
	cols := columns(t, db, "agent_usage")
	if !cols["model"] || !cols["user_id"] || cols["cost_usd"] || cols["scenario"] {
		t.Fatalf("agent_usage after rollback: %v", cols)
	}
	if columns(t, db, "chat_messages")["error_class"] {
		t.Fatal("chat_messages columns left")
	}
	// And forward again.
	must(t, goose.UpContext(context.Background(), db, "."))
}

// FTR.HMR.CMN-0005 MIG-01, MIG-02, MIG-04, MIG-07: the index migrations on a
// schema with a feature; existing features get source = hammurapi; the rollback
// removes tables and columns and keeps the 'indexed' enum value.
func TestSpecIndexMigrations(t *testing.T) {
	db := migDB(t, 8)
	seedMVP(t, db, 1)
	_, err := db.Exec(`WITH d AS (INSERT INTO domains (key, name) VALUES ('FMS', 'Fleet') RETURNING id),
		s AS (INSERT INTO systems (domain_id, key, name) SELECT id, 'CAR', 'Cars' FROM d RETURNING id)
		INSERT INTO features (unique_id, system_id, number, title, branch_name, pr_number, pr_url, created_by)
		SELECT 'FTR.FMS.CAR-0001', id, 1, 'Booking', 'feature/FTR.FMS.CAR-0001', 1, 'u', '00000000-0000-0000-0000-000000000001' FROM s`)
	must(t, err)
	must(t, goose.UpContext(context.Background(), db, "."))
	if n := count(t, db, `SELECT count(*) FROM features WHERE source = 'hammurapi' AND indexed_at IS NULL AND repo_deleted_at IS NULL`); n != 1 {
		t.Fatalf("existing feature: %d", n)
	}
	if n := count(t, db, `SELECT count(*) FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid WHERE t.typname = 'feature_phase' AND e.enumlabel = 'indexed'`); n != 1 {
		t.Fatal("no 'indexed' phase")
	}
	if n := count(t, db, `SELECT count(*) FROM admin_settings WHERE key = 'spec_scan' AND value->>'interval' = '1h'`); n != 1 {
		t.Fatal("no spec_scan setting")
	}
	for _, table := range []string{"spec_scan_runs", "spec_index_issues", "spec_documents", "spec_files", "spec_requirements", "spec_references"} {
		if n := count(t, db, `SELECT count(*) FROM information_schema.tables WHERE table_name = $1`, table); n != 1 {
			t.Errorf("%s missing", table)
		}
	}
	// One queued run at most.
	_, err = db.Exec(`INSERT INTO spec_scan_runs (trigger) VALUES ('manual')`)
	must(t, err)
	if _, err := db.Exec(`INSERT INTO spec_scan_runs (trigger) VALUES ('schedule')`); err == nil {
		t.Fatal("a second queued run was accepted")
	}

	must(t, goose.DownToContext(context.Background(), db, ".", 8))
	for _, table := range []string{"spec_scan_runs", "spec_index_issues", "spec_documents", "spec_files", "spec_requirements", "spec_references"} {
		if n := count(t, db, `SELECT count(*) FROM information_schema.tables WHERE table_name = $1`, table); n != 0 {
			t.Errorf("%s left after rollback", table)
		}
	}
	if cols := columns(t, db, "features"); cols["source"] || cols["indexed_at"] || cols["repo_deleted_at"] {
		t.Fatalf("features columns left: %v", cols)
	}
	if n := count(t, db, `SELECT count(*) FROM pg_enum e JOIN pg_type t ON t.oid = e.enumtypid WHERE e.enumlabel = 'indexed'`); n != 1 {
		t.Fatal("the enum value is kept on rollback")
	}
	must(t, goose.UpContext(context.Background(), db, "."))
}

// MIG-05 (scaled down): the chat reset deletes in batches.
func TestChatResetInBatches(t *testing.T) {
	db := migDB(t, 7)
	seedMVP(t, db, 25)
	old := migrations.ChatResetBatch
	migrations.ChatResetBatch = 4
	t.Cleanup(func() { migrations.ChatResetBatch = old })
	must(t, goose.UpContext(context.Background(), db, "."))
	if n := count(t, db, `SELECT count(*) FROM chat_messages`); n != 0 {
		t.Fatalf("%d messages left", n)
	}
}
