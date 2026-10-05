package git

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// GitHub implements Provider for github.com and GitHub Enterprise using a
// GitHub App's user-to-server tokens.
type GitHub struct {
	api     *apiClient
	baseURL string
	repo    string // owner/name
	oauth   oauth2.Config
	app     *githubApp
}

// NewGitHub builds a GitHub provider. baseURL is https://github.com or a GHE host.
func NewGitHub(baseURL, oauthURL, repo, clientID, clientSecret string) *GitHub {
	apiBase := "https://api.github.com"
	if baseURL != "https://github.com" {
		apiBase = baseURL + "/api/v3"
	}
	return &GitHub{
		api: &apiClient{provider: "github", baseAPI: apiBase, http: newHTTPClient(),
			authHeader: func(t string) (string, string) { return "Authorization", "Bearer " + t }},
		baseURL: baseURL,
		repo:    repo,
		oauth: oauth2.Config{
			ClientID: clientID, ClientSecret: clientSecret,
			Endpoint: oauth2.Endpoint{AuthURL: oauthURL + "/login/oauth/authorize", TokenURL: baseURL + "/login/oauth/access_token"},
		},
	}
}

func (g *GitHub) Name() string { return "github" }

// WithScopes sets the OAuth scopes (an OAuth App needs them; a GitHub App
// ignores them and uses its permissions).
func (g *GitHub) WithScopes(scopes ...string) *GitHub {
	g.oauth.Scopes = scopes
	return g
}

func (g *GitHub) r(p string) string { return "/repos/" + g.repo + p }

func (g *GitHub) AuthCodeURL(state, redirectURL string) string {
	return authCodeURL(g.oauth, state, redirectURL)
}

func (g *GitHub) Exchange(ctx context.Context, code, redirectURL string) (*Token, error) {
	return exchange(ctx, g.oauth, code, redirectURL)
}

func (g *GitHub) Refresh(ctx context.Context, rt string) (*Token, error) {
	return refresh(ctx, g.oauth, rt)
}

func (g *GitHub) CurrentUser(ctx context.Context, token string) (*User, error) {
	var u struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
	}
	if _, err := g.api.call(ctx, "user", token, http.MethodGet, "/user", nil, &u); err != nil {
		return nil, err
	}
	name := u.Name
	if name == "" {
		name = u.Login
	}
	return &User{ID: fmt.Sprint(u.ID), Username: u.Login, Name: name, AvatarURL: u.AvatarURL}, nil
}

func (g *GitHub) BranchHead(ctx context.Context, token, branch string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if _, err := g.api.call(ctx, "branch_head", token, http.MethodGet, g.r("/git/ref/heads/"+pathEscapeSegments(branch)), nil, &ref); err != nil {
		return "", err
	}
	return ref.Object.SHA, nil
}

func (g *GitHub) CreateBranch(ctx context.Context, token, branch, fromSHA string) error {
	_, err := g.api.call(ctx, "create_branch", token, http.MethodPost, g.r("/git/refs"),
		map[string]string{"ref": "refs/heads/" + branch, "sha": fromSHA}, nil)
	return err
}

func (g *GitHub) DeleteBranch(ctx context.Context, token, branch string) error {
	_, err := g.api.call(ctx, "delete_branch", token, http.MethodDelete, g.r("/git/refs/heads/"+pathEscapeSegments(branch)), nil, nil)
	var ae *APIError
	if errors.Is(err, ErrNotFound) || (errors.As(err, &ae) && ae.Status == 422) {
		return nil // already gone (e.g. auto-deleted after merge)
	}
	return err
}

func (g *GitHub) GetFile(ctx context.Context, token, ref, path string) (*File, error) {
	var f struct {
		SHA      string `json:"sha"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Type     string `json:"type"`
	}
	p := g.r("/contents/" + pathEscapeSegments(path) + "?ref=" + url.QueryEscape(ref))
	if _, err := g.api.call(ctx, "get_file", token, http.MethodGet, p, nil, &f); err != nil {
		return nil, err
	}
	if f.Type != "" && f.Type != "file" {
		return nil, ErrNotFound
	}
	if f.Encoding == "none" || (f.Content == "" && f.SHA != "") {
		// Files over 1 MB: fetch the blob.
		var b struct {
			Content string `json:"content"`
		}
		if _, err := g.api.call(ctx, "get_blob", token, http.MethodGet, g.r("/git/blobs/"+f.SHA), nil, &b); err != nil {
			return nil, err
		}
		f.Content = b.Content
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("github: decode file: %w", err)
	}
	return &File{Content: data, BlobSHA: f.SHA}, nil
}

func (g *GitHub) ListFiles(ctx context.Context, token, ref, dir string) ([]string, error) {
	var t struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
		Truncated bool `json:"truncated"`
	}
	if _, err := g.api.call(ctx, "list_files", token, http.MethodGet, g.r("/git/trees/"+pathEscapeSegments(ref)+"?recursive=1"), nil, &t); err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(dir, "/") + "/"
	var out []string
	for _, e := range t.Tree {
		if e.Type == "blob" && (dir == "" || strings.HasPrefix(e.Path, prefix)) {
			out = append(out, e.Path)
		}
	}
	return out, nil
}

type ghTreeItem struct {
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

// Tree reads the recursive tree in one request; when GitHub truncates it (very
// large repositories) it walks dir level by level instead.
func (g *GitHub) Tree(ctx context.Context, token, ref, dir string) ([]TreeEntry, error) {
	var t struct {
		Tree      []ghTreeItem `json:"tree"`
		Truncated bool         `json:"truncated"`
	}
	if _, err := g.api.call(ctx, "get_tree", token, http.MethodGet, g.r("/git/trees/"+pathEscapeSegments(ref)+"?recursive=1"), nil, &t); err != nil {
		return nil, err
	}
	dir = strings.Trim(dir, "/")
	if !t.Truncated {
		var out []TreeEntry
		for _, e := range t.Tree {
			if e.Type == "blob" && (dir == "" || strings.HasPrefix(e.Path, dir+"/")) {
				out = append(out, TreeEntry{Path: e.Path, SHA: e.SHA, Size: e.Size})
			}
		}
		return out, nil
	}
	// Truncated: find the tree of dir from the root, then walk it.
	sha := ref
	if dir != "" {
		cur := ref
		for _, part := range strings.Split(dir, "/") {
			items, err := g.treeLevel(ctx, token, cur)
			if err != nil {
				return nil, err
			}
			next := ""
			for _, it := range items {
				if it.Type == "tree" && it.Path == part {
					next = it.SHA
				}
			}
			if next == "" {
				return nil, nil // no such directory
			}
			cur = next
		}
		sha = cur
	}
	var out []TreeEntry
	var walk func(sha, prefix string) error
	walk = func(sha, prefix string) error {
		items, err := g.treeLevel(ctx, token, sha)
		if err != nil {
			return err
		}
		for _, it := range items {
			p := it.Path
			if prefix != "" {
				p = prefix + "/" + it.Path
			}
			switch it.Type {
			case "blob":
				out = append(out, TreeEntry{Path: p, SHA: it.SHA, Size: it.Size})
			case "tree":
				if err := walk(it.SHA, p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return out, walk(sha, dir)
}

func (g *GitHub) treeLevel(ctx context.Context, token, sha string) ([]ghTreeItem, error) {
	var t struct {
		Tree []ghTreeItem `json:"tree"`
	}
	_, err := g.api.call(ctx, "get_tree", token, http.MethodGet, g.r("/git/trees/"+pathEscapeSegments(sha)), nil, &t)
	return t.Tree, err
}

// Blob returns the content of a blob.
func (g *GitHub) Blob(ctx context.Context, token, sha string) ([]byte, error) {
	var b struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if _, err := g.api.call(ctx, "get_blob", token, http.MethodGet, g.r("/git/blobs/"+url.PathEscape(sha)), nil, &b); err != nil {
		return nil, err
	}
	if b.Encoding == "utf-8" {
		return []byte(b.Content), nil
	}
	data, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(b.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("github: decode blob: %w", err)
	}
	return data, nil
}

// Commit creates a single commit via the git data API: blobs → tree → commit → ref.
func (g *GitHub) Commit(ctx context.Context, token, branch, message string, changes []FileChange) (string, error) {
	head, err := g.BranchHead(ctx, token, branch)
	if err != nil {
		return "", err
	}
	var parent struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if _, err := g.api.call(ctx, "get_commit", token, http.MethodGet, g.r("/git/commits/"+head), nil, &parent); err != nil {
		return "", err
	}
	type entry struct {
		Path string  `json:"path"`
		Mode string  `json:"mode"`
		Type string  `json:"type"`
		SHA  *string `json:"sha"`
	}
	entries := make([]entry, 0, len(changes))
	for _, ch := range changes {
		if ch.Delete {
			entries = append(entries, entry{Path: ch.Path, Mode: "100644", Type: "blob", SHA: nil})
			continue
		}
		var blob struct {
			SHA string `json:"sha"`
		}
		if _, err := g.api.call(ctx, "create_blob", token, http.MethodPost, g.r("/git/blobs"),
			map[string]string{"content": base64.StdEncoding.EncodeToString(ch.Content), "encoding": "base64"}, &blob); err != nil {
			return "", err
		}
		sha := blob.SHA
		entries = append(entries, entry{Path: ch.Path, Mode: "100644", Type: "blob", SHA: &sha})
	}
	var tree struct {
		SHA string `json:"sha"`
	}
	if _, err := g.api.call(ctx, "create_tree", token, http.MethodPost, g.r("/git/trees"),
		map[string]any{"base_tree": parent.Tree.SHA, "tree": entries}, &tree); err != nil {
		return "", err
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if _, err := g.api.call(ctx, "create_commit", token, http.MethodPost, g.r("/git/commits"),
		map[string]any{"message": message, "tree": tree.SHA, "parents": []string{head}}, &commit); err != nil {
		return "", err
	}
	_, err = g.api.call(ctx, "update_ref", token, http.MethodPatch, g.r("/git/refs/heads/"+pathEscapeSegments(branch)),
		map[string]any{"sha": commit.SHA, "force": false}, nil)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 422 {
		return "", ErrConflict
	}
	if err != nil {
		return "", err
	}
	return commit.SHA, nil
}

func (g *GitHub) LatestCommit(ctx context.Context, token, ref, path string) (string, error) {
	var commits []struct {
		SHA string `json:"sha"`
	}
	q := url.Values{"sha": {ref}, "path": {path}, "per_page": {"1"}}
	if _, err := g.api.call(ctx, "latest_commit", token, http.MethodGet, g.r("/commits?"+q.Encode()), nil, &commits); err != nil {
		return "", err
	}
	if len(commits) == 0 {
		return "", ErrNotFound
	}
	return commits[0].SHA, nil
}

func (g *GitHub) CreatePR(ctx context.Context, token, head, base, title, body string) (*PR, error) {
	var pr struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if _, err := g.api.call(ctx, "create_pr", token, http.MethodPost, g.r("/pulls"),
		map[string]string{"title": title, "head": head, "base": base, "body": body}, &pr); err != nil {
		return nil, err
	}
	return &PR{Number: pr.Number, URL: pr.HTMLURL}, nil
}

func (g *GitHub) MergePR(ctx context.Context, token string, number int, message string) error {
	_, err := g.api.call(ctx, "merge_pr", token, http.MethodPut, g.r(fmt.Sprintf("/pulls/%d/merge", number)),
		map[string]string{"merge_method": "merge", "commit_title": message}, nil)
	return err
}

func (g *GitHub) ClosePR(ctx context.Context, token string, number int) error {
	_, err := g.api.call(ctx, "close_pr", token, http.MethodPatch, g.r(fmt.Sprintf("/pulls/%d", number)),
		map[string]string{"state": "closed"}, nil)
	return err
}

func (g *GitHub) ApprovePR(ctx context.Context, token string, number int) error {
	_, err := g.api.call(ctx, "approve_pr", token, http.MethodPost, g.r(fmt.Sprintf("/pulls/%d/reviews", number)),
		map[string]string{"event": "APPROVE"}, nil)
	return err
}

func (g *GitHub) CreateIssue(ctx context.Context, token, title, body string) (string, error) {
	var is struct {
		HTMLURL string `json:"html_url"`
	}
	if _, err := g.api.call(ctx, "create_issue", token, http.MethodPost, g.r("/issues"),
		map[string]string{"title": title, "body": body}, &is); err != nil {
		return "", err
	}
	return is.HTMLURL, nil
}

func (g *GitHub) SearchCode(ctx context.Context, token, query string) ([]SearchHit, error) {
	var res struct {
		Items []struct {
			Path        string `json:"path"`
			TextMatches []struct {
				Fragment string `json:"fragment"`
			} `json:"text_matches"`
		} `json:"items"`
	}
	q := url.Values{"q": {query + " repo:" + g.repo + " path:specs"}, "per_page": {"20"}}
	req := "/search/code?" + q.Encode()
	if _, err := g.api.call(ctx, "search_code", token, http.MethodGet, req, nil, &res); err != nil {
		return nil, err
	}
	hits := make([]SearchHit, 0, len(res.Items))
	for _, it := range res.Items {
		h := SearchHit{Path: it.Path}
		if len(it.TextMatches) > 0 {
			h.Snippet = it.TextMatches[0].Fragment
		}
		hits = append(hits, h)
	}
	return hits, nil
}

func (g *GitHub) CommitURL(sha string) string {
	return fmt.Sprintf("%s/%s/commit/%s", g.baseURL, g.repo, sha)
}

// VerifyWebhook checks the X-Hub-Signature-256 HMAC.
func (g *GitHub) VerifyWebhook(h http.Header, body []byte, secret string) bool {
	sig := h.Get("X-Hub-Signature-256")
	if secret == "" || !strings.HasPrefix(sig, "sha256=") {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(sig, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// ParsePush normalizes a push event. ok=false for other event types.
func (g *GitHub) ParsePush(h http.Header, body []byte) (*PushEvent, bool, error) {
	if h.Get("X-GitHub-Event") != "push" {
		return nil, false, nil
	}
	var p struct {
		Ref    string `json:"ref"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
		Commits []struct {
			ID        string    `json:"id"`
			Message   string    `json:"message"`
			Timestamp time.Time `json:"timestamp"`
			Added     []string  `json:"added"`
			Modified  []string  `json:"modified"`
			Removed   []string  `json:"removed"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, false, err
	}
	ev := &PushEvent{EventID: "github:" + h.Get("X-GitHub-Delivery"), Branch: strings.TrimPrefix(p.Ref, "refs/heads/"), Actor: p.Sender.Login}
	for _, c := range p.Commits {
		ev.Commits = append(ev.Commits, PushCommit{SHA: c.ID, Message: c.Message, Timestamp: c.Timestamp, Added: c.Added, Modified: c.Modified, Removed: c.Removed})
	}
	return ev, true, nil
}
