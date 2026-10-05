package app

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/config"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
)

// FTR.HMR.CMN-0006 R1, R2: GitHub sign-in links the git account only when it
// uses the OAuth app of the git provider; otherwise it is a separate GitHub
// OAuth App with its own scopes on github.com.
func TestLoginProvider(t *testing.T) {
	authURL := func(lp auth.LoginProvider) *url.URL {
		t.Helper()
		raw, err := lp.AuthURL(context.Background(), "st", "", "https://api.x/cb")
		if err != nil {
			t.Fatal(err)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	links := func(lp auth.LoginProvider) bool {
		l, ok := lp.(auth.GitLinker)
		return ok && l.LinksGitAccount()
	}

	gitlab := git.NewGitLab("https://gitlab.x", "https://gitlab.x", "o/r", "gl", "gls")
	cfg := &config.Config{AuthProvider: "github", GitProvider: "gitlab", GitBaseURL: "https://gitlab.x",
		GitHubLoginClientID: "login", GitHubLoginClientSecret: "s"}
	lp := loginProvider(cfg, gitlab)
	u := authURL(lp)
	if u.Host != "github.com" || u.Query().Get("client_id") != "login" || links(lp) {
		t.Fatalf("GitHub sign-in with GitLab: %s links=%v", u, links(lp))
	}
	if !strings.Contains(u.Query().Get("scope"), "user:email") || !strings.Contains(u.Query().Get("scope"), "read:org") {
		t.Fatalf("scopes %q", u.Query().Get("scope"))
	}

	gh := git.NewGitHub("https://github.com", "https://github.com", "o/r", "app", "apps")
	cfg = &config.Config{AuthProvider: "github", GitProvider: "github", GitBaseURL: "https://github.com", GitHubClientID: "app", GitHubSecret: "apps"}
	lp = loginProvider(cfg, gh)
	if u = authURL(lp); u.Query().Get("client_id") != "app" || !links(lp) {
		t.Fatalf("GitHub sign-in through the git app: %s links=%v", u, links(lp))
	}
	cfg.GitHubLoginClientID, cfg.GitHubLoginClientSecret = "login", "s"
	lp = loginProvider(cfg, gh)
	if u = authURL(lp); u.Query().Get("client_id") != "login" || links(lp) {
		t.Fatalf("separate sign-in app: %s links=%v", u, links(lp))
	}
	if lp = loginProvider(&config.Config{GitProvider: "gitlab", GitBaseURL: "https://gitlab.x"}, gitlab); !links(lp) || lp.Kind() != "git" {
		t.Fatal("the legacy sign-in links the git account")
	}
}
