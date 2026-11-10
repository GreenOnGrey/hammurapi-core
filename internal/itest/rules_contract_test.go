//go:build integration

package itest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/GreenOnGrey/speclint/contract"
	"github.com/GreenOnGrey/speclint/starter"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/rules"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
)

// rulesRepo is a specification repository of branches for the rules: the
// default branch "main" and the branches of rule changes.
type rulesRepo struct {
	mu       sync.Mutex
	branches map[string]map[string]string
	prs      map[int]string
	commits  int
}

func (r *rulesRepo) provider(t *testing.T) *mocks.MockProvider {
	p := mocks.NewMockProvider(gomock.NewController(t))
	p.EXPECT().BotToken(gomock.Any()).Return("bot", nil).AnyTimes()
	p.EXPECT().GetFile(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _, ref, path string) (*git.File, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		c, ok := r.branches[ref][path]
		if !ok {
			return nil, git.ErrNotFound
		}
		return &git.File{Content: []byte(c), BlobSHA: git.BlobSHA([]byte(c))}, nil
	}).AnyTimes()
	p.EXPECT().BranchHead(gomock.Any(), gomock.Any(), gomock.Any()).Return("head", nil).AnyTimes()
	p.EXPECT().CreateBranch(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _, name, _ string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		b := map[string]string{}
		for k, v := range r.branches["main"] {
			b[k] = v
		}
		r.branches[name] = b
		return nil
	}).AnyTimes()
	p.EXPECT().Commit(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, branch, _ string, changes []git.FileChange) (string, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.branches[branch] == nil {
				r.branches[branch] = map[string]string{}
			}
			for _, c := range changes {
				r.branches[branch][c.Path] = string(c.Content)
			}
			r.commits++
			return "c", nil
		}).AnyTimes()
	p.EXPECT().CreatePR(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, head, _, _, _ string) (*git.PR, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			n := len(r.prs) + 1
			r.prs[n] = head
			return &git.PR{Number: n, URL: "http://pr"}, nil
		}).AnyTimes()
	p.EXPECT().ApprovePR(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	p.EXPECT().MergePR(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, n int, _ string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		for k, v := range r.branches[r.prs[n]] {
			r.branches["main"][k] = v
		}
		return nil
	}).AnyTimes()
	p.EXPECT().DeleteBranch(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	p.EXPECT().ClosePR(gomock.Any(), gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	return p
}

// FTR.HMR.DEV-0003 R18–R20, R24: the document contract of the rules.
func TestRulesContract(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "rulescontract")
	must(t, err)
	defer drop()
	db, err := postgres.Connect(ctx, url)
	must(t, err)
	defer db.Close()
	must(t, postgres.Migrate(ctx, url))

	// LANG-07: an empty repository gets the starter rules of the language of the documents, in one commit.
	repo := &rulesRepo{branches: map[string]map[string]string{"main": {}}, prs: map[int]string{}}
	gp := repo.provider(t)
	tokens := mocks.NewMockTokenSource(gomock.NewController(t))
	tokens.EXPECT().Token(gomock.Any(), gomock.Any()).Return("tok", nil).AnyTimes()
	svc := rules.NewService(db, gp, tokens, "main")
	svc.Lang = contract.EN
	must(t, svc.Start(ctx))
	if repo.commits != 1 || len(repo.branches["main"]) != len(starter.Files(contract.EN)) ||
		!strings.Contains(repo.branches["main"]["rules/product/template.md"], "**Depends on:**") {
		t.Fatalf("starter rules: %d commits, %d files", repo.commits, len(repo.branches["main"]))
	}
	// LANG-08: a repository with rules gets no commit.
	must(t, svc.Start(ctx))
	if repo.commits != 1 {
		t.Fatalf("a second seed: %d commits", repo.commits)
	}

	admin, other := tu.User("admin:product"), tu.User("admin:product")
	for _, u := range []*domain.Principal{admin, other} {
		must(t, db.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, agent_name, agent_tone)
			VALUES ($1, $1, 'Admin', 'Hammurapi', 'business') RETURNING id`, u.UserID.String()).Scan(&u.UserID))
	}
	product := repo.branches["main"]["rules/product/template.md"]
	sha := git.BlobSHA([]byte(product))

	// CTR-05: a template without the section of changes is refused, no branch and no PR.
	broken := strings.Replace(product, "## Changes to other features", "## Other", 1)
	_, err = svc.Propose(ctx, admin, domain.AreaProduct, rules.ProposeInput{File: rules.FileTemplate, Content: broken, BaseSHA: sha})
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Status != 422 || ae.Code != "rules_contract" || len(repo.prs) != 0 {
		t.Fatalf("propose broken %v, prs %d", err, len(repo.prs))
	}
	// CTR-02: a renamed free section is taken.
	c, err := svc.Propose(ctx, admin, domain.AreaProduct, rules.ProposeInput{File: rules.FileTemplate,
		Content: strings.Replace(product, "## Success metrics", "## Metrics", 1), BaseSHA: sha})
	must(t, err)

	// CTR-06: the branch of the change broke the contract since the proposal: the approval is refused.
	repo.branches[c.Branch]["rules/product/template.md"] = broken
	_, err = svc.Approve(ctx, other, c.ID)
	if !errors.As(err, &ae) || ae.Code != "rules_contract" || strings.Contains(repo.branches["main"]["rules/product/template.md"], "## Other") {
		t.Fatalf("approve broken %v", err)
	}

	// CTR-08, CTR-09: a commit in git past Hammurapi: the violation is recorded for the area, and leaves when fixed.
	repo.branches["main"]["rules/arch/template.md"] = strings.Replace(repo.branches["main"]["rules/arch/template.md"], "| A | Decision | Reason |", "| # | Decision | Reason |", 1)
	must(t, svc.RulesPushed(ctx, "main", []string{"rules/arch/template.md"}))
	got, err := svc.Violations(ctx, tu.User("admin:arch"))
	must(t, err)
	if len(got) != 1 || got[0].Area != domain.AreaArch || got[0].File != rules.FileTemplate || got[0].Violations[0].Element != contract.ElemDecisions {
		t.Fatalf("violations %+v", got)
	}
	if got, err := svc.Violations(ctx, tu.User("admin:qa")); err != nil || len(got) != 0 {
		t.Fatalf("the admin of qa sees %+v %v", got, err)
	}
	if got, err := svc.Violations(ctx, tu.User("global")); err != nil || len(got) != 1 {
		t.Fatalf("the global admin sees %+v %v", got, err)
	}
	must(t, svc.RulesPushed(ctx, "feature/x", []string{"rules/arch/template.md"})) // another branch: nothing
	repo.branches["main"]["rules/arch/template.md"] = strings.Replace(repo.branches["main"]["rules/arch/template.md"], "| # | Decision | Reason |", "| A | Decision | Reason |", 1)
	must(t, svc.RulesPushed(ctx, "main", []string{"rules/arch/template.md"}))
	if got, err := svc.Violations(ctx, tu.User("global")); err != nil || len(got) != 0 {
		t.Fatalf("after the fix %+v %v", got, err)
	}
	// A template deleted in git is a violation; rules/ deleted entirely is not seeded again by a push.
	delete(repo.branches["main"], "rules/qa/fix-template.md")
	must(t, svc.RulesPushed(ctx, "main", []string{"rules/"}))
	if got, err := svc.Violations(ctx, tu.User("global")); err != nil || len(got) != 1 || got[0].Violations[0].Element != "file" {
		t.Fatalf("a missing template %+v %v", got, err)
	}
	commits := repo.commits
	repo.branches["main"] = map[string]string{}
	must(t, svc.RulesPushed(ctx, "main", []string{"rules/product/template.md"}))
	if repo.commits != commits {
		t.Fatal("a push that deleted rules/ was undone by the bot")
	}

	// CTR-04: the description of the contract of arch.
	d, err := svc.Describe(tu.User("admin:arch"), domain.AreaArch)
	must(t, err)
	if d["lang"] != contract.EN || len(d["template"].([]contract.Element)) != 3 || len(d["fixTemplate"].([]contract.Element)) != 2 {
		t.Fatalf("describe %+v", d)
	}
}
