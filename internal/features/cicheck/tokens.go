// Package cicheck delivers the check of speclint to the CI of repositories (FTR.HMR.SPEC-0002): the tokens of
// services, the test cases of a service for its CI, the status of the check in the repository of
// specifications and of services, the pull requests that add the check and keep its version on the contract.
package cicheck

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// tokenPrefix starts every token of a service: a scanner of secrets knows it, and the API tells it from a session.
const tokenPrefix = "hmr_svc_"

// shownPrefix is the part of a token the card of the service shows: hmr_svc_ and four characters.
const shownPrefix = len(tokenPrefix) + 4

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// newToken is hmr_svc_ and 32 random bytes in base32 (arch §3.5).
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return tokenPrefix + strings.ToLower(b32.EncodeToString(b)), nil
}

func hashToken(t string) []byte {
	h := sha256.Sum256([]byte(t))
	return h[:]
}

// TokenInfo is the token of a service as its card shows it (tech §3.2): never the token itself.
type TokenInfo struct {
	Issued     bool       `json:"issued"`
	Prefix     *string    `json:"prefix"`
	CreatedAt  *time.Time `json:"createdAt"`
	CreatedBy  *string    `json:"createdBy"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
}

// IssuedToken is the answer of issuing: the token is shown once.
type IssuedToken struct {
	Token  string `json:"token"`
	Prefix string `json:"prefix"`
}

// canManage: the owners of the service and the administrators (R10).
func canManage(p *domain.Principal, key string) bool {
	return p.Owns(key) || p.IsAnyAdmin()
}

func (s *Service) service(ctx context.Context, q postgres.Querier, key string) (*cycledata.Service, error) {
	svc, err := cycledata.New(q).ServiceByKey(ctx, key)
	if errors.Is(err, cycledata.ErrNotFound) {
		return nil, apperr.NotFound("service_not_found", "service not found")
	}
	return svc, err
}

// Token describes the active token of a service.
func (s *Service) Token(ctx context.Context, p *domain.Principal, key string) (*TokenInfo, error) {
	if !canManage(p, key) {
		return nil, apperr.Forbidden("forbidden", "only owners of the service and administrators see its token")
	}
	svc, err := s.service(ctx, s.pool, key)
	if err != nil {
		return nil, err
	}
	var t TokenInfo
	err = s.pool.QueryRow(ctx, `SELECT t.prefix, t.created_at, u.display_name, t.last_used_at
		FROM service_tokens t JOIN users u ON u.id = t.created_by
		WHERE t.service_id = $1 AND t.revoked_at IS NULL`, svc.ID).Scan(&t.Prefix, &t.CreatedAt, &t.CreatedBy, &t.LastUsedAt)
	if postgres.IsNoRows(err) {
		return &TokenInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	t.Issued = true
	return &t, nil
}

// IssueToken issues a token of a service; an active one is revoked in the same transaction (R10).
func (s *Service) IssueToken(ctx context.Context, p *domain.Principal, key string) (*IssuedToken, error) {
	if !canManage(p, key) {
		return nil, apperr.Forbidden("forbidden", "only owners of the service and administrators issue its token")
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	prefix := token[:shownPrefix]
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		svc, err := s.service(ctx, tx, key)
		if err != nil {
			return err
		}
		// two issues at once wait for each other: the second revokes the first
		if _, err := tx.Exec(ctx, `SELECT 1 FROM services WHERE id = $1 FOR UPDATE`, svc.ID); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE service_tokens SET revoked_at = now() WHERE service_id = $1 AND revoked_at IS NULL`, svc.ID)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO service_tokens (service_id, token_hash, prefix, created_by) VALUES ($1, $2, $3, $4)`,
			svc.ID, hashToken(token), prefix, p.UserID); err != nil {
			return err
		}
		kind := "ci_token_issued"
		if tag.RowsAffected() > 0 {
			kind = "ci_token_reissued"
		}
		return cycledata.New(tx).AddActivity(ctx, "service", svc.ID, kind, &p.UserID, false, map[string]any{"prefix": prefix})
	})
	if err != nil {
		return nil, err
	}
	return &IssuedToken{Token: token, Prefix: prefix}, nil
}

// RevokeToken revokes the active token of a service; without one it does nothing.
func (s *Service) RevokeToken(ctx context.Context, p *domain.Principal, key string) error {
	if !canManage(p, key) {
		return apperr.Forbidden("forbidden", "only owners of the service and administrators revoke its token")
	}
	return postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		svc, err := s.service(ctx, tx, key)
		if err != nil {
			return err
		}
		var prefix string
		err = tx.QueryRow(ctx, `UPDATE service_tokens SET revoked_at = now() WHERE service_id = $1 AND revoked_at IS NULL RETURNING prefix`, svc.ID).Scan(&prefix)
		if postgres.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return cycledata.New(tx).AddActivity(ctx, "service", svc.ID, "ci_token_revoked", &p.UserID, false, map[string]any{"prefix": prefix})
	})
}

// tokenOwner is the service of a token presented by CI.
type tokenOwner struct {
	id         uuid.UUID
	serviceID  uuid.UUID
	serviceKey string
	revoked    bool
	lastUsedAt *time.Time
}

func lookupToken(ctx context.Context, pool *pgxpool.Pool, token string) (*tokenOwner, error) {
	var o tokenOwner
	var revokedAt *time.Time
	err := pool.QueryRow(ctx, `SELECT t.id, t.service_id, s.key, t.revoked_at, t.last_used_at
		FROM service_tokens t JOIN services s ON s.id = t.service_id WHERE t.token_hash = $1`, hashToken(token)).
		Scan(&o.id, &o.serviceID, &o.serviceKey, &revokedAt, &o.lastUsedAt)
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.revoked = revokedAt != nil
	return &o, nil
}
