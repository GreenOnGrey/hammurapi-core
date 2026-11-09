package specindex

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/GreenOnGrey/speclint/contract"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Config are the limits of the index (tech spec §7).
type Config struct {
	DefaultBranch     string
	PushDebounce      time.Duration // SPEC_SCAN_PUSH_DEBOUNCE
	PushDebounceMax   time.Duration // SPEC_SCAN_PUSH_DEBOUNCE_MAX
	MaxFileBytes      int64         // SPEC_SCAN_MAX_FILE_BYTES
	Timeout           time.Duration // SPEC_SCAN_TIMEOUT
	PreviewMaxBytes   int64         // SPEC_FILE_PREVIEW_MAX_BYTES
	SearchMaxLimit    int           // SPEC_SEARCH_MAX_LIMIT
	AgentReadMaxChars int           // SPEC_AGENT_READ_MAX_CHARS
	AgentPageSize     int           // SPEC_AGENT_PAGE_SIZE
}

func (c *Config) defaults() {
	if c.DefaultBranch == "" {
		c.DefaultBranch = "main"
	}
	if c.PushDebounce <= 0 {
		c.PushDebounce = 30 * time.Second
	}
	if c.PushDebounceMax <= 0 {
		c.PushDebounceMax = 5 * time.Minute
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = 2 << 20
	}
	if c.Timeout <= 0 {
		c.Timeout = 15 * time.Minute
	}
	if c.PreviewMaxBytes <= 0 {
		c.PreviewMaxBytes = 5 << 20
	}
	if c.SearchMaxLimit <= 0 {
		c.SearchMaxLimit = 50
	}
	if c.AgentReadMaxChars <= 0 {
		c.AgentReadMaxChars = 40000
	}
	if c.AgentPageSize <= 0 {
		c.AgentPageSize = 20
	}
}

// Service is the index of the specification repository.
type Service struct {
	pool   *pgxpool.Pool
	git    git.Provider // bound to the specification repository
	events events.Publisher
	cfg    Config
	now    func() time.Time
}

// NewService creates the service.
func NewService(pool *pgxpool.Pool, provider git.Provider, ev events.Publisher, cfg Config) *Service {
	cfg.defaults()
	return &Service{pool: pool, git: provider, events: ev, cfg: cfg, now: time.Now}
}

// ─── settings (R1) ──────────────────────────────────────────────────

// Intervals are the allowed periods of the check.
var Intervals = map[string]time.Duration{
	"15m": 15 * time.Minute, "30m": 30 * time.Minute, "1h": time.Hour, "3h": 3 * time.Hour,
	"6h": 6 * time.Hour, "12h": 12 * time.Hour, "24h": 24 * time.Hour,
}

// Settings is GET/PUT /admin/api/v1/spec-scan/settings.
type Settings struct {
	Interval string `json:"interval"`
	Branch   string `json:"branch"`
}

// Settings returns the period of the check.
func (s *Service) Settings(ctx context.Context, p *domain.Principal) (*Settings, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	iv, err := s.interval(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	return &Settings{Interval: iv, Branch: s.cfg.DefaultBranch}, nil
}

func (s *Service) interval(ctx context.Context, q postgres.Querier) (string, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT value FROM admin_settings WHERE key = 'spec_scan'`).Scan(&raw)
	if postgres.IsNoRows(err) {
		return "1h", nil
	}
	if err != nil {
		return "", err
	}
	var v struct {
		Interval string `json:"interval"`
	}
	if json.Unmarshal(raw, &v) != nil || Intervals[v.Interval] == 0 {
		return "1h", nil
	}
	return v.Interval, nil
}

// PutSettings changes the period (SCN-02, SCN-03).
func (s *Service) PutSettings(ctx context.Context, p *domain.Principal, in Settings) (*Settings, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	if Intervals[in.Interval] == 0 {
		return nil, apperr.Unprocessable("invalid_interval", "interval is one of 15m, 30m, 1h, 3h, 6h, 12h, 24h").With("field", "interval")
	}
	raw, _ := json.Marshal(map[string]string{"interval": in.Interval})
	if _, err := s.pool.Exec(ctx, `INSERT INTO admin_settings (key, value, updated_by, updated_at) VALUES ('spec_scan', $1, $2, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now()`, raw, p.UserID); err != nil {
		return nil, err
	}
	return &Settings{Interval: in.Interval, Branch: s.cfg.DefaultBranch}, nil
}

func requireGlobal(p *domain.Principal) error {
	if p == nil || !p.GlobalAdmin {
		return apperr.Forbidden("forbidden", "global administrator required")
	}
	return nil
}

func requireAnyAdmin(p *domain.Principal) error {
	if p == nil || !p.IsAnyAdmin() {
		return apperr.Forbidden("forbidden", "administrator role required")
	}
	return nil
}

// ─── queue (tech spec §3) ───────────────────────────────────────────

// Triggers of a check.
const (
	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
	TriggerCatalog  = "catalog"
	TriggerPush     = "push"
)

// Enqueue asks for a check. All requests fold into the single queued row:
// "Check now" and a new domain or system move it to now; pushes delay it by
// the debounce, but not later than the debounce limit after the first push.
// q may be the transaction that adds a domain or a system.
func (s *Service) Enqueue(ctx context.Context, q postgres.Querier, trigger string, by *uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	var err error
	switch trigger {
	case TriggerPush:
		d := s.cfg.PushDebounce.Seconds()
		m := s.cfg.PushDebounceMax.Seconds()
		err = q.QueryRow(ctx, `INSERT INTO spec_scan_runs (trigger, requested_by, not_before)
			VALUES ('push', $1, now() + make_interval(secs => $2))
			ON CONFLICT ((true)) WHERE status = 'queued' DO UPDATE SET not_before =
				LEAST(now() + make_interval(secs => $2), spec_scan_runs.created_at + make_interval(secs => $3))
				WHERE spec_scan_runs.not_before > now()
			RETURNING id`, by, d, m).Scan(&id)
		if postgres.IsNoRows(err) { // the queued run is already due
			err = q.QueryRow(ctx, `SELECT id FROM spec_scan_runs WHERE status = 'queued'`).Scan(&id)
		}
	default:
		err = q.QueryRow(ctx, `INSERT INTO spec_scan_runs (trigger, requested_by) VALUES ($1, $2)
			ON CONFLICT ((true)) WHERE status = 'queued' DO UPDATE SET not_before = LEAST(spec_scan_runs.not_before, now())
			RETURNING id`, trigger, by).Scan(&id)
	}
	return id, err
}

// RequestCatalog is called when a domain or a system appears (R6): an extra
// check indexes the specifications that waited for it.
func (s *Service) RequestCatalog(ctx context.Context, q postgres.Querier) error {
	_, err := s.Enqueue(ctx, q, TriggerCatalog, nil)
	return err
}

// scheduleDue queues a scheduled check when the period has passed since the
// last start and nothing is queued or running (SCN-01, SCN-07).
func (s *Service) scheduleDue(ctx context.Context) error {
	iv, err := s.interval(ctx, s.pool)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO spec_scan_runs (trigger)
		SELECT 'schedule' WHERE NOT EXISTS (SELECT 1 FROM spec_scan_runs WHERE status IN ('queued', 'running'))
		AND COALESCE((SELECT max(started_at) FROM spec_scan_runs), '-infinity') + make_interval(secs => $1) <= now()
		ON CONFLICT DO NOTHING`, Intervals[iv].Seconds())
	return err
}

const lockKey = `hashtext('hammurapi.spec_scan')`

// RunOnce takes a due queued check and runs it under the advisory lock; it
// returns false when there was nothing to do or another worker holds the lock
// (SCN-08).
func (s *Service) RunOnce(ctx context.Context) (bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+lockKey+`)`).Scan(&locked); err != nil || !locked {
		return false, err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(`+lockKey+`)`) }()
	// Runs left "running" by a worker that died are finished as failed.
	if _, err := conn.Exec(ctx, `UPDATE spec_scan_runs SET status = 'failed', finished_at = now(), error = 'interrupted'
		WHERE status = 'running'`); err != nil {
		return false, err
	}
	var id uuid.UUID
	var trigger string
	err = conn.QueryRow(ctx, `UPDATE spec_scan_runs SET status = 'running', started_at = now()
		WHERE id = (SELECT id FROM spec_scan_runs WHERE status = 'queued' AND not_before <= now() FOR UPDATE SKIP LOCKED)
		RETURNING id, trigger`).Scan(&id, &trigger)
	if postgres.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	rctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	s.scan(rctx, id, trigger) // records its own result (SCN-09, SCN-11)
	return true, nil
}

// Run is the worker loop: an index of another contract read again once, then the
// schedule every minute, the queue every few seconds.
func (s *Service) Run(ctx context.Context) error {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var lastSchedule time.Time
	backfilled := false
	for {
		if !backfilled { // tried on every tick until a check does not hold the lock
			n, err := s.ReindexContract(ctx)
			switch {
			case err == nil:
				backfilled = true
				if n > 0 {
					slog.InfoContext(ctx, "spec index read again by the document contract", "documents", n, "contract", contract.Version)
				}
			case !errors.Is(err, errScanBusy) && ctx.Err() == nil:
				logWarn(ctx, "spec index contract reindex", err)
			}
		}
		if s.now().Sub(lastSchedule) >= time.Minute {
			lastSchedule = s.now()
			if err := s.scheduleDue(ctx); err != nil && ctx.Err() == nil {
				logWarn(ctx, "spec scan schedule", err)
			}
		}
		for {
			ran, err := s.RunOnce(ctx)
			if err != nil && ctx.Err() == nil {
				logWarn(ctx, "spec scan", err)
			}
			if !ran || ctx.Err() != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
