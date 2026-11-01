package git

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// GitLab implements Provider for gitlab.com and self-hosted GitLab via OAuth tokens.
type GitLab struct {
	api      *apiClient
	baseURL  string
	repo     string
	project  string // url-encoded project path
	oauth    oauth2.Config
	botToken string
}

// NewGitLab builds a GitLab provider.
func NewGitLab(baseURL, oauthURL, repo, clientID, clientSecret string) *GitLab {
	return &GitLab{
		api: &apiClient{provider: "gitlab", baseAPI: baseURL + "/api/v4", http: newHTTPClient(),
			authHeader: func(t string) (string, string) { return "Authorization", "Bearer " + t }},
		baseURL: baseURL,
		repo:    repo,
		project: url.PathEscape(repo),
		oauth: oauth2.Config{
			ClientID: clientID, ClientSecret: clientSecret, Scopes: []string{"api", "read_user"},
			Endpoint: oauth2.Endpoint{AuthURL: oauthURL + "/oauth/authorize", TokenURL: baseURL + "/oauth/token"},
		},
	}
}

func (g *GitLab) Name() string { return "gitlab" }

func (g *GitLab) p(p string) string { return "/projects/" + g.project + p }

func (g *GitLab) AuthCodeURL(state, redirectURL string) string {
	return authCodeURL(g.oauth, state, redirectURL)
}

func (g *GitLab) Exchange(ctx context.Context, code, redirectURL string) (*Token, error) {
	return exchange(ctx, g.oauth, code, redirectURL)
}

func (g *GitLab) Refresh(ctx context.Context, rt string) (*Token, error) {
	return refresh(ctx, g.oauth, rt)
}

func (g *GitLab) CurrentUser(ctx context.Context, token string) (*User, error) {
	var u struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		Name      string `json:"name"`
		AvatarURL string `json:"avatar_url"`
	}
	if _, err := g.api.call(ctx, "user", token, http.MethodGet, "/user", nil, &u); err != nil {
		return nil, err
	}
	return &User{ID: fmt.Sprint(u.ID), Username: u.Username, Name: u.Name, AvatarURL: u.AvatarURL}, nil
}

func (g *GitLab) BranchHead(ctx context.Context, token, branch string) (string, error) {
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if _, err := g.api.call(ctx, "branch_head", token, http.MethodGet, g.p("/repository/branches/"+url.PathEscape(branch)), nil, &b); err != nil {
		return "", err
	}
	return b.Commit.ID, nil
}

func (g *GitLab) CreateBranch(ctx context.Context, token, branch, fromSHA string) error {
	q := url.Values{"branch": {branch}, "ref": {fromSHA}}
	_, err := g.api.call(ctx, "create_branch", token, http.MethodPost, g.p("/repository/branches?"+q.Encode()), nil, nil)
	return err
}

func (g *GitLab) DeleteBranch(ctx context.Context, token, branch string) error {
	_, err := g.api.call(ctx, "delete_branch", token, http.MethodDelete, g.p("/repository/branches/"+url.PathEscape(branch)), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (g *GitLab) GetFile(ctx context.Context, token, ref, path string) (*File, error) {
	var f struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		BlobID   string `json:"blob_id"`
	}
	req := g.p("/repository/files/" + url.PathEscape(path) + "?ref=" + url.QueryEscape(ref))
	if _, err := g.api.call(ctx, "get_file", token, http.MethodGet, req, nil, &f); err != nil {
		return nil, err
	}
	data := []byte(f.Content)
	if f.Encoding == "base64" {
		var err error
		if data, err = base64.StdEncoding.DecodeString(f.Content); err != nil {
			return nil, fmt.Errorf("gitlab: decode file: %w", err)
		}
	}
	return &File{Content: data, BlobSHA: f.BlobID}, nil
}

func (g *GitLab) ListFiles(ctx context.Context, token, ref, dir string) ([]string, error) {
	var out []string
	page := "1"
	for page != "" {
		q := url.Values{"ref": {ref}, "recursive": {"true"}, "per_page": {"100"}, "page": {page}}
		if dir != "" {
			q.Set("path", dir)
		}
		var items []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		}
		resp, err := g.api.call(ctx, "list_files", token, http.MethodGet, g.p("/repository/tree?"+q.Encode()), nil, &items)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.Type == "blob" {
				out = append(out, it.Path)
			}
		}
		page = resp.Header.Get("X-Next-Page")
	}
	return out, nil
}

// Tree lists files with their blob ids; GitLab does not report sizes in the
// tree, so Size is -1.
func (g *GitLab) Tree(ctx context.Context, token, ref, dir string) ([]TreeEntry, error) {
	var out []TreeEntry
	page := "1"
	for page != "" {
		q := url.Values{"ref": {ref}, "recursive": {"true"}, "per_page": {"100"}, "page": {page}}
		if dir = strings.Trim(dir, "/"); dir != "" {
			q.Set("path", dir)
		}
		var items []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
			Type string `json:"type"`
		}
		resp, err := g.api.call(ctx, "get_tree", token, http.MethodGet, g.p("/repository/tree?"+q.Encode()), nil, &items)
		if errors.Is(err, ErrNotFound) {
			return nil, nil // no such directory
		}
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			if it.Type == "blob" {
				out = append(out, TreeEntry{Path: it.Path, SHA: it.ID, Size: -1})
			}
		}
		page = resp.Header.Get("X-Next-Page")
	}
	return out, nil
}

// Blob returns the raw content of a blob.
func (g *GitLab) Blob(ctx context.Context, token, sha string) ([]byte, error) {
	return g.api.raw(ctx, "get_blob", token, g.p("/repository/blobs/"+url.PathEscape(sha)+"/raw"))
}

func (g *GitLab) fileExists(ctx context.Context, token, ref, path string) (bool, error) {
	req := g.p("/repository/files/" + url.PathEscape(path) + "?ref=" + url.QueryEscape(ref))
	_, err := g.api.call(ctx, "file_exists", token, http.MethodHead, req, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Commit creates one commit with create/update/delete actions.
func (g *GitLab) Commit(ctx context.Context, token, branch, message string, changes []FileChange) (string, error) {
	actions := make([]map[string]string, 0, len(changes))
	for _, ch := range changes {
		if ch.Delete {
			actions = append(actions, map[string]string{"action": "delete", "file_path": ch.Path})
			continue
		}
		exists, err := g.fileExists(ctx, token, branch, ch.Path)
		if err != nil {
			return "", err
		}
		action := "create"
		if exists {
			action = "update"
		}
		actions = append(actions, map[string]string{
			"action": action, "file_path": ch.Path,
			"content": base64.StdEncoding.EncodeToString(ch.Content), "encoding": "base64",
		})
	}
	var c struct {
		ID string `json:"id"`
	}
	_, err := g.api.call(ctx, "commit", token, http.MethodPost, g.p("/repository/commits"),
		map[string]any{"branch": branch, "commit_message": message, "actions": actions}, &c)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 400 && strings.Contains(strings.ToLower(ae.Message), "already exists") {
		return "", ErrConflict
	}
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

func (g *GitLab) LatestCommit(ctx context.Context, token, ref, path string) (string, error) {
	var commits []struct {
		ID string `json:"id"`
	}
	q := url.Values{"ref_name": {ref}, "path": {path}, "per_page": {"1"}}
	if _, err := g.api.call(ctx, "latest_commit", token, http.MethodGet, g.p("/repository/commits?"+q.Encode()), nil, &commits); err != nil {
		return "", err
	}
	if len(commits) == 0 {
		return "", ErrNotFound
	}
	return commits[0].ID, nil
}

func (g *GitLab) CreatePR(ctx context.Context, token, head, base, title, body string) (*PR, error) {
	var mr struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
	}
	if _, err := g.api.call(ctx, "create_mr", token, http.MethodPost, g.p("/merge_requests"),
		map[string]string{"source_branch": head, "target_branch": base, "title": title, "description": body}, &mr); err != nil {
		return nil, err
	}
	return &PR{Number: mr.IID, URL: mr.WebURL}, nil
}

func (g *GitLab) MergePR(ctx context.Context, token string, number int, message string) error {
	_, err := g.api.call(ctx, "merge_mr", token, http.MethodPut, g.p(fmt.Sprintf("/merge_requests/%d/merge", number)),
		map[string]any{"merge_commit_message": message}, nil)
	return err
}

func (g *GitLab) ClosePR(ctx context.Context, token string, number int) error {
	_, err := g.api.call(ctx, "close_mr", token, http.MethodPut, g.p(fmt.Sprintf("/merge_requests/%d", number)),
		map[string]string{"state_event": "close"}, nil)
	return err
}

func (g *GitLab) ApprovePR(ctx context.Context, token string, number int) error {
	_, err := g.api.call(ctx, "approve_mr", token, http.MethodPost, g.p(fmt.Sprintf("/merge_requests/%d/approve", number)), nil, nil)
	return err
}

func (g *GitLab) CreateIssue(ctx context.Context, token, title, body string) (string, error) {
	var is struct {
		WebURL string `json:"web_url"`
	}
	if _, err := g.api.call(ctx, "create_issue", token, http.MethodPost, g.p("/issues"),
		map[string]string{"title": title, "description": body}, &is); err != nil {
		return "", err
	}
	return is.WebURL, nil
}

func (g *GitLab) SearchCode(ctx context.Context, token, query string) ([]SearchHit, error) {
	var res []struct {
		Path string `json:"path"`
		Data string `json:"data"`
	}
	q := url.Values{"scope": {"blobs"}, "search": {query}, "per_page": {"20"}}
	if _, err := g.api.call(ctx, "search_code", token, http.MethodGet, g.p("/search?"+q.Encode()), nil, &res); err != nil {
		return nil, err
	}
	hits := make([]SearchHit, 0, len(res))
	for _, r := range res {
		if strings.HasPrefix(r.Path, "specs/") {
			hits = append(hits, SearchHit{Path: r.Path, Snippet: r.Data})
		}
	}
	return hits, nil
}

func (g *GitLab) CommitURL(sha string) string {
	return fmt.Sprintf("%s/%s/-/commit/%s", g.baseURL, g.repo, sha)
}

// VerifyWebhook compares X-Gitlab-Token with the secret in constant time.
func (g *GitLab) VerifyWebhook(h http.Header, _ []byte, secret string) bool {
	tok := h.Get("X-Gitlab-Token")
	return secret != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(secret)) == 1
}

// ParsePush normalizes a push hook. ok=false for other event types.
func (g *GitLab) ParsePush(h http.Header, body []byte) (*PushEvent, bool, error) {
	if h.Get("X-Gitlab-Event") != "Push Hook" {
		return nil, false, nil
	}
	var p struct {
		Ref          string `json:"ref"`
		After        string `json:"after"`
		UserUsername string `json:"user_username"`
		Commits      []struct {
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
	id := h.Get("X-Gitlab-Event-UUID")
	if id == "" {
		sum := sha256.Sum256(body)
		id = hex.EncodeToString(sum[:])
	}
	ev := &PushEvent{EventID: "gitlab:" + id, Branch: strings.TrimPrefix(p.Ref, "refs/heads/"), Actor: p.UserUsername}
	for _, c := range p.Commits {
		ev.Commits = append(ev.Commits, PushCommit{SHA: c.ID, Message: c.Message, Timestamp: c.Timestamp, Added: c.Added, Modified: c.Modified, Removed: c.Removed})
	}
	return ev, true, nil
}

// Archive implements Archiver: GET /projects/{id}/repository/archive.tar.gz.
func (g *GitLab) Archive(ctx context.Context, token, ref string) (io.ReadCloser, error) {
	return g.api.stream(ctx, "archive", token, g.p("/repository/archive.tar.gz?sha="+url.QueryEscape(ref)))
}
