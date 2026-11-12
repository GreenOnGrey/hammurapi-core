package git

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// githubApp mints installation tokens of the GitHub App (the bot identity).
type githubApp struct {
	id  string
	key *rsa.PrivateKey

	mu    sync.Mutex
	cache map[string]cachedToken // repo → token
}

type cachedToken struct {
	token   string
	expires time.Time
}

// WithApp configures the GitHub App used as the bot (FTR.HMR.CMN-0002 arch §8).
func (g *GitHub) WithApp(appID, privateKeyPEM string) (*GitHub, error) {
	if appID == "" || privateKeyPEM == "" {
		return g, nil
	}
	key, err := parseRSAKey(strings.ReplaceAll(privateKeyPEM, `\n`, "\n"))
	if err != nil {
		return nil, fmt.Errorf("GITHUB_APP_PRIVATE_KEY: %w", err)
	}
	g.app = &githubApp{id: appID, key: key, cache: map[string]cachedToken{}}
	return g, nil
}

func parseRSAKey(p string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(p))
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return rk, nil
}

// jwt builds the App JWT (RS256, 9 minutes).
func (a *githubApp) jwt(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-30 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": a.id})
	unsigned := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + enc.EncodeToString(sig), nil
}

func (g *GitHub) Repo() string { return g.repo }

func (g *GitHub) ForRepo(repo string) Provider {
	c := *g
	c.repo = repo
	return &c
}

// BotToken returns an installation token limited to the bound repository (≤ 1 h).
func (g *GitHub) BotToken(ctx context.Context) (string, error) {
	if g.app == nil {
		return "", errors.New("github: GitHub App is not configured (GITHUB_APP_ID, GITHUB_APP_PRIVATE_KEY)")
	}
	a := g.app
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.cache[g.repo]; ok && time.Until(c.expires) > 5*time.Minute {
		return c.token, nil
	}
	jwt, err := a.jwt(time.Now())
	if err != nil {
		return "", err
	}
	var inst struct {
		ID int64 `json:"id"`
	}
	if _, err := g.api.call(ctx, "app_installation", jwt, http.MethodGet, g.r("/installation"), nil, &inst); err != nil {
		return "", err
	}
	_, name, _ := strings.Cut(g.repo, "/")
	var tok struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := g.api.call(ctx, "app_token", jwt, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID),
		map[string]any{"repositories": []string{name}}, &tok); err != nil {
		return "", err
	}
	a.cache[g.repo] = cachedToken{token: tok.Token, expires: tok.ExpiresAt}
	return tok.Token, nil
}

func (g *GitHub) GetPR(ctx context.Context, token string, number int) (*PRInfo, error) {
	var pr struct {
		Number    int    `json:"number"`
		HTMLURL   string `json:"html_url"`
		Title     string `json:"title"`
		State     string `json:"state"`
		Merged    bool   `json:"merged"`
		Mergeable *bool  `json:"mergeable"`
		MergeSHA  string `json:"merge_commit_sha"`
		Head      struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if _, err := g.api.call(ctx, "get_pr", token, http.MethodGet, g.r(fmt.Sprintf("/pulls/%d", number)), nil, &pr); err != nil {
		return nil, err
	}
	state := pr.State
	if pr.Merged {
		state = "merged"
	}
	info := &PRInfo{Number: pr.Number, URL: pr.HTMLURL, Title: pr.Title, State: state, Branch: pr.Head.Ref, HeadSHA: pr.Head.SHA, Mergeable: pr.Mergeable}
	if pr.Merged {
		info.MergeSHA = pr.MergeSHA
	}
	return info, nil
}

func (g *GitHub) UpdatePRBranch(ctx context.Context, token string, number int) error {
	_, err := g.api.call(ctx, "update_branch", token, http.MethodPut, g.r(fmt.Sprintf("/pulls/%d/update-branch", number)), map[string]any{}, nil)
	return err
}

func (g *GitHub) RequestReview(ctx context.Context, token string, number int, reviewers []string) error {
	if len(reviewers) == 0 {
		return nil
	}
	_, err := g.api.call(ctx, "request_review", token, http.MethodPost, g.r(fmt.Sprintf("/pulls/%d/requested_reviewers", number)),
		map[string]any{"reviewers": reviewers}, nil)
	return err
}

func (g *GitHub) CommentPR(ctx context.Context, token string, number int, body string) error {
	_, err := g.api.call(ctx, "comment_pr", token, http.MethodPost, g.r(fmt.Sprintf("/issues/%d/comments", number)),
		map[string]string{"body": body}, nil)
	return err
}

func (g *GitHub) PRChangedFiles(ctx context.Context, token string, number int) ([]ChangedFile, error) {
	var out []ChangedFile
	for page := 1; page <= 30; page++ {
		var files []struct {
			Filename string `json:"filename"`
			Status   string `json:"status"`
			Previous string `json:"previous_filename"`
		}
		if _, err := g.api.call(ctx, "pr_files", token, http.MethodGet, g.r(fmt.Sprintf("/pulls/%d/files?per_page=100&page=%d", number, page)), nil, &files); err != nil {
			return nil, err
		}
		for _, f := range files {
			st := f.Status
			if st == "changed" {
				st = "modified"
			}
			out = append(out, ChangedFile{Path: f.Filename, OldPath: f.Previous, Status: st})
		}
		if len(files) < 100 {
			break
		}
	}
	return out, nil
}

func (g *GitHub) CommitParents(ctx context.Context, token, sha string) ([]string, error) {
	var c struct {
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	if _, err := g.api.call(ctx, "commit_parents", token, http.MethodGet, g.r("/git/commits/"+sha), nil, &c); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(c.Parents))
	for _, p := range c.Parents {
		out = append(out, p.SHA)
	}
	return out, nil
}

func (g *GitHub) IsAncestor(ctx context.Context, token, ancestor, descendant string) (bool, error) {
	var cmp struct {
		Status string `json:"status"`
	}
	_, err := g.api.call(ctx, "compare", token, http.MethodGet, g.r("/compare/"+url.PathEscape(ancestor)+"..."+url.PathEscape(descendant)), nil, &cmp)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return cmp.Status == "ahead" || cmp.Status == "identical", nil
}

// GroupMembers resolves a team: "team" (in the repository owner's org) or "org/team".
func (g *GitHub) GroupMembers(ctx context.Context, token, group string) ([]string, error) {
	org, slug, ok := strings.Cut(group, "/")
	if !ok {
		org, _, _ = strings.Cut(g.repo, "/")
		slug = group
	}
	var members []struct {
		Login string `json:"login"`
	}
	if _, err := g.api.call(ctx, "team_members", token, http.MethodGet, fmt.Sprintf("/orgs/%s/teams/%s/members?per_page=100", org, slug), nil, &members); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.Login)
	}
	return out, nil
}

// RunPipeline dispatches a workflow (workflow_dispatch).
func (g *GitHub) RunPipeline(ctx context.Context, token, workflow, ref string, params map[string]string) (string, error) {
	_, err := g.api.call(ctx, "workflow_dispatch", token, http.MethodPost, g.r("/actions/workflows/"+url.PathEscape(workflow)+"/dispatches"),
		map[string]any{"ref": ref, "inputs": params}, nil)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s/%s/actions/workflows/%s", g.baseURL, g.repo, workflow), nil
}

// ParseHook normalizes GitHub webhooks.
func (g *GitHub) ParseHook(h http.Header, body []byte) (*HookEvent, error) {
	id := "github:" + h.Get("X-GitHub-Delivery")
	var common struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	if err := json.Unmarshal(body, &common); err != nil {
		return nil, err
	}
	repo := common.Repository.FullName
	switch h.Get("X-GitHub-Event") {
	case "push":
		ev, _, err := g.ParsePush(h, body)
		if err != nil {
			return nil, err
		}
		var p struct {
			Ref   string `json:"ref"`
			After string `json:"after"`
		}
		_ = json.Unmarshal(body, &p)
		ev.Repo, ev.After = repo, p.After
		if tag, ok := strings.CutPrefix(p.Ref, "refs/tags/"); ok {
			ev.Tag, ev.Branch = tag, ""
			return &HookEvent{Kind: "tag", Push: ev}, nil
		}
		return &HookEvent{Kind: "push", Push: ev}, nil
	case "pull_request":
		var p struct {
			PR struct {
				Number   int    `json:"number"`
				Title    string `json:"title"`
				HTMLURL  string `json:"html_url"`
				Merged   bool   `json:"merged"`
				MergeSHA string `json:"merge_commit_sha"`
				User     struct {
					Login string `json:"login"`
				} `json:"user"`
				Head struct {
					Ref string `json:"ref"`
					SHA string `json:"sha"`
				} `json:"head"`
				Base struct {
					Ref string `json:"ref"`
				} `json:"base"`
			} `json:"pull_request"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, err
		}
		action := map[string]string{"opened": "opened", "reopened": "reopened", "synchronize": "updated", "edited": "updated"}[common.Action]
		if common.Action == "closed" {
			action = "closed"
			if p.PR.Merged {
				action = "merged"
			}
		}
		if action == "" {
			return nil, nil
		}
		ev := &PREvent{EventID: id, Repo: repo, Number: p.PR.Number, Action: action, Title: p.PR.Title, Branch: p.PR.Head.Ref,
			Base: p.PR.Base.Ref, HeadSHA: p.PR.Head.SHA, URL: p.PR.HTMLURL, Author: p.PR.User.Login, Actor: common.Sender.Login}
		if p.PR.Merged {
			ev.MergeSHA = p.PR.MergeSHA
		}
		return &HookEvent{Kind: "pr", PR: ev}, nil
	case "check_suite", "status":
		// FTR.HMR.DEV-0004 R17: a suite of checks or a status of a commit finished — the cue to ask
		// for all of them. Single check runs are not taken: a suite ends when its runs do (tech §14).
		var p struct {
			SHA   string `json:"sha"`
			Suite struct {
				HeadSHA string `json:"head_sha"`
			} `json:"check_suite"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, err
		}
		sha := p.SHA
		if h.Get("X-GitHub-Event") != "status" {
			if common.Action != "completed" {
				return nil, nil
			}
			sha = p.Suite.HeadSHA
		}
		if sha == "" {
			return nil, nil
		}
		return &HookEvent{Kind: "ci", CI: &CIEvent{EventID: id, Repo: repo, SHA: sha}}, nil
	case "pull_request_review":
		var p struct {
			Review struct {
				State string `json:"state"`
				Body  string `json:"body"`
				User  struct {
					Login string `json:"login"`
				} `json:"user"`
			} `json:"review"`
			PR struct {
				Number int `json:"number"`
			} `json:"pull_request"`
		}
		if err := json.Unmarshal(body, &p); err != nil || common.Action != "submitted" {
			return nil, err
		}
		return &HookEvent{Kind: "review", Review: &ReviewEvent{EventID: id, Repo: repo, Number: p.PR.Number,
			State: strings.ToLower(p.Review.State), Body: p.Review.Body, Author: p.Review.User.Login}}, nil
	case "issue_comment", "pull_request_review_comment":
		var p struct {
			Comment struct {
				Body string `json:"body"`
				User struct {
					Login string `json:"login"`
				} `json:"user"`
			} `json:"comment"`
			Issue struct {
				Number      int             `json:"number"`
				PullRequest json.RawMessage `json:"pull_request"`
			} `json:"issue"`
			PR struct {
				Number int `json:"number"`
			} `json:"pull_request"`
		}
		if err := json.Unmarshal(body, &p); err != nil || common.Action != "created" {
			return nil, err
		}
		n := p.PR.Number
		if n == 0 {
			if len(p.Issue.PullRequest) == 0 {
				return nil, nil // comment on a plain issue
			}
			n = p.Issue.Number
		}
		return &HookEvent{Kind: "review", Review: &ReviewEvent{EventID: id, Repo: repo, Number: n, State: "commented",
			Body: p.Comment.Body, Author: p.Comment.User.Login}}, nil
	}
	return nil, nil
}

// DefaultBranch returns the default branch of the bound repository.
func (g *GitHub) DefaultBranch(ctx context.Context, token string) (string, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := g.api.call(ctx, "get_repo", token, http.MethodGet, g.r(""), nil, &repo); err != nil {
		return "", err
	}
	return repo.DefaultBranch, nil
}
