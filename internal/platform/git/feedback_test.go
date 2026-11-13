package git

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, routes map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for suffix, body := range routes {
			if strings.HasSuffix(r.URL.Path, suffix) {
				if s, ok := body.(string); ok {
					_, _ = w.Write([]byte(s))
					return
				}
				if code, ok := body.(int); ok {
					w.WriteHeader(code)
					return
				}
				_ = json.NewEncoder(w).Encode(body)
				return
			}
		}
		http.NotFound(w, r)
	}))
}

// FTR.HMR.DEV-0004 qa REG-02: the feedback of a GitHub PR — comments of every kind in their order, the failed
// checks with the end of the log of an Actions job, a failed commit status; a log without permission is skipped.
func TestGitHubPRFeedback(t *testing.T) {
	log := strings.Repeat("ok line\n", 200) + "--- FAIL: TestQA03_Discount (0.01s)\n    discount_test.go:12: got 5, want 10\n"
	srv := serve(t, map[string]any{
		"/issues/7/comments": []map[string]any{{"user": map[string]any{"login": "ira"}, "body": "Please rename the handler", "created_at": "2026-11-10T10:00:00Z"}},
		"/pulls/7/reviews": []map[string]any{
			{"user": map[string]any{"login": "max"}, "body": "", "state": "APPROVED", "submitted_at": "2026-11-10T09:00:00Z"},
			{"user": map[string]any{"login": "max"}, "body": "The discount is wrong", "state": "CHANGES_REQUESTED", "submitted_at": "2026-11-10T11:00:00Z"}},
		"/pulls/7/comments": []map[string]any{{"user": map[string]any{"login": "max"}, "body": "off by one", "path": "discount.go", "line": 12, "created_at": "2026-11-10T08:00:00Z"}},
		"/commits/abc/check-runs": map[string]any{"check_runs": []map[string]any{
			{"id": 11, "name": "test", "status": "completed", "conclusion": "failure", "html_url": "http://ci/11", "app": map[string]any{"slug": "github-actions"},
				"output": map[string]any{"title": "1 failed", "summary": "", "text": ""}},
			{"id": 12, "name": "lint", "status": "completed", "conclusion": "success", "app": map[string]any{"slug": "github-actions"}},
			{"id": 13, "name": "e2e", "status": "completed", "conclusion": "timed_out", "app": map[string]any{"slug": "github-actions"}}}},
		"/actions/jobs/11/logs": log,
		"/actions/jobs/13/logs": http.StatusForbidden,
		"/commits/abc/statuses": []map[string]any{
			{"context": "deploy", "state": "failure", "description": "preview failed", "target_url": "http://d"},
			{"context": "deploy", "state": "success"},
			{"context": "cov", "state": "success"}},
	})
	defer srv.Close()
	fb, err := NewGitHub(srv.URL, srv.URL, "o/r", "", "").PRFeedback(context.Background(), "tok", 7, "abc")
	if err != nil {
		t.Fatal(err)
	}
	var who []string
	for _, c := range fb.Comments {
		who = append(who, c.Author+":"+c.Body)
	}
	if got := strings.Join(who, " | "); got != "max:off by one | ira:Please rename the handler | max:Changes requested. The discount is wrong" {
		t.Fatalf("comments %q", got)
	}
	if fb.Comments[0].Path != "discount.go" || fb.Comments[0].Line != 12 {
		t.Fatalf("line comment %+v", fb.Comments[0])
	}
	if len(fb.Failed) != 3 || fb.Failed[0].Name != "test" || fb.Failed[1].Name != "e2e" || fb.Failed[2].Name != "deploy" {
		t.Fatalf("failed %+v", fb.Failed)
	}
	if l := fb.Failed[0].Log; !strings.HasSuffix(l, "got 5, want 10") || strings.Count(l, "\n") >= feedbackLogLines || fb.Failed[0].Output != "1 failed" {
		t.Fatalf("the end of the log %q", l)
	}
	if fb.Failed[1].Log != "" {
		t.Fatalf("a log without permission %q", fb.Failed[1].Log)
	}
	// without a head only the comments are read
	if fb, err := NewGitHub(srv.URL, srv.URL, "o/r", "", "").PRFeedback(context.Background(), "tok", 7, ""); err != nil || len(fb.Failed) != 0 || len(fb.Comments) != 3 {
		t.Fatalf("no sha: %+v %v", fb, err)
	}
}

// REG-03: the feedback of a GitLab MR — notes without the system ones, the failed jobs of the last pipeline.
func TestGitLabPRFeedback(t *testing.T) {
	srv := serve(t, map[string]any{
		"/merge_requests/4/notes": []map[string]any{
			{"body": "added 1 commit", "system": true, "created_at": "2026-11-10T12:00:00Z", "author": map[string]any{"username": "bot"}},
			{"body": "use the new API", "system": false, "created_at": "2026-11-10T11:00:00Z", "author": map[string]any{"username": "ira"},
				"position": map[string]any{"new_path": "api.go", "new_line": 3}}},
		"/repository/commits/abc": map[string]any{"last_pipeline": map[string]any{"id": 90}},
		"/pipelines/90/jobs": []map[string]any{
			{"id": 1, "name": "unit", "stage": "test", "web_url": "http://j/1", "failure_reason": "script_failure"},
			{"id": 2, "name": "flaky", "stage": "test", "allow_failure": true}},
		"/jobs/1/trace": "FAIL api_test.go\n",
	})
	defer srv.Close()
	fb, err := NewGitLab(srv.URL, srv.URL, "g/r", "", "").PRFeedback(context.Background(), "tok", 4, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(fb.Comments) != 1 || fb.Comments[0].Author != "ira" || fb.Comments[0].Path != "api.go" || fb.Comments[0].Line != 3 {
		t.Fatalf("notes %+v", fb.Comments)
	}
	if len(fb.Failed) != 1 || fb.Failed[0].Name != "test: unit" || fb.Failed[0].Log != "FAIL api_test.go" || fb.Failed[0].Output != "script_failure" {
		t.Fatalf("jobs %+v", fb.Failed)
	}
}

func TestClip(t *testing.T) {
	if got := clip("привет мир", 7); got != "при …" {
		t.Fatalf("%q", got)
	}
}
