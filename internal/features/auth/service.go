// Package auth implements sign-in through the instance's git provider,
// browser sessions with CSRF protection, and user token management.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/crypto"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
)

// SessionTTL is the sliding lifetime of a browser session.
const SessionTTL = 30 * 24 * time.Hour

// ErrReauth means the user's provider token cannot be refreshed; sign in again.
var ErrReauth = apperr.Unauthorized("reauth_required", "git provider session expired, sign in again")

var agentNames = []string{"Codex", "Stele", "Nabu", "Scribe", "Tablet", "Enki", "Lamassu", "Ziggurat", "Marduk", "Cuneus"}

// Service handles sign-in and tokens.
type Service struct {
	repo            *Repository
	provider        git.Provider
	box             *crypto.Box
	publicURL       string
	bootstrapAdmins map[string]bool
	defaultLanguage string
	// login is the sign-in provider (FTR.HMR.CMN-0006 R1); nil — the git provider.
	login  LoginProvider
	gitAPI string // REST API of the git provider (emails of git accounts)

	refreshMu sync.Map // per-user refresh serialization
}

// ErrGitAccountRequired: the action edits the repository on behalf of a user
// without a linked git account (R2, IN-06).
var ErrGitAccountRequired = apperr.Conflict("git_account_required", "link your git account to save changes to the repository")

// SetLogin sets the sign-in provider and the API of the git provider.
func (s *Service) SetLogin(lp LoginProvider, gitAPIURL string) {
	s.login, s.gitAPI = lp, strings.TrimRight(gitAPIURL, "/")
}

// LoginProvider returns the sign-in provider.
func (s *Service) LoginProvider() LoginProvider {
	if s.login == nil {
		s.login = &GitLogin{Provider: s.provider, KindName: "git", APIURL: s.gitAPI, LinksGit: true}
	}
	return s.login
}

// GitIsLogin reports whether the sign-in links the git account itself (the
// OAuth app of the git provider); otherwise the user links it apart.
func (s *Service) GitIsLogin() bool {
	l, ok := s.LoginProvider().(GitLinker)
	return ok && l.LinksGitAccount()
}

// GitRedirectURL is the OAuth callback of linking a git account.
func (s *Service) GitRedirectURL() string { return s.publicURL + "/api/v1/auth/git/callback" }

func (s *Service) newUser(email, login string) NewUser {
	return NewUser{Language: s.defaultLanguage, AgentName: randomPick(agentNames), AgentTone: randomPick(domain.Tones),
		Admin: (email != "" && s.bootstrapAdmins[strings.ToLower(email)]) || (login != "" && s.bootstrapAdmins[strings.ToLower(login)])}
}

// LoginWith completes a sign-in through the provider of the deployment:
// the user is found by identity, by the old git id or by email (tech §1.1),
// the git account is linked when the provider is the git provider.
func (s *Service) LoginWith(ctx context.Context, code, verifier string) (uuid.UUID, string, error) {
	res, err := s.LoginProvider().Exchange(ctx, code, verifier, s.RedirectURL())
	if err != nil {
		return uuid.Nil, "", err
	}
	u, created, err := s.repo.SignIn(ctx, res, s.provider.Name(), s.newUser(res.Identity.Email, res.Identity.Username))
	if err != nil {
		return uuid.Nil, "", err
	}
	if created {
		slog.InfoContext(ctx, "user created", "user_id", u.ID, "username", u.Username, "global_admin", u.GlobalAdmin, "via", s.LoginProvider().Kind())
	}
	if res.GitToken != nil {
		if err := s.storeToken(ctx, u.ID, res.GitToken); err != nil {
			return uuid.Nil, "", err
		}
	}
	csrf := randomHex(32)
	sid, err := s.repo.CreateSession(ctx, u.ID, csrf, SessionTTL)
	if err != nil {
		return uuid.Nil, "", err
	}
	return sid, csrf, nil
}

// LinkGit completes the OAuth of the git provider for a signed-in user (R2).
func (s *Service) LinkGit(ctx context.Context, uid uuid.UUID, code string) error {
	tok, err := s.provider.Exchange(ctx, code, s.GitRedirectURL())
	if err != nil {
		return apperr.Unauthorized("oauth_failed", err.Error())
	}
	gu, err := s.provider.CurrentUser(ctx, tok.AccessToken)
	if err != nil {
		return fmt.Errorf("load provider user: %w", err)
	}
	_, emails := GitEmails(ctx, s.provider.Name(), s.gitAPI, tok.AccessToken)
	if err := s.repo.LinkGitAccount(ctx, uid, s.provider.Name(), gu.ID, gu.Username, emails); err != nil {
		return err
	}
	return s.storeToken(ctx, uid, tok)
}

// Repo exposes the repository to the other slices of the package's users.
func (s *Service) Repo() *Repository { return s.repo }

// EnsureDelegated returns the user Nabu acts for, creating one without roles (R7).
func (s *Service) EnsureDelegated(ctx context.Context, email string) (uuid.UUID, error) {
	nu := s.newUser("", "")
	nu.Admin = false
	return s.repo.EnsureByEmail(ctx, email, nu)
}

// BackfillEmails fills the verified emails of git accounts with the tokens of
// their users (FTR.HMR.CMN-0006 arch §9 "+2", run by api in the background
// instead of a migration: migrations have no network). Failing tokens are
// skipped and logged; such users are linked by hand.
func (s *Service) BackfillEmails(ctx context.Context) {
	if err := s.repo.FixGitProvider(ctx, s.provider.Name()); err != nil {
		slog.WarnContext(ctx, "git accounts provider", "err", err)
	}
	rows, err := s.repo.pool.Query(ctx, `SELECT g.user_id FROM git_accounts g JOIN user_git_tokens t ON t.user_id = g.user_id
		WHERE g.emails_checked_at IS NULL LIMIT 1000`)
	if err != nil {
		return
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	done := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		tok, err := s.Token(ctx, id)
		if err == nil {
			primary, emails := GitEmails(ctx, s.provider.Name(), s.gitAPI, tok)
			if len(emails) > 0 {
				if _, err := s.repo.pool.Exec(ctx, `UPDATE git_accounts SET emails = $2 WHERE user_id = $1`, id, emails); err != nil {
					slog.WarnContext(ctx, "git account emails not saved", "user_id", id, "err", err)
				}
				// Never an empty string: NULL stays until a verified address is known.
				if primary != "" {
					if _, err := s.repo.pool.Exec(ctx, `UPDATE users SET email = $2 WHERE id = $1 AND email IS NULL
						AND NOT EXISTS (SELECT 1 FROM users WHERE lower(email) = $2)`, id, primary); err != nil {
						slog.WarnContext(ctx, "user email not saved", "user_id", id, "err", err)
					}
				}
				done++
			}
		} else {
			slog.InfoContext(ctx, "git account emails not read", "user_id", id, "err", err)
		}
		_, _ = s.repo.pool.Exec(ctx, `UPDATE git_accounts SET emails_checked_at = now() WHERE user_id = $1`, id)
	}
	if len(ids) > 0 {
		slog.InfoContext(ctx, "git account emails backfilled", "users", len(ids), "with_emails", done)
	}
}

// NewService creates the service.
func NewService(repo *Repository, provider git.Provider, box *crypto.Box, publicURL string, bootstrapAdmins []string, defaultLanguage string) *Service {
	ba := map[string]bool{}
	for _, a := range bootstrapAdmins {
		ba[strings.ToLower(a)] = true
	}
	if !domain.ValidLanguage(defaultLanguage) {
		defaultLanguage = "en"
	}
	return &Service{repo: repo, provider: provider, box: box, publicURL: publicURL, bootstrapAdmins: ba, defaultLanguage: defaultLanguage}
}

// RedirectURL is the OAuth callback URL registered at the provider.
func (s *Service) RedirectURL() string { return s.publicURL + "/api/v1/auth/callback" }

func (s *Service) storeToken(ctx context.Context, userID uuid.UUID, t *git.Token) error {
	acc, err := s.box.Seal(t.AccessToken)
	if err != nil {
		return err
	}
	var ref []byte
	if t.RefreshToken != "" {
		if ref, err = s.box.Seal(t.RefreshToken); err != nil {
			return err
		}
	}
	return s.repo.SaveToken(ctx, userID, EncryptedToken{Access: acc, Refresh: ref, ExpiresAt: t.Expiry})
}

// Token implements git.TokenSource: returns a valid access token, refreshing it
// with the refresh token when it is about to expire.
func (s *Service) Token(ctx context.Context, userID uuid.UUID) (string, error) {
	mu, _ := s.refreshMu.LoadOrStore(userID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	et, err := s.repo.Token(ctx, userID)
	if err != nil {
		return "", err
	}
	if et == nil {
		if !s.GitIsLogin() {
			return "", ErrGitAccountRequired
		}
		return "", ErrReauth
	}
	if time.Until(et.ExpiresAt) > 2*time.Minute {
		return s.box.Open(et.Access)
	}
	if len(et.Refresh) == 0 {
		return "", ErrReauth
	}
	rt, err := s.box.Open(et.Refresh)
	if err != nil {
		return "", err
	}
	nt, err := s.provider.Refresh(ctx, rt)
	if err != nil {
		slog.WarnContext(ctx, "token refresh failed", "user_id", userID, "err", err)
		if errors.Is(err, git.ErrUnauthorized) {
			_ = s.repo.DeleteToken(ctx, userID)
			return "", ErrReauth
		}
		return "", err
	}
	if nt.RefreshToken == "" {
		nt.RefreshToken = rt
	}
	if err := s.storeToken(ctx, userID, nt); err != nil {
		return "", err
	}
	return nt.AccessToken, nil
}

// MapGitError converts provider errors of user-initiated calls to API errors.
func MapGitError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, git.ErrUnauthorized):
		return ErrReauth
	}
	var ae *git.APIError
	if errors.As(err, &ae) {
		return apperr.Unprocessable("provider_refused", ae.Message).With("providerStatus", ae.Status)
	}
	return err
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomPick[T any](xs []T) T {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(xs))))
	return xs[n.Int64()]
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
