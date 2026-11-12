package codegen

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	gmocks "github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
)

func commitDescription() *Description {
	return &Description{Type: TaskImplement, Branch: "hammurapi/FTR.FMS.CAR-0005/booking", Feature: "FTR.FMS.CAR-0005",
		FeatureTitle: "Usage", Service: "booking", Autonomy: domain.AutonomyPR}
}

func noPauses(t *testing.T) {
	t.Helper()
	old := newBranchRetries
	newBranchRetries = []time.Duration{0, 0, 0}
	t.Cleanup(func() { newBranchRetries = old })
}

// FTR.HMR.DEV-0004 qa GIT-01, PRT-01: a 404 right after the branch is created is retried, and the PR keeps the title of Hammurapi.
func TestCommitRetriesNotFoundOnNewBranch(t *testing.T) {
	noPauses(t)
	ctx := context.Background()
	d := commitDescription()
	p := gmocks.NewMockProvider(gomock.NewController(t))
	changes := []git.FileChange{{Path: "a.go", Content: []byte("package a")}}
	gomock.InOrder(
		p.EXPECT().DefaultBranch(ctx, "tok").Return("main", nil),
		p.EXPECT().BranchHead(ctx, "tok", d.Branch).Return("", git.ErrNotFound),
		p.EXPECT().BranchHead(ctx, "tok", "main").Return("base", nil),
		p.EXPECT().CreateBranch(ctx, "tok", d.Branch, "base").Return(nil),
		p.EXPECT().Commit(ctx, "tok", d.Branch, gomock.Any(), changes).Return("", git.ErrNotFound),
		p.EXPECT().Commit(ctx, "tok", d.Branch, gomock.Any(), changes).Return("c1", nil),
		p.EXPECT().CreatePR(ctx, "tok", d.Branch, "main", "FTR.FMS.CAR-0005 booking: Usage", gomock.Any()).
			Return(&git.PR{Number: 7, URL: "u"}, nil),
	)
	res := commit(ctx, p, "tok", d, Result{Summary: "I'll start by exploring the repository"}, changes, "done")
	if res.Status != "succeeded" || res.HeadSHA != "c1" || res.PRNumber != 7 {
		t.Fatalf("result: %+v", res)
	}
	if res.PRTitle != "FTR.FMS.CAR-0005 booking: Usage" {
		t.Fatalf("PR title: %q", res.PRTitle)
	}
	// FTR.HMR.DEV-0004 qa LOG-03: the steps of Hammurapi for the log, the head of the branch for a retry
	if got := stepsOf(res); got != "branch:ok commit:ok pr:ok" || res.BranchHead != "c1" {
		t.Fatalf("steps %s, head %s", got, res.BranchHead)
	}
}

func stepsOf(r Result) string {
	var b []string
	for _, s := range r.Steps {
		b = append(b, s.Step+":"+s.Status)
	}
	return strings.Join(b, " ")
}

// FTR.HMR.DEV-0004 qa GIT-02: on an existing branch a 404 is not a lag; the error names the branch and the answer.
func TestCommitNotFoundOnExistingBranch(t *testing.T) {
	noPauses(t)
	ctx := context.Background()
	d := commitDescription()
	p := gmocks.NewMockProvider(gomock.NewController(t))
	changes := []git.FileChange{{Path: "a.go", Content: []byte("package a")}}
	gomock.InOrder(
		p.EXPECT().DefaultBranch(ctx, "tok").Return("main", nil),
		p.EXPECT().BranchHead(ctx, "tok", d.Branch).Return("head", nil),
		p.EXPECT().Commit(ctx, "tok", d.Branch, gomock.Any(), changes).Return("", errors.Join(errors.New("create the tree"), git.ErrNotFound)),
	)
	res := commit(ctx, p, "tok", d, Result{}, changes, "done")
	if res.Status != "failed" || res.ErrorClass != ClassGit {
		t.Fatalf("result: %+v", res)
	}
	for _, want := range []string{"commit to " + d.Branch, "create the tree", "HTTP 404"} {
		if !strings.Contains(res.Error, want) {
			t.Fatalf("error %q lacks %q", res.Error, want)
		}
	}
	if strings.Contains(res.Error, "git: not found") {
		t.Fatalf("error reads as a missing binary: %q", res.Error)
	}
	if got := stepsOf(res); got != "branch:ok commit:failed" || res.BranchHead != "head" || !strings.Contains(res.Steps[1].Detail, "create the tree") {
		t.Fatalf("LOG-03 steps %s, head %s", got, res.BranchHead)
	}
}

// FTR.HMR.SPEC-0005 qa TCS-09: the cases of the service are to write, the cases of other services are for reference.
func TestPromptSplitsTestCasesByService(t *testing.T) {
	d := commitDescription()
	d.Service = "nabu-web"
	d.TestCases = []cycledata.TestCase{{ID: "TCS-10", Level: "E", ReqIDs: []string{"R2"}, Title: "Screen"},
		{ID: "CTR-01", Level: "I", ReqIDs: []string{"R3"}, Title: "Contract", Services: []string{"nabu-core", "nabu-web"}}}
	d.OtherTestCases = []cycledata.TestCase{{ID: "MIG-01", Level: "I", ReqIDs: []string{"R1"}, Title: "Migration", Services: []string{"nabu-core"}}}
	p := Prompt(d)
	mine := strings.Index(p, "Test cases — put the ID")
	other := strings.Index(p, "Test cases of other services, for reference only")
	if mine < 0 || other < mine {
		t.Fatalf("blocks: mine %d, other %d", mine, other)
	}
	if strings.Contains(p[mine:other], "MIG-01") || !strings.Contains(p[mine:other], "CTR-01") || !strings.Contains(p[other:], "MIG-01 [nabu-core]") {
		t.Fatalf("prompt:\n%s", p[mine:])
	}
}
