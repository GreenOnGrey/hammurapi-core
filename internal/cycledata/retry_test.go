package cycledata

import (
	"testing"
	"time"
)

// FTR.HMR.DEV-0004 qa RTC-03, RTC-04: what «Повторить» of a task does.
func TestTaskRetryOf(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	s := func(v string) *string { return &v }
	later, earlier := now.Add(time.Hour), now.Add(-time.Hour)
	cases := []struct {
		name string
		t    Task
		want string
	}{
		{"commit failed, changes kept", Task{Status: "failed", ErrorClass: s("git"), ChangesKey: s("k"), ChangesUntil: &later}, "commit"},
		{"changes past their term", Task{Status: "failed", ErrorClass: s("git"), ChangesKey: s("k"), ChangesUntil: &earlier}, "agent"},
		{"no changes kept", Task{Status: "failed", ErrorClass: s("git")}, "agent"},
		{"the agent failed", Task{Status: "failed", ErrorClass: s("limit"), ChangesKey: s("k"), ChangesUntil: &later}, "agent"},
		{"the branch moved", Task{Status: "failed", ErrorClass: s("branch_moved"), ChangesKey: s("k"), ChangesUntil: &later}, "rerun"},
		{"succeeded", Task{Status: "succeeded", ErrorClass: s("git"), ChangesKey: s("k"), ChangesUntil: &later}, "agent"},
	}
	for _, c := range cases {
		if got := c.t.RetryOf(now); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}
