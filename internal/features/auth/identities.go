package auth

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Created-via values of users (FTR.HMR.CMN-0006 arch §9).
const (
	CreatedViaLogin          = "login"
	CreatedViaNabuDelegation = "nabu_delegation"
)

// NewUser are the defaults of a user created at sign-in or by delegation.
type NewUser struct {
	Language  string
	AgentName string
	AgentTone domain.AgentTone
	Admin     bool
}

var loginChars = regexp.MustCompile(`[^a-z0-9._-]+`)

// uniqueUsername derives a free username from a login or an email.
func uniqueUsername(ctx context.Context, q postgres.Querier, base string) (string, error) {
	base = strings.ToLower(base)
	if i := strings.IndexByte(base, '@'); i > 0 {
		base = base[:i]
	}
	base = strings.Trim(loginChars.ReplaceAllString(base, "-"), "-.")
	if base == "" {
		base = "user"
	}
	for i := 0; i < 1000; i++ {
		cand := base
		if i > 0 {
			cand = fmt.Sprintf("%s-%d", base, i+1)
		}
		var n int
		if err := q.QueryRow(ctx, `SELECT count(*) FROM users WHERE username = $1`, cand).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return cand, nil
		}
	}
	return base + "-" + uuid.NewString()[:8], nil
}

// SignIn finds or creates the user of a sign-in (tech §1.1):
//  1. the identity (issuer, subject);
//  2. a user signed in before FTR.HMR.CMN-0006 through the same git provider
//     (provider_uid = subject);
//  3. a user whose email or one of the verified emails of the git account is
//     the email of the sign-in and who has no identity of this provider (IN-04);
//  4. otherwise a new user, marked for review when a probable match by name
//     or login exists (IN-05).
//
// The email is refreshed on every sign-in; the git account is linked when
// the sign-in provider is the git provider (IN-07).
func (r *Repository) SignIn(ctx context.Context, res *LoginResult, gitProvider string, nu NewUser) (row UserRow, created bool, err error) {
	id := res.Identity
	email := strings.ToLower(strings.TrimSpace(id.Email))
	err = postgres.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		var uid uuid.UUID
		err := tx.QueryRow(ctx, `SELECT user_id FROM user_identities WHERE issuer = $1 AND subject = $2`, id.Issuer, id.Subject).Scan(&uid)
		if postgres.IsNoRows(err) && id.Issuer == gitProvider {
			err = tx.QueryRow(ctx, `SELECT id FROM users WHERE provider_uid = $1`, id.Subject).Scan(&uid)
		}
		if postgres.IsNoRows(err) && email != "" {
			err = tx.QueryRow(ctx, `SELECT u.id FROM users u LEFT JOIN git_accounts g ON g.user_id = u.id
				WHERE (lower(u.email) = $1 OR $1 = ANY(g.emails))
				AND NOT EXISTS (SELECT 1 FROM user_identities i WHERE i.user_id = u.id AND i.issuer = $2)
				ORDER BY u.created_at LIMIT 1`, email, id.Issuer).Scan(&uid)
		}
		if postgres.IsNoRows(err) {
			created = true
			login := id.Username
			if login == "" {
				login = email
			}
			username, uerr := uniqueUsername(ctx, tx, login)
			if uerr != nil {
				return uerr
			}
			// provider_uid keeps the git user id for git sign-in; others get a unique placeholder.
			puid := id.Issuer + ":" + id.Subject
			if id.Issuer == gitProvider {
				puid = id.Subject
			}
			name := id.Name
			if name == "" {
				name = username
			}
			review := false
			if id.Issuer != gitProvider {
				var n int
				_ = tx.QueryRow(ctx, `SELECT count(*) FROM users u LEFT JOIN git_accounts g ON g.user_id = u.id
					WHERE lower(u.display_name) = lower($1) OR lower(g.login) = lower($2) OR lower(u.username) = lower($2)`,
					name, strings.SplitN(email, "@", 2)[0]).Scan(&n)
				review = n > 0
			}
			err = tx.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, avatar_url, language, agent_name, agent_tone,
				is_global_admin, email, link_review) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10) RETURNING id`,
				puid, username, name, nilIfEmpty(id.Avatar), nu.Language, nu.AgentName, nu.AgentTone, nu.Admin, email, review).Scan(&uid)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO user_identities (issuer, subject, user_id) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
			id.Issuer, id.Subject, uid); err != nil {
			return err
		}
		// The email follows the provider unless another user owns it.
		if email != "" {
			if _, err := tx.Exec(ctx, `UPDATE users SET email = $2 WHERE id = $1
				AND NOT EXISTS (SELECT 1 FROM users WHERE lower(email) = $2 AND id <> $1)`, uid, email); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET display_name = COALESCE(NULLIF($2,''), display_name),
			avatar_url = COALESCE(NULLIF($3,''), avatar_url), is_global_admin = is_global_admin OR $4 WHERE id = $1`,
			uid, id.Name, id.Avatar, nu.Admin); err != nil {
			return err
		}
		if res.GitUser != nil {
			if err := saveGitAccount(ctx, tx, uid, gitProvider, res.GitUser.ID, res.GitUser.Username, res.Emails); err != nil {
				return err
			}
		}
		row, err = scanUserRow(tx.QueryRow(ctx, userSelect+` WHERE id = $1`, uid))
		return err
	})
	return row, created, err
}

const userSelect = `SELECT id, provider_uid, username, display_name, avatar_url, language, theme, agent_name,
	agent_tone, is_global_admin, created_at FROM users`

func scanUserRow(row pgx.Row) (UserRow, error) {
	var u UserRow
	err := row.Scan(&u.ID, &u.ProviderUID, &u.Username, &u.DisplayName, &u.AvatarURL, &u.Language, &u.Theme, &u.AgentName,
		&u.AgentTone, &u.GlobalAdmin, &u.CreatedAt)
	return u, err
}

func saveGitAccount(ctx context.Context, q postgres.Querier, uid uuid.UUID, provider, extID, login string, emails []string) error {
	if emails == nil {
		emails = []string{}
	}
	_, err := q.Exec(ctx, `INSERT INTO git_accounts (user_id, provider, external_id, login, emails, emails_checked_at)
		VALUES ($1,$2,$3,$4,$5,now())
		ON CONFLICT (user_id) DO UPDATE SET provider = EXCLUDED.provider, external_id = EXCLUDED.external_id, login = EXCLUDED.login,
			emails = CASE WHEN cardinality(EXCLUDED.emails) > 0 THEN EXCLUDED.emails ELSE git_accounts.emails END,
			emails_checked_at = now(), linked_at = now()`, uid, provider, extID, login, emails)
	if postgres.IsUniqueViolation(err) {
		return apperr.Conflict("git_account_taken", "the git account is linked to another user")
	}
	return err
}

// LinkGitAccount links the git account of a user (after /auth/git/callback).
func (r *Repository) LinkGitAccount(ctx context.Context, uid uuid.UUID, provider, extID, login string, emails []string) error {
	return saveGitAccount(ctx, r.pool, uid, provider, extID, login, emails)
}

// GitAccount is the linked git account of a user.
type GitAccount struct {
	Provider string    `json:"provider"`
	Login    string    `json:"login"`
	Emails   []string  `json:"emails"`
	LinkedAt time.Time `json:"linkedAt"`
}

// GitAccountOf loads the git account of a user (nil — not linked).
func (r *Repository) GitAccountOf(ctx context.Context, uid uuid.UUID) (*GitAccount, error) {
	var g GitAccount
	err := r.pool.QueryRow(ctx, `SELECT g.provider, g.login, g.emails, g.linked_at FROM git_accounts g
		JOIN user_git_tokens t ON t.user_id = g.user_id WHERE g.user_id = $1`, uid).Scan(&g.Provider, &g.Login, &g.Emails, &g.LinkedAt)
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	return &g, err
}

// UnlinkGitAccount removes the git account and its token.
func (r *Repository) UnlinkGitAccount(ctx context.Context, uid uuid.UUID) error {
	return postgres.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM user_git_tokens WHERE user_id = $1`, uid); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM git_accounts WHERE user_id = $1`, uid)
		return err
	})
}

// FixGitProvider sets the provider of git accounts created by the migration.
func (r *Repository) FixGitProvider(ctx context.Context, provider string) error {
	_, err := r.pool.Exec(ctx, `UPDATE git_accounts SET provider = $1 WHERE provider = ''`, provider)
	return err
}

// EnsureByEmail returns the user with the email, creating one without roles
// for a call of Nabu on behalf of the user (R7, NB-08).
func (r *Repository) EnsureByEmail(ctx context.Context, email string, nu NewUser) (uuid.UUID, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if !strings.Contains(email, "@") {
		return uuid.Nil, apperr.BadRequest("invalid_email", "a valid email is required")
	}
	var uid uuid.UUID
	err := r.pool.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = $1`, email).Scan(&uid)
	if err == nil || !postgres.IsNoRows(err) {
		return uid, err
	}
	err = postgres.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		username, err := uniqueUsername(ctx, tx, email)
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, language, agent_name, agent_tone, email, created_via)
			VALUES ($1,$2,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING RETURNING id`,
			"nabu:"+email, username, nu.Language, nu.AgentName, nu.AgentTone, email, CreatedViaNabuDelegation).Scan(&uid)
	})
	if postgres.IsNoRows(err) || postgres.IsUniqueViolation(err) {
		err = r.pool.QueryRow(ctx, `SELECT id FROM users WHERE lower(email) = $1`, email).Scan(&uid)
	}
	return uid, err
}

// ─── administration: users without a link (tech §2) ─────────────────

// Candidate is a probable earlier account of a new user.
type Candidate struct {
	UserID   uuid.UUID `json:"userId"`
	Name     string    `json:"name"`
	GitLogin string    `json:"gitLogin"`
	Emails   []string  `json:"emails"`
}

// Unlinked is a new user waiting for a decision of the global administrator.
type Unlinked struct {
	User       UserBrief   `json:"user"`
	Candidates []Candidate `json:"candidates"`
}

// UserBrief identifies a user.
type UserBrief struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	Email     *string   `json:"email"`
	CreatedAt time.Time `json:"createdAt"`
}

// Unlinked lists the users marked for review with their candidates.
func (r *Repository) Unlinked(ctx context.Context) ([]Unlinked, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, display_name, email, created_at FROM users WHERE link_review ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	var list []Unlinked
	for rows.Next() {
		var u Unlinked
		if err := rows.Scan(&u.User.ID, &u.User.Name, &u.User.Email, &u.User.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, u)
	}
	rows.Close()
	out := []Unlinked{}
	for _, u := range list {
		local := ""
		if u.User.Email != nil {
			local = strings.SplitN(*u.User.Email, "@", 2)[0]
		}
		crows, err := r.pool.Query(ctx, `SELECT u.id, u.display_name, COALESCE(g.login, u.username), COALESCE(g.emails, '{}')
			FROM users u LEFT JOIN git_accounts g ON g.user_id = u.id
			WHERE u.id <> $1 AND (lower(u.display_name) = lower($2) OR lower(g.login) = lower($3) OR lower(u.username) = lower($3))
			ORDER BY u.created_at LIMIT 5`, u.User.ID, u.User.Name, local)
		if err != nil {
			return nil, err
		}
		u.Candidates = []Candidate{}
		for crows.Next() {
			var c Candidate
			if err := crows.Scan(&c.UserID, &c.Name, &c.GitLogin, &c.Emails); err != nil {
				crows.Close()
				return nil, err
			}
			u.Candidates = append(u.Candidates, c)
		}
		crows.Close()
		out = append(out, u)
	}
	return out, nil
}

// Link moves the identities and the email of a new user to the earlier
// account and deletes the new one (IN-05). The caller writes the journal.
func (r *Repository) Link(ctx context.Context, newID, targetID uuid.UUID) error {
	if newID == targetID {
		return apperr.BadRequest("invalid_target", "a user cannot be linked to itself")
	}
	return postgres.InTx(ctx, r.pool, func(tx pgx.Tx) error {
		var email *string
		if err := tx.QueryRow(ctx, `SELECT email FROM users WHERE id = $1 AND link_review`, newID).Scan(&email); err != nil {
			return apperr.NotFound("not_found", "no such user to link")
		}
		if _, err := tx.Exec(ctx, `UPDATE user_identities SET user_id = $2 WHERE user_id = $1`, newID, targetID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id = $1`, newID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, newID); err != nil {
			return apperr.Conflict("user_has_data", "the new user already has data and cannot be merged")
		}
		if email != nil {
			if _, err := tx.Exec(ctx, `UPDATE users SET email = $2 WHERE id = $1`, targetID, *email); err != nil {
				return err
			}
		}
		return nil
	})
}

// ConfirmNew clears the review mark: the user is really new.
func (r *Repository) ConfirmNew(ctx context.Context, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `UPDATE users SET link_review = false WHERE id = $1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return apperr.NotFound("not_found", "user not found")
	}
	return err
}

// UserEmail returns the email of a user ("" when unknown).
func (r *Repository) UserEmail(ctx context.Context, id uuid.UUID) string {
	var e *string
	_ = r.pool.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, id).Scan(&e)
	if e == nil {
		return ""
	}
	return *e
}
