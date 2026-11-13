package git

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The feedback of a PR for a regeneration of its code (FTR.HMR.DEV-0004 R18, tech §14): the comments of
// people and tools and the checks of its head that failed, with the end of their logs.

// Limits of the feedback: the prompt of the agent stays readable and bounded.
const (
	FeedbackComments = 100  // the latest comments a provider gives; the task keeps fewer
	FeedbackChecks   = 8    // failed checks
	feedbackLogLines = 80   // the end of the log of a failed check
	feedbackLogBytes = 8000 // and at most so many bytes of it
	feedbackText     = 4000 // of one comment or one output of a check
)

// PRComment is a comment of a PR: a review, a comment of the conversation or a line comment.
type PRComment struct {
	Author string    `json:"author"`
	Body   string    `json:"body"`
	Path   string    `json:"path,omitempty"`
	Line   int       `json:"line,omitempty"`
	At     time.Time `json:"at"`
}

// FailedCheck is a check of the head of a PR that failed.
type FailedCheck struct {
	Name   string `json:"name"`
	URL    string `json:"url,omitempty"`
	Output string `json:"output,omitempty"` // the summary of the check or its description
	Log    string `json:"log,omitempty"`    // the end of its log, when the provider gives it
}

// PRFeedback is what a PR says about its code.
type PRFeedback struct {
	Comments []PRComment   `json:"comments"`
	Failed   []FailedCheck `json:"failed"`
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + " …"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// logTail is the end of a log: its last lines, at most feedbackLogBytes.
func logTail(raw []byte) string {
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > feedbackLogLines {
		lines = lines[len(lines)-feedbackLogLines:]
	}
	s := strings.Join(lines, "\n")
	if len(s) > feedbackLogBytes {
		s = s[len(s)-feedbackLogBytes:]
		for len(s) > 0 && !utf8Start(s[0]) {
			s = s[1:]
		}
	}
	return s
}

// latest keeps the latest comments in the order they were written.
func latest(cs []PRComment) []PRComment {
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].At.Before(cs[j].At) })
	if len(cs) > FeedbackComments {
		cs = cs[len(cs)-FeedbackComments:]
	}
	return cs
}

// newest reads a list of GitHub oldest first, its last two pages of 100: the newest items of a busy PR.
func newest[T any](ctx context.Context, api *apiClient, op, token, path string) ([]T, error) {
	var first []T
	resp, err := api.call(ctx, op, token, http.MethodGet, path+"?per_page=100", nil, &first)
	if err != nil {
		return nil, err
	}
	last := lastPage(resp.Header.Get("Link"))
	if last <= 1 {
		return first, nil
	}
	var out []T
	for page := max(2, last-1); page <= last; page++ {
		var items []T
		if _, err := api.call(ctx, op, token, http.MethodGet, path+"?per_page=100&page="+strconv.Itoa(page), nil, &items); err != nil {
			return nil, err
		}
		out = append(out, items...)
	}
	if last == 2 {
		out = append(first, out...)
	}
	return out, nil
}

// lastPage is the number of the page rel="last" of a Link header of GitHub; 0 — none.
func lastPage(link string) int {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="last"`) {
			continue
		}
		u := strings.TrimSpace(part)
		if i, j := strings.Index(u, "<"), strings.Index(u, ">"); i >= 0 && j > i {
			if parsed, err := url.Parse(u[i+1 : j]); err == nil {
				n, _ := strconv.Atoi(parsed.Query().Get("page"))
				return n
			}
		}
	}
	return 0
}

// PRFeedback reads the reviews, the comments and the line comments of a PR and the failed check runs
// and commit statuses of sha with the end of the logs of GitHub Actions jobs.
func (g *GitHub) PRFeedback(ctx context.Context, token string, number int, sha string) (*PRFeedback, error) {
	out := &PRFeedback{Comments: []PRComment{}, Failed: []FailedCheck{}}
	type user struct {
		Login string `json:"login"`
	}
	n := strconv.Itoa(number)
	type issueComment struct {
		User      user      `json:"user"`
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
	}
	issue, err := newest[issueComment](ctx, g.api, "pr_comments", token, g.r("/issues/"+n+"/comments"))
	if err != nil {
		return nil, err
	}
	for _, c := range issue {
		out.Comments = append(out.Comments, PRComment{Author: c.User.Login, Body: clip(c.Body, feedbackText), At: c.CreatedAt})
	}
	type review struct {
		User        user      `json:"user"`
		Body        string    `json:"body"`
		State       string    `json:"state"`
		SubmittedAt time.Time `json:"submitted_at"`
	}
	reviews, err := newest[review](ctx, g.api, "pr_reviews", token, g.r("/pulls/"+n+"/reviews"))
	if err != nil {
		return nil, err
	}
	for _, r := range reviews {
		body := strings.TrimSpace(r.Body)
		if r.State == "CHANGES_REQUESTED" {
			body = strings.TrimSpace("Changes requested. " + body)
		}
		if body != "" {
			out.Comments = append(out.Comments, PRComment{Author: r.User.Login, Body: clip(body, feedbackText), At: r.SubmittedAt})
		}
	}
	type lineComment struct {
		User      user      `json:"user"`
		Body      string    `json:"body"`
		Path      string    `json:"path"`
		Line      int       `json:"line"`
		CreatedAt time.Time `json:"created_at"`
	}
	lines, err := newest[lineComment](ctx, g.api, "pr_line_comments", token, g.r("/pulls/"+n+"/comments"))
	if err != nil {
		return nil, err
	}
	for _, c := range lines {
		out.Comments = append(out.Comments, PRComment{Author: c.User.Login, Body: clip(c.Body, feedbackText), Path: c.Path, Line: c.Line, At: c.CreatedAt})
	}
	out.Comments = latest(out.Comments)
	if sha == "" {
		return out, nil
	}
	var runs struct {
		CheckRuns []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			HTMLURL    string `json:"html_url"`
			App        struct {
				Slug string `json:"slug"`
			} `json:"app"`
			Output struct {
				Title   string `json:"title"`
				Summary string `json:"summary"`
				Text    string `json:"text"`
			} `json:"output"`
		} `json:"check_runs"`
	}
	if _, err := g.api.call(ctx, "check_runs", token, http.MethodGet, g.r("/commits/"+url.PathEscape(sha)+"/check-runs?filter=latest&per_page=100"), nil, &runs); err != nil {
		return nil, err
	}
	for _, r := range runs.CheckRuns {
		if githubCheckRun(r.Status, r.Conclusion) != CIFailure || len(out.Failed) >= FeedbackChecks {
			continue
		}
		fc := FailedCheck{Name: r.Name, URL: r.HTMLURL,
			Output: clip(strings.Join(nonEmpty(r.Output.Title, r.Output.Summary, r.Output.Text), "\n"), feedbackText)}
		if r.App.Slug == "github-actions" { // the check run of a job is the job: its log needs Actions: read
			// a log the bot may not read (no Actions permission), past its retention or that failed to come
			// leaves the check its summary: it does not fail the feedback
			if raw, err := g.api.raw(ctx, "job_log", token, g.r("/actions/jobs/"+strconv.FormatInt(r.ID, 10)+"/logs")); err == nil {
				fc.Log = logTail(raw)
			}
		}
		out.Failed = append(out.Failed, fc)
	}
	var statuses []struct {
		Context     string `json:"context"`
		State       string `json:"state"`
		Description string `json:"description"`
		TargetURL   string `json:"target_url"`
	}
	if _, err := g.api.call(ctx, "commit_statuses", token, http.MethodGet, g.r("/commits/"+url.PathEscape(sha)+"/statuses?per_page=100"), nil, &statuses); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, s := range statuses { // the newest first: the latest status of every context
		if seen[s.Context] {
			continue
		}
		seen[s.Context] = true
		if (s.State == "failure" || s.State == "error") && len(out.Failed) < FeedbackChecks {
			out.Failed = append(out.Failed, FailedCheck{Name: s.Context, URL: s.TargetURL, Output: clip(s.Description, feedbackText)})
		}
	}
	return out, nil
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// PRFeedback reads the notes of a merge request (not the system ones) and the failed jobs of the last
// pipeline of sha with the end of their logs.
func (g *GitLab) PRFeedback(ctx context.Context, token string, number int, sha string) (*PRFeedback, error) {
	out := &PRFeedback{Comments: []PRComment{}, Failed: []FailedCheck{}}
	var notes []struct {
		Body      string    `json:"body"`
		System    bool      `json:"system"`
		CreatedAt time.Time `json:"created_at"`
		Author    struct {
			Username string `json:"username"`
		} `json:"author"`
		Position *struct {
			NewPath string `json:"new_path"`
			NewLine int    `json:"new_line"`
		} `json:"position"`
	}
	path := g.p("/merge_requests/" + strconv.Itoa(number) + "/notes?per_page=100&sort=desc&order_by=created_at")
	if _, err := g.api.call(ctx, "mr_notes", token, http.MethodGet, path, nil, &notes); err != nil {
		return nil, err
	}
	for _, n := range notes {
		if n.System || strings.TrimSpace(n.Body) == "" {
			continue
		}
		c := PRComment{Author: n.Author.Username, Body: clip(n.Body, feedbackText), At: n.CreatedAt}
		if n.Position != nil {
			c.Path, c.Line = n.Position.NewPath, n.Position.NewLine
		}
		out.Comments = append(out.Comments, c)
	}
	out.Comments = latest(out.Comments)
	if sha == "" {
		return out, nil
	}
	var commit struct {
		LastPipeline *struct {
			ID int64 `json:"id"`
		} `json:"last_pipeline"`
	}
	if _, err := g.api.call(ctx, "commit", token, http.MethodGet, g.p("/repository/commits/"+url.PathEscape(sha)), nil, &commit); err != nil {
		return nil, err
	}
	if commit.LastPipeline == nil {
		return out, nil
	}
	var jobs []struct {
		ID            int64  `json:"id"`
		Name          string `json:"name"`
		Stage         string `json:"stage"`
		WebURL        string `json:"web_url"`
		FailureReason string `json:"failure_reason"`
		AllowFailure  bool   `json:"allow_failure"`
	}
	jp := g.p("/pipelines/" + strconv.FormatInt(commit.LastPipeline.ID, 10) + "/jobs?scope[]=failed&per_page=100")
	if _, err := g.api.call(ctx, "pipeline_jobs", token, http.MethodGet, jp, nil, &jobs); err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if j.AllowFailure || len(out.Failed) >= FeedbackChecks {
			continue
		}
		fc := FailedCheck{Name: j.Stage + ": " + j.Name, URL: j.WebURL, Output: j.FailureReason}
		if raw, err := g.api.raw(ctx, "job_trace", token, g.p("/jobs/"+strconv.FormatInt(j.ID, 10)+"/trace")); err == nil { // as on GitHub
			fc.Log = logTail(raw)
		}
		out.Failed = append(out.Failed, fc)
	}
	return out, nil
}
