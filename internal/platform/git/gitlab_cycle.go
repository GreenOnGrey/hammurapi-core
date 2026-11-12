package git

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// WithBot configures the GitLab bot user token (GITLAB_BOT_TOKEN).
func (g *GitLab) WithBot(token string) *GitLab {
	g.botToken = token
	return g
}

func (g *GitLab) Repo() string { return g.repo }

func (g *GitLab) ForRepo(repo string) Provider {
	c := *g
	c.repo = repo
	c.project = url.PathEscape(repo)
	return &c
}

// BotToken returns the bot user's token. GitLab has no per-repository tokens
// for users; restrict the bot's membership to the product's groups.
func (g *GitLab) BotToken(context.Context) (string, error) {
	if g.botToken == "" {
		return "", errors.New("gitlab: GITLAB_BOT_TOKEN is not configured")
	}
	return g.botToken, nil
}

func (g *GitLab) GetPR(ctx context.Context, token string, number int) (*PRInfo, error) {
	var mr struct {
		IID          int    `json:"iid"`
		WebURL       string `json:"web_url"`
		Title        string `json:"title"`
		State        string `json:"state"`
		SourceBranch string `json:"source_branch"`
		SHA          string `json:"sha"`
		MergeSHA     string `json:"merge_commit_sha"`
		Detailed     string `json:"detailed_merge_status"`
		MergeStatus  string `json:"merge_status"`
	}
	if _, err := g.api.call(ctx, "get_mr", token, http.MethodGet, g.p(fmt.Sprintf("/merge_requests/%d", number)), nil, &mr); err != nil {
		return nil, err
	}
	state := map[string]string{"opened": "open", "merged": "merged", "closed": "closed", "locked": "open"}[mr.State]
	info := &PRInfo{Number: mr.IID, URL: mr.WebURL, Title: mr.Title, State: state, Branch: mr.SourceBranch, HeadSHA: mr.SHA, MergeSHA: mr.MergeSHA}
	status := mr.Detailed
	if status == "" {
		status = mr.MergeStatus
	}
	switch status {
	case "mergeable", "can_be_merged":
		t := true
		info.Mergeable = &t
	case "conflict", "need_rebase", "broken_status", "ci_must_pass", "cannot_be_merged", "not_approved", "discussions_not_resolved":
		f := false
		info.Mergeable = &f
	}
	return info, nil
}

func (g *GitLab) UpdatePRBranch(ctx context.Context, token string, number int) error {
	_, err := g.api.call(ctx, "rebase_mr", token, http.MethodPut, g.p(fmt.Sprintf("/merge_requests/%d/rebase", number)), nil, nil)
	return err
}

func (g *GitLab) RequestReview(ctx context.Context, token string, number int, reviewers []string) error {
	var ids []int64
	for _, login := range reviewers {
		var users []struct {
			ID int64 `json:"id"`
		}
		if _, err := g.api.call(ctx, "find_user", token, http.MethodGet, "/users?username="+url.QueryEscape(login), nil, &users); err != nil {
			return err
		}
		if len(users) > 0 {
			ids = append(ids, users[0].ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := g.api.call(ctx, "request_review", token, http.MethodPut, g.p(fmt.Sprintf("/merge_requests/%d", number)),
		map[string]any{"reviewer_ids": ids}, nil)
	return err
}

func (g *GitLab) CommentPR(ctx context.Context, token string, number int, body string) error {
	_, err := g.api.call(ctx, "comment_mr", token, http.MethodPost, g.p(fmt.Sprintf("/merge_requests/%d/notes", number)),
		map[string]string{"body": body}, nil)
	return err
}

func (g *GitLab) PRChangedFiles(ctx context.Context, token string, number int) ([]ChangedFile, error) {
	var out []ChangedFile
	for page := 1; page <= 30; page++ {
		var diffs []struct {
			NewPath string `json:"new_path"`
			OldPath string `json:"old_path"`
			New     bool   `json:"new_file"`
			Deleted bool   `json:"deleted_file"`
			Renamed bool   `json:"renamed_file"`
		}
		if _, err := g.api.call(ctx, "mr_diffs", token, http.MethodGet, g.p(fmt.Sprintf("/merge_requests/%d/diffs?per_page=100&page=%d", number, page)), nil, &diffs); err != nil {
			return nil, err
		}
		for _, d := range diffs {
			cf := ChangedFile{Path: d.NewPath, Status: "modified"}
			switch {
			case d.New:
				cf.Status = "added"
			case d.Deleted:
				cf.Status, cf.Path = "removed", d.OldPath
			case d.Renamed:
				cf.Status, cf.OldPath = "renamed", d.OldPath
			}
			out = append(out, cf)
		}
		if len(diffs) < 100 {
			break
		}
	}
	return out, nil
}

func (g *GitLab) CommitParents(ctx context.Context, token, sha string) ([]string, error) {
	var c struct {
		ParentIDs []string `json:"parent_ids"`
	}
	if _, err := g.api.call(ctx, "commit_parents", token, http.MethodGet, g.p("/repository/commits/"+url.PathEscape(sha)), nil, &c); err != nil {
		return nil, err
	}
	return c.ParentIDs, nil
}

func (g *GitLab) IsAncestor(ctx context.Context, token, ancestor, descendant string) (bool, error) {
	var mb struct {
		ID string `json:"id"`
	}
	q := url.Values{"refs[]": {ancestor, descendant}}
	_, err := g.api.call(ctx, "merge_base", token, http.MethodGet, g.p("/repository/merge_base?"+q.Encode()), nil, &mb)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return mb.ID == ancestor, nil
}

func (g *GitLab) GroupMembers(ctx context.Context, token, group string) ([]string, error) {
	var members []struct {
		Username string `json:"username"`
	}
	if _, err := g.api.call(ctx, "group_members", token, http.MethodGet, "/groups/"+url.PathEscape(group)+"/members/all?per_page=100", nil, &members); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.Username)
	}
	return out, nil
}

// RunPipeline creates a pipeline on ref with variables. `workflow` is unused
// for GitLab (the project's .gitlab-ci.yml decides which jobs run).
func (g *GitLab) RunPipeline(ctx context.Context, token, _, ref string, params map[string]string) (string, error) {
	vars := make([]map[string]string, 0, len(params))
	for k, v := range params {
		vars = append(vars, map[string]string{"key": k, "value": v})
	}
	var pl struct {
		WebURL string `json:"web_url"`
	}
	if _, err := g.api.call(ctx, "create_pipeline", token, http.MethodPost, g.p("/pipeline"),
		map[string]any{"ref": ref, "variables": vars}, &pl); err != nil {
		return "", err
	}
	return pl.WebURL, nil
}

// ParseHook normalizes GitLab webhooks.
func (g *GitLab) ParseHook(h http.Header, body []byte) (*HookEvent, error) {
	id := h.Get("X-Gitlab-Event-UUID")
	if id == "" {
		sum := sha256.Sum256(body)
		id = hex.EncodeToString(sum[:])
	}
	id = "gitlab:" + id
	var common struct {
		Project struct {
			Path string `json:"path_with_namespace"`
		} `json:"project"`
		User struct {
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &common); err != nil {
		return nil, err
	}
	repo := common.Project.Path
	switch h.Get("X-Gitlab-Event") {
	case "Push Hook":
		ev, _, err := g.ParsePush(h, body)
		if err != nil {
			return nil, err
		}
		var p struct {
			After string `json:"after"`
		}
		_ = json.Unmarshal(body, &p)
		ev.Repo, ev.After = repo, p.After
		return &HookEvent{Kind: "push", Push: ev}, nil
	case "Tag Push Hook":
		var p struct {
			Ref          string `json:"ref"`
			CheckoutSHA  string `json:"checkout_sha"`
			After        string `json:"after"`
			UserUsername string `json:"user_username"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, err
		}
		sha := p.CheckoutSHA
		if sha == "" {
			sha = p.After
		}
		return &HookEvent{Kind: "tag", Push: &PushEvent{EventID: id, Repo: repo, Tag: strings.TrimPrefix(p.Ref, "refs/tags/"),
			After: sha, Actor: p.UserUsername}}, nil
	case "Merge Request Hook":
		var p struct {
			OA struct {
				IID          int    `json:"iid"`
				Title        string `json:"title"`
				Action       string `json:"action"`
				SourceBranch string `json:"source_branch"`
				TargetBranch string `json:"target_branch"`
				URL          string `json:"url"`
				MergeSHA     string `json:"merge_commit_sha"`
				LastCommit   struct {
					ID string `json:"id"`
				} `json:"last_commit"`
			} `json:"object_attributes"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, err
		}
		base := &PREvent{EventID: id, Repo: repo, Number: p.OA.IID, Title: p.OA.Title, Branch: p.OA.SourceBranch,
			Base: p.OA.TargetBranch, HeadSHA: p.OA.LastCommit.ID, URL: p.OA.URL, Author: common.User.Username, Actor: common.User.Username}
		switch p.OA.Action {
		case "open":
			base.Action = "opened"
		case "reopen":
			base.Action = "reopened"
		case "update":
			base.Action = "updated"
		case "merge":
			base.Action, base.MergeSHA = "merged", p.OA.MergeSHA
		case "close":
			base.Action = "closed"
		case "approved", "approval":
			return &HookEvent{Kind: "review", Review: &ReviewEvent{EventID: id, Repo: repo, Number: p.OA.IID, State: "approved", Author: common.User.Username}}, nil
		default:
			return nil, nil
		}
		return &HookEvent{Kind: "pr", PR: base}, nil
	case "Pipeline Hook":
		// FTR.HMR.DEV-0004 R17: a pipeline of a commit changed — the cue to ask for its status
		var p struct {
			OA struct {
				SHA string `json:"sha"`
			} `json:"object_attributes"`
		}
		if err := json.Unmarshal(body, &p); err != nil || p.OA.SHA == "" {
			return nil, err
		}
		return &HookEvent{Kind: "ci", CI: &CIEvent{EventID: id, Repo: repo, SHA: p.OA.SHA}}, nil
	case "Note Hook":
		var p struct {
			OA struct {
				Note         string `json:"note"`
				NoteableType string `json:"noteable_type"`
			} `json:"object_attributes"`
			MR struct {
				IID int `json:"iid"`
			} `json:"merge_request"`
		}
		if err := json.Unmarshal(body, &p); err != nil || p.OA.NoteableType != "MergeRequest" {
			return nil, err
		}
		return &HookEvent{Kind: "review", Review: &ReviewEvent{EventID: id, Repo: repo, Number: p.MR.IID, State: "commented",
			Body: p.OA.Note, Author: common.User.Username}}, nil
	}
	return nil, nil
}

// DefaultBranch returns the default branch of the bound project.
func (g *GitLab) DefaultBranch(ctx context.Context, token string) (string, error) {
	var p struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := g.api.call(ctx, "get_project", token, http.MethodGet, g.p(""), nil, &p); err != nil {
		return "", err
	}
	return p.DefaultBranch, nil
}
