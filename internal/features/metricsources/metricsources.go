// Package metricsources manages read-only metric sources (FTR.HMR.CMN-0002 arch
// §14): administration, and the dry run of success-metric queries used by
// Discovery verification (R6) and the agent.
package metricsources

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metricsource"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/signing"
)

// Service implements metric source use cases.
type Service struct {
	q       postgres.Querier
	secrets *signing.Secrets
	// New builds a source (replaced in tests).
	New func(metricsource.Config) (metricsource.Source, error)
}

// NewService creates the service.
func NewService(q postgres.Querier, secrets *signing.Secrets) *Service {
	return &Service{q: q, secrets: secrets, New: metricsource.New}
}

// Input is the body of POST/PATCH.
type Input struct {
	Name      string              `json:"name"`
	Type      string              `json:"type"`
	Endpoint  string              `json:"endpoint"`
	Username  *string             `json:"username"`
	Password  *string             `json:"password"`  // stored encrypted as an enc: reference
	SecretRef *string             `json:"secretRef"` // or env:NAME of a Kubernetes Secret
	Limits    metricsource.Limits `json:"limits"`
}

// View is a source without secrets.
type View struct {
	Name      string              `json:"name"`
	Type      string              `json:"type"`
	Endpoint  string              `json:"endpoint"`
	Username  *string             `json:"username"`
	SecretRef string              `json:"secretRef"` // env:NAME is shown, enc: is masked
	Limits    metricsource.Limits `json:"limits"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

func requireGlobal(p *domain.Principal) error {
	if !p.GlobalAdmin {
		return apperr.Forbidden("forbidden", "global administrator role required")
	}
	return nil
}

func toView(m cycledata.MetricSource) View {
	v := View{Name: m.Name, Type: m.Type, Endpoint: m.Endpoint, Username: m.Username, SecretRef: m.SecretRef}
	_ = json.Unmarshal(m.Limits, &v.Limits)
	if strings.HasPrefix(v.SecretRef, "enc:") {
		v.SecretRef = "enc:…"
	}
	return v
}

// List returns all sources.
func (s *Service) List(ctx context.Context) ([]View, error) {
	ms, err := cycledata.New(s.q).MetricSources(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(ms))
	for _, m := range ms {
		out = append(out, toView(m))
	}
	return out, nil
}

// Save creates or updates a source.
func (s *Service) Save(ctx context.Context, p *domain.Principal, in Input, create bool) (*View, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	if !nameRe.MatchString(in.Name) {
		return nil, apperr.Unprocessable("invalid_name", "name must be lowercase latin letters, digits, - or _")
	}
	if in.Type != "clickhouse" && in.Type != "prometheus" {
		return nil, apperr.Unprocessable("invalid_type", "type must be clickhouse or prometheus")
	}
	if u, err := url.Parse(in.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, apperr.Unprocessable("invalid_endpoint", "endpoint must be an http(s) URL")
	}
	cd := cycledata.New(s.q)
	_, err := cd.MetricSourceByName(ctx, in.Name)
	exists := err == nil
	if err != nil && !errors.Is(err, cycledata.ErrNotFound) {
		return nil, err
	}
	if create && exists {
		return nil, apperr.Conflict("name_exists", "a metric source with this name already exists")
	}
	if !create && !exists {
		return nil, apperr.NotFound("metric_source_not_found", "metric source not found")
	}
	ref := ""
	switch {
	case in.Password != nil && *in.Password != "":
		if ref, err = s.secrets.Encrypt(*in.Password); err != nil {
			return nil, err
		}
	case in.SecretRef != nil && strings.HasPrefix(*in.SecretRef, "env:"):
		ref = *in.SecretRef
	case create:
		ref = "none"
	}
	limits, _ := json.Marshal(in.Limits)
	m := cycledata.MetricSource{Name: in.Name, Type: in.Type, Endpoint: strings.TrimRight(in.Endpoint, "/"), Username: in.Username, SecretRef: ref, Limits: limits}
	if err := cd.UpsertMetricSource(ctx, m); err != nil {
		return nil, err
	}
	saved, err := cd.MetricSourceByName(ctx, in.Name)
	if err != nil {
		return nil, err
	}
	v := toView(*saved)
	return &v, nil
}

// Delete removes a source.
func (s *Service) Delete(ctx context.Context, p *domain.Principal, name string) error {
	if err := requireGlobal(p); err != nil {
		return err
	}
	return cycledata.New(s.q).DeleteMetricSource(ctx, name)
}

// Source builds a configured source by name.
func (s *Service) Source(ctx context.Context, name string) (metricsource.Source, error) {
	m, err := cycledata.New(s.q).MetricSourceByName(ctx, name)
	if errors.Is(err, cycledata.ErrNotFound) {
		return nil, apperr.Unprocessable("unknown_metric_source", "metric source "+name+" is not configured").With("source", name)
	}
	if err != nil {
		return nil, err
	}
	cfg := metricsource.Config{Type: m.Type, Endpoint: m.Endpoint}
	_ = json.Unmarshal(m.Limits, &cfg.Limits)
	if m.Username != nil {
		cfg.Username = *m.Username
	}
	if m.SecretRef != "" && m.SecretRef != "none" {
		if cfg.Password, err = s.secrets.Resolve(m.SecretRef); err != nil {
			return nil, apperr.Unprocessable("metric_source_secret", err.Error())
		}
	}
	return s.New(cfg)
}

// Test dry-runs a query; source errors are 422 metric_query_failed (DSC-08, MET-04).
func (s *Service) Test(ctx context.Context, source, query string) (float64, error) {
	src, err := s.Source(ctx, source)
	if err != nil {
		return 0, err
	}
	if err := src.Validate(query); err != nil {
		return 0, apperr.Unprocessable("metric_query_invalid", err.Error()).With("source", source)
	}
	tctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	v, err := src.Run(tctx, query, time.Now())
	if err != nil {
		return 0, apperr.Unprocessable("metric_query_failed", err.Error()).With("source", source)
	}
	return v, nil
}

// Routes mounts GET /api/v1/metric-sources: the names and types of the
// sources for every user — the choice of the source of a way to measure
// (FTR.HMR.DISC-0001 R23); no endpoints and no secrets.
func (s *Service) Routes(r chi.Router) {
	r.Get("/metric-sources", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		srcs, err := cycledata.New(s.q).MetricSources(r.Context())
		if err != nil {
			return err
		}
		type item struct {
			Name string `json:"name"`
			Type string `json:"type"`
		}
		out := []item{}
		for _, m := range srcs {
			out = append(out, item{m.Name, m.Type})
		}
		httpx.JSON(w, 200, map[string]any{"items": out})
		return nil
	}))
}

// AdminRoutes mounts /admin/api/v1/metric-sources.
func (s *Service) AdminRoutes(r chi.Router) {
	r.Get("/metric-sources", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := requireGlobal(p); err != nil {
			return err
		}
		out, err := s.List(r.Context())
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, out)
		return nil
	}))
	save := func(create bool) http.Handler {
		return httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			p, err := httpx.MustPrincipal(r)
			if err != nil {
				return err
			}
			var in Input
			if err := httpx.Decode(r, &in); err != nil {
				return err
			}
			if !create {
				in.Name = chi.URLParam(r, "name")
			}
			v, err := s.Save(r.Context(), p, in, create)
			if err != nil {
				return err
			}
			status := 200
			if create {
				status = http.StatusCreated
			}
			httpx.JSON(w, status, v)
			return nil
		})
	}
	r.Method(http.MethodPost, "/metric-sources", save(true))
	r.Method(http.MethodPatch, "/metric-sources/{name}", save(false))
	r.Delete("/metric-sources/{name}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := s.Delete(r.Context(), p, chi.URLParam(r, "name")); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}))
	r.Post("/metric-sources/{name}/test", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := requireGlobal(p); err != nil {
			return err
		}
		var in struct {
			Query string `json:"query"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		v, err := s.Test(r.Context(), chi.URLParam(r, "name"), in.Query)
		if e, ok := apperr.As(err); ok && e.Status == 422 {
			httpx.JSON(w, 200, map[string]any{"ok": false, "error": e.Message})
			return nil
		}
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, map[string]any{"ok": true, "value": v})
		return nil
	}))
}
