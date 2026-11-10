package cicheck

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
)

// Routes mounts the token of a service (tech §3.2) and the status of the check (tech §3.3) for a session.
func (s *Service) Routes(r chi.Router) {
	key := func(r *http.Request) string { return chi.URLParam(r, "service") }
	r.Get("/services/{service}/token", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		t, err := s.Token(r.Context(), p, key(r))
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusOK, t)
		return nil
	}))
	r.Post("/services/{service}/token", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		t, err := s.IssueToken(r.Context(), p, key(r))
		if err != nil {
			return err
		}
		w.Header().Set("Cache-Control", "no-store")
		httpx.JSON(w, http.StatusCreated, t)
		return nil
	}))
	r.Delete("/services/{service}/token", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := s.RevokeToken(r.Context(), p, key(r)); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}))

	// the status of the repository of specifications: the global administrators; of a service: every user, as its card
	status := func(target func(*http.Request) string, specOnly bool) http.HandlerFunc {
		return httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			p, err := httpx.MustPrincipal(r)
			if err != nil {
				return err
			}
			if specOnly && !p.GlobalAdmin {
				return apperr.NoRole("admin", "global")
			}
			st, err := s.Check(r.Context(), target(r))
			if err != nil {
				return err
			}
			httpx.JSON(w, http.StatusOK, st)
			return nil
		})
	}
	rescan := func(target func(*http.Request) string, can func(*domain.Principal, *http.Request) bool) http.HandlerFunc {
		return httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			p, err := httpx.MustPrincipal(r)
			if err != nil {
				return err
			}
			if !can(p, r) {
				return apperr.Forbidden("forbidden", "only administrators and the owners of the service check now")
			}
			st, err := s.Rescan(r.Context(), target(r))
			if err != nil {
				return err
			}
			httpx.JSON(w, http.StatusOK, st)
			return nil
		})
	}
	propose := func(target func(*http.Request) string) http.HandlerFunc {
		return httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			p, err := httpx.MustPrincipal(r)
			if err != nil {
				return err
			}
			st, err := s.Propose(r.Context(), p, target(r))
			if err != nil {
				return err
			}
			httpx.JSON(w, http.StatusCreated, st)
			return nil
		})
	}
	spec := func(*http.Request) string { return TargetSpec }
	svc := func(r *http.Request) string { return serviceTarget(key(r)) }
	isGlobal := func(p *domain.Principal, _ *http.Request) bool { return p.GlobalAdmin }
	manages := func(p *domain.Principal, r *http.Request) bool { return canManage(p, key(r)) }
	r.Get("/ci-checks/spec", status(spec, true))
	r.Post("/ci-checks/spec/scan", rescan(spec, isGlobal))
	r.Post("/ci-checks/spec/propose", propose(spec))
	r.Get("/ci-checks/services/{service}", status(svc, false))
	r.Post("/ci-checks/services/{service}/scan", rescan(svc, manages))
	r.Post("/ci-checks/services/{service}/propose", propose(svc))
}

// lockKey keeps one worker checking the repositories at a time.
const lockKey = "hashtext('cicheck')"

// Run checks the repository of specifications and the repositories of services once an hour and bumps the
// outdated checks (arch §3.6); the first pass goes a minute after the start.
func (s *Service) Run(ctx context.Context) error {
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if err := s.pass(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "cicheck pass", "err", err)
		}
		t.Reset(time.Hour)
	}
}

func (s *Service) pass(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+lockKey+`)`).Scan(&locked); err != nil || !locked {
		return err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(`+lockKey+`)`) }()
	targets := []string{TargetSpec}
	rows, err := s.pool.Query(ctx, `SELECT key FROM services WHERE NOT deleted_in_catalog ORDER BY key`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, serviceTarget(k))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, target := range targets {
		if ctx.Err() != nil {
			return nil
		}
		st, err := s.Rescan(ctx, target)
		switch {
		case err != nil:
			slog.WarnContext(ctx, "cicheck", "target", target, "err", err)
		case st != nil && st.Error != nil:
			slog.WarnContext(ctx, "cicheck", "target", target, "repo", st.Repo, "status", st.Status, "err", *st.Error)
		}
	}
	return s.gauge(ctx)
}

// gauge sets hammurapi_ci_checks by kind and status (arch §7).
func (s *Service) gauge(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT CASE WHEN target = 'spec' THEN 'spec' ELSE 'service' END, status, count(*)
		FROM ci_checks GROUP BY 1, 2`)
	if err != nil {
		return err
	}
	defer rows.Close()
	metrics.CIChecks.Reset()
	for rows.Next() {
		var kind, status string
		var n int
		if err := rows.Scan(&kind, &status, &n); err != nil {
			return err
		}
		metrics.CIChecks.WithLabelValues(kind, status).Set(float64(n))
	}
	return rows.Err()
}
