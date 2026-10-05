// Package nabuconn connects Hammurapi to Nabu (FTR.HMR.CMN-0006 R4–R11): the
// settings of the connection (bindings of scenarios to service agents of Nabu,
// the mark of the transfer), the transfer of the agent settings of
// FTR.HMR.CMN-0004, the proxy of the chat to the user's personal agent and the
// authentication of Nabu's calls to Hammurapi's MCP.
package nabuconn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/crypto"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// ServiceScenarios are the scenarios service agents of Nabu perform; the chat
// is the personal agent and needs no binding (tech §3.3).
var ServiceScenarios = []agent.Scenario{agent.ScenarioIssueAnalysis, agent.ScenarioGateGeneration, agent.ScenarioConformanceCheck,
	agent.ScenarioCodegen, agent.ScenarioReviewUpdate, agent.ScenarioRollbackRevert}

// Settings are admin_settings.nabu.
type Settings struct {
	Scenarios map[string]string `json:"scenarios"`
	Migrated  bool              `json:"migrated"`
}

// Service is the connection to Nabu; Client is nil without NABU_URL (R9).
type Service struct {
	Pool   *pgxpool.Pool
	Client *nabu.Client
	Box    *crypto.Box
	// SkillsRepo is the specification repository whose /agent/skills/ go to Nabu.
	SkillsRepo, DefaultBranch string
}

// Enabled reports whether the agent of Nabu is connected (R4).
func (s *Service) Enabled() bool { return s != nil && s.Client != nil }

// ErrDisabled is agent_disabled (tech §8): an action of the agent without Nabu.
func ErrDisabled() error { return apperr.AgentDisabled() }

// ErrUnavailable is nabu_unavailable.
func ErrUnavailable() error {
	return apperr.Unavailable("nabu_unavailable", "the agent is temporarily unavailable")
}

// MapErr turns errors of Nabu into errors of Hammurapi with the same codes (tech §3.2).
func MapErr(err error) error {
	var ae *nabu.APIError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, nabu.ErrUnavailable):
		return ErrUnavailable()
	case errors.As(err, &ae):
		if ae.Status >= 500 {
			return ErrUnavailable()
		}
		e := apperr.New(ae.Status, ae.Code, ae.Message)
		e.Details = ae.Details
		return e
	}
	return err
}

// Get reads the settings.
func (s *Service) Get(ctx context.Context) (Settings, error) {
	var raw []byte
	st := Settings{Scenarios: map[string]string{}}
	err := s.Pool.QueryRow(ctx, `SELECT value FROM admin_settings WHERE key = 'nabu'`).Scan(&raw)
	if postgres.IsNoRows(err) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, err
	}
	if st.Scenarios == nil {
		st.Scenarios = map[string]string{}
	}
	return st, nil
}

func (s *Service) put(ctx context.Context, st Settings) error {
	b, _ := json.Marshal(st)
	_, err := s.Pool.Exec(ctx, `INSERT INTO admin_settings (key, value) VALUES ('nabu', $1)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, b)
	return err
}

// AgentFor returns the service agent of Nabu bound to a scenario ("" — none).
func (s *Service) AgentFor(ctx context.Context, sc agent.Scenario) string {
	st, err := s.Get(ctx)
	if err != nil {
		return ""
	}
	return st.Scenarios[string(sc)]
}

// UsesNabu reports whether scenarios run in Nabu: connected and either
// transferred or at least one scenario bound (the operator of
// FTR.HMR.CMN-0004 serves the others until the transfer).
func (s *Service) UsesNabu(ctx context.Context, sc agent.Scenario) bool {
	return s.Enabled() && s.AgentFor(ctx, sc) != ""
}

// View is GET /admin/api/v1/nabu (tech §3.3).
type View struct {
	Connected bool              `json:"connected"`
	URL       string            `json:"url"`
	Error     string            `json:"error,omitempty"`
	Agents    []nabu.Agent      `json:"agents"`
	Scenarios map[string]string `json:"scenarios"`
	Migrated  bool              `json:"migrated"`
	Client    string            `json:"client"`
}

// View collects the state of the connection.
func (s *Service) View(ctx context.Context) (*View, error) {
	st, err := s.Get(ctx)
	if err != nil {
		return nil, err
	}
	v := &View{Scenarios: st.Scenarios, Migrated: st.Migrated, Agents: []nabu.Agent{}}
	if !s.Enabled() {
		return v, nil
	}
	v.URL, v.Client = s.Client.URL, s.Client.ClientID
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	agents, err := s.Client.Agents(cctx)
	if err != nil {
		v.Error = err.Error()
		return v, nil
	}
	v.Connected, v.Agents = true, agents
	return v, nil
}

// PutScenarios binds scenarios to service agents allowed to the client (NB-09).
func (s *Service) PutScenarios(ctx context.Context, in map[string]*string) (*View, error) {
	if !s.Enabled() {
		return nil, ErrDisabled()
	}
	agents, err := s.Client.Agents(ctx)
	if err != nil {
		return nil, MapErr(err)
	}
	allowed := map[string]bool{}
	for _, a := range agents {
		allowed[a.Name] = true
	}
	st, err := s.Get(ctx)
	if err != nil {
		return nil, err
	}
	for sc, name := range in {
		if !agent.Scenario(sc).Valid() || agent.Scenario(sc) == agent.ScenarioChat {
			return nil, apperr.Unprocessable("invalid_scenario", "unknown scenario "+sc)
		}
		if name == nil || *name == "" {
			delete(st.Scenarios, sc)
			continue
		}
		if !allowed[*name] {
			return nil, apperr.Unprocessable("nabu_agent_not_allowed", "the service agent is not allowed to Hammurapi in Nabu").With("agent", *name)
		}
		st.Scenarios[sc] = *name
	}
	if err := s.put(ctx, st); err != nil {
		return nil, err
	}
	return s.View(ctx)
}

// ─── transfer of the agent settings (R11, tech §4) ──────────────────

type choice struct {
	ConnectionRef string `json:"connectionRef"`
	Model         string `json:"model"`
	Thinking      string `json:"thinking"`
}

// ImportBody builds the body of POST /client/v1/import/hammurapi-agent from
// the settings of FTR.HMR.CMN-0004; secrets are decrypted in memory only.
func (s *Service) ImportBody(ctx context.Context) (map[string]any, error) {
	body := map[string]any{}
	rows, err := s.Pool.Query(ctx, `SELECT id, name, type, base_url, models, api_key_enc FROM llm_connections WHERE enabled ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	conns := []map[string]any{}
	for rows.Next() {
		var id, name, typ, base string
		var models json.RawMessage
		var keyEnc []byte
		if err := rows.Scan(&id, &name, &typ, &base, &models, &keyEnc); err != nil {
			rows.Close()
			return nil, err
		}
		key, err := s.Box.Open(keyEnc)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("decrypt the key of %s: %w", name, err)
		}
		var defs []json.RawMessage
		_ = json.Unmarshal(models, &defs)
		conns = append(conns, map[string]any{"ref": id, "name": name, "type": typ, "baseUrl": base, "apiKey": key, "models": defs})
	}
	rows.Close()
	body["connections"] = conns
	scen := map[string]choice{}
	rows, err = s.Pool.Query(ctx, `SELECT scenario::text, connection_id::text, model, thinking FROM agent_scenario_models`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var sc string
		var c choice
		if err := rows.Scan(&sc, &c.ConnectionRef, &c.Model, &c.Thinking); err != nil {
			rows.Close()
			return nil, err
		}
		scen[sc] = c
	}
	rows.Close()
	body["scenarios"] = scen
	var d choice
	if err := s.Pool.QueryRow(ctx, `SELECT connection_id::text, model, thinking FROM agent_default_model`).Scan(&d.ConnectionRef, &d.Model, &d.Thinking); err == nil {
		body["default"] = d
	}
	rows, err = s.Pool.Query(ctx, `SELECT name, url, headers_enc, exposure::text, scenarios::text[] FROM mcp_servers WHERE enabled ORDER BY name`)
	if err != nil {
		return nil, err
	}
	mcp := []map[string]any{}
	for rows.Next() {
		var name, url, exp string
		var enc []byte
		var scs []string
		if err := rows.Scan(&name, &url, &enc, &exp, &scs); err != nil {
			rows.Close()
			return nil, err
		}
		headers := map[string]string{}
		if len(enc) > 0 {
			pt, err := s.Box.Open(enc)
			if err != nil {
				rows.Close()
				return nil, fmt.Errorf("decrypt the headers of %s: %w", name, err)
			}
			_ = json.Unmarshal([]byte(pt), &headers)
		}
		mcp = append(mcp, map[string]any{"name": name, "url": url, "headers": headers, "exposure": exp, "scenarios": scs})
	}
	rows.Close()
	body["mcp"] = mcp
	assignments := map[string][]string{}
	var skills json.RawMessage
	if err := s.Pool.QueryRow(ctx, `SELECT skills FROM agent_skill_snapshots ORDER BY created_at DESC LIMIT 1`).Scan(&skills); err == nil {
		var list []struct {
			Name      string   `json:"name"`
			Scenarios []string `json:"scenarios"`
		}
		_ = json.Unmarshal(skills, &list)
		for _, sk := range list {
			assignments[sk.Name] = sk.Scenarios
		}
	}
	body["skills"] = map[string]any{"repo": s.SkillsRepo, "path": "agent/skills", "ref": s.DefaultBranch, "assignments": assignments}
	return body, nil
}

// Migrate transfers the settings and binds the scenarios to the created
// service agents (MG-01); on any error Hammurapi stays unchanged (MG-03).
func (s *Service) Migrate(ctx context.Context) (map[string]any, error) {
	if !s.Enabled() {
		return nil, ErrDisabled()
	}
	body, err := s.ImportBody(ctx)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	rep, err := s.Client.Import(cctx, body)
	if err != nil {
		return nil, MapErr(err)
	}
	st, err := s.Get(ctx)
	if err != nil {
		return nil, err
	}
	if sa, ok := rep["serviceAgents"].(map[string]any); ok {
		for sc, name := range sa {
			if n, ok := name.(string); ok && n != "" {
				st.Scenarios[sc] = n
			}
		}
	}
	st.Migrated = true
	if err := s.put(ctx, st); err != nil {
		return nil, err
	}
	// MG-02: the journal holds counts, never secrets.
	slog.InfoContext(ctx, "agent settings transferred to Nabu", "connections", len(body["connections"].([]map[string]any)),
		"mcp", len(body["mcp"].([]map[string]any)), "scenarios", len(st.Scenarios))
	return rep, nil
}

// AdminRoutes mounts /nabu (global administrators only).
func (s *Service) AdminRoutes(r chi.Router) {
	r.Route("/nabu", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if p := httpx.PrincipalFrom(r.Context()); p == nil || !p.GlobalAdmin {
					httpx.Error(w, r, apperr.Forbidden("forbidden", "the Nabu section is available to global administrators only"))
					return
				}
				next.ServeHTTP(w, r)
			})
		})
		r.Get("/", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			v, err := s.View(r.Context())
			if err != nil {
				return err
			}
			httpx.JSON(w, 200, v)
			return nil
		}))
		r.Put("/scenarios", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			var in map[string]*string
			if err := httpx.Decode(r, &in); err != nil {
				return err
			}
			v, err := s.PutScenarios(r.Context(), in)
			if err != nil {
				return err
			}
			httpx.JSON(w, 200, v)
			return nil
		}))
		r.Post("/migrate", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			rep, err := s.Migrate(r.Context())
			if err != nil {
				return err
			}
			httpx.JSON(w, 200, rep)
			return nil
		}))
	})
}
