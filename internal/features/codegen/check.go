package codegen

import (
	"fmt"
	"path"
	"strings"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
)

// States of the check in a task: those of Nabu and, in Hammurapi only, a
// service without a command (FTR.HMR.CMN-0008 R4, tech §7).
const (
	CheckPassed      = "passed"
	CheckFailed      = "failed"
	CheckUnavailable = "unavailable"
	CheckNotRun      = "not_run"
	CheckUnverified  = "unverified"

	// StopStalled is the stop reason of Nabu (FTR.NAB.CMN-0010 R6).
	StopStalled = "stalled"

	warningStalled   = "The agent was stopped: it kept repeating the same action with no result."
	warningProtected = "The agent changed existing tests or CI files: "
)

// checkOf is the check of a finished run: nil where a task has none (R18).
func checkOf(f RunFinished, d *Description) *nabu.CheckResult {
	if d.Autonomy == domain.AutonomyPlan {
		return nil
	}
	if f.Check == nil {
		return &nabu.CheckResult{Status: CheckUnverified}
	}
	return f.Check
}

// needsReview tells that nothing proves the code, so a person has to look at
// it even on the autonomous level (R16, R20).
func (r Result) needsReview() bool {
	if r.Check == nil {
		return false
	}
	switch r.Check.Status {
	case CheckFailed, CheckUnavailable:
		return true
	case CheckUnverified:
		return r.Stalled
	}
	return false
}

// checkWarnings is the warning of a PR: what did not pass, what was changed in
// the tests, why the agent stopped (R15, R17, R20). Empty when all is well.
func checkWarnings(r Result) string {
	var parts []string
	if c := r.Check; c != nil && (c.Status == CheckFailed || c.Status == CheckUnavailable) {
		head := "⚠️ The readiness check did not pass."
		if c.Status == CheckUnavailable {
			head = "⚠️ The readiness check could not run."
		}
		b := fmt.Sprintf("%s\nCommand: `%s` · attempts: %d", head, c.Command, c.Attempts)
		if c.OutputTail != "" {
			b += "\n<details><summary>The end of the output</summary>\n\n```\n" + strings.ReplaceAll(c.OutputTail, "```", "'''") + "\n```\n</details>"
		}
		parts = append(parts, b)
	}
	for _, w := range r.Warnings {
		if strings.HasPrefix(w, warningProtected) || w == warningStalled {
			parts = append(parts, "⚠️ "+w)
		}
	}
	return strings.Join(parts, "\n\n")
}

// protectedPath tells a file whose change may weaken the checks (R17).
func protectedPath(p string) bool {
	base := path.Base(p)
	for _, suffix := range []string{"_test.go", "_test.py"} {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	if strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") {
		return true
	}
	if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true
	}
	switch base {
	case "Makefile", ".gitlab-ci.yml", "Jenkinsfile", "azure-pipelines.yml":
		return true
	}
	for _, dir := range strings.Split(path.Dir(p), "/") {
		switch dir {
		case "test", "tests", "__tests__", "spec", ".github", ".gitlab", ".circleci":
			return true
		}
	}
	return false
}
