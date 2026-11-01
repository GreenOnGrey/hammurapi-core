//go:build integration

package itest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
)

// FTR.HMR.CMN-0006 §1.1: sign-in through a new provider finds the earlier
// account by a verified email (IN-04); a probable match without email waits
// for the administrator (IN-05); Nabu delegation creates a reader (NB-05).
func TestIdentities(t *testing.T) {
	ctx := context.Background()
	repo := auth.NewRepository(pool)
	nu := auth.NewUser{Language: "en", AgentName: "Codex", AgentTone: "business"}

	var ivan uuid.UUID
	must(t, pool.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, language, agent_name, agent_tone)
		VALUES ('gh-ivan','ivan','Ivan Petrov','en','Codex','business') RETURNING id`).Scan(&ivan))
	must(t, repo.LinkGitAccount(ctx, ivan, "github", "gh-ivan", "ivan", []string{"ivan@company.ru"}))

	kc := func(sub, email, name string) *auth.LoginResult {
		return &auth.LoginResult{Identity: auth.Identity{Issuer: "https://kc.example/realms/x", Subject: sub, Email: email, Name: name}}
	}
	row, created, err := repo.SignIn(ctx, kc("kc-1", "Ivan@Company.ru", "Ivan Petrov"), "github", nu)
	must(t, err)
	if created || row.ID != ivan {
		t.Fatalf("IN-04: %v created=%v, want the earlier account", row.ID, created)
	}
	row, created, err = repo.SignIn(ctx, kc("kc-1", "ivan@company.ru", ""), "github", nu)
	must(t, err)
	if created || row.ID != ivan {
		t.Fatal("the identity does not find the user again")
	}
	if e := repo.UserEmail(ctx, ivan); e != "ivan@company.ru" {
		t.Fatalf("email %q", e)
	}

	// IN-05: another email, the same name — a new user waiting for review.
	row, created, err = repo.SignIn(ctx, kc("kc-2", "i.petrov@other.org", "Ivan Petrov"), "github", nu)
	must(t, err)
	if !created || row.ID == ivan {
		t.Fatal("IN-05: a new user is expected")
	}
	list, err := repo.Unlinked(ctx)
	must(t, err)
	found := false
	for _, u := range list {
		if u.User.ID == row.ID {
			for _, c := range u.Candidates {
				found = found || c.UserID == ivan
			}
		}
	}
	if !found {
		t.Fatalf("IN-05: Ivan is not a candidate: %+v", list)
	}
	must(t, repo.Link(ctx, row.ID, ivan))
	row, created, err = repo.SignIn(ctx, kc("kc-2", "i.petrov@other.org", ""), "github", nu)
	must(t, err)
	if created || row.ID != ivan {
		t.Fatal("IN-05: after linking the identity leads to the earlier account")
	}

	// NB-05: a user unknown to Hammurapi is created without roles.
	nu.Admin = true
	id, err := repo.EnsureByEmail(ctx, "New.Reader@company.ru", nu)
	must(t, err)
	again, err := repo.EnsureByEmail(ctx, "new.reader@company.ru", nu)
	must(t, err)
	var via string
	var admin bool
	must(t, pool.QueryRow(ctx, `SELECT created_via, is_global_admin FROM users WHERE id = $1`, id).Scan(&via, &admin))
	if again != id || via != "nabu_delegation" {
		t.Fatalf("NB-05: %v %v %s", id, again, via)
	}
}

func agentInput(content, value string, m *cycledata.Measure) agent.DiscoveryInput {
	return agent.DiscoveryInput{Content: content, Value: value, Measure: m}
}
