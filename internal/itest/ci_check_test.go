//go:build integration

package itest

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GreenOnGrey/speclint/v2/contract"
	"github.com/GreenOnGrey/speclint/v2/starter"
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
		path := "/ci/services/" + service + "/test-cases"
		if service == "" { // FTR.HMR.SPEC-0004 tech §3.2: the catalog
			path = "/ci/services"
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
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
	global := tu.User("global")
	global.UserID = owner.UserID
	var ae *apperr.Error

	// FTR.HMR.SPEC-0003: the OIDC token of a CI job opens the services of its repository
	gh := newCIIssuer(t)
	svc.OIDC = &cicheck.OIDC{Issuers: map[string]string{gh.URL: "github"}, Audience: "https://hammurapi.example"}
	must(t, cd.UpsertService(ctx, &cycledata.Service{Key: "booking-admin", Name: "booking-admin", Repo: "fms/booking", Source: "manual"}))
	job := func(repo string) string {
		return gh.token(t, map[string]any{"iss": gh.URL, "aud": "https://hammurapi.example", "repository": repo})
	}

	// OIDC-01, SVC-06, SVC-07: the features, their phases, required from code generation on, rolled back not
	code, body := get("booking", job("FMS/Booking"))
	if code != 200 || body["contractVersion"] != contract.Version || len(body["cases"].([]any)) != 4 {
		t.Fatalf("OIDC-01: %d %v", code, body)
	}
	required := map[string]bool{}
	for _, f := range body["features"].([]any) {
		m := f.(map[string]any)
		required[m["phase"].(string)] = m["required"].(bool)
	}
	if !required["released"] || !required["codegen"] || required["spec"] || required["rolled_back"] {
		t.Fatalf("SVC-07: %v", required)
	}
	// FTR.HMR.SPEC-0005 qa TCS-02, TCS-03: a case marked with another service is left out; a case without services stays
	_, err = pool.Exec(ctx, `INSERT INTO test_cases (feature_id, tc_id, level, req_ids, services)
		SELECT f.id, x.id, 'I', '{R1}', x.s FROM features f,
		(VALUES ('MIG-01', '{pricing}'::text[]), ('TCS-10', '{booking,pricing}'::text[])) AS x(id, s) WHERE f.unique_id = 'FTR.FMS.CAR-0002'`)
	must(t, err)
	code, body = get("booking", job("fms/booking"))
	ids := map[string][]any{}
	for _, c := range body["cases"].([]any) {
		m := c.(map[string]any)
		ids[m["id"].(string)], _ = m["services"].([]any)
	}
	if code != 200 || len(ids) != 5 || ids["TCS-10"] == nil || ids["BOOK-02"] == nil || len(ids["BOOK-02"]) != 0 {
		t.Fatalf("TCS-02, TCS-03: %d %v", code, ids)
	}
	if _, ok := ids["MIG-01"]; ok {
		t.Fatalf("TCS-02: the case of pricing is given to booking: %v", ids)
	}
	_, err = pool.Exec(ctx, `DELETE FROM test_cases WHERE tc_id IN ('MIG-01', 'TCS-10')`)
	must(t, err)
	// SCOPE-02: the other service of the same repository
	if code, _ = get("booking-admin", job("fms/booking")); code != 200 {
		t.Fatalf("SCOPE-02: %d", code)
	}
	// SCOPE-01: a service of another repository; a missing one is refused the same way
	for _, key := range []string{"pricing", "missing"} {
		if code, body = get(key, job("fms/booking")); code != 403 || body["error"].(map[string]any)["code"] != "token_scope" {
			t.Fatalf("SCOPE-01 %s: %d %v", key, code, body)
		}
	}
	// SCOPE-03: a repository outside the catalog
	if code, body = get("booking", job("fms/other")); code != 403 || body["error"].(map[string]any)["code"] != "repo_not_in_catalog" {
		t.Fatalf("SCOPE-03: %d %v", code, body)
	}
	// FTR.HMR.SPEC-0004 SVC-04: the catalog for the CI of the repository of specifications only
	if code, body = get("", job("Org/Specs")); code != 200 || fmt.Sprint(body["services"]) != "[booking booking-admin pricing]" {
		t.Fatalf("catalog: %d %v", code, body)
	}
	if code, body = get("", job("fms/booking")); code != 403 || body["error"].(map[string]any)["code"] != "token_scope" {
		t.Fatalf("catalog to a service: %d %v", code, body)
	}
	if code, _ = get("", ""); code != 401 {
		t.Fatalf("catalog without a token: %d", code)
	}
	// another audience; no token; a token of a service of SPEC-0002
	if code, _ = get("booking", gh.token(t, map[string]any{"iss": gh.URL, "aud": "https://other.example", "repository": "fms/booking"})); code != 401 {
		t.Fatalf("another audience: %d", code)
	}
	for _, tok := range []string{"", "hmr_svc_old", "hmr_ci_unknown"} {
		if code, _ = get("booking", tok); code != 401 {
			t.Fatalf("token %q: %d", tok, code)
		}
	}

	// INST-03: only the global administrators issue the installation token
	if _, err := svc.IssueToken(ctx, owner); !errors.As(err, &ae) || ae.Status != 403 {
		t.Fatalf("INST-03: %v", err)
	}
	// INST-01: shown once, the database keeps its hash and prefix, every service is open
	issued, err := svc.IssueToken(ctx, global)
	must(t, err)
	var stored int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM ci_tokens WHERE prefix = $1 AND token_hash = sha256(convert_to($2, 'UTF8'))`, issued.Prefix, issued.Token).Scan(&stored))
	if stored != 1 || !strings.HasPrefix(issued.Token, "hmr_ci_") {
		t.Fatalf("INST-01: %d", stored)
	}
	for _, key := range []string{"booking", "pricing", ""} {
		if code, _ = get(key, issued.Token); code != 200 {
			t.Fatalf("INST-01 %s: %d", key, code)
		}
	}
	if info, _ := svc.Token(ctx, global); !info.Issued || info.LastUsedAt == nil {
		t.Fatalf("INST-01: %+v", info)
	}
	// INST-02: reissued — the old token is revoked, the new one works; then revoked
	again, err := svc.IssueToken(ctx, global)
	must(t, err)
	if code, body = get("booking", issued.Token); code != 401 || body["error"].(map[string]any)["code"] != "token_revoked" {
		t.Fatalf("INST-02 old: %d %v", code, body)
	}
	if code, _ = get("booking", again.Token); code != 200 {
		t.Fatalf("INST-02 new: %d", code)
	}
	must(t, svc.RevokeToken(ctx, global))
	if info, _ := svc.Token(ctx, global); info.Issued {
		t.Fatal("revoked: still issued")
	}

	// SPECCI-03: no file and no PR
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
	if strings.Contains(file, "secrets.") || !strings.Contains(file, "id-token: write") {
		t.Fatalf("SVC-02: the file asks for a secret:\n%s", file)
	}
	// SVC-03: a file of 1.1.0 asks for the token of a service: manual, no PR of the bump
	repos.merge(st.PR.Number)
	old = strings.Replace(strings.Replace(file, "speclint@v"+contract.Version, "speclint@v1.1.0", 1), `service: "booking"`,
		`service: "booking"`+"\n          token: ${{ secrets.HAMMURAPI_SERVICE_TOKEN }}", 1)
	repos.files["fms/booking"]["main"][starter.GitHubCIPath] = old
	prs = len(repos.prs)
	if st, err = svc.Rescan(ctx, "service:booking"); err != nil || st.Status != cicheck.StatusManual || len(repos.prs) != prs {
		t.Fatalf("SVC-03: %+v %v", st, err)
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

// ciIssuer is an issuer of OIDC tokens of CI with one RSA key.
type ciIssuer struct {
	*httptest.Server
	key *rsa.PrivateKey
}

func newCIIssuer(t *testing.T) *ciIssuer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	must(t, err)
	is := &ciIssuer{key: k}
	enc := base64.RawURLEncoding.EncodeToString
	is.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": is.URL + "/keys"})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{
				{"kty": "RSA", "kid": "k1", "n": enc(k.N.Bytes()), "e": enc(big.NewInt(int64(k.E)).Bytes())}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(is.Close)
	return is
}

func (is *ciIssuer) token(t *testing.T, claims map[string]any) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	claims["exp"] = time.Now().Add(5 * time.Minute).Unix()
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1"})
	c, _ := json.Marshal(claims)
	signed := enc(h) + "." + enc(c)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, is.key, crypto.SHA256, sum[:])
	must(t, err)
	return signed + "." + enc(sig)
}
