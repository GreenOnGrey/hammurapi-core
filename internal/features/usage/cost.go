package usage

// The cost of a feature (FTR.HMR.DEV-0004 R4–R7, R4a, tech §API): every run of
// an agent by stage — the research of its issues, the gates and the runs of the
// writer recorded before the attribution, the services, the checks — in money
// and tokens.

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Kinds of an item of the cost of a feature.
const (
	CostResearch = "research"
	CostArea     = "area"
	CostService  = "service"
	CostChecks   = "checks"
	CostLegacy   = "legacy"
)

// logRetention is how long Nabu keeps the events of a run (FTR.HMR.DEV-0004 R10).
const logRetention = 30 * 24 * time.Hour

var areaOrder = []string{"product", "design", "arch", "tech", "qa"}

// Tokens are the tokens of a run or a sum by kind.
type Tokens struct {
	In         float64 `json:"in"`
	Out        float64 `json:"out"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

func (t *Tokens) add(o Tokens, share float64) {
	t.In += o.In * share
	t.Out += o.Out * share
	t.CacheRead += o.CacheRead * share
	t.CacheWrite += o.CacheWrite * share
}

// Shared is the whole of a run of the writer split between gates (R4a).
type Shared struct {
	Areas   []string `json:"areas"`
	CostUSD *float64 `json:"costUsd"`
	Tokens  Tokens   `json:"tokens"`
}

// CostRun is one run of an agent in the detail of an item.
type CostRun struct {
	ID           string   `json:"id"`
	RunID        *string  `json:"runId"`
	At           string   `json:"at"`
	Reason       *string  `json:"reason"`
	Status       string   `json:"status"` // succeeded | failed | cancelled | running
	Model        string   `json:"model"`
	CostUSD      *float64 `json:"costUsd"` // nil — the price is unknown (R6)
	CostKnown    bool     `json:"costKnown"`
	Tokens       Tokens   `json:"tokens"`
	Shared       *Shared  `json:"shared,omitempty"`
	LogAvailable bool     `json:"logAvailable"`
}

// CostSum is the total of an item or of the feature.
type CostSum struct {
	CostUSD     float64 `json:"costUsd"`
	UnknownRuns int     `json:"unknownRuns"`
	Runs        int     `json:"runs"`
	Running     int     `json:"running"`
	Tokens      Tokens  `json:"tokens"`
}

// CostItem is the research, a gate, the runs of the writer before the attribution, a service or the checks.
type CostItem struct {
	Kind string  `json:"kind"`
	Key  *string `json:"key"`
	CostSum
	List []CostRun `json:"list,omitempty"`
}

// FeatureCost is the answer of GET /api/v1/features/{id}/cost.
type FeatureCost struct {
	Total      CostSum    `json:"total"`
	Items      []CostItem `json:"items"`
	CanViewLog bool       `json:"canViewLog"`
}

type costRow struct {
	id         uuid.UUID
	runID      *uuid.UUID
	at         time.Time
	reason     *string
	model      string
	costKnown  bool
	areas      []string
	service    *string
	scenario   *string
	context    string
	tokens     Tokens
	cost       float64
	taskStatus *string
	running    bool
}

// FeatureCost reads the cost of a feature; detail ("<kind>:<key>", "research",
// "checks", "legacy") adds the runs of one item.
func (s *Service) FeatureCost(ctx context.Context, featureID uuid.UUID, detail string, now time.Time) (*FeatureCost, error) {
	rows, err := s.Pool.Query(ctx, `SELECT u.id, u.nabu_run_id, u.created_at, u.reason, COALESCE(u.model, ''), u.cost_known, u.areas,
		COALESCE(s.key, ts.key), u.scenario::text, u.context, u.tokens_in, u.tokens_out, u.cache_read, u.cache_write, u.cost_usd::float8,
		t.status::text
		FROM agent_usage u
		LEFT JOIN services s ON s.id = u.service_id
		LEFT JOIN agent_tasks t ON t.id = u.task_id
		LEFT JOIN services ts ON ts.id = t.service_id
		WHERE u.feature_id = $1
			OR (u.feature_id IS NULL AND u.context = 'discovery' AND u.issue_id IN (SELECT issue_id FROM feature_issues WHERE feature_id = $1))
		ORDER BY u.created_at DESC`, featureID)
	if err != nil {
		return nil, err
	}
	var all []costRow
	for rows.Next() {
		var r costRow
		var in, out, cr, cw int64
		if err := rows.Scan(&r.id, &r.runID, &r.at, &r.reason, &r.model, &r.costKnown, &r.areas, &r.service, &r.scenario, &r.context,
			&in, &out, &cr, &cw, &r.cost, &r.taskStatus); err != nil {
			rows.Close()
			return nil, err
		}
		r.tokens = Tokens{In: float64(in), Out: float64(out), CacheRead: float64(cr), CacheWrite: float64(cw)}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	running, err := s.runningTasks(ctx, featureID)
	if err != nil {
		return nil, err
	}
	all = append(running, all...)
	return groupCost(all, detail, now), nil
}

// runningTasks are the code tasks of the feature still running: their usage
// so far, from the last poll of the run (arch §4.1).
func (s *Service) runningTasks(ctx context.Context, featureID uuid.UUID) ([]costRow, error) {
	rows, err := s.Pool.Query(ctx, `SELECT t.id, t.nabu_run_id, COALESCE(t.started_at, t.created_at), sv.key, t.usage
		FROM agent_tasks t JOIN services sv ON sv.id = t.service_id
		WHERE t.feature_id = $1 AND t.status IN ('queued', 'running') AND t.usage IS NOT NULL
		ORDER BY t.created_at DESC`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []costRow
	for rows.Next() {
		var r costRow
		var key string
		var raw []byte
		if err := rows.Scan(&r.id, &r.runID, &r.at, &key, &raw); err != nil {
			return nil, err
		}
		var u agent.Usage
		_ = json.Unmarshal(raw, &u)
		r.service, r.model, r.running, r.costKnown = &key, "nabu", true, u.PriceKnown()
		r.tokens = Tokens{In: float64(u.TokensIn), Out: float64(u.TokensOut), CacheRead: float64(u.CacheRead), CacheWrite: float64(u.CacheWrite)}
		r.cost = u.CostUSD
		out = append(out, r)
	}
	return out, rows.Err()
}

// itemsOf are the items a run counts in, with its share: a run of the writer
// for several gates is split evenly (R4a, arch A2). The analysis of an issue of
// the feature is its research (R7).
func itemsOf(r costRow) (keys []string, share float64) {
	switch {
	case r.context == "discovery":
		return []string{CostResearch}, 1
	case len(r.areas) > 0:
		for _, a := range r.areas {
			keys = append(keys, CostArea+":"+a)
		}
		return keys, 1 / float64(len(r.areas))
	case r.service != nil && (r.scenario == nil || *r.scenario != "conformance_check"):
		return []string{CostService + ":" + *r.service}, 1
	case r.context == "gate":
		return []string{CostLegacy}, 1
	}
	return []string{CostChecks}, 1
}

func statusOf(r costRow) string {
	switch {
	case r.running:
		return "running"
	case r.taskStatus != nil && (*r.taskStatus == "failed" || *r.taskStatus == "cancelled"):
		return *r.taskStatus
	}
	return "succeeded"
}

func (c *CostSum) add(r costRow, share float64) {
	c.Runs++
	if r.running {
		c.Running++
	}
	if r.costKnown {
		c.CostUSD += r.cost * share
	} else {
		c.UnknownRuns++
	}
	c.Tokens.add(r.tokens, share)
}

func groupCost(all []costRow, detail string, now time.Time) *FeatureCost {
	slices.SortStableFunc(all, func(a, b costRow) int { return b.at.Compare(a.at) }) // the newest run first
	out := &FeatureCost{Items: []CostItem{}}
	items := map[string]*CostItem{}
	for _, r := range all {
		out.Total.add(r, 1) // a shared run counts once in the total (R4a)
		keys, share := itemsOf(r)
		for _, k := range keys {
			it := items[k]
			if it == nil {
				kind, key, _ := strings.Cut(k, ":")
				it = &CostItem{Kind: kind}
				if key != "" {
					it.Key = &key
				}
				items[k] = it
			}
			it.add(r, share)
			if k == detail {
				it.List = append(it.List, costRun(r, share, now))
			}
		}
	}
	// the stages in their order: research, specification, the changes of the services, the check (R7)
	order := []string{CostResearch}
	for _, a := range areaOrder {
		order = append(order, CostArea+":"+a)
	}
	var services []string
	for k := range items {
		if strings.HasPrefix(k, CostService+":") {
			services = append(services, k)
		}
	}
	slices.Sort(services)
	order = append(order, CostLegacy)
	order = append(order, services...)
	order = append(order, CostChecks)
	for _, k := range order {
		if it := items[k]; it != nil {
			out.Items = append(out.Items, *it)
		}
	}
	return out
}

func costRun(r costRow, share float64, now time.Time) CostRun {
	c := CostRun{ID: r.id.String(), At: r.at.UTC().Format(time.RFC3339), Reason: r.reason, Status: statusOf(r), Model: r.model,
		CostKnown: r.costKnown, LogAvailable: r.runID != nil && now.Sub(r.at) < logRetention}
	if r.runID != nil {
		id := r.runID.String()
		c.RunID = &id
	}
	c.Tokens.add(r.tokens, share)
	if r.costKnown {
		v := r.cost * share
		c.CostUSD = &v
	}
	if len(r.areas) > 1 {
		sh := &Shared{Areas: r.areas}
		sh.Tokens.add(r.tokens, 1)
		if r.costKnown {
			v := r.cost
			sh.CostUSD = &v
		}
		c.Shared = sh
	}
	return c
}

// CostRoutes mounts GET /features/{uniqueId}/cost under /api/v1: anyone who
// sees the feature sees its cost (R4); canViewLog — its experts and the
// administrators (R10).
func (s *Service) CostRoutes(r chi.Router) {
	r.Get("/features/{uniqueId}/cost", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var id uuid.UUID
		var domainKey string
		err = s.Pool.QueryRow(r.Context(), `SELECT f.id, d.key FROM features f JOIN systems sy ON sy.id = f.system_id
			JOIN domains d ON d.id = sy.domain_id WHERE f.unique_id = $1 AND f.deleted_at IS NULL`, chi.URLParam(r, "uniqueId")).Scan(&id, &domainKey)
		if postgres.IsNoRows(err) {
			return apperr.NotFound("feature_not_found", "feature not found")
		}
		if err != nil {
			return err
		}
		out, err := s.FeatureCost(r.Context(), id, r.URL.Query().Get("detail"), time.Now())
		if err != nil {
			return err
		}
		out.CanViewLog = p.GlobalAdmin || p.IsExpertOf(domainKey)
		httpx.JSON(w, http.StatusOK, out)
		return nil
	}))
}
