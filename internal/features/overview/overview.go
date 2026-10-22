// Package overview serves the "General" section (FTR.HMR.CMN-0002 R37): "In focus"
// — what waits for the user's decision now, by stage, longest waiting first —
// and "Overview" — all active issues, features and releases in three columns.
package overview

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
)

// Item is an element of "In focus".
type Item struct {
	Kind         string    `json:"kind"` // issue | feature | release
	Key          string    `json:"key"`
	Title        string    `json:"title"`
	Action       string    `json:"action"`
	WaitingSince time.Time `json:"waitingSince"`
	Hint         *string   `json:"hint,omitempty"`
}

// Focus is GET /focus.
type Focus struct {
	Research    []Item `json:"research"`
	Development []Item `json:"development"`
	Release     []Item `json:"release"`
	// Agent is shown to global administrators: the agent is not configured,
	// an LLM connection or an MCP server needs attention (FTR.HMR.CMN-0004 R21).
	Agent []Item `json:"agent"`
	// Spec is shown to global administrators: problems of indexing the
	// specification repository (FTR.HMR.CMN-0005 R10).
	Spec []Item `json:"spec"`
}

// Service implements the General section.
type Service struct {
	pool *pgxpool.Pool
	// AgentFocus returns the Agent group; nil disables it.
	AgentFocus func(ctx context.Context) ([]Item, error)
	// SpecFocus returns the Specification group; nil disables it.
	SpecFocus func(ctx context.Context) ([]Item, error)
}

// NewService creates the service.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) collect(ctx context.Context, sql string, args ...any) ([]Item, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.Kind, &it.Key, &it.Title, &it.Action, &it.WaitingSince, &it.Hint); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// expertDomains: the domain ids where the user is an expert (any kind).
const expertDomains = `(SELECT domain_id FROM domain_experts WHERE user_id = $1)`

// Focus builds "In focus" for the user (NAV-02).
func (s *Service) Focus(ctx context.Context, p *domain.Principal) (*Focus, error) {
	uid := p.UserID
	f := &Focus{Research: []Item{}, Development: []Item{}, Release: []Item{}, Agent: []Item{}, Spec: []Item{}}
	if p.GlobalAdmin && s.AgentFocus != nil {
		items, err := s.AgentFocus(ctx)
		if err != nil {
			return nil, err
		}
		f.Agent = append(f.Agent, items...)
	}
	if p.GlobalAdmin && s.SpecFocus != nil {
		items, err := s.SpecFocus(ctx)
		if err != nil {
			return nil, err
		}
		f.Spec = append(f.Spec, items...)
	}
	research, err := s.collect(ctx, `
		SELECT 'issue', i.key, i.title, 'verify_discovery', i.updated_at, NULL::text
		FROM issues i WHERE i.status = 'verification' AND i.domain_id IN `+expertDomains+`
		UNION ALL
		SELECT 'issue', i.key, i.title, 'resolve_blocked', w.updated_at, w.last_error
		FROM workflow_runs w JOIN issues i ON i.id = w.subject_id
		WHERE w.kind = 'discovery' AND w.state = 'blocked' AND i.domain_id IN `+expertDomains, uid)
	if err != nil {
		return nil, err
	}
	f.Research = append(f.Research, research...)
	dev, err := s.collect(ctx, `
		SELECT 'feature', f.unique_id, f.title, 'approve_gate', g.submitted_at, g.area::text
		FROM gates g JOIN features f ON f.id = g.feature_id JOIN systems s ON s.id = f.system_id JOIN domains d ON d.id = s.domain_id
		JOIN domain_experts e ON e.domain_id = d.id AND e.user_id = $1
		 AND e.kind = CASE WHEN g.area IN ('product','design') THEN 'product'::expert_kind ELSE 'technical'::expert_kind END
		WHERE g.status = 'in_review' AND g.deleted_at IS NULL AND f.phase = 'spec' AND d.approval_required
		UNION ALL
		SELECT DISTINCT 'feature', f.unique_id, f.title, 'sign_validation', w.updated_at, e.kind::text
		FROM features f JOIN systems s ON s.id = f.system_id
		JOIN domain_experts e ON e.domain_id = s.domain_id AND e.user_id = $1
		JOIN workflow_runs w ON w.subject_id = f.id AND w.kind = 'validation' AND w.state = 'awaiting_signatures'
		WHERE f.phase = 'validation'
		  AND NOT EXISTS (SELECT 1 FROM validation_signatures v WHERE v.feature_id = f.id AND v.side = e.kind)
		UNION ALL
		SELECT 'feature', f.unique_id, f.title, 'resolve_blocked', w.updated_at, w.last_error
		FROM workflow_runs w JOIN features f ON f.id = w.subject_id JOIN systems s ON s.id = f.system_id
		WHERE w.kind IN ('codegen','gate_generation','validation') AND w.state = 'blocked' AND s.domain_id IN `+expertDomains, uid)
	if err != nil {
		return nil, err
	}
	f.Development = append(f.Development, dev...)
	rel, err := s.collect(ctx, `
		SELECT 'release', r.key, f.title,
			CASE
				WHEN w.state = 'blocked' THEN 'retry_or_rollback'
				WHEN w.step = 'awaiting_start' THEN 'start_merge'
				WHEN w.step = 'deploy_wait' THEN 'mark_deploy'
				WHEN w.step = 'flags_wait' THEN 'mark_flag'
				WHEN w.step = 'awaiting_confirmation' THEN 'confirm_release'
			END, w.updated_at,
			CASE WHEN w.state = 'blocked' THEN w.last_error WHEN w.kind = 'rollback' AND w.step = 'flags_wait' THEN 'off' END
		FROM workflow_runs w JOIN releases r ON r.id = w.subject_id JOIN features f ON f.id = r.feature_id JOIN systems s ON s.id = r.system_id
		WHERE w.kind IN ('release','rollback') AND w.state NOT IN ('done','succeeded','failed','cancelled','rolled_back','rolling_back')
		  AND (w.state = 'blocked' OR w.step IN ('awaiting_start','flags_wait','awaiting_confirmation')
		       OR (w.step = 'deploy_wait' AND EXISTS (SELECT 1 FROM domain_experts e WHERE e.domain_id = s.domain_id AND e.user_id = $1 AND e.kind = 'technical')
		           AND NOT EXISTS (SELECT 1 FROM deploy_settings ds WHERE ds.environment = 'production')))
		  AND s.domain_id IN `+expertDomains, uid)
	if err != nil {
		return nil, err
	}
	f.Release = append(f.Release, rel...)
	for _, l := range [][]Item{f.Research, f.Development, f.Release} {
		sort.SliceStable(l, func(i, j int) bool { return l[i].WaitingSince.Before(l[j].WaitingSince) })
	}
	return f, nil
}

// Card is an element of the Overview columns.
type Card struct {
	Key     string   `json:"key"`
	Title   string   `json:"title"`
	Type    string   `json:"type,omitempty"`
	Status  string   `json:"status"`
	Feature *string  `json:"feature,omitempty"`
	Issues  []string `json:"issues"`
	Step    *string  `json:"step,omitempty"`
	Done    int      `json:"done"`
	Total   int      `json:"total"`
	Blocked bool     `json:"blocked"`
	Domain  string   `json:"domain"`
	// Estimate is the days and person-hours of the draft estimate of an
	// issue (FTR.HMR.DISC-0001 R17); the cost of agents is not on cards.
	Estimate *CardEstimate `json:"estimate,omitempty"`
}

// CardEstimate is the estimate on the card of an issue.
type CardEstimate struct {
	Days        int     `json:"days"`
	PersonHours float64 `json:"personHours"`
}

// Overview is GET /overview.
type Overview struct {
	Issues   []Card `json:"issues"`
	Features []Card `json:"features"`
	Releases []Card `json:"releases"`
}

func domainFilter(alias string, arg int) string {
	return ` AND (` + "$" + itoa(arg) + ` = 'all' OR (` + "$" + itoa(arg) + ` = 'mine' AND ` + alias + ` IN (SELECT domain_id FROM domain_experts WHERE user_id = $1
		UNION SELECT domain_id FROM user_domains WHERE user_id = $1)) OR ` + alias + ` = (SELECT id FROM domains WHERE key = ` + "$" + itoa(arg) + `))`
}

func itoa(i int) string { return string(rune('0' + i)) }

// Overview builds the three columns (NAV-04, NAV-05).
func (s *Service) Overview(ctx context.Context, userID uuid.UUID, dom string) (*Overview, error) {
	if dom == "" {
		dom = "mine"
	}
	out := &Overview{Issues: []Card{}, Features: []Card{}, Releases: []Card{}}
	rows, err := s.pool.Query(ctx, `SELECT i.key, i.title, i.type::text, i.status::text, d.key,
			EXISTS (SELECT 1 FROM workflow_runs w WHERE w.subject_id = i.id AND w.kind = 'discovery' AND w.state = 'blocked'),
			(dd.estimate->>'days')::int,
			(SELECT sum((p->>'personHours')::numeric)::float8 FROM jsonb_array_elements(dd.estimate->'people') p)
		FROM issues i JOIN domains d ON d.id = i.domain_id LEFT JOIN discovery_docs dd ON dd.issue_id = i.id
		WHERE i.status IN ('new','discovery','verification')`+domainFilter("i.domain_id", 2)+` ORDER BY i.created_at DESC LIMIT 200`, userID, dom)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		c := Card{Issues: []string{}}
		var days *int
		var hours *float64
		if err := rows.Scan(&c.Key, &c.Title, &c.Type, &c.Status, &c.Domain, &c.Blocked, &days, &hours); err != nil {
			rows.Close()
			return nil, err
		}
		if days != nil {
			c.Estimate = &CardEstimate{Days: *days}
			if hours != nil {
				c.Estimate.PersonHours = *hours
			}
		}
		out.Issues = append(out.Issues, c)
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT f.unique_id, f.title, f.phase::text, d.key,
			COALESCE((SELECT array_agg(i.key ORDER BY i.key) FROM feature_issues fi JOIN issues i ON i.id = fi.issue_id WHERE fi.feature_id = f.id), '{}'),
			CASE f.phase
				WHEN 'spec' THEN (SELECT count(*) FROM gates g WHERE g.feature_id = f.id AND g.deleted_at IS NULL AND g.status = 'approved')
				WHEN 'codegen' THEN (SELECT count(DISTINCT p.service_id) FROM pull_requests p WHERE p.feature_id = f.id AND p.kind = 'service' AND p.state = 'open')
				ELSE (SELECT count(*) FROM validation_signatures v WHERE v.feature_id = f.id) END,
			CASE f.phase
				WHEN 'spec' THEN (SELECT count(*) FROM gates g WHERE g.feature_id = f.id AND g.deleted_at IS NULL) + CASE WHEN EXISTS (SELECT 1 FROM gates g WHERE g.feature_id = f.id AND g.area = 'tech' AND g.deleted_at IS NULL) THEN 0 ELSE 2 END
				WHEN 'codegen' THEN (SELECT count(*) FROM feature_services fs WHERE fs.feature_id = f.id)
				ELSE 2 END,
			EXISTS (SELECT 1 FROM workflow_runs w WHERE w.subject_id = f.id AND w.state = 'blocked')
		FROM features f JOIN systems s ON s.id = f.system_id JOIN domains d ON d.id = s.domain_id
		WHERE f.phase IN ('spec','codegen','validation')`+domainFilter("d.id", 2)+` ORDER BY f.created_at DESC LIMIT 200`, userID, dom)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c Card
		if err := rows.Scan(&c.Key, &c.Title, &c.Status, &c.Domain, &c.Issues, &c.Done, &c.Total, &c.Blocked); err != nil {
			rows.Close()
			return nil, err
		}
		out.Features = append(out.Features, c)
	}
	rows.Close()
	rows, err = s.pool.Query(ctx, `SELECT r.key, f.title, r.status::text, d.key, f.unique_id,
			COALESCE((SELECT array_agg(i.key ORDER BY i.key) FROM feature_issues fi JOIN issues i ON i.id = fi.issue_id WHERE fi.feature_id = f.id), '{}'),
			w.step, COALESCE((w.context->>'idx')::int, 0), COALESCE(jsonb_array_length(r.plan->'order'), 0), COALESCE(w.state = 'blocked', false)
		FROM releases r JOIN features f ON f.id = r.feature_id JOIN systems s ON s.id = r.system_id JOIN domains d ON d.id = s.domain_id
		LEFT JOIN workflow_runs w ON w.subject_id = r.id AND w.kind = 'release'
		WHERE r.status NOT IN ('succeeded','rolled_back')`+domainFilter("d.id", 2)+` ORDER BY r.created_at DESC LIMIT 200`, userID, dom)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Card
		var feature string
		if err := rows.Scan(&c.Key, &c.Title, &c.Status, &c.Domain, &feature, &c.Issues, &c.Step, &c.Done, &c.Total, &c.Blocked); err != nil {
			return nil, err
		}
		c.Feature = &feature
		out.Releases = append(out.Releases, c)
	}
	return out, rows.Err()
}

// Routes mounts /focus and /overview.
func (s *Service) Routes(r chi.Router) {
	r.Get("/focus", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		f, err := s.Focus(r.Context(), p)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, f)
		return nil
	}))
	r.Get("/overview", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		o, err := s.Overview(r.Context(), p.UserID, r.URL.Query().Get("domain"))
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, o)
		return nil
	}))
}
