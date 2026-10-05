//go:build integration

package itest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/codegen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/discovery"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/issues"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
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

// NA-01…04: without the agent an issue goes to verification with an empty
// Discovery, tech and qa are edited by people, code generation is refused.
func TestWithoutAgent(t *testing.T) {
	ctx := context.Background()
	domain.SetAgentDisabled(true)
	defer domain.SetAgentDisabled(false)
	store := specdata.NewPG(pool)

	var olga uuid.UUID
	must(t, pool.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, language, agent_name, agent_tone)
		VALUES ('o1','olga','Olga','en','Codex','business') RETURNING id`).Scan(&olga))
	must(t, pool.QueryRow(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('NAG','No agent') RETURNING id)
		INSERT INTO domain_experts (domain_id, user_id, kind) SELECT d.id, $1, 'product' FROM d RETURNING $1::uuid`, olga).Scan(&olga))
	p := tu.User("expert:NAG:product")
	p.UserID = olga

	iss := issues.NewService(store, nil, nil, nopEvents{})
	is, err := iss.Create(ctx, p, issues.CreateInput{Type: domain.IssueIdea, Domain: "NAG", Title: "Manual analysis"})
	must(t, err)
	if is.Status != domain.IssueVerification {
		t.Fatalf("NA-01: status %s", is.Status)
	}
	var runs int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE subject_id = $1`, is.ID).Scan(&runs))
	if runs != 0 {
		t.Fatal("NA-01: Discovery must not start")
	}
	disc := discovery.NewService(pool, nopEvents{})
	v, err := disc.Get(ctx, is.Key)
	must(t, err)
	if v.Content != discovery.EmptyTemplate || len(v.Missing) == 0 {
		t.Fatalf("NA-01: document %+v", v)
	}
	// NA-02: the expert fills the document by hand.
	tu.Code(t, iss.Rediscover(ctx, p, is.Key), 409, "agent_disabled")
	measure := &cycledata.Measure{Source: "prom", Query: "q", Target: ">1", Window: "7d"}
	must(t, disc.Edit(ctx, p, is.Key, agentInput("## Ценность\n\nA lot", "A lot", measure)))
	v, err = disc.Get(ctx, is.Key)
	must(t, err)
	if len(v.Missing) != 0 {
		t.Fatalf("NA-02: missing %v", v.Missing)
	}
	var byAgent bool
	must(t, pool.QueryRow(ctx, `SELECT is_agent FROM discovery_revisions WHERE issue_id = $1 ORDER BY revision DESC LIMIT 1`, is.ID).Scan(&byAgent))
	if byAgent {
		t.Fatal("a manual edit is recorded as the agent's")
	}

	// NA-03, NA-04.
	if domain.AreaTech.Generated() || domain.AreaQA.Generated() {
		t.Fatal("NA-03: tech and qa are written by people")
	}
	_, err = codegen.StartTask(ctx, pool, codegen.TaskSpec{Type: codegen.TaskImplement})
	tu.Code(t, err, 409, "agent_disabled")
}

func agentInput(content, value string, m *cycledata.Measure) agent.DiscoveryInput {
	return agent.DiscoveryInput{Content: content, Value: value, Measure: m}
}
