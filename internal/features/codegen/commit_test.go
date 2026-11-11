package codegen

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

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
}
