package codegen

import (
	"strings"
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
)

// CHK-37: existing tests and CI files, not new or ordinary ones.
func TestCHK37ProtectedPaths(t *testing.T) {
	for p, want := range map[string]bool{
		"foo_test.go": true, "a/b/test_x.py": true, "a/x_test.py": true, "src/a.test.ts": true, "src/a.spec.js": true,
		"tests/data.json": true, "pkg/__tests__/x.js": true, "spec/a.rb": true, "Makefile": true, "cmd/Makefile": true,
		".github/workflows/ci.yml": true, ".gitlab-ci.yml": true, ".gitlab/x.yml": true, ".circleci/config.yml": true, "Jenkinsfile": true, "azure-pipelines.yml": true,
		"foo.go": false, "latest/a.go": false, "contest/b.go": false, "attest.py": false, "README.md": false,
	} {
		if got := protectedPath(p); got != want {
			t.Errorf("%s: %v", p, got)
		}
	}
}

// CHK-05, CHK-31: a task without a command is unverified; a plan has no check.
func TestCHK05CheckOf(t *testing.T) {
	if c := checkOf(RunFinished{}, &Description{Autonomy: domain.AutonomyPR}); c.Status != CheckUnverified {
		t.Fatalf("%+v", c)
	}
	if c := checkOf(RunFinished{}, &Description{Autonomy: domain.AutonomyPlan}); c != nil {
		t.Fatalf("%+v", c)
	}
	in := &nabu.CheckResult{Status: CheckPassed}
	if c := checkOf(RunFinished{Check: in}, &Description{Autonomy: domain.AutonomyPR}); c != in {
		t.Fatalf("%+v", c)
	}
}

// CHK-34, CHK-36, CHK-41: what needs a person.
func TestCHK34NeedsReview(t *testing.T) {
	for _, c := range []struct {
		status  string
		stalled bool
		want    bool
	}{
		{CheckFailed, false, true}, {CheckUnavailable, false, true}, {CheckPassed, false, false}, {CheckPassed, true, false},
		{CheckNotRun, false, false}, {CheckUnverified, false, false}, {CheckUnverified, true, true},
	} {
		r := Result{Check: &nabu.CheckResult{Status: c.status}, Stalled: c.stalled}
		if r.needsReview() != c.want {
			t.Errorf("%+v", c)
		}
	}
	if (Result{}).needsReview() {
		t.Error("no check")
	}
}

// CHK-32, CHK-33: the warning of a PR comes first and names the command.
func TestCHK32PRWarning(t *testing.T) {
	r := Result{Check: &nabu.CheckResult{Status: CheckFailed, Command: "npm test", Attempts: 3, OutputTail: "FAIL a\n```x"},
		Warnings: []string{"left out by the sandbox (too_large): x", warningProtected + "a_test.go", warningStalled}}
	body := prBody(&Description{Feature: "F", FeatureTitle: "T"}, r)
	if !strings.HasPrefix(body, "⚠️ The readiness check did not pass.") {
		t.Fatal(body)
	}
	for _, want := range []string{"`npm test`", "attempts: 3", "FAIL a", warningProtected + "a_test.go", warningStalled} {
		if !strings.Contains(body, want) {
			t.Fatalf("%q is not in:\n%s", want, body)
		}
	}
	if strings.Contains(checkWarnings(r), "too_large") || strings.Contains(checkWarnings(r), "```x") {
		t.Fatal(checkWarnings(r))
	}
	if w := checkWarnings(Result{Check: &nabu.CheckResult{Status: CheckUnavailable, Command: "make"}}); !strings.Contains(w, "could not run") {
		t.Fatal(w)
	}
	if w := checkWarnings(Result{Check: &nabu.CheckResult{Status: CheckPassed}}); w != "" {
		t.Fatal(w)
	}
}
