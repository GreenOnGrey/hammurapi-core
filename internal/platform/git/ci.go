package git

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// The CI status of a commit from the checks of the provider (FTR.HMR.DEV-0004 R17, tech §13).
const (
	CISuccess = "success"
	CIFailure = "failure"
	CIPending = "pending"
	CINone    = "none" // the commit has no checks
)

// CIEvent is a check of a commit that changed: the cue to ask the provider for the
// status of all the checks of the commit, not the status itself (arch A14).
type CIEvent struct {
	EventID string `json:"eventId"`
	Repo    string `json:"repo"`
	SHA     string `json:"sha"`
}

// ciTally sums the checks of a commit: any failure fails it, else any running
// check keeps it pending, else any success makes it green; nothing — none.
type ciTally struct{ success, failure, pending bool }

func (t *ciTally) add(status string) {
	switch status {
	case CISuccess:
		t.success = true
	case CIFailure:
		t.failure = true
	case CIPending:
		t.pending = true
	}
}

func (t ciTally) status() string {
	switch {
	case t.failure:
		return CIFailure
	case t.pending:
		return CIPending
	case t.success:
		return CISuccess
	}
	return CINone
}

// githubCheckRun maps one check run of GitHub; "" — not counted (stale).
func githubCheckRun(status, conclusion string) string {
	if status != "completed" {
		return CIPending
	}
	switch conclusion {
	case "success", "neutral", "skipped":
		return CISuccess
	case "stale":
		return "" // GitHub marks so a check that did not finish in 14 days (tech decision 9)
	}
	return CIFailure // failure, timed_out, cancelled, action_required, startup_failure
}

// githubCombined maps the combined commit status of GitHub; "" — no statuses.
func githubCombined(state string, total int) string {
	if total == 0 {
		return ""
	}
	switch state {
	case "success":
		return CISuccess
	case "failure", "error":
		return CIFailure
	}
	return CIPending
}

// checkRunPages bounds the pages of check runs read for one commit (tech decision 8).
const checkRunPages = 5

// CommitCI is the status of the checks of a commit: its check runs (the latest of every name, up to
// five pages of 100) and its commit statuses (tech §13).
func (g *GitHub) CommitCI(ctx context.Context, token, sha string) (string, error) {
	var t ciTally
	for page := 1; page <= checkRunPages; page++ {
		var runs struct {
			Total     int `json:"total_count"`
			CheckRuns []struct {
				Status     string `json:"status"`
				Conclusion string `json:"conclusion"`
			} `json:"check_runs"`
		}
		path := g.r("/commits/" + url.PathEscape(sha) + "/check-runs?per_page=100&page=" + strconv.Itoa(page))
		if _, err := g.api.call(ctx, "check_runs", token, http.MethodGet, path, nil, &runs); err != nil {
			return "", err
		}
		for _, r := range runs.CheckRuns {
			t.add(githubCheckRun(r.Status, r.Conclusion))
		}
		if len(runs.CheckRuns) < 100 || page*100 >= runs.Total {
			break
		}
	}
	var combined struct {
		State      string `json:"state"`
		TotalCount int    `json:"total_count"`
	}
	if _, err := g.api.call(ctx, "commit_status", token, http.MethodGet, g.r("/commits/"+url.PathEscape(sha)+"/status"), nil, &combined); err != nil {
		return "", err
	}
	t.add(githubCombined(combined.State, combined.TotalCount))
	return t.status(), nil
}

// gitlabPipeline maps the status of the last pipeline of a commit; "" — no pipeline.
func gitlabPipeline(status string) string {
	switch status {
	case "":
		return CINone
	case "success", "skipped":
		return CISuccess
	case "failed", "canceled":
		return CIFailure
	}
	return CIPending // created, waiting_for_resource, preparing, pending, running, manual, scheduled
}

// CommitCI is the status of the last pipeline of a commit (tech §13).
func (g *GitLab) CommitCI(ctx context.Context, token, sha string) (string, error) {
	var c struct {
		LastPipeline *struct {
			Status string `json:"status"`
		} `json:"last_pipeline"`
	}
	if _, err := g.api.call(ctx, "commit", token, http.MethodGet, g.p("/repository/commits/"+url.PathEscape(sha)), nil, &c); err != nil {
		return "", err
	}
	if c.LastPipeline == nil {
		return CINone, nil
	}
	return gitlabPipeline(c.LastPipeline.Status), nil
}
