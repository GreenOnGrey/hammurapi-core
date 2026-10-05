// Package admin implements global administration: users and roles, and
// instance settings.
package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// SettingRetentionDays is the key of the attachment retention setting.
const SettingRetentionDays = "attachment_retention_days"

// User is a user with roles.
type User struct {
	ID          uuid.UUID            `json:"id"`
	Username    string               `json:"username"`
	DisplayName string               `json:"displayName"`
	AvatarURL   *string              `json:"avatarUrl"`
	GlobalAdmin bool                 `json:"globalAdmin"`
	AreaAdmin   []domain.Area        `json:"areaAdmin"`
	Experts     []auth.ExpertDomains `json:"experts"`
	CreatedAt   time.Time            `json:"createdAt"`
	// Email, sign-in identities (issuers) and the git account
	// (FTR.HMR.CMN-0006 design: columns «Вход» and «Git-аккаунт»).
	Email      *string  `json:"email"`
	Logins     []string `json:"logins"`
	GitLogin   *string  `json:"gitLogin"`
	CreatedVia string   `json:"createdVia"`
	LinkReview bool     `json:"linkReview"`
}

// RolesInput is the body of PUT /users/{id}/roles. There are no editor or
// approver roles (FTR.HMR.CMN-0002): experts are assigned per domain.
type RolesInput struct {
	GlobalAdmin bool          `json:"globalAdmin"`
	AreaAdmin   []domain.Area `json:"areaAdmin"`
}

// Service implements admin use cases.
type Service struct {
	pool       *pgxpool.Pool
	identities *auth.Repository
	// RunnerExecutor is shown in the admin panel (local is not for production, RUN-10).
	RunnerExecutor string
}

// NewService creates the service.
func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// WithIdentities enables linking of new users (FTR.HMR.CMN-0006 tech §2).
func (s *Service) WithIdentities(r *auth.Repository) *Service { s.identities = r; return s }

func requireGlobal(p *domain.Principal) error {
	if !p.GlobalAdmin {
		return apperr.Forbidden("forbidden", "global administrator role required")
	}
	return nil
}

// Users lists users with roles.
func (s *Service) Users(ctx context.Context, q string, page httpx.Page) ([]User, error) {
	args := []any{"%" + q + "%", page.Limit + 1}
	cond := ""
	if c := page.Cursor; c != nil {
		args = append(args, c.T, c.ID)
		cond = ` AND (created_at, id::text) > ($3, $4)`
	}
	rows, err := s.pool.Query(ctx, `SELECT id, username, display_name, avatar_url, is_global_admin, created_at, email,
			COALESCE((SELECT array_agg(issuer ORDER BY issuer) FROM user_identities i WHERE i.user_id = users.id), '{}'),
			(SELECT login FROM git_accounts g WHERE g.user_id = users.id), created_via, link_review
		FROM users
		WHERE (username ILIKE $1 OR display_name ILIKE $1 OR email ILIKE $1)`+cond+` ORDER BY created_at, id::text LIMIT $2`, args...)
	if err != nil {
		return nil, err
	}
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.DisplayName, &u.AvatarURL, &u.GlobalAdmin, &u.CreatedAt, &u.Email,
			&u.Logins, &u.GitLogin, &u.CreatedVia, &u.LinkReview); err != nil {
			rows.Close()
			return nil, err
		}
		users = append(users, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range users {
		p := domain.NewPrincipal(users[i].ID, "", "", users[i].GlobalAdmin)
		rr, err := s.pool.Query(ctx, `SELECT 'admin', area::text FROM area_admins WHERE user_id = $1
			UNION ALL SELECT e.kind::text, d.key FROM domain_experts e JOIN domains d ON d.id = e.domain_id WHERE e.user_id = $1`, users[i].ID)
		if err != nil {
			return nil, err
		}
		for rr.Next() {
			var kind, target string
			if err := rr.Scan(&kind, &target); err != nil {
				rr.Close()
				return nil, err
			}
			if kind == "admin" {
				p.GrantAreaAdmin(domain.Area(target))
			} else {
				p.GrantExpert(target, domain.ExpertKind(kind))
			}
		}
		rr.Close()
		users[i].AreaAdmin = p.AdminAreas()
		if users[i].AreaAdmin == nil {
			users[i].AreaAdmin = []domain.Area{}
		}
		users[i].Experts = auth.ExpertsOf(p)
	}
	return users, nil
}

// SetRoles replaces the user's roles. The last global administrator cannot be demoted.
func (s *Service) SetRoles(ctx context.Context, userID uuid.UUID, in RolesInput) error {
	for _, a := range in.AreaAdmin {
		if !a.Valid() {
			return apperr.Unprocessable("invalid_area", fmt.Sprintf("unknown area %q", a))
		}
	}
	return postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// Serialize role changes of global admins.
		if _, err := tx.Exec(ctx, `LOCK TABLE users IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		var current bool
		if err := tx.QueryRow(ctx, `SELECT is_global_admin FROM users WHERE id = $1`, userID).Scan(&current); err != nil {
			if postgres.IsNoRows(err) {
				return apperr.NotFound("user_not_found", "user not found")
			}
			return err
		}
		if current && !in.GlobalAdmin {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM users WHERE is_global_admin`).Scan(&n); err != nil {
				return err
			}
			if n <= 1 {
				return apperr.Conflict("last_global_admin", "the last global administrator cannot be removed")
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET is_global_admin = $2 WHERE id = $1`, userID, in.GlobalAdmin); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM area_admins WHERE user_id = $1`, userID); err != nil {
			return err
		}
		for _, a := range in.AreaAdmin {
			if _, err := tx.Exec(ctx, `INSERT INTO area_admins (user_id, area) VALUES ($1,$2) ON CONFLICT DO NOTHING`, userID, a); err != nil {
				return err
			}
		}
		return nil
	})
}

// RetentionDays reads the attachment retention setting (default 90).
func RetentionDays(ctx context.Context, q postgres.Querier) (int, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT value FROM admin_settings WHERE key = $1`, SettingRetentionDays).Scan(&raw)
	if postgres.IsNoRows(err) {
		return 90, nil
	}
	if err != nil {
		return 0, err
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		// tolerate "90" stored as a string
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return strconv.Atoi(s)
		}
		return 0, err
	}
	return n, nil
}

// Settings is the instance settings object.
type Settings struct {
	AttachmentRetentionDays int                    `json:"attachmentRetentionDays"`
	FeatureFlags            FlagsView              `json:"featureFlags"`
	Stage                   cycledata.StageSetting `json:"stage"`
	RunnerExecutor          string                 `json:"runnerExecutor"`
}

// FlagsView is the feature flag setting without secrets.
type FlagsView struct {
	Enabled       bool `json:"enabled"`
	ActiveSecrets int  `json:"activeSecrets"`
}

func (s *Service) settings(ctx context.Context) (*Settings, error) {
	n, err := RetentionDays(ctx, s.pool)
	if err != nil {
		return nil, err
	}
	out := &Settings{AttachmentRetentionDays: n, RunnerExecutor: s.RunnerExecutor}
	cd := cycledata.New(s.pool)
	var fs cycledata.FlagsSetting
	if _, err := cd.Setting(ctx, "feature_flags", &fs); err != nil {
		return nil, err
	}
	out.FeatureFlags = FlagsView{Enabled: fs.Enabled, ActiveSecrets: len(fs.SecretRefs)}
	if _, err := cd.Setting(ctx, "stage", &out.Stage); err != nil {
		return nil, err
	}
	return out, nil
}

// Routes mounts /admin/api/v1 users and settings.
func (s *Service) Routes(r chi.Router) {
	r.Get("/users", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := requireGlobal(p); err != nil {
			return err
		}
		page, err := httpx.ParsePage(r)
		if err != nil {
			return err
		}
		users, err := s.Users(r.Context(), r.URL.Query().Get("q"), page)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, httpx.NewList(users, page.Limit, func(u User) (time.Time, string) { return u.CreatedAt, u.ID.String() }))
		return nil
	}))
	r.Get("/users/unlinked", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := requireGlobal(p); err != nil {
			return err
		}
		items, err := s.identities.Unlinked(r.Context())
		if err != nil {
			return err
		}
		if items == nil {
			items = []auth.Unlinked{}
		}
		httpx.JSON(w, 200, map[string]any{"items": items})
		return nil
	}))
	r.Post("/users/{id}/link", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := requireGlobal(p); err != nil {
			return err
		}
		id, err := httpx.ParamUUID(r, "id")
		if err != nil {
			return err
		}
		var in struct {
			TargetUserID uuid.UUID `json:"targetUserId"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		if err := s.identities.Link(r.Context(), id, in.TargetUserID); err != nil {
			return err
		}
		slog.InfoContext(r.Context(), "admin journal: user linked", "actor", p.UserID, "user", id, "target", in.TargetUserID)
		httpx.NoContent(w)
		return nil
	}))
	r.Post("/users/{id}/confirm-new", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := requireGlobal(p); err != nil {
			return err
		}
		id, err := httpx.ParamUUID(r, "id")
		if err != nil {
			return err
		}
		if err := s.identities.ConfirmNew(r.Context(), id); err != nil {
			return err
		}
		slog.InfoContext(r.Context(), "admin journal: user confirmed as new", "actor", p.UserID, "user", id)
		httpx.NoContent(w)
		return nil
	}))
	r.Put("/users/{id}/roles", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := requireGlobal(p); err != nil {
			return err
		}
		id, err := httpx.ParamUUID(r, "id")
		if err != nil {
			return err
		}
		var in RolesInput
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		if err := s.SetRoles(r.Context(), id, in); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}))
	r.Get("/settings", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := requireGlobal(p); err != nil {
			return err
		}
		out, err := s.settings(r.Context())
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, out)
		return nil
	}))
	r.Patch("/settings", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if err := requireGlobal(p); err != nil {
			return err
		}
		var in struct {
			AttachmentRetentionDays *int `json:"attachmentRetentionDays"`
			FeatureFlags            *struct {
				Enabled bool `json:"enabled"`
			} `json:"featureFlags"`
			Stage *struct {
				Enabled bool `json:"enabled"`
			} `json:"stage"`
		}
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		if in.AttachmentRetentionDays != nil {
			n := *in.AttachmentRetentionDays
			if n < 1 || n > 3650 {
				return apperr.Unprocessable("invalid_retention", "retention must be between 1 and 3650 days")
			}
			if _, err := s.pool.Exec(r.Context(), `INSERT INTO admin_settings (key, value, updated_by, updated_at) VALUES ($1, $2, $3, now())
				ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now()`,
				SettingRetentionDays, strconv.Itoa(n), p.UserID); err != nil {
				return err
			}
		}
		cd := cycledata.New(s.pool)
		if in.FeatureFlags != nil {
			var fs cycledata.FlagsSetting
			if _, err := cd.Setting(r.Context(), "feature_flags", &fs); err != nil {
				return err
			}
			fs.Enabled = in.FeatureFlags.Enabled
			if err := cd.PutSetting(r.Context(), "feature_flags", fs, &p.UserID); err != nil {
				return err
			}
		}
		if in.Stage != nil {
			if err := cd.PutSetting(r.Context(), "stage", cycledata.StageSetting{Enabled: in.Stage.Enabled}, &p.UserID); err != nil {
				return err
			}
		}
		out, err := s.settings(r.Context())
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, out)
		return nil
	}))
}
