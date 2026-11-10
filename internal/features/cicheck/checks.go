package cicheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/GreenOnGrey/speclint/contract"
	"github.com/GreenOnGrey/speclint/starter"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.yaml.in/yaml/v3"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// The statuses of a check in CI (arch §3.6).
const (
	StatusNone     = "none"
	StatusProposed = "proposed"
	StatusOn       = "on"
	StatusOutdated = "outdated"
	StatusManual   = "manual"
)

// TargetSpec is the repository of specifications; a service is "service:<key>".
const TargetSpec = "spec"

func serviceTarget(key string) string { return "service:" + key }

// Service implements the check of speclint in CI of repositories.
type Service struct {
	pool       *pgxpool.Pool
	git        git.Provider // bound to the repository of specifications
	tokens     git.TokenSource
	specBranch string
	publicURL  string   // the public address of the API: the mode service of a service calls it
	limits     *limiter // per token
	failures   *limiter // failed attempts per address
}

// NewService creates the service.
func NewService(pool *pgxpool.Pool, provider git.Provider, tokens git.TokenSource, specBranch, publicURL string) *Service {
	return &Service{pool: pool, git: provider, tokens: tokens, specBranch: specBranch, publicURL: publicURL,
		limits: newLimiter(60, time.Minute), failures: newLimiter(60, time.Minute)}
}

// PRRef is the open pull request of a check.
type PRRef struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
	Kind   string `json:"kind"` // propose | bump
}

// Status is the status of the check of a repository (tech §3.3).
type Status struct {
	Status          string     `json:"status"`
	Version         *string    `json:"version"`
	ContractVersion string     `json:"contractVersion"`
	Path            *string    `json:"path"`
	Repo            string     `json:"repo"`
	PR              *PRRef     `json:"pr"`
	Error           *string    `json:"error"`
	CheckedAt       *time.Time `json:"checkedAt"`
}

// repoOf is a repository of a target: its provider, its default branch and, for a service, its key.
type repoOf struct {
	target  string
	repo    string
	prov    git.Provider
	service string
}

func (s *Service) resolve(ctx context.Context, target string) (*repoOf, error) {
	if target == TargetSpec {
		return &repoOf{target: target, repo: s.git.Repo(), prov: s.git}, nil
	}
	key, ok := strings.CutPrefix(target, "service:")
	if !ok {
		return nil, apperr.NotFound("not_found", "unknown target")
	}
	svc, err := s.service(ctx, s.pool, key)
	if err != nil {
		return nil, err
	}
	return &repoOf{target: target, repo: svc.Repo, prov: s.git.ForRepo(svc.Repo), service: svc.Key}, nil
}

func (s *Service) branch(ctx context.Context, r *repoOf, token string) (string, error) {
	if r.target == TargetSpec && s.specBranch != "" {
		return s.specBranch, nil
	}
	return r.prov.DefaultBranch(ctx, token)
}

// ciPath is the file of the check for the provider.
func ciPath(provider string) string {
	if provider == "gitlab" {
		return starter.GitLabCIPath
	}
	return starter.GitHubCIPath
}

// load reads the stored status; nil when the target was never checked.
func (s *Service) load(ctx context.Context, target string) (*Status, error) {
	var st Status
	var num *int
	var url, kind *string
	var checked time.Time
	err := s.pool.QueryRow(ctx, `SELECT status, version, path, repo, pr_number, pr_url, pr_kind, error, checked_at FROM ci_checks WHERE target = $1`, target).
		Scan(&st.Status, &st.Version, &st.Path, &st.Repo, &num, &url, &kind, &st.Error, &checked)
	if postgres.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st.ContractVersion, st.CheckedAt = contract.Version, &checked
	if num != nil && url != nil && kind != nil {
		st.PR = &PRRef{Number: *num, URL: *url, Kind: *kind}
	}
	return &st, nil
}

func (s *Service) save(ctx context.Context, target string, st *Status) error {
	var num *int
	var url, kind *string
	if st.PR != nil {
		num, url, kind = &st.PR.Number, &st.PR.URL, &st.PR.Kind
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO ci_checks (target, repo, status, version, path, pr_number, pr_url, pr_kind, error, checked_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, now())
		ON CONFLICT (target) DO UPDATE SET repo = $2, status = $3, version = $4, path = $5, pr_number = $6, pr_url = $7,
		pr_kind = $8, error = $9, checked_at = now()`, target, st.Repo, st.Status, st.Version, st.Path, num, url, kind, st.Error)
	return err
}

// scanned is a status with the file it was read from, for the bump.
type scanned struct {
	*Status
	file   []byte
	branch string
	r      *repoOf
	token  string
}

// scan reads the file of the check in the default branch and the open pull request of the check, and stores the
// status (arch §3.6). An error of the provider is stored with the previous status.
// The token is the bot's when token is "", or the user's who proposes the check.
func (s *Service) scan(ctx context.Context, target, token string) (*scanned, error) {
	r, err := s.resolve(ctx, target)
	if err != nil {
		return nil, err
	}
	prev, err := s.load(ctx, target)
	if err != nil {
		return nil, err
	}
	out, err := s.read(ctx, r, prev, token)
	if err != nil {
		// the previous status stays with its time: only the error is new
		if _, serr := s.pool.Exec(ctx, `INSERT INTO ci_checks (target, repo, status, error) VALUES ($1, $2, 'none', $3)
			ON CONFLICT (target) DO UPDATE SET error = $3`, target, r.repo, err.Error()); serr != nil {
			return nil, serr
		}
		return nil, err
	}
	if err := s.save(ctx, target, out.Status); err != nil {
		return nil, err
	}
	stored, err := s.load(ctx, target)
	if err != nil {
		return nil, err
	}
	out.Status = stored
	return out, nil
}

func (s *Service) read(ctx context.Context, r *repoOf, prev *Status, token string) (*scanned, error) {
	if token == "" {
		var err error
		if token, err = r.prov.BotToken(ctx); err != nil {
			return nil, err
		}
	}
	branch, err := s.branch(ctx, r, token)
	if err != nil {
		return nil, err
	}
	path := ciPath(r.prov.Name())
	st := &Status{Repo: r.repo, Path: &path, ContractVersion: contract.Version}
	out := &scanned{Status: st, branch: branch, r: r, token: token}
	// the open pull request of the check, if Hammurapi opened one and it is still open
	if prev != nil && prev.PR != nil {
		info, err := r.prov.GetPR(ctx, token, prev.PR.Number)
		switch {
		case err == nil && info.State == "open":
			st.PR = prev.PR
		case err != nil && !errors.Is(err, git.ErrNotFound):
			return nil, err
		}
	}
	file, err := r.prov.GetFile(ctx, token, branch, path)
	switch {
	case errors.Is(err, git.ErrNotFound):
		st.Status = StatusNone
		if st.PR != nil {
			st.Status = StatusProposed
		}
		return out, nil
	case err != nil:
		return nil, err
	}
	out.file = file.Content
	v, managed := starter.CIVersion(file.Content)
	if !managed || !forTarget(file.Content, r) {
		st.Status = StatusManual
		return out, nil
	}
	st.Version = &v
	st.Status = StatusOn
	if c, ok := starter.CompareVersions(v, contract.Version); ok && c < 0 {
		st.Status = StatusOutdated
	}
	return out, nil
}

// forTarget tells whether a managed file checks this target: a repository of several services, or of the
// specifications and a service, has one file of the check, and it belongs to one of them; for the others it is
// a file of people.
func forTarget(content []byte, r *repoOf) bool {
	c := string(content)
	if r.service == "" {
		return strings.Contains(c, "mode: spec") || strings.Contains(c, `SPECLINT_MODE: "spec"`)
	}
	return strings.Contains(c, `service: "`+r.service+`"`) || strings.Contains(c, `HAMMURAPI_SERVICE: "`+r.service+`"`)
}

// Check is the status of a target, checked now when it was never checked.
func (s *Service) Check(ctx context.Context, target string) (*Status, error) {
	st, err := s.load(ctx, target)
	if err != nil || st != nil {
		return st, err
	}
	sc, err := s.scan(ctx, target, "")
	if err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			return nil, err
		}
		if st, lerr := s.load(ctx, target); lerr == nil && st != nil {
			return st, nil // the status with the error of the provider
		}
		return nil, err
	}
	return sc.Status, nil
}

// lockTarget holds a lock of a target for one rescan or proposal, across workers and replicas of the API: two of
// them would open the same branch. ok is false when another holds it.
func (s *Service) lockTarget(ctx context.Context, target string) (unlock func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('cicheck:' || $1))`, target).Scan(&ok); err != nil || !ok {
		conn.Release()
		return nil, false, err
	}
	return func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtext('cicheck:' || $1))`, target)
		conn.Release()
	}, true, nil
}

// Rescan checks a target now and bumps its version when it is outdated (R7); while another check of the target
// runs, it returns the stored status.
func (s *Service) Rescan(ctx context.Context, target string) (*Status, error) {
	if _, err := s.resolve(ctx, target); err != nil {
		return nil, err
	}
	unlock, ok, err := s.lockTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	if !ok {
		return s.load(ctx, target)
	}
	defer unlock()
	sc, err := s.scan(ctx, target, "")
	if err != nil {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			return nil, err
		}
		return s.load(ctx, target)
	}
	if err := s.bump(ctx, sc); err != nil {
		slog.WarnContext(ctx, "cicheck bump", "target", sc.r.target, "repo", sc.r.repo, "err", err)
		msg := err.Error()
		sc.Error = &msg
		_ = s.save(ctx, sc.r.target, sc.Status)
	}
	return s.load(ctx, sc.r.target)
}

// branchName is the branch of a pull request of the check.
func branchName(version string) string { return "hammurapi/speclint-" + version }

// openBranch creates the branch of a pull request from the head of base; a branch left by a closed pull request
// is created anew.
func openBranch(ctx context.Context, prov git.Provider, token, branch, base string) error {
	head, err := prov.BranchHead(ctx, token, base)
	if err != nil {
		return err
	}
	if err := prov.CreateBranch(ctx, token, branch, head); err != nil {
		if derr := prov.DeleteBranch(ctx, token, branch); derr != nil {
			return err
		}
		return prov.CreateBranch(ctx, token, branch, head)
	}
	return nil
}

// bump opens the pull request that changes only the version line, by the bot, when the check is outdated and no
// pull request of the check is open (R7, arch A7).
func (s *Service) bump(ctx context.Context, sc *scanned) error {
	if sc.Status.Status != StatusOutdated || sc.PR != nil || sc.file == nil {
		return nil
	}
	old := *sc.Version
	next, err := starter.SetCIVersion(sc.file, contract.Version)
	if err != nil {
		return err
	}
	prov, token := sc.r.prov, sc.token
	branch := branchName(contract.Version)
	result := "ok"
	defer func() { metrics.CICheckPRs.WithLabelValues("bump", result).Inc() }()
	if err := openBranch(ctx, prov, token, branch, sc.branch); err != nil {
		result = "error"
		return err
	}
	title := fmt.Sprintf("speclint %s → %s", old, contract.Version)
	msg := git.Trailers{Generated: true}.Message(title)
	if _, err := prov.Commit(ctx, token, branch, msg, []git.FileChange{{Path: *sc.Path, Content: next}}); err != nil {
		result = "error"
		_ = prov.DeleteBranch(context.WithoutCancel(ctx), token, branch)
		return err
	}
	body := fmt.Sprintf("Hammurapi is on the document contract %s: the check of speclint follows it.\n\n"+
		"Only the version line changes (FTR.HMR.SPEC-0002 R7).", contract.Version)
	pr, err := prov.CreatePR(ctx, token, branch, sc.branch, title, body)
	if err != nil {
		result = "error"
		_ = prov.DeleteBranch(context.WithoutCancel(ctx), token, branch)
		return err
	}
	sc.PR = &PRRef{Number: pr.Number, URL: pr.URL, Kind: "bump"}
	sc.Error = nil
	return s.save(ctx, sc.r.target, sc.Status)
}

// canPropose: the global administrators for the repository of specifications, the owners of the service and the
// administrators for a service (R6, R11).
func canPropose(p *domain.Principal, r *repoOf) bool {
	if r.target == TargetSpec {
		return p != nil && p.GlobalAdmin
	}
	return canManage(p, r.service)
}

// Propose opens the pull request that adds the check on behalf of the user (R6, R11, arch A7).
func (s *Service) Propose(ctx context.Context, p *domain.Principal, target string) (*Status, error) {
	r, err := s.resolve(ctx, target)
	if err != nil {
		return nil, err
	}
	if !canPropose(p, r) {
		return nil, apperr.Forbidden("forbidden", "only administrators and the owners of the service propose the check")
	}
	unlock, ok, err := s.lockTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, apperr.Conflict("ci_check_exists", "the check of this repository is being changed now")
	}
	defer unlock()
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	sc, err := s.scan(ctx, target, token) // the user reads the repository, a bot is not needed
	if err != nil {
		return nil, providerError(r.prov.Name(), err)
	}
	if sc.Status.Status != StatusNone {
		return nil, apperr.Conflict("ci_check_exists", "the repository has the file of the check or its open pull request").With("status", sc.Status.Status)
	}
	prov := r.prov
	result := "ok"
	defer func() { metrics.CICheckPRs.WithLabelValues("propose", result).Inc() }()
	fail := func(err error) (*Status, error) {
		result = "error"
		return nil, providerError(prov.Name(), err)
	}
	base, err := s.branch(ctx, r, token)
	if err != nil {
		return fail(err)
	}
	params := starter.CIParams{Version: contract.Version, Mode: "spec", Branch: base}
	if r.service != "" {
		params.Mode, params.Hammurapi, params.Service = "service", s.publicURL, r.service
	}
	path, content, err := starter.CIFile(prov.Name(), params)
	if err != nil {
		result = "error"
		return nil, err
	}
	changes := []git.FileChange{{Path: path, Content: content}}
	manualInclude := false
	if prov.Name() == "gitlab" { // the include of the file in .gitlab-ci.yml (arch A8)
		var cur []byte
		f, err := prov.GetFile(ctx, token, base, starter.GitLabRootCI)
		switch {
		case err == nil:
			cur = f.Content
		case !errors.Is(err, git.ErrNotFound):
			return fail(err)
		}
		next, ok := addInclude(cur, path)
		switch {
		case !ok:
			manualInclude = true
		case string(next) != string(cur):
			changes = append(changes, git.FileChange{Path: starter.GitLabRootCI, Content: next})
		}
	}
	branch := branchName(contract.Version)
	if err := openBranch(ctx, prov, token, branch, base); err != nil {
		return fail(err)
	}
	title := "speclint: the check in CI"
	if _, err := prov.Commit(ctx, token, branch, git.Trailers{}.Message(title), changes); err != nil {
		_ = prov.DeleteBranch(context.WithoutCancel(ctx), token, branch)
		return fail(err)
	}
	pr, err := prov.CreatePR(ctx, token, branch, base, title, proposeBody(params, manualInclude))
	if err != nil {
		_ = prov.DeleteBranch(context.WithoutCancel(ctx), token, branch)
		return fail(err)
	}
	st := sc.Status
	st.Status, st.PR, st.Error = StatusProposed, &PRRef{Number: pr.Number, URL: pr.URL, Kind: "propose"}, nil
	if err := s.save(ctx, target, st); err != nil {
		return nil, err
	}
	return s.load(ctx, target)
}

func proposeBody(p starter.CIParams, manualInclude bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Hammurapi proposes the check of speclint %s in CI (FTR.HMR.SPEC-0002), mode `%s`.\n\n", p.Version, p.Mode)
	if p.Mode == "service" {
		fmt.Fprintf(&b, "The check asks Hammurapi at %s for the test cases of the features of service `%s` and fails when "+
			"a test case of a feature in code generation or later has no test in the code.\n\n"+
			"Before merging: issue the token in the card of the service in Hammurapi and put it into the secret "+
			"(CI/CD variable) `HAMMURAPI_SERVICE_TOKEN` of this repository. Old features without tests go to "+
			"`speclint-baseline.txt` (`speclint service -write-baseline`).\n\n", p.Hammurapi, p.Service)
	} else {
		b.WriteString("The check reads the documents and the rules by the document contract of Hammurapi.\n\n")
	}
	b.WriteString("Hammurapi keeps the version line of the file on its contract with pull requests of its own.\n")
	if manualInclude {
		fmt.Fprintf(&b, "\nHammurapi could not add the include to `.gitlab-ci.yml`: add\n\n```yaml\ninclude:\n  - local: %s\n```\n", starter.GitLabCIPath)
	}
	return b.String()
}

// providerError is a refusal of the provider for the user: a GitHub App without the permission Workflows: write
// cannot commit a workflow (arch §3.7).
func providerError(provider string, err error) error {
	var ae *git.APIError
	if errors.As(err, &ae) && ae.Status == http.StatusForbidden {
		e := apperr.Forbidden("provider_forbidden", ae.Message)
		// GitHub names the missing permission: "refusing to allow a GitHub App to create or update workflow … without
		// `workflows` permission"; another 403 is the rights of the user in the repository
		if provider == "github" && strings.Contains(strings.ToLower(ae.Message), "workflow") {
			e = e.With("missing", "workflows")
		}
		return e
	}
	return auth.MapGitError(err)
}

var includeKeyRe = regexp.MustCompile(`^include:\s*(#.*)?$`)

// addInclude adds `- local: path` to the include of a .gitlab-ci.yml, keeping the rest of its text: an include
// that is a block list gets an item, a file without an include gets one, another form is left to people (ok false).
func addInclude(content []byte, path string) ([]byte, bool) {
	item := "- local: " + path
	if strings.Contains(string(content), path) {
		return content, true
	}
	if strings.TrimSpace(string(content)) == "" {
		return []byte("include:\n  " + item + "\n"), true
	}
	lines := strings.Split(string(content), "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "---") {
			return nil, false // several documents: a header of inputs goes first, the include is for people
		}
	}
	var out string
	for i, l := range lines {
		if strings.HasPrefix(l, "include:") && !includeKeyRe.MatchString(l) {
			return nil, false // include: 'file', include: [..]
		}
		if !includeKeyRe.MatchString(l) {
			continue
		}
		indent := ""
		for _, n := range lines[i+1:] {
			t := strings.TrimSpace(n)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if !strings.HasPrefix(t, "- ") || len(n) == len(strings.TrimLeft(n, " ")) {
				return nil, false // a mapping under include, or an empty include
			}
			indent = n[:len(n)-len(strings.TrimLeft(n, " "))]
			break
		}
		if indent == "" {
			return nil, false
		}
		next := append(append(append([]string{}, lines[:i+1]...), indent+item), lines[i+1:]...)
		out = strings.Join(next, "\n")
		break
	}
	if out == "" {
		out = "include:\n  " + item + "\n\n" + string(content)
	}
	var check map[string]any
	if err := yaml.Unmarshal([]byte(out), &check); err != nil {
		return nil, false
	}
	return []byte(out), true
}
