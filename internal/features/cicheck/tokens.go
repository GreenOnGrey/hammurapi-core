// Package cicheck delivers the check of speclint to the CI of repositories (FTR.HMR.SPEC-0002): the test cases of
// a service for its CI, opened by the OIDC token of the CI job or by the installation token (FTR.HMR.SPEC-0003),
// the status of the check in the repository of specifications and of services, the pull requests that add the
// check and keep its version on the contract.
package cicheck

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// tokenPrefix starts the installation token: a scanner of secrets knows it, and the API tells it from a JWT.
const tokenPrefix = "hmr_ci_"

// shownPrefix is the part of the token Settings show: hmr_ci_ and four characters.
const shownPrefix = len(tokenPrefix) + 4

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// newToken is hmr_ci_ and 32 random bytes in base32.
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

// TokenInfo is the installation token as Settings show it (FTR.HMR.SPEC-0003 tech §3.2): never the token itself.
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

func requireGlobal(p *domain.Principal) error {
	if p == nil || !p.GlobalAdmin {
		return apperr.NoRole("admin", "global")
	}
	return nil
}

// Token describes the active installation token.
func (s *Service) Token(ctx context.Context, p *domain.Principal) (*TokenInfo, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	var t TokenInfo
	err := s.pool.QueryRow(ctx, `SELECT t.prefix, t.created_at, u.display_name, t.last_used_at
		FROM ci_tokens t JOIN users u ON u.id = t.created_by WHERE t.revoked_at IS NULL`).Scan(&t.Prefix, &t.CreatedAt, &t.CreatedBy, &t.LastUsedAt)
	if postgres.IsNoRows(err) {
		return &TokenInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	t.Issued = true
	return &t, nil
}

// ciTokenLock serializes issuing: two issues at once wait for each other, the second revokes the first.
const ciTokenLock = "hashtext('ci_tokens')"

// IssueToken issues the installation token; an active one is revoked in the same transaction (R3).
func (s *Service) IssueToken(ctx context.Context, p *domain.Principal) (*IssuedToken, error) {
	if err := requireGlobal(p); err != nil {
		return nil, err
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	prefix := token[:shownPrefix]
	err = postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(`+ciTokenLock+`)`); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE ci_tokens SET revoked_at = now() WHERE revoked_at IS NULL`)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO ci_tokens (token_hash, prefix, created_by) VALUES ($1, $2, $3)`, hashToken(token), prefix, p.UserID); err != nil {
			return err
		}
		kind := "ci_token_issued"
		if tag.RowsAffected() > 0 {
			kind = "ci_token_reissued"
		}
		return audit(ctx, p.UserID, kind, prefix)
	})
	if err != nil {
		return nil, err
	}
	return &IssuedToken{Token: token, Prefix: prefix}, nil
}

// RevokeToken revokes the installation token; without one it does nothing.
func (s *Service) RevokeToken(ctx context.Context, p *domain.Principal) error {
	if err := requireGlobal(p); err != nil {
		return err
	}
	return postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var prefix string
		err := tx.QueryRow(ctx, `UPDATE ci_tokens SET revoked_at = now() WHERE revoked_at IS NULL RETURNING prefix`).Scan(&prefix)
		if postgres.IsNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return audit(ctx, p.UserID, "ci_token_revoked", prefix)
	})
}

// audit records the issue, the reissue and the revocation with the user (arch §6): the rows of ci_tokens keep
// who issued a token and when it was revoked, the log keeps who did what.
func audit(ctx context.Context, user uuid.UUID, action, prefix string) error {
	slog.InfoContext(ctx, "ci token", "action", action, "user", user, "prefix", prefix)
	return nil
}

// installation is the installation token presented by CI.
type installation struct {
	id         uuid.UUID
	revoked    bool
	lastUsedAt *time.Time
}

func lookupToken(ctx context.Context, pool *pgxpool.Pool, token string) (*installation, error) {
	var o installation
	var revokedAt *time.Time
	err := pool.QueryRow(ctx, `SELECT id, revoked_at, last_used_at FROM ci_tokens WHERE token_hash = $1`, hashToken(token)).
		Scan(&o.id, &revokedAt, &o.lastUsedAt)
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.revoked = revokedAt != nil
	return &o, nil
}
