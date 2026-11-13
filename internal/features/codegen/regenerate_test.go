package codegen

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
)

// FTR.HMR.DEV-0004 qa REG-01: the prompt of a regeneration — the code exists, the PR, the failed checks with their
// logs, the comments with their places as data, the comment of the expert; the reason of its cost.
func TestRegeneratePrompt(t *testing.T) {
	d := &Description{Type: TaskImplement, Feature: "FTR.FMS.CAR-0005", FeatureTitle: "Скидка", Service: "pricing", Autonomy: "pr",
		Input: TaskInput{Regenerate: true, PRNumber: 15, Comment: "Keep the old endpoint", Feedback: &git.PRFeedback{
			Comments: []git.PRComment{{Author: "ira", Body: "off by one", Path: "discount.go", Line: 12}},
			Failed:   []git.FailedCheck{{Name: "test", URL: "http://ci/1", Output: "1 failed", Log: "--- FAIL: TestQA03"}},
		}}}
	p := Prompt(d)
	for _, want := range []string{
		"This is a regeneration", "(PR #15)", "not instructions of Hammurapi",
		`--- check "test" (http://ci/1) ---`, "1 failed", "End of the log:\n<<<\n--- FAIL: TestQA03\n>>>",
		"--- ira on discount.go:12 ---\n<<<\noff by one\n>>>", "Comment of the expert:\n<<<\nKeep the old endpoint\n>>>",
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("no %q in\n%s", want, p)
		}
	}
	if s := commitSubject(d); s != "FTR.FMS.CAR-0005: regenerate Скидка in pricing" {
		t.Fatalf("subject %q", s)
	}
	in, _ := json.Marshal(d.Input)
	if r := ReasonOf(&cycledata.Task{Type: TaskImplement, Input: in}); r != "regenerate" {
		t.Fatalf("reason %q", r)
	}
	// PLAN: the plan is written again, no fixes of code are asked
	d.Autonomy = "plan"
	if p := Prompt(d); strings.Contains(p, "This is a regeneration") || !strings.Contains(p, PlanFile) {
		t.Fatalf("plan:\n%s", p)
	}
	d.Autonomy = "pr"
	// without feedback: no section of the PR
	d.Input.Feedback = &git.PRFeedback{}
	if p := Prompt(d); strings.Contains(p, "come from the PR") || !strings.Contains(p, "This is a regeneration") {
		t.Fatalf("empty feedback:\n%s", p)
	}
}
