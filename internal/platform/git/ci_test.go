package git

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type check struct{ Status, Conclusion string }

// FTR.HMR.DEV-0004 qa PCI-01: the status of a commit from its check runs and commit statuses on GitHub.
func TestGitHubCommitCI(t *testing.T) {
	ok := check{"completed", "success"}
	cases := []struct {
		name     string
		runs     []check
		state    string
		total    int
		expected string
	}{
		{"all green", []check{ok, ok, {"completed", "skipped"}}, "pending", 0, CISuccess},
		{"one of four failed", []check{ok, ok, ok, {"completed", "failure"}}, "pending", 0, CIFailure},
		{"one running", []check{ok, {"in_progress", ""}}, "pending", 0, CIPending},
		{"a stale check is not counted", []check{ok, {"completed", "stale"}}, "pending", 0, CISuccess},
		{"a red commit status over green checks", []check{ok}, "failure", 1, CIFailure},
		{"a pending commit status", nil, "pending", 2, CIPending},
		{"no checks", nil, "pending", 0, CINone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/repos/o/r/commits/abc/check-runs"):
					var runs []map[string]any
					for _, x := range c.runs {
						runs = append(runs, map[string]any{"status": x.Status, "conclusion": x.Conclusion})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "check_runs": runs})
				case strings.HasSuffix(r.URL.Path, "/repos/o/r/commits/abc/status"):
					_ = json.NewEncoder(w).Encode(map[string]any{"state": c.state, "total_count": c.total})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			got, err := NewGitHub(srv.URL, srv.URL, "o/r", "", "").CommitCI(context.Background(), "tok", "abc")
			if err != nil || got != c.expected {
				t.Fatalf("got %q %v, want %q", got, err, c.expected)
			}
		})
	}
}

// PCI-02: the status of the last pipeline of a commit on GitLab.
func TestGitLabCommitCI(t *testing.T) {
	for pipeline, want := range map[string]string{"success": CISuccess, "failed": CIFailure, "running": CIPending, "manual": CIPending, "": CINone} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/repository/commits/abc") {
				http.NotFound(w, r)
				return
			}
			body := map[string]any{"id": "abc", "last_pipeline": nil}
			if pipeline != "" {
				body["last_pipeline"] = map[string]any{"status": pipeline}
			}
			_ = json.NewEncoder(w).Encode(body)
		}))
		got, err := NewGitLab(srv.URL, srv.URL, "o/r", "", "").CommitCI(context.Background(), "tok", "abc")
		srv.Close()
		if err != nil || got != want {
			t.Fatalf("pipeline %q: got %q %v, want %q", pipeline, got, err, want)
		}
	}
}

// PCI-03: check events of GitHub and pipelines of GitLab become the event ci of the commit.
func TestParseCheckHooks(t *testing.T) {
	gh := NewGitHub("", "", "o/r", "", "")
	hook := func(p Provider, header, event, body string) *HookEvent {
		t.Helper()
		h := http.Header{}
		h.Set(header, event)
		ev, err := p.ParseHook(h, []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		return ev
	}
	repo := `"repository":{"full_name":"o/svc"}`
	for name, c := range map[string]struct{ event, body, sha string }{
		"check suite completed": {"check_suite", `{"action":"completed",` + repo + `,"check_suite":{"head_sha":"s1"}}`, "s1"},
		"check run completed":   {"check_run", `{"action":"completed",` + repo + `,"check_run":{"head_sha":"s2"}}`, ""},
		"check suite requested": {"check_suite", `{"action":"requested",` + repo + `,"check_suite":{"head_sha":"s3"}}`, ""},
		"commit status":         {"status", `{"state":"success","sha":"s4",` + repo + `}`, "s4"},
	} {
		ev := hook(gh, "X-GitHub-Event", c.event, c.body)
		if c.sha == "" {
			if ev != nil {
				t.Fatalf("%s: %+v", name, ev)
			}
			continue
		}
		if ev == nil || ev.Kind != "ci" || ev.CI.SHA != c.sha || ev.CI.Repo != "o/svc" {
			t.Fatalf("%s: %+v", name, ev)
		}
	}
	gl := NewGitLab("", "", "o/r", "", "")
	ev := hook(gl, "X-Gitlab-Event", "Pipeline Hook", `{"project":{"path_with_namespace":"g/svc"},"object_attributes":{"sha":"s5","status":"success"}}`)
	if ev == nil || ev.Kind != "ci" || ev.CI.SHA != "s5" || ev.CI.Repo != "g/svc" {
		t.Fatalf("gitlab pipeline: %+v", ev)
	}
}
