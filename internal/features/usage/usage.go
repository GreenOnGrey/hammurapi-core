// Package usage keeps the usage of agents for reports (FTR.HMR.CMN-0007 R3:
// the history stays in Hammurapi after the agent left it): every scenario
// and code task writes a row of agent_usage; global administrators read the
// report of the page «Usage».
package usage

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
)

// Service writes and reads agent_usage.
type Service struct{ Pool *pgxpool.Pool }

func requireGlobal(p *domain.Principal) error {
	if p == nil || !p.GlobalAdmin {
		return apperr.Forbidden("forbidden", "the usage of agents is available to global administrators only")
	}
	return nil
}

// Routes mounts GET /agent/usage under /admin/api/v1.
func (s *Service) Routes(r chi.Router) {
	r.Get("/agent/usage", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		q := r.URL.Query()
		to := time.Now()
		from := to.AddDate(0, 0, -7)
		if v := q.Get("from"); v != "" {
			if from, err = time.Parse(time.RFC3339, v); err != nil {
				return apperr.BadRequest("invalid_period", "from must be RFC 3339")
			}
		}
		if v := q.Get("to"); v != "" {
			if to, err = time.Parse(time.RFC3339, v); err != nil {
				return apperr.BadRequest("invalid_period", "to must be RFC 3339")
			}
		}
		var groups []string
		for _, g := range strings.Split(q.Get("groupBy"), ",") {
			if g = strings.TrimSpace(g); g != "" {
				groups = append(groups, g)
			}
		}
		rep, err := s.Report(r.Context(), p, from, to, groups)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusOK, rep)
		return nil
	}))
}

// ─── usage (R19) ────────────────────────────────────────────────────

// Record is one row of agent_usage.
type Record struct {
	Scenario     agent.Scenario
	Context      string // free-form context: chat, discovery, gategen, check, task
	ConnectionID *uuid.UUID
	Model        string
	UserID       *uuid.UUID
	IssueID      *uuid.UUID
	FeatureID    *uuid.UUID
	ReleaseID    *uuid.UUID
	TaskID       *uuid.UUID
	agent.Usage
}

// Add stores usage; zero usage is skipped.
func (s *Service) Add(ctx context.Context, r Record) error {
	if r.IsZero() {
		return nil
	}
	if r.Context == "" {
		r.Context = string(r.Scenario)
	}
	_, err := s.Pool.Exec(ctx, `INSERT INTO agent_usage (context, scenario, connection_id, model, user_id, issue_id, feature_id, release_id, task_id,
		tokens_in, tokens_out, cache_read, cache_write, cost_usd) VALUES ($1,$2::agent_scenario,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		r.Context, string(r.Scenario), r.ConnectionID, r.Model, r.UserID, r.IssueID, r.FeatureID, r.ReleaseID, r.TaskID,
		r.TokensIn, r.TokensOut, r.CacheRead, r.CacheWrite, r.CostUSD)
	return err
}

// Totals are the four indicators of the Usage page.
type Totals struct {
	CostUSD    float64 `json:"costUsd"`
	TokensIn   int64   `json:"tokensIn"`
	TokensOut  int64   `json:"tokensOut"`
	CacheRead  int64   `json:"cacheRead"`
	CacheWrite int64   `json:"cacheWrite"`
	Runs       int64   `json:"runs"`
}

// Row is a group of the report.
type Row struct {
	Key       map[string]string `json:"key"`
	CostUSD   float64           `json:"costUsd"`
	TokensIn  int64             `json:"tokensIn"`
	TokensOut int64             `json:"tokensOut"`
	CacheRead int64             `json:"cacheRead"`
	Runs      int64             `json:"runs"`
}

// Report is GET /usage.
type Report struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Totals Totals    `json:"totals"`
	Rows   []Row     `json:"rows"`
}

var groupColumns = map[string]string{
	"scenario":   "COALESCE(u.scenario::text, u.context)",
	"connection": "COALESCE(c.name, '')",
	"model":      "COALESCE(u.model, '')",
	"day":        "to_char(date_trunc('day', u.created_at), 'YYYY-MM-DD')",
}

// Usage aggregates usage over [from, to) grouped by any of scenario,
// connection, model, day (USE-04).
func (s *Service) Report(ctx context.Context, p *domain.Principal, from, to time.Time, groupBy []string) (*Report, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	if !to.After(from) {
		return nil, apperr.Unprocessable("invalid_period", "to must be after from")
	}
	var cols, keys []string
	for _, g := range groupBy {
		col, ok := groupColumns[g]
		if !ok {
			return nil, apperr.Unprocessable("invalid_group", "groupBy is scenario, connection, model or day")
		}
		cols, keys = append(cols, col), append(keys, g)
	}
	rep := &Report{From: from, To: to, Rows: []Row{}}
	const sums = `COALESCE(sum(u.cost_usd),0)::float8, COALESCE(sum(u.tokens_in),0), COALESCE(sum(u.tokens_out),0),
		COALESCE(sum(u.cache_read),0), COALESCE(sum(u.cache_write),0), count(*)`
	const from_ = ` FROM agent_usage u LEFT JOIN llm_connections c ON c.id = u.connection_id WHERE u.created_at >= $1 AND u.created_at < $2`
	if err := s.Pool.QueryRow(ctx, `SELECT `+sums+from_, from, to).Scan(&rep.Totals.CostUSD, &rep.Totals.TokensIn, &rep.Totals.TokensOut,
		&rep.Totals.CacheRead, &rep.Totals.CacheWrite, &rep.Totals.Runs); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return rep, nil
	}
	sel := make([]string, len(cols))
	for i, c := range cols {
		sel[i] = c + " AS g" + fmt.Sprint(i)
	}
	grp := make([]string, len(cols))
	for i := range cols {
		grp[i] = fmt.Sprint(i + 1)
	}
	q := `SELECT ` + strings.Join(sel, ", ") + `, ` + sums + from_ + ` GROUP BY ` + strings.Join(grp, ", ") +
		` ORDER BY ` + fmt.Sprint(len(cols)+1) + ` DESC`
	rows, err := s.Pool.Query(ctx, q, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		vals := make([]string, len(cols))
		dest := make([]any, 0, len(cols)+6)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		var r Row
		var cacheWrite int64
		dest = append(dest, &r.CostUSD, &r.TokensIn, &r.TokensOut, &r.CacheRead, &cacheWrite, &r.Runs)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		r.Key = map[string]string{}
		for i, k := range keys {
			r.Key[k] = vals[i]
		}
		rep.Rows = append(rep.Rows, r)
	}
	return rep, rows.Err()
}
