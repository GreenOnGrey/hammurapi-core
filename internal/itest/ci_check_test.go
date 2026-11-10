//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GreenOnGrey/speclint/contract"
	"github.com/GreenOnGrey/speclint/starter"
	"github.com/go-chi/chi/v5"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/cicheck"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
)

// ciRepos are repositories of a provider: files by repository and branch, pull requests and their states.
type ciRepos struct {
	mu        sync.Mutex
	name      string                                  // github | gitlab
	files     map[string]map[string]map[string]string // repo → branch → path → content
	prs       map[int]*git.PRInfo
	byRepo    map[int]string
	commits   []git.FileChange
	forbidden bool // a GitHub App without Workflows: write
}

func (c *ciRepos) provider(t *testing.T, repo string) *mocks.MockProvider {
	p := mocks.NewMockProvider(gomock.NewController(t))
	p.EXPECT().Name().Return(c.name).AnyTimes()
	p.EXPECT().Repo().Return(repo).AnyTimes()
	p.EXPECT().ForRepo(gomock.Any()).DoAndReturn(func(r string) git.Provider { return c.provider(t, r) }).AnyTimes()
	p.EXPECT().BotToken(gomock.Any()).Return("bot", nil).AnyTimes()
	p.EXPECT().DefaultBranch(gomock.Any(), gomock.Any()).Return("main", nil).AnyTimes()
	p.EXPECT().GetFile(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _, ref, path string) (*git.File, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		s, ok := c.files[repo][ref][path]
		if !ok {
			return nil, git.ErrNotFound
		}
		return &git.File{Content: []byte(s)}, nil
	}).AnyTimes()
	p.EXPECT().BranchHead(gomock.Any(), gomock.Any(), gomock.Any()).Return("head", nil).AnyTimes()
	p.EXPECT().CreateBranch(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _, name, _ string) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		b := map[string]string{}
		for k, v := range c.files[repo]["main"] {
			b[k] = v
		}
		c.files[repo][name] = b
		return nil
	}).AnyTimes()
	p.EXPECT().DeleteBranch(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	p.EXPECT().Commit(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, token, branch, _ string, changes []git.FileChange) (string, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.forbidden {
				return "", &git.APIError{Provider: c.name, Status: 403, Message: "refusing to allow a GitHub App to create or update workflow without `workflows` permission"}
			}
			for _, ch := range changes {
				c.files[repo][branch][ch.Path] = string(ch.Content)
			}
			c.commits = append(c.commits, changes...)
			return token, nil
		}).AnyTimes()
	p.EXPECT().CreatePR(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, head, _, title, _ string) (*git.PR, error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			n := len(c.prs) + 1
			c.prs[n] = &git.PRInfo{Number: n, URL: "http://pr", Title: title, State: "open", Branch: head}
			c.byRepo[n] = repo
			return &git.PR{Number: n, URL: "http://pr"}, nil
		}).AnyTimes()
	p.EXPECT().GetPR(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, n int) (*git.PRInfo, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if pr, ok := c.prs[n]; ok {
			return pr, nil
		}
		return nil, git.ErrNotFound
	}).AnyTimes()
	return p
}

// merge merges a pull request into main of its repository.
func (c *ciRepos) merge(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pr, repo := c.prs[n], c.byRepo[n]
	for k, v := range c.files[repo][pr.Branch] {
		c.files[repo]["main"][k] = v
	}
	pr.State = "merged"
}

// FTR.HMR.SPEC-0002 R6–R11: the tokens of services, the test cases for CI, the check in the repositories.
func TestCICheck(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "cicheck")
	must(t, err)
	defer drop()
	pool, err := postgres.Connect(ctx, url)
	must(t, err)
	defer pool.Close()
	must(t, postgres.Migrate(ctx, url))

	var uid string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, language, agent_name, agent_tone)
		VALUES ('o1','olga','Olga','en','Codex','business') RETURNING id`).Scan(&uid))
	var sysID string
	must(t, pool.QueryRow(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('FMS','Fleet') RETURNING id)
		INSERT INTO systems (domain_id, key, name) SELECT id, 'CAR', 'Cars' FROM d RETURNING id`).Scan(&sysID))
	cd := cycledata.New(pool)
	for _, s := range []string{"booking", "pricing"} {
		must(t, cd.UpsertService(ctx, &cycledata.Service{Key: s, Name: s, Repo: "fms/" + s, Source: "manual"}))
	}
	// SVC-06, SVC-07: features of booking in several phases with their test cases
	for i, ph := range []string{"released", "codegen", "spec", "rolled_back"} {
		n := i + 1
		key := "FTR.FMS.CAR-000" + string(rune('0'+n))
		var fid string
		must(t, pool.QueryRow(ctx, `INSERT INTO features (unique_id, system_id, number, title, branch_name, pr_number, pr_url, phase, created_by)
			VALUES ($1, $2, $3, 'F', 'b', 1, 'http://pr', $4, $5) RETURNING id`, key, sysID, n, ph, uid).Scan(&fid))
		must(t, pool.QueryRow(ctx, `INSERT INTO feature_services (feature_id, service_id) SELECT $1, id FROM services WHERE key = 'booking' RETURNING feature_id`, fid).Scan(&fid))
		_, err := pool.Exec(ctx, `INSERT INTO test_cases (feature_id, tc_id, level, req_ids) VALUES ($1, $2, 'I', '{R1}')`, fid, "BOOK-0"+string(rune('0'+n)))
		must(t, err)
	}

	repos := &ciRepos{name: "github", files: map[string]map[string]map[string]string{
		"org/specs": {"main": {}}, "fms/booking": {"main": {}}, "fms/pricing": {"main": {}},
	}, prs: map[int]*git.PRInfo{}, byRepo: map[int]string{}}
	tokens := mocks.NewMockTokenSource(gomock.NewController(t))
	tokens.EXPECT().Token(gomock.Any(), gomock.Any()).Return("user", nil).AnyTimes()
	svc := cicheck.NewService(pool, repos.provider(t, "org/specs"), tokens, "main", "https://hammurapi.example")
	r := chi.NewRouter()
	svc.CIRoutes(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	get := func(service, token string) (int, map[string]any) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/ci/services/"+service+"/test-cases", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		defer resp.Body.Close()
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	owner := tu.User("owner:booking")
	must(t, pool.QueryRow(ctx, `SELECT id FROM users WHERE username = 'olga'`).Scan(&owner.UserID))
	var ae *apperr.Error
	// TOK-07: a user without rights on the service
	if _, err := svc.IssueToken(ctx, tu.User("owner:pricing"), "booking"); !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("TOK-07: %v", err)
	}
	// TOK-01: the token is shown once, the database keeps its hash and prefix
	issued, err := svc.IssueToken(ctx, owner, "booking")
	must(t, err)
	var stored int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM service_tokens WHERE prefix = $1 AND token_hash <> convert_to($2, 'UTF8')`, issued.Prefix, issued.Token).Scan(&stored))
	info, err := svc.Token(ctx, owner, "booking")
	must(t, err)
	if stored != 1 || !info.Issued || *info.Prefix != issued.Prefix || info.LastUsedAt != nil {
		t.Fatalf("TOK-01: %d %+v", stored, info)
	}

	// SVC-06, SVC-07: the features, their phases, required from code generation on, rolled back not
	code, body := get("booking", issued.Token)
	if code != 200 || body["contractVersion"] != contract.Version || len(body["cases"].([]any)) != 4 {
		t.Fatalf("SVC-06: %d %v", code, body)
	}
	required := map[string]bool{}
	for _, f := range body["features"].([]any) {
		m := f.(map[string]any)
		required[m["phase"].(string)] = m["required"].(bool)
	}
	if !required["released"] || !required["codegen"] || required["spec"] || required["rolled_back"] {
		t.Fatalf("SVC-07: %v", required)
	}
	// TOK-06: the use is seen in the card
	if info, _ = svc.Token(ctx, owner, "booking"); info.LastUsedAt == nil {
		t.Fatal("TOK-06: last_used_at")
	}
	// TOK-02: a token of booking for pricing
	if code, body = get("pricing", issued.Token); code != 403 || body["error"].(map[string]any)["code"] != "token_scope" {
		t.Fatalf("TOK-02: %d %v", code, body)
	}
	if code, _ = get("booking", "hmr_svc_unknown"); code != 401 {
		t.Fatalf("an unknown token: %d", code)
	}
	if code, _ = get("missing", issued.Token); code != 403 { // a token does not tell which services exist
		t.Fatalf("a missing service: %d", code)
	}
	// TOK-05: the token of a service opens no session route
	if code, _ = get("booking", ""); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	// TOK-03: reissued — the old token is revoked, the new one works
	again, err := svc.IssueToken(ctx, owner, "booking")
	must(t, err)
	if code, body = get("booking", issued.Token); code != 401 || body["error"].(map[string]any)["code"] != "token_revoked" {
		t.Fatalf("TOK-03 old: %d %v", code, body)
	}
	if code, _ = get("booking", again.Token); code != 200 {
		t.Fatalf("TOK-03 new: %d", code)
	}
	// TOK-04: revoked
	must(t, svc.RevokeToken(ctx, owner, "booking"))
	if code, _ = get("booking", again.Token); code != 401 {
		t.Fatalf("TOK-04: %d", code)
	}
	if info, _ = svc.Token(ctx, owner, "booking"); info.Issued {
		t.Fatal("TOK-04: still issued")
	}

	// SPECCI-03: no file and no PR
	global := tu.User("global")
	global.UserID = owner.UserID
	st, err := svc.Check(ctx, cicheck.TargetSpec)
	must(t, err)
	if st.Status != cicheck.StatusNone || st.ContractVersion != contract.Version {
		t.Fatalf("SPECCI-03: %+v", st)
	}
	// SPECCI-07: a GitHub App without Workflows: write
	repos.forbidden = true
	if _, err := svc.Propose(ctx, global, cicheck.TargetSpec); !errors.As(err, &ae) || ae.Code != "provider_forbidden" || ae.Details["missing"] != "workflows" {
		t.Fatalf("SPECCI-07: %v", err)
	}
	repos.forbidden = false
	// only the global administrators propose for the repository of specifications
	if _, err := svc.Propose(ctx, owner, cicheck.TargetSpec); !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("propose by an owner: %v", err)
	}
	// SPECCI-04: the branch, the file with the marker and the version of the contract
	st, err = svc.Propose(ctx, global, cicheck.TargetSpec)
	must(t, err)
	file := repos.files["org/specs"]["hammurapi/speclint-"+contract.Version][starter.GitHubCIPath]
	if st.Status != cicheck.StatusProposed || st.PR == nil || st.PR.Kind != "propose" || !strings.HasPrefix(file, starter.CIMarker) ||
		!strings.Contains(file, "GreenOnGrey/speclint@v"+contract.Version) || !strings.Contains(file, "mode: spec") {
		t.Fatalf("SPECCI-04: %+v\n%s", st, file)
	}
	if _, err := svc.Propose(ctx, global, cicheck.TargetSpec); !errors.As(err, &ae) || ae.Code != "ci_check_exists" {
		t.Fatalf("a second proposal: %v", err)
	}
	// SPECCI-08: merged — on with its version
	repos.merge(st.PR.Number)
	st, err = svc.Rescan(ctx, cicheck.TargetSpec)
	must(t, err)
	if st.Status != cicheck.StatusOn || st.Version == nil || *st.Version != contract.Version || st.PR != nil {
		t.Fatalf("SPECCI-08: %+v", st)
	}

	// SPECCI-09: pinned on an older version — a PR by the bot that changes one line
	old := strings.Replace(file, "speclint@v"+contract.Version, "speclint@v0.9.0", 1)
	repos.files["org/specs"]["main"][starter.GitHubCIPath] = old
	prs := len(repos.prs)
	st, err = svc.Rescan(ctx, cicheck.TargetSpec)
	must(t, err)
	if st.Status != cicheck.StatusOutdated || st.PR == nil || st.PR.Kind != "bump" || len(repos.prs) != prs+1 ||
		repos.prs[st.PR.Number].Title != "speclint 0.9.0 → "+contract.Version {
		t.Fatalf("SPECCI-09: %+v", st)
	}
	bumped := repos.files["org/specs"][repos.prs[st.PR.Number].Branch][starter.GitHubCIPath]
	if bumped != file {
		t.Fatalf("SPECCI-09: the bump changes more than the version line:\n%s", bumped)
	}
	// SPECCI-10: the PR of the bump is open — no second one
	if _, err := svc.Rescan(ctx, cicheck.TargetSpec); err != nil || len(repos.prs) != prs+1 {
		t.Fatalf("SPECCI-10: %v, %d PRs", err, len(repos.prs))
	}
	// SPECCI-11: the version line removed from the managed file — manual, no PR
	repos.prs[st.PR.Number].State = "closed"
	repos.files["org/specs"]["main"][starter.GitHubCIPath] = strings.Replace(old, "speclint@v0.9.0", "speclint@main", 1)
	st, err = svc.Rescan(ctx, cicheck.TargetSpec)
	must(t, err)
	if st.Status != cicheck.StatusManual || st.PR != nil || len(repos.prs) != prs+1 {
		t.Fatalf("SPECCI-11: %+v", st)
	}

	// SVCCI-01: the check of a service: mode service, the address and the key
	st, err = svc.Propose(ctx, owner, "service:booking")
	must(t, err)
	file = repos.files["fms/booking"]["hammurapi/speclint-"+contract.Version][starter.GitHubCIPath]
	if st.Status != cicheck.StatusProposed || !strings.Contains(file, "mode: service") ||
		!strings.Contains(file, `hammurapi-url: "https://hammurapi.example"`) || !strings.Contains(file, `service: "booking"`) {
		t.Fatalf("SVCCI-01: %+v\n%s", st, file)
	}
	if st, err = svc.Check(ctx, "service:pricing"); err != nil || st.Status != cicheck.StatusNone || st.Repo != "fms/pricing" {
		t.Fatalf("another service: %+v %v", st, err)
	}
}

// SPECCI-05, SPECCI-06: a GitLab repository gets the file and an item of the include of .gitlab-ci.yml, or a note
// in the description when its structure is not recognized.
func TestCICheckGitLab(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "cicheckgl")
	must(t, err)
	defer drop()
	pool, err := postgres.Connect(ctx, url)
	must(t, err)
	defer pool.Close()
	must(t, postgres.Migrate(ctx, url))
	var uid string
	must(t, pool.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, language, agent_name, agent_tone)
		VALUES ('a1','anna','Anna','en','Codex','business') RETURNING id`).Scan(&uid))
	for i, root := range []string{"include:\n  - project: ci/templates\n    file: go.yml\n", "include: 'ci.yml'\n"} {
		repos := &ciRepos{name: "gitlab", files: map[string]map[string]map[string]string{
			"org/specs": {"main": {starter.GitLabRootCI: root}},
		}, prs: map[int]*git.PRInfo{}, byRepo: map[int]string{}}
		tokens := mocks.NewMockTokenSource(gomock.NewController(t))
		tokens.EXPECT().Token(gomock.Any(), gomock.Any()).Return("user", nil).AnyTimes()
		svc := cicheck.NewService(pool, repos.provider(t, "org/specs"), tokens, "main", "https://hammurapi.example")
		_, err := pool.Exec(ctx, `DELETE FROM ci_checks`)
		must(t, err)
		global := tu.User("global")
		st, err := svc.Propose(ctx, global, cicheck.TargetSpec)
		must(t, err)
		branch := repos.files["org/specs"]["hammurapi/speclint-"+contract.Version]
		if !strings.Contains(branch[starter.GitLabCIPath], `SPECLINT_VERSION: "`+contract.Version+`"`) || st.Status != cicheck.StatusProposed {
			t.Fatalf("%d: %s", i, branch[starter.GitLabCIPath])
		}
		switch i {
		case 0:
			if !strings.Contains(branch[starter.GitLabRootCI], "  - local: "+starter.GitLabCIPath+"\n  - project: ci/templates") {
				t.Fatalf("SPECCI-05: %s", branch[starter.GitLabRootCI])
			}
		case 1:
			if branch[starter.GitLabRootCI] != root {
				t.Fatalf("SPECCI-06: .gitlab-ci.yml changed: %s", branch[starter.GitLabRootCI])
			}
		}
	}
}
