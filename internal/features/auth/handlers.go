package auth

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/logging"
)

// Cookie names.
const (
	SessionCookie = "hmr_session"
	CSRFCookie    = "csrf_token"
	stateCookie   = "hmr_oauth_state"
	// gitStateCookie keeps the state of linking a git account and the page to return to.
	gitStateCookie = "hmr_git_state"
)

// PublicConfig is returned by GET /api/v1/config.
type PublicConfig struct {
	Provider        string   `json:"provider"`
	UploadMaxBytes  int64    `json:"uploadMaxBytes"`
	UploadTypes     []string `json:"uploadAllowedTypes"`
	ImportMaxBytes  int64    `json:"importMaxBytes"`
	Languages       []string `json:"languages"`
	DefaultLanguage string   `json:"defaultLanguage"`
	DefaultBranch   string   `json:"defaultBranch"`
	// BootstrapAdminsConfigured tells deploy checks that the stand owner will
	// become the first global administrator.
	BootstrapAdminsConfigured bool `json:"bootstrapAdminsConfigured"`
	// Login is the sign-in provider (FTR.HMR.CMN-0006 R1): git | github | oidc,
	// its label on the button and the organization of GitHub sign-in.
	Login LoginInfo `json:"login"`
	// Agent says whether the agent of Nabu is connected (R4, R9).
	Agent AgentInfo `json:"agent"`
}

// LoginInfo describes the sign-in provider for the sign-in screen.
type LoginInfo struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Org   string `json:"org,omitempty"`
	// LinksGit: the sign-in account is the git account (it cannot be unlinked).
	LinksGit bool `json:"linksGit"`
}

// AgentInfo is the agent block of /api/v1/config (tech §3.1).
type AgentInfo struct {
	Enabled  bool   `json:"enabled"`
	Provider string `json:"provider,omitempty"`
}

// Handlers serves auth endpoints.
type Handlers struct {
	svc          *Service
	cfg          PublicConfig
	secure       bool
	webURL       string // where the browser returns after sign-in ("" — same origin)
	cookieDomain string // Domain of the session and CSRF cookies ("" — host only)
}

// NewHandlers creates handlers; secure sets the Secure cookie attribute.
func NewHandlers(svc *Service, cfg PublicConfig, secure bool) *Handlers {
	return &Handlers{svc: svc, cfg: cfg, secure: secure}
}

// WithWeb sets the SPA URL for redirects after sign-in and the cookie domain
// shared by the SPA and the API when they live on different subdomains.
func (h *Handlers) WithWeb(webURL, cookieDomain string) *Handlers {
	h.webURL = strings.TrimRight(webURL, "/")
	h.cookieDomain = cookieDomain
	return h
}

func (h *Handlers) web(path string) string { return h.webURL + path }

// Public mounts unauthenticated routes.
func (h *Handlers) Public(r chi.Router) {
	r.Get("/config", func(w http.ResponseWriter, _ *http.Request) { httpx.JSON(w, 200, h.cfg) })
	r.Get("/auth/login", h.login)
	r.Get("/auth/callback", h.callback)
	r.Get("/auth/git/link", h.gitLink)
	r.Get("/auth/git/callback", h.gitCallback)
}

// Private mounts routes that need a session.
func (h *Handlers) Private(r chi.Router) {
	r.Get("/auth/me", httpx.Handler(h.me))
	r.Post("/auth/logout", httpx.Handler(h.logout))
	r.Delete("/me/git-account", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		if h.svc.GitIsLogin() {
			return apperr.Conflict("git_account_is_login", "the git account is the sign-in account and cannot be unlinked")
		}
		if err := h.svc.repo.UnlinkGitAccount(r.Context(), p.UserID); err != nil {
			return err
		}
		httpx.NoContent(w)
		return nil
	}))
}

func (h *Handlers) login(w http.ResponseWriter, r *http.Request) {
	state, verifier := randomHex(16), randomHex(32)
	u, err := h.svc.LoginProvider().AuthURL(r.Context(), state, verifier, h.svc.RedirectURL())
	if err != nil {
		slog.ErrorContext(r.Context(), "sign-in provider unavailable", "err", err)
		http.Redirect(w, r, h.web("/login?error=provider"), http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state + "." + verifier, Path: "/api/v1/auth", HttpOnly: true,
		Secure: h.secure, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.Redirect(w, r, u, http.StatusFound)
}

// gitLink starts linking the git account (R2); returnTo is a path of the SPA.
func (h *Handlers) gitLink(w http.ResponseWriter, r *http.Request) {
	if httpx.SessionFrom(r.Context()) == nil {
		http.Redirect(w, r, h.web("/login"), http.StatusFound)
		return
	}
	ret := r.URL.Query().Get("returnTo")
	if !strings.HasPrefix(ret, "/") || strings.HasPrefix(ret, "//") {
		ret = "/"
	}
	state := randomHex(16)
	http.SetCookie(w, &http.Cookie{Name: gitStateCookie, Value: state + "|" + ret, Path: "/api/v1/auth/git", HttpOnly: true,
		Secure: h.secure, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	http.Redirect(w, r, h.svc.provider.AuthCodeURL(state, h.svc.GitRedirectURL()), http.StatusFound)
}

func (h *Handlers) gitCallback(w http.ResponseWriter, r *http.Request) {
	s := httpx.SessionFrom(r.Context())
	c, err := r.Cookie(gitStateCookie)
	q := r.URL.Query()
	if s == nil || err != nil {
		http.Redirect(w, r, h.web("/login"), http.StatusFound)
		return
	}
	state, ret, _ := strings.Cut(c.Value, "|")
	http.SetCookie(w, &http.Cookie{Name: gitStateCookie, Path: "/api/v1/auth/git", MaxAge: -1})
	sep := "?"
	if strings.Contains(ret, "?") {
		sep = "&"
	}
	if q.Get("state") == "" || subtle.ConstantTimeCompare([]byte(state), []byte(q.Get("state"))) != 1 || q.Get("error") != "" {
		http.Redirect(w, r, h.web(ret+sep+"gitLink=failed"), http.StatusFound)
		return
	}
	if err := h.svc.LinkGit(r.Context(), s.UserID, q.Get("code")); err != nil {
		slog.ErrorContext(r.Context(), "git account link failed", "err", err)
		code := "failed"
		if e, ok := apperr.As(err); ok && e.Code == "git_account_taken" {
			code = "taken"
		}
		http.Redirect(w, r, h.web(ret+sep+"gitLink="+code), http.StatusFound)
		return
	}
	http.Redirect(w, r, h.web(ret+sep+"gitLink=ok"), http.StatusFound)
}

func (h *Handlers) callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(stateCookie)
	q := r.URL.Query()
	var state, verifier string
	if err == nil {
		state, verifier, _ = strings.Cut(c.Value, ".")
	}
	if err != nil || q.Get("state") == "" || subtle.ConstantTimeCompare([]byte(state), []byte(q.Get("state"))) != 1 {
		http.Redirect(w, r, h.web("/login?error=state"), http.StatusFound)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/api/v1/auth", MaxAge: -1})
	if q.Get("error") != "" {
		http.Redirect(w, r, h.web("/login?error=denied"), http.StatusFound)
		return
	}
	sid, csrf, err := h.svc.LoginWith(r.Context(), q.Get("code"), verifier)
	var denied *ErrDenied
	if errors.As(err, &denied) {
		slog.InfoContext(r.Context(), "sign-in denied", "reason", denied.Reason)
		http.Redirect(w, r, h.web("/login?error="+denied.Reason), http.StatusFound)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "login failed", "err", err)
		http.Redirect(w, r, h.web("/login?error=failed"), http.StatusFound)
		return
	}
	h.setSessionCookies(w, sid, csrf)
	http.Redirect(w, r, h.web("/"), http.StatusFound)
}

func (h *Handlers) setSessionCookies(w http.ResponseWriter, sid uuid.UUID, csrf string) {
	maxAge := int(SessionTTL.Seconds())
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: sid.String(), Path: "/", Domain: h.cookieDomain, HttpOnly: true,
		Secure: h.secure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
	http.SetCookie(w, &http.Cookie{Name: CSRFCookie, Value: csrf, Path: "/", Domain: h.cookieDomain, HttpOnly: false,
		Secure: h.secure, SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}

// Me is the response of /auth/me.
type Me struct {
	ID          uuid.UUID       `json:"id"`
	Username    string          `json:"username"`
	DisplayName string          `json:"displayName"`
	AvatarURL   *string         `json:"avatarUrl"`
	GlobalAdmin bool            `json:"globalAdmin"`
	AreaAdmin   []domain.Area   `json:"areaAdmin"`
	Experts     []ExpertDomains `json:"experts"`
	Services    []string        `json:"ownedServices"`
	Language    string          `json:"language"`
	Theme       string          `json:"theme"`
	AgentName   string          `json:"agentName"`
	AgentTone   string          `json:"agentTone"`
	// FTR.HMR.CMN-0006: the email (the key to Nabu), the sign-in provider and
	// the linked git account (the profile's "Accounts" block).
	Email      *string     `json:"email"`
	Login      LoginInfo   `json:"login"`
	GitAccount *GitAccount `json:"gitAccount"`
}

// ExpertDomains lists the expert kinds of a user in one domain.
type ExpertDomains struct {
	Domain string              `json:"domain"`
	Kinds  []domain.ExpertKind `json:"kinds"`
}

// ExpertsOf lists expert roles of a principal in a stable order.
func ExpertsOf(p *domain.Principal) []ExpertDomains {
	out := []ExpertDomains{}
	for _, d := range p.ExpertDomains() {
		e := ExpertDomains{Domain: d, Kinds: []domain.ExpertKind{}}
		for _, k := range []domain.ExpertKind{domain.ExpertProduct, domain.ExpertTechnical} {
			if p.HasExpert(d, k) {
				e.Kinds = append(e.Kinds, k)
			}
		}
		out = append(out, e)
	}
	return out
}

// OwnedServices lists owned service keys, sorted.
func OwnedServices(p *domain.Principal) []string {
	out := []string{}
	for k, ok := range p.OwnedServices {
		if ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (h *Handlers) me(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	u, err := h.svc.repo.User(r.Context(), p.UserID)
	if err != nil {
		return err
	}
	if u == nil {
		return apperr.ErrNoSession
	}
	me := Me{ID: u.ID, Username: u.Username, DisplayName: u.DisplayName, AvatarURL: u.AvatarURL,
		GlobalAdmin: p.GlobalAdmin, AreaAdmin: nonNil(p.AdminAreas()), Experts: ExpertsOf(p), Services: OwnedServices(p),
		Language: u.Language, Theme: u.Theme,
		AgentName: u.AgentName, AgentTone: string(u.AgentTone)}
	if e := h.svc.repo.UserEmail(r.Context(), u.ID); e != "" {
		me.Email = &e
	}
	me.Login = h.cfg.Login
	if me.GitAccount, err = h.svc.repo.GitAccountOf(r.Context(), u.ID); err != nil {
		return err
	}
	httpx.JSON(w, 200, me)
	return nil
}

func (h *Handlers) logout(w http.ResponseWriter, r *http.Request) error {
	if s := httpx.SessionFrom(r.Context()); s != nil {
		if err := h.svc.repo.DeleteSession(r.Context(), s.ID); err != nil {
			return err
		}
	}
	// A cookie is removed only with the same Domain it was set with.
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Path: "/", Domain: h.cookieDomain, MaxAge: -1})
	http.SetCookie(w, &http.Cookie{Name: CSRFCookie, Path: "/", Domain: h.cookieDomain, MaxAge: -1})
	httpx.NoContent(w)
	return nil
}

// Authenticate loads the session and principal when a session cookie is
// present. It never rejects; RequireSession does.
func (h *Handlers) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(SessionCookie)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		sid, err := uuid.Parse(c.Value)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		sess, err := h.svc.repo.Session(ctx, sid)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		if sess == nil {
			next.ServeHTTP(w, r)
			return
		}
		p, err := h.svc.repo.Principal(ctx, sess.UserID)
		if err != nil {
			httpx.Error(w, r, err)
			return
		}
		if p == nil {
			next.ServeHTTP(w, r)
			return
		}
		if time.Since(sess.LastSeenAt) > 5*time.Minute {
			_ = h.svc.repo.TouchSession(ctx, sess.ID, SessionTTL)
		}
		httpx.SetUserForLog(r, p.UserID.String())
		ctx = httpx.WithSession(ctx, &httpx.Session{ID: sess.ID, UserID: sess.UserID, CSRFToken: sess.CSRFToken})
		ctx = httpx.WithPrincipal(ctx, p)
		ctx = logging.With(ctx, slog.String("user_id", p.UserID.String()))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireSession rejects requests without a session and enforces the CSRF
// double-submit token on state-changing methods.
func RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := httpx.SessionFrom(r.Context())
		if s == nil {
			httpx.Error(w, r, apperr.ErrNoSession)
			return
		}
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			if !validCSRF(r, s.CSRFToken) {
				httpx.Error(w, r, apperr.ErrCSRF)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func validCSRF(r *http.Request, sessionToken string) bool {
	hdr := strings.TrimSpace(r.Header.Get("X-CSRF-Token"))
	c, err := r.Cookie(CSRFCookie)
	if hdr == "" || err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hdr), []byte(c.Value)) == 1 &&
		subtle.ConstantTimeCompare([]byte(hdr), []byte(sessionToken)) == 1
}

func nonNil(a []domain.Area) []domain.Area {
	if a == nil {
		return []domain.Area{}
	}
	return a
}
