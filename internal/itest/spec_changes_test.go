//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GreenOnGrey/speclint/contract"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/approvals"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gategen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gates"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/releases"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specchanges"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
)

// branchRepo is a specification repository with the default branch and the
// branches of features, for the mock provider: commits, the PR of a branch,
// "update branch" with conflicts and the merge (qa §1: a conflict on demand).
type branchRepo struct {
	mu       sync.Mutex
	main     map[string]string
	branches map[string]map[string]string
	base     map[string]map[string]string // the default branch at the last update of the branch
	prs      map[int]string               // PR number → branch
	merged   map[int]bool
	commits  []repoCommit
	mainVer  int            // changes of the default branch
	baseVer  map[string]int // the version of the default branch a branch was updated with
}

type repoCommit struct {
	branch, msg string
	paths       []string
}

func newBranchRepo(main map[string]string) *branchRepo {
	return &branchRepo{main: maps.Clone(main), branches: map[string]map[string]string{}, base: map[string]map[string]string{},
		prs: map[int]string{}, merged: map[int]bool{}, baseVer: map[string]int{}}
}

func (r *branchRepo) branch(name string, pr int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.branches[name], r.base[name], r.prs[pr], r.baseVer[name] = maps.Clone(r.main), maps.Clone(r.main), name, r.mainVer
}

func (r *branchRepo) files(ref string) map[string]string {
	if ref == "main" {
		return r.main
	}
	return r.branches[ref]
}

// conflicts are the paths changed differently in the branch and in the default branch since the update.
func (r *branchRepo) conflicts(name string) []string {
	var out []string
	b, o := r.branches[name], r.base[name]
	for _, p := range unionKeys(b, o, r.main) {
		if r.main[p] != o[p] && b[p] != o[p] && r.main[p] != b[p] {
			out = append(out, p)
		}
	}
	return out
}

func unionKeys(ms ...map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range ms {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	return out
}

func (r *branchRepo) commitsTo(branch string) []repoCommit {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []repoCommit
	for _, c := range r.commits {
		if c.branch == branch {
			out = append(out, c)
		}
	}
	return out
}

func (r *branchRepo) file(ref, path string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.files(ref)[path]
}

func (r *branchRepo) set(ref, path, content string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files(ref)[path] = content
	if ref == "main" {
		r.mainVer++
	}
}

func (r *branchRepo) provider(t *testing.T) *mocks.MockProvider {
	p := mocks.NewMockProvider(gomock.NewController(t))
	p.EXPECT().GetFile(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _, ref, path string) (*git.File, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		c, ok := r.files(ref)[path]
		if !ok {
			return nil, git.ErrNotFound
		}
		return &git.File{Content: []byte(c), BlobSHA: git.BlobSHA([]byte(c))}, nil
	}).AnyTimes()
	p.EXPECT().Commit(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _, branch, msg string, changes []git.FileChange) (string, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			c := repoCommit{branch: branch, msg: msg}
			for _, ch := range changes {
				if ch.Delete {
					delete(r.files(branch), ch.Path)
				} else {
					r.files(branch)[ch.Path] = string(ch.Content)
				}
				c.paths = append(c.paths, ch.Path)
			}
			r.commits = append(r.commits, c)
			return fmt.Sprintf("c%d", len(r.commits)), nil
		}).AnyTimes()
	p.EXPECT().PRChangedFiles(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, n int) ([]git.ChangedFile, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		name := r.prs[n]
		var out []git.ChangedFile
		b, o := r.branches[name], r.base[name]
		for _, p := range unionKeys(b, o) {
			switch {
			case b[p] == o[p]:
			case o[p] == "":
				out = append(out, git.ChangedFile{Path: p, Status: "added"})
			case b[p] == "":
				out = append(out, git.ChangedFile{Path: p, Status: "removed"})
			default:
				out = append(out, git.ChangedFile{Path: p, Status: "modified"})
			}
		}
		return out, nil
	}).AnyTimes()
	p.EXPECT().UpdatePRBranch(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, n int) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		name := r.prs[n]
		if len(r.conflicts(name)) > 0 {
			return &git.APIError{Status: 422, Message: "merge conflict between base and head"}
		}
		b, o := r.branches[name], r.base[name]
		for _, p := range unionKeys(r.main, o) {
			if r.main[p] == o[p] {
				continue
			}
			if c, ok := r.main[p]; ok {
				b[p] = c
			} else {
				delete(b, p)
			}
		}
		r.base[name], r.baseVer[name] = maps.Clone(r.main), r.mainVer
		return nil
	}).AnyTimes()
	p.EXPECT().BranchHead(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _, branch string) (string, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if branch == "main" {
			return fmt.Sprintf("main@%d", r.mainVer), nil
		}
		return fmt.Sprintf("main@%d", r.baseVer[branch]), nil // the head of a branch contains the default branch it was updated with
	}).AnyTimes()
	p.EXPECT().IsAncestor(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _, a, d string) (bool, error) {
		return a == d, nil
	}).AnyTimes()
	p.EXPECT().GetPR(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, n int) (*git.PRInfo, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.merged[n] {
			return &git.PRInfo{Number: n, State: "merged", MergeSHA: "m1"}, nil
		}
		ok := len(r.conflicts(r.prs[n])) == 0
		return &git.PRInfo{Number: n, State: "open", Mergeable: &ok}, nil
	}).AnyTimes()
	p.EXPECT().MergePR(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, n int, _ string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		name := r.prs[n]
		if len(r.conflicts(name)) > 0 {
			return git.ErrConflict
		}
		b, o := r.branches[name], r.base[name]
		for _, p := range unionKeys(b, o) {
			if b[p] == o[p] {
				continue
			}
			if c, ok := b[p]; ok {
				r.main[p] = c
			} else {
				delete(r.main, p)
			}
		}
		r.merged[n] = true
		r.mainVer++
		return nil
	}).AnyTimes()
	p.EXPECT().LatestCommit(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return("head", nil).AnyTimes()
	return p
}

const (
	pathBooking = "specs/FMS/CAR/FTR.FMS.CAR-0002/product/spec.md"
	pathCatalog = "specs/FMS/CAR/FTR.FMS.CAR-0001/product/spec.md"
	pathTariffs = "specs/FMS/CAR/FTR.FMS.CAR-0042/product/spec.md"
	booking     = "# Бронь\n\n**Версия:** 1.0\n**Решение:** `FTR.FMS.CAR-0002`\n\n---\n\n## 4. Требования\n\n" +
		"**R5.** Бронь на сутки.\n- Дано бронь, когда сутки прошли, тогда бронь снята.\n\n" +
		"**R13.** Отмена брони бесплатна.\n- Дано бронь, когда отмена через 10 минут, тогда бесплатно.\n"
	catalog = "# Каталог\n\n**Версия:** 1.0\n\n---\n\n## 4. Требования\n\n**R1.** Каталог машин.\n"
)

// tariffs is the product document of FTR.FMS.CAR-0042 with rows of the section.
func tariffs(rows ...string) string {
	return "# Тарифы выходного дня\n\n**Опирается на:** `FTR.FMS.CAR-0002`\n\n---\n\n## 4. Требования\n\n" +
		"**R4.** Отмена платная после 10 минут, см. `FTR.FMS.CAR-0002` R13.\n- Дано бронь, когда отмена через 20 минут, тогда платно.\n\n" +
		"## Изменения в других решениях\n\n| Решение | Требование | Что изменилось |\n|---|---|---|\n" + strings.Join(rows, "\n") + "\n"
}

const (
	rowR13     = "| `FTR.FMS.CAR-0002` | R13 | Отмена платная после 10 минут. |"
	rowR5      = "| `FTR.FMS.CAR-0002` | R5 | требование больше не действует |"
	rowR99     = "| `FTR.FMS.CAR-0002` | R99 | Нет такого. |"
	rowCatalog = "| `FTR.FMS.CAR-0001` | всё решение | Новая страница тарифов. |"
	rowInWork  = "| `FTR.FMS.CAR-0007` | R1 | Решение в работе. |"
	noteR13    = "*Изменено в `FTR.FMS.CAR-0042` R4: Отмена платная после 10 минут.*"
)

// FTR.HMR.DEV-0002: notes in the features a product document changes — the
// manual save, the writer, the submission and the merge at the release.
func TestSpecChangeNotes(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "specchg")
	must(t, err)
	defer drop()
	db, err := postgres.Connect(ctx, url)
	must(t, err)
	defer db.Close()
	must(t, postgres.Migrate(ctx, url))
	_, err = db.Exec(ctx, `WITH d AS (INSERT INTO domains (key, name, approval_required) VALUES ('FMS', 'Fleet', true) RETURNING id)
		INSERT INTO systems (domain_id, key, name) SELECT id, 'CAR', 'Cars' FROM d`)
	must(t, err)

	// The index of the default branch: FTR.FMS.CAR-0002 and FTR.FMS.CAR-0001 are released.
	idx := &fakeRepo{files: map[string]string{}, blobs: map[string]string{}}
	idx.set(pathBooking, booking)
	idx.set(pathCatalog, catalog)
	spec := specindex.NewService(db, idx.provider(t), nil, specindex.Config{DefaultBranch: "main"})
	_, err = spec.Enqueue(ctx, db, specindex.TriggerManual, nil)
	must(t, err)
	if ran, err := spec.RunOnce(ctx); err != nil || !ran {
		t.Fatalf("index ran=%v err=%v", ran, err)
	}

	expert := tu.User("expert:FMS:product")
	must(t, db.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, agent_name, agent_tone)
		VALUES ('chg1', 'anna', 'Anna', 'Hammurapi', 'business') RETURNING id`).Scan(&expert.UserID))
	store := specdata.NewPG(db)
	var sysID uuid.UUID
	must(t, db.QueryRow(ctx, `SELECT id FROM systems WHERE key = 'CAR'`).Scan(&sysID))
	newFeature := func(n int, branch string, pr int) *specdata.Feature {
		f := &specdata.Feature{UniqueID: fmt.Sprintf("FTR.FMS.CAR-%04d", n), SystemID: sysID, Number: n, Title: "Тарифы", Branch: branch,
			PRNumber: pr, PRURL: "http://pr", CreatedBy: expert.UserID}
		must(t, store.InsertFeature(ctx, f))
		must(t, store.InsertGate(ctx, &specdata.Gate{FeatureID: f.ID, Area: domain.AreaProduct, Status: domain.GateDraft, HeadCommit: "head", CreatedBy: expert.UserID}))
		out, err := store.FeatureByID(ctx, f.ID)
		must(t, err)
		return out
	}
	newFeature(7, "feature/FTR.FMS.CAR-0007", 0) // in work: no documents in the index
	f := newFeature(42, "feature/FTR.FMS.CAR-0042", 7)

	repo := newBranchRepo(map[string]string{pathBooking: booking, pathCatalog: catalog})
	repo.branch(f.Branch, f.PRNumber)
	repo.set(f.Branch, pathTariffs, tariffs())
	gp := repo.provider(t)
	tokens := mocks.NewMockTokenSource(gomock.NewController(t))
	tokens.EXPECT().Token(gomock.Any(), gomock.Any()).Return("tok", nil).AnyTimes()
	changes := &specchanges.Service{Q: db, Git: gp, Lang: contract.RU}
	gs := gates.NewService(store, gp, tokens, nopEvents{}, nil, "main")
	gs.Changes = changes
	items := func() map[string]specchanges.Item {
		t.Helper()
		list, err := gs.SpecChanges(ctx, expert, f.UniqueID, domain.AreaProduct)
		must(t, err)
		out := map[string]specchanges.Item{}
		for _, it := range list {
			k := it.Feature
			if it.Requirement != nil {
				k += " " + *it.Requirement
			}
			out[k] = it
		}
		return out
	}

	// SAVE-01, R5: one commit with product and the note; the version of the target is the same, its gates are not touched.
	_, err = gs.SaveDocument(ctx, expert, f.UniqueID, domain.AreaProduct, gates.SaveInput{Content: tariffs(rowR13)})
	must(t, err)
	cs := repo.commitsTo(f.Branch)
	if len(cs) != 1 || strings.Join(cs[0].paths, ",") != pathTariffs+","+pathBooking {
		t.Fatalf("commits %+v", cs)
	}
	noted := repo.file(f.Branch, pathBooking)
	if !strings.Contains(noted, "**R13.** Отмена брони бесплатна.\n"+noteR13+"\n") || !strings.Contains(noted, "**Версия:** 1.0\n") {
		t.Fatalf("target\n%s", noted)
	}
	// VIEW-01: the row is marked, with a link to the requirement.
	it := items()["FTR.FMS.CAR-0002 R13"]
	if it.State != "marked" || it.URL != "/spec/FTR.FMS.CAR-0002/product#r13" {
		t.Fatalf("item %+v", it)
	}

	// SAVE-02, SAVE-03: wrong rows do not stop the save; the right ones get notes, the wrong ones are errors of the block.
	err = store.ReleaseLock(ctx, f.ID, expert.UserID)
	must(t, err)
	_, err = gs.SaveDocument(ctx, expert, f.UniqueID, domain.AreaProduct, gates.SaveInput{Content: tariffs(rowR13, rowR99, rowCatalog, rowInWork)})
	must(t, err)
	if !strings.Contains(repo.file(f.Branch, pathCatalog), "**Версия:** 1.0\n*Изменено в `FTR.FMS.CAR-0042`: Новая страница тарифов.*\n\n---") {
		t.Fatalf("catalog\n%s", repo.file(f.Branch, pathCatalog))
	}
	got := items()
	if got["FTR.FMS.CAR-0002 R99"].State != "error" || got["FTR.FMS.CAR-0002 R99"].Error.Reason != "unknown_requirement" ||
		got["FTR.FMS.CAR-0007 R1"].Error.Reason != "target_in_work" || got["FTR.FMS.CAR-0001"].State != "marked" {
		b, _ := json.Marshal(got)
		t.Fatalf("items %s", b)
	}
	// SAVE-03: the submission is refused with the wrong rows.
	as := approvals.NewService(store, approvals.NewRepository(db), gp, tokens, nopEvents{}, gategen.Generator{Q: db})
	as.Changes = changes
	before := len(repo.commitsTo(f.Branch))
	_, err = as.Submit(ctx, expert, f.UniqueID, domain.AreaProduct, true, true)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Status != 409 || ae.Code != "changes_invalid" || len(repo.commitsTo(f.Branch)) != before {
		t.Fatalf("submit %v, commits %d → %d", err, before, len(repo.commitsTo(f.Branch)))
	}

	// SAVE-04: the writer gets the reasons and no revision is taken.
	var sunk int
	tools := map[string]mcp.Tool{}
	for _, tl := range agent.Tools(agent.ToolDeps{CheckChanges: func(ctx context.Context, key, area, doc string) (string, error) {
		errs, err := changes.Check(ctx, key, area, doc)
		if err != nil || len(errs) == 0 {
			return "", err
		}
		return specchanges.Messages(errs), nil
	}}) {
		tools[tl.Name] = tl
	}
	grant := mcp.Grant{Feature: f.UniqueID, Writer: &mcp.WriterScope{Mode: "write", Areas: []domain.Area{domain.AreaProduct}},
		Sink: func(string, json.RawMessage) error { sunk++; return nil }}
	raw, _ := json.Marshal(map[string]any{"area": "product", "content": tariffs(rowR13, rowInWork)})
	_, err = tools["submit_gate"].Handler(ctx, grant, raw)
	var te *mcp.ToolError
	if !errors.As(err, &te) || !strings.Contains(te.Msg, "FTR.FMS.CAR-0007 is still in work") || sunk != 0 {
		t.Fatalf("submit_gate %v, sunk %d", err, sunk)
	}
	raw, _ = json.Marshal(map[string]any{"area": "product", "content": tariffs(rowR13)})
	if _, err := tools["submit_gate"].Handler(ctx, grant, raw); err != nil || sunk != 1 {
		t.Fatalf("a right section is taken: %v, sunk %d", err, sunk)
	}

	// SAVE-07, VIEW-02, CHG-05: the section edited in git — a new row is pending; the submission
	// puts the notes in a commit Generated first, the removed row loses its note.
	repo.set(f.Branch, pathTariffs, tariffs(rowR13, rowR5))
	if it := items()["FTR.FMS.CAR-0002 R5"]; it.State != "pending" {
		t.Fatalf("pending %+v", it)
	}
	before = len(repo.commitsTo(f.Branch))
	g, err := as.Submit(ctx, expert, f.UniqueID, domain.AreaProduct, true, true)
	must(t, err)
	cs = repo.commitsTo(f.Branch)
	if g.Status != domain.GateInReview || len(cs) != before+1 || !strings.Contains(cs[len(cs)-1].msg, "Generated") {
		t.Fatalf("submit %s, commits %+v", g.Status, cs[before:])
	}
	noted = repo.file(f.Branch, pathBooking)
	if !strings.Contains(noted, "**R5.** Бронь на сутки.\n*Изменено в `FTR.FMS.CAR-0042` R4: требование больше не действует.*\n") ||
		strings.Contains(repo.file(f.Branch, pathCatalog), "CAR-0042") {
		t.Fatalf("after the submission\n%s\n%s", noted, repo.file(f.Branch, pathCatalog))
	}

	// MERGE-02: another feature noted R13 and was merged first; the release of this one
	// returns the file to the default branch, puts the notes again and merges.
	other := strings.Replace(booking, "**R13.** Отмена брони бесплатна.\n", "**R13.** Отмена брони бесплатна.\n*Изменено в `FTR.FMS.CAR-0050`: Отмена только в приложении.*\n", 1)
	repo.set("main", pathBooking, other)
	rel := &releases.Effects{Q: db, Store: store, Git: gp, Tokens: tokens, Changes: changes, DefaultBranch: "main",
		UpdateWait: 50 * time.Millisecond, UpdatePoll: time.Millisecond}
	payload, _ := json.Marshal(map[string]any{"featureId": f.ID, "userId": expert.UserID})
	evs, err := rel.MergeSpec(ctx, workflows.RunRef{}, payload)
	must(t, err)
	if len(evs) != 1 || evs[0].Type != "spec_merged" {
		t.Fatalf("merge %+v", evs)
	}
	main := repo.file("main", pathBooking)
	if !strings.Contains(main, "*Изменено в `FTR.FMS.CAR-0050`: Отмена только в приложении.*\n"+noteR13+"\n") ||
		!strings.Contains(main, "*Изменено в `FTR.FMS.CAR-0042` R4: требование больше не действует.*") || repo.file("main", pathTariffs) == "" {
		t.Fatalf("main\n%s", main)
	}

	// MERGE-01: no conflict — the notes over the fresh default branch, then the merge.
	f2 := newFeature(43, "feature/FTR.FMS.CAR-0043", 8)
	repo.branch(f2.Branch, f2.PRNumber)
	path43 := "specs/FMS/CAR/FTR.FMS.CAR-0043/product/spec.md"
	repo.set(f2.Branch, path43, strings.ReplaceAll(tariffs(rowCatalog), "R4.** Отмена", "R4.** Каталог `FTR.FMS.CAR-0001`, отмена"))
	repo.set("main", pathCatalog, strings.Replace(catalog, "Каталог машин.", "Каталог машин и самокатов.", 1))
	payload, _ = json.Marshal(map[string]any{"featureId": f2.ID, "userId": expert.UserID})
	evs, err = rel.MergeSpec(ctx, workflows.RunRef{}, payload)
	must(t, err)
	if len(evs) != 1 || evs[0].Type != "spec_merged" ||
		!strings.Contains(repo.file("main", pathCatalog), "*Изменено в `FTR.FMS.CAR-0043` R4: Новая страница тарифов.*\n\n---\n\n## 4. Требования\n\n**R1.** Каталог машин и самокатов.") {
		t.Fatalf("merge %+v\n%s", evs, repo.file("main", pathCatalog))
	}

	// MERGE-03: a conflict in the files of the feature itself is not resolved.
	f3 := newFeature(44, "feature/FTR.FMS.CAR-0044", 9)
	repo.branch(f3.Branch, f3.PRNumber)
	path44 := "specs/FMS/CAR/FTR.FMS.CAR-0044/product/spec.md"
	repo.set(f3.Branch, path44, "# Ветка\n")
	repo.set("main", path44, "# Основная\n")
	payload, _ = json.Marshal(map[string]any{"featureId": f3.ID, "userId": expert.UserID})
	evs, err = rel.MergeSpec(ctx, workflows.RunRef{}, payload)
	must(t, err)
	if len(evs) != 1 || evs[0].Type != "merge_failed" {
		t.Fatalf("merge of a conflict in own files %+v", evs)
	}
}
