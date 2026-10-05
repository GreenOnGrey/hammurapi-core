package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
)

// Sign-in through the provider of the deployment (FTR.HMR.CMN-0006 R1, arch §3):
//   - git (AUTH_PROVIDER empty): the OAuth of the git provider, as before;
//   - github: GitHub with the email and an organization restriction; when it is
//     the git provider too, the git account is linked by the same sign-in (R2);
//   - oidc: any OpenID Connect provider (Keycloak, ADFS), PKCE, email_verified.
// The model is the one of Nabu (FTR.NAB.CMN-0001 tech §2).

// Identity is a user as the sign-in provider sees them.
type Identity struct {
	Issuer, Subject     string
	Email, Name, Avatar string
	Username            string
}

// LoginResult is a completed sign-in. GitToken and GitUser are set when the
// sign-in provider is the git provider: the git account is linked at once.
type LoginResult struct {
	Identity Identity
	GitToken *git.Token
	GitUser  *git.User
	Emails   []string // verified emails of the git account
}

// LoginProvider is a sign-in provider.
type LoginProvider interface {
	Kind() string  // git | github | oidc
	Label() string // the name on the sign-in button
	AuthURL(ctx context.Context, state, verifier, redirectURL string) (string, error)
	Exchange(ctx context.Context, code, verifier, redirectURL string) (*LoginResult, error)
}

// ErrDenied is a sign-in refused by the rules; Reason goes to /login?error=.
type ErrDenied struct{ Reason string }

func (e *ErrDenied) Error() string { return "sign-in denied: " + e.Reason }

var loginHTTP = &http.Client{Timeout: 20 * time.Second}

func getJSON(ctx context.Context, u, bearer string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := loginHTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, fmt.Errorf("GET %s: %d", u, resp.StatusCode)
	}
	return resp.StatusCode, json.Unmarshal(b, out)
}

// ─── the git provider (legacy and GitHub) ───────────────────────────

// GitLogin signs in with the OAuth of the git provider. APIURL is the REST API
// of the provider (emails, organization membership); AllowedOrg restricts
// GitHub sign-in to members of an organization.
//
// LinksGit: the sign-in uses the OAuth app of the git provider of the
// deployment, so its user and token become the git account at once (R2).
// A separate sign-in app (GITHUB_LOGIN_*, or GitHub sign-in with GitLab as the
// git provider) gives only the identity; the git account is linked apart.
type GitLogin struct {
	Provider   git.Provider
	KindName   string // git | github
	APIURL     string
	AllowedOrg string
	LinksGit   bool
}

// GitLinker is implemented by sign-in providers that may link the git account.
type GitLinker interface{ LinksGitAccount() bool }

// LinksGitAccount implements GitLinker.
func (g *GitLogin) LinksGitAccount() bool { return g.LinksGit }

// Kind implements LoginProvider.
func (g *GitLogin) Kind() string { return g.KindName }

// Label implements LoginProvider.
func (g *GitLogin) Label() string {
	if g.Provider.Name() == "gitlab" && g.KindName == "git" {
		return "GitLab"
	}
	return "GitHub"
}

// AuthURL implements LoginProvider.
func (g *GitLogin) AuthURL(_ context.Context, state, _, redirectURL string) (string, error) {
	return g.Provider.AuthCodeURL(state, redirectURL), nil
}

// Exchange implements LoginProvider.
func (g *GitLogin) Exchange(ctx context.Context, code, _, redirectURL string) (*LoginResult, error) {
	tok, err := g.Provider.Exchange(ctx, code, redirectURL)
	if err != nil {
		return nil, err
	}
	u, err := g.Provider.CurrentUser(ctx, tok.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("load provider user: %w", err)
	}
	res := &LoginResult{
		Identity: Identity{Issuer: g.Provider.Name(), Subject: u.ID, Name: u.Name, Avatar: u.AvatarURL, Username: u.Username}}
	primary, emails := GitEmails(ctx, g.Provider.Name(), g.APIURL, tok.AccessToken)
	res.Identity.Email, res.Emails = primary, emails
	if g.LinksGit {
		res.GitToken, res.GitUser = tok, u
	}
	if g.KindName == "github" {
		if primary == "" {
			return nil, &ErrDenied{Reason: "no_verified_email"}
		}
		if g.AllowedOrg != "" {
			var m struct {
				State string `json:"state"`
			}
			status, err := getJSON(ctx, g.APIURL+"/user/memberships/orgs/"+url.PathEscape(g.AllowedOrg), tok.AccessToken, &m)
			if status == http.StatusNotFound || status == http.StatusForbidden || (err == nil && m.State != "active") {
				return nil, &ErrDenied{Reason: "not_org_member"} // IN-03
			}
			if err != nil {
				return nil, err
			}
		}
	}
	return res, nil
}

// GitEmails reads the verified emails of a git account: the primary first.
func GitEmails(ctx context.Context, provider, apiURL, token string) (string, []string) {
	var list []struct {
		Email     string     `json:"email"`
		Primary   bool       `json:"primary"`
		Verified  bool       `json:"verified"`
		Confirmed *time.Time `json:"confirmed_at"`
	}
	if _, err := getJSON(ctx, apiURL+"/user/emails", token, &list); err != nil {
		return "", nil
	}
	primary := ""
	var out []string
	for _, e := range list {
		ok := e.Verified || e.Confirmed != nil
		if !ok {
			continue
		}
		out = append(out, strings.ToLower(e.Email))
		if e.Primary || (provider == "gitlab" && primary == "") {
			primary = strings.ToLower(e.Email)
		}
	}
	if primary == "" && len(out) > 0 {
		primary = out[0] // the primary address is not verified: the first verified one
	}
	return primary, out
}

// ─── OIDC ───────────────────────────────────────────────────────────

// OIDCLogin signs in with any OpenID Connect provider: the authorization code
// flow with PKCE (S256); the identity comes from userinfo; email_verified is
// required.
type OIDCLogin struct {
	Issuer, ClientID, Secret, Scopes, Name string

	mu   sync.Mutex
	meta *oidcMeta
}

type oidcMeta struct {
	Authorization string `json:"authorization_endpoint"`
	Token         string `json:"token_endpoint"`
	UserInfo      string `json:"userinfo_endpoint"`
}

func (o *OIDCLogin) discover(ctx context.Context) (*oidcMeta, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.meta != nil {
		return o.meta, nil
	}
	var m oidcMeta
	if _, err := getJSON(ctx, o.Issuer+"/.well-known/openid-configuration", "", &m); err != nil {
		return nil, err
	}
	if m.Authorization == "" || m.Token == "" || m.UserInfo == "" {
		return nil, errors.New("oidc: incomplete discovery document")
	}
	o.meta = &m
	return o.meta, nil
}

// Kind implements LoginProvider.
func (o *OIDCLogin) Kind() string { return "oidc" }

// Label implements LoginProvider.
func (o *OIDCLogin) Label() string { return o.Name }

func challenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// AuthURL implements LoginProvider.
func (o *OIDCLogin) AuthURL(ctx context.Context, state, verifier, redirectURL string) (string, error) {
	m, err := o.discover(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{"response_type": {"code"}, "client_id": {o.ClientID}, "redirect_uri": {redirectURL},
		"scope": {o.Scopes}, "state": {state}, "code_challenge": {challenge(verifier)}, "code_challenge_method": {"S256"}}
	sep := "?"
	if strings.Contains(m.Authorization, "?") {
		sep = "&"
	}
	return m.Authorization + sep + q.Encode(), nil
}

// UserInfo is the userinfo answer.
type UserInfo struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified any    `json:"email_verified"`
	Name          string `json:"name"`
	PreferredName string `json:"preferred_username"`
	Picture       string `json:"picture"`
}

// Identity validates the userinfo (IN-02: a verified email is required).
func (u UserInfo) Identity(issuer string) (Identity, error) {
	verified := false
	switch v := u.EmailVerified.(type) {
	case bool:
		verified = v
	case string:
		verified = v == "true"
	}
	if u.Subject == "" {
		return Identity{}, errors.New("oidc: userinfo has no sub")
	}
	if u.Email == "" || !verified {
		return Identity{}, &ErrDenied{Reason: "email_not_verified"}
	}
	name := u.Name
	if name == "" {
		name = u.PreferredName
	}
	return Identity{Issuer: issuer, Subject: u.Subject, Email: strings.ToLower(u.Email), Name: name, Avatar: u.Picture, Username: u.PreferredName}, nil
}

// Exchange implements LoginProvider.
func (o *OIDCLogin) Exchange(ctx context.Context, code, verifier, redirectURL string) (*LoginResult, error) {
	m, err := o.discover(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURL},
		"client_id": {o.ClientID}, "client_secret": {o.Secret}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.Token, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := loginHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(b, &tok); err != nil || tok.AccessToken == "" {
		return nil, fmt.Errorf("oidc token: %d %s", resp.StatusCode, tok.Error)
	}
	var ui UserInfo
	if _, err := getJSON(ctx, m.UserInfo, tok.AccessToken, &ui); err != nil {
		return nil, err
	}
	id, err := ui.Identity(o.Issuer)
	if err != nil {
		return nil, err
	}
	return &LoginResult{Identity: id}, nil
}
