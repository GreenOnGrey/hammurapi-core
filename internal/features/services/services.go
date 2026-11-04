// Package services serves the service catalog (FTR.HMR.CMN-0002 R10, R11, R17):
// the list with owners and autonomy, the autonomy level set by service owners,
// and the manual catalog in the admin panel when Backstage is not used.
package services

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/catalog"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/domains"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Service implements the service catalog use cases.
type Service struct {
	pool    *pgxpool.Pool
	catalog *catalog.Syncer
}

// NewService creates the service.
func NewService(pool *pgxpool.Pool, c *catalog.Syncer) *Service {
	return &Service{pool: pool, catalog: c}
}

var (
	keyRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+(/[A-Za-z0-9_.-]+)+$`)
)

// SetAutonomy changes the autonomy level; only owners of the service (CG-07).
func (s *Service) SetAutonomy(ctx context.Context, p *domain.Principal, key string, level domain.Autonomy) error {
	if !level.Valid() {
		return apperr.Unprocessable("invalid_level", "level must be plan, pr or autonomous")
	}
	if !p.Owns(key) {
		return apperr.Forbidden("forbidden", "only owners of the service change its autonomy")
	}
	cd := cycledata.New(s.pool)
	svc, err := cd.ServiceByKey(ctx, key)
	if errors.Is(err, cycledata.ErrNotFound) {
		return apperr.NotFound("service_not_found", "service not found")
	}
	if err != nil {
		return err
	}
	return cd.SetAutonomy(ctx, svc.ID, level)
}

// Bounds of the readiness check (FTR.HMR.CMN-0008 R1).
const (
	checkMaxCommand     = 2000
	checkDefaultTimeout = 600
	checkMinTimeout     = 10
	checkMaxTimeout     = 1800
)

// CheckInput is the body of PUT /services/{service}/check; an empty command
// clears the check.
type CheckInput struct {
	Command    string `json:"command"`
	TimeoutSec int    `json:"timeoutSec"`
}

// SetCheck changes the readiness check of a service: its owners and the
// administrators, with an entry in its history (FTR.HMR.CMN-0008 R2).
func (s *Service) SetCheck(ctx context.Context, p *domain.Principal, key string, in CheckInput) error {
	if !p.Owns(key) && !p.IsAnyAdmin() {
		return apperr.Forbidden("forbidden", "only owners of the service and administrators change its check")
	}
	in.Command = strings.TrimSpace(in.Command)
	var next *cycledata.ServiceCheck
	if in.Command != "" {
		if in.TimeoutSec == 0 {
			in.TimeoutSec = checkDefaultTimeout
		}
		switch {
		case utf8.RuneCountInString(in.Command) > checkMaxCommand:
			return apperr.Unprocessable("invalid_check", fmt.Sprintf("the command is at most %d characters", checkMaxCommand)).With("field", "command")
		case in.TimeoutSec < checkMinTimeout || in.TimeoutSec > checkMaxTimeout:
			return apperr.Unprocessable("invalid_check", fmt.Sprintf("the timeout is %d–%d seconds", checkMinTimeout, checkMaxTimeout)).With("field", "timeoutSec")
		}
		next = &cycledata.ServiceCheck{Command: in.Command, TimeoutSec: in.TimeoutSec}
	}
	return postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		cd := cycledata.New(tx)
		svc, err := cd.ServiceByKey(ctx, key)
		if errors.Is(err, cycledata.ErrNotFound) {
			return apperr.NotFound("service_not_found", "service not found")
		}
		if err != nil {
			return err
		}
		if err := cd.SetCheck(ctx, svc.ID, next); err != nil {
			return err
		}
		return cd.AddActivity(ctx, "service", svc.ID, "check_changed", &p.UserID, false, map[string]any{"from": svc.Check, "to": next})
	})
}

// Input is the body of the admin service endpoints.
type Input struct {
	Key      string  `json:"key"`
	Name     string  `json:"name"`
	System   *string `json:"system"` // DOMAIN/SYSTEM
	Repo     string  `json:"repo"`
	OwnerRef string  `json:"ownerRef"` // user:<login> | group:<name>
}

func requireAdmin(p *domain.Principal) error {
	if !p.IsAnyAdmin() {
		return apperr.Forbidden("forbidden", "administrator role required")
	}
	return nil
}

// Save creates or updates a manual service (CAT-09); 409 catalog_managed with Backstage (CAT-07).
func (s *Service) Save(ctx context.Context, p *domain.Principal, in Input, create bool) (*cycledata.Service, error) {
	if err := requireAdmin(p); err != nil {
		return nil, err
	}
	if err := domains.RequireManual(ctx, s.pool); err != nil {
		return nil, err
	}
	if !keyRe.MatchString(in.Key) {
		return nil, apperr.Unprocessable("invalid_key", "service key must be lowercase latin letters, digits, dots, - or _")
	}
	if !repoRe.MatchString(in.Repo) {
		return nil, apperr.Unprocessable("invalid_repo", "repository must be owner/name")
	}
	cd := cycledata.New(s.pool)
	existing, err := cd.ServiceByKey(ctx, in.Key)
	if err != nil && !errors.Is(err, cycledata.ErrNotFound) {
		return nil, err
	}
	if create && existing != nil && err == nil {
		return nil, apperr.Conflict("key_exists", "a service with this key already exists")
	}
	if !create && (existing == nil || err != nil) {
		return nil, apperr.NotFound("service_not_found", "service not found")
	}
	svc := &cycledata.Service{Key: in.Key, Name: strings.TrimSpace(in.Name), Repo: in.Repo, OwnerRef: strings.TrimSpace(in.OwnerRef), Source: "manual"}
	if in.System != nil && *in.System != "" {
		dk, sk, ok := strings.Cut(*in.System, "/")
		if !ok {
			return nil, apperr.Unprocessable("invalid_system", "system must be DOMAIN/SYSTEM")
		}
		var sid uuid.UUID
		err := s.pool.QueryRow(ctx, `SELECT s.id FROM systems s JOIN domains d ON d.id = s.domain_id WHERE d.key = $1 AND s.key = $2`, dk, sk).Scan(&sid)
		if err != nil {
			return nil, apperr.Unprocessable("unknown_system", "system is not in the dictionary")
		}
		svc.SystemID = &sid
	}
	if existing != nil && err == nil {
		svc.DeployOverride = existing.DeployOverride
	}
	if err := cd.UpsertService(ctx, svc); err != nil {
		return nil, err
	}
	if s.catalog != nil {
		if users, err := s.catalog.ResolveOwners(ctx, svc.OwnerRef); err == nil {
			if err := cd.SetServiceOwners(ctx, svc.ID, users); err != nil {
				return nil, err
			}
		}
	}
	return cd.ServiceByKey(ctx, svc.Key)
}

// Delete removes a manual service, or marks it when features still use it.
func (s *Service) Delete(ctx context.Context, p *domain.Principal, key string) error {
	if err := requireAdmin(p); err != nil {
		return err
	}
	if err := domains.RequireManual(ctx, s.pool); err != nil {
		return err
	}
	cd := cycledata.New(s.pool)
	svc, err := cd.ServiceByKey(ctx, key)
	if errors.Is(err, cycledata.ErrNotFound) {
		return apperr.NotFound("service_not_found", "service not found")
	}
	if err != nil {
		return err
	}
	_, err = cd.DeleteService(ctx, svc.ID)
	return err
}

// Routes mounts GET /services, PUT /services/{service}/autonomy and
// PUT /services/{service}/check.
func (s *Service) Routes(r chi.Router) {
	r.Get("/services", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		out, err := cycledata.New(s.pool).ListServices(r.Context(), cycledata.ServiceFilter{System: q.Get("system"), Domain: q.Get("domain"), Query: q.Get("q")})
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, out)
		return nil
	}))
	r.Put("/services/{service}/check", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in CheckInput
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		if err := s.SetCheck(r.Context(), p, chi.URLParam(r, "service"), in); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}))
	r.Put("/services/{service}/autonomy", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in struct {
			Level domain.Autonomy `json:"level"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		if err := s.SetAutonomy(r.Context(), p, chi.URLParam(r, "service"), in.Level); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}))
}

// AdminRoutes mounts the manual service catalog.
func (s *Service) AdminRoutes(r chi.Router) {
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
				in.Key = chi.URLParam(r, "service")
			}
			svc, err := s.Save(r.Context(), p, in, create)
			if err != nil {
				return err
			}
			status := 200
			if create {
				status = http.StatusCreated
			}
			httpx.JSON(w, status, svc)
			return nil
		})
	}
	r.Method(http.MethodPost, "/services", save(true))
	r.Method(http.MethodPatch, "/services/{service}", save(false))
	r.Delete("/services/{service}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := s.Delete(r.Context(), p, chi.URLParam(r, "service")); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}))
}
