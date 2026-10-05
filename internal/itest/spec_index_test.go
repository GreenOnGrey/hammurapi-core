//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/domains"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// fakeRepo is the default branch of a specification repository for the mock provider.
type fakeRepo struct {
	mu    sync.Mutex
	files map[string]string
	blobs map[string]string
	reads int
}

func (r *fakeRepo) set(path, content string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[path] = content
	r.blobs[git.BlobSHA([]byte(content))] = content
}

func (r *fakeRepo) remove(prefix string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for p := range r.files {
		if strings.HasPrefix(p, prefix) {
			delete(r.files, p)
		}
	}
}

func (r *fakeRepo) provider(t *testing.T) *mocks.MockProvider {
	ctrl := gomock.NewController(t)
	p := mocks.NewMockProvider(ctrl)
	p.EXPECT().BotToken(gomock.Any()).Return("bot", nil).AnyTimes()
	p.EXPECT().BranchHead(gomock.Any(), gomock.Any(), "main").Return("c1", nil).AnyTimes()
	p.EXPECT().Repo().Return("o/specs").AnyTimes()
	p.EXPECT().CommitURL(gomock.Any()).DoAndReturn(func(sha string) string { return "https://git.example/o/specs/commit/" + sha }).AnyTimes()
	p.EXPECT().Tree(gomock.Any(), gomock.Any(), "c1", "specs").DoAndReturn(func(context.Context, string, string, string) ([]git.TreeEntry, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		var out []git.TreeEntry
		for path, c := range r.files {
			out = append(out, git.TreeEntry{Path: path, SHA: git.BlobSHA([]byte(c)), Size: int64(len(c))})
		}
		return out, nil
	}).AnyTimes()
	p.EXPECT().GetFile(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _, _, path string) (*git.File, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		c, ok := r.files[path]
		if !ok {
			return nil, git.ErrNotFound
		}
		return &git.File{Content: []byte(c), BlobSHA: git.BlobSHA([]byte(c))}, nil
	}).AnyTimes()
	p.EXPECT().Blob(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _ string, sha string) ([]byte, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.reads++
		c, ok := r.blobs[sha]
		if !ok {
			return nil, git.ErrNotFound
		}
		return []byte(c), nil
	}).AnyTimes()
	return p
}

// FTR.HMR.CMN-0005: the check indexes new specifications, records problems,
// fills the index of documents, and the navigator and the agent read it.
func TestSpecIndex(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "specidx")
	must(t, err)
	defer drop()
	db, err := postgres.Connect(ctx, url)
	must(t, err)
	defer db.Close()
	must(t, postgres.Migrate(ctx, url))

	// Catalog: FMS/CAR (last number 17); a feature released through Hammurapi.
	_, err = db.Exec(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('FMS', 'Fleet') RETURNING id)
		INSERT INTO systems (domain_id, key, name, last_number) SELECT id, 'CAR', 'Cars', 17 FROM d`)
	must(t, err)
	_, err = db.Exec(ctx, `INSERT INTO features (unique_id, system_id, number, title, branch_name, pr_number, pr_url, phase)
		SELECT 'FTR.FMS.CAR-0002', id, 2, 'Booking', 'feature/FTR.FMS.CAR-0002', 7, 'https://git.example/pr/7', 'released' FROM systems WHERE key = 'CAR'`)
	must(t, err)

	repo := &fakeRepo{files: map[string]string{}, blobs: map[string]string{}}
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0002/product/spec.md", "# Booking\n\n## 4. Требования\n\n**R3.** Отмена брони бесплатна в течение 30 минут.\n- Дано бронь, когда отмена через 10 минут, тогда бесплатно.\n")
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0042/product/spec.md", "# Weekend tariffs\n\n## Связи\n\nУчитывает FTR.FMS.CAR-0002-R3.\n")
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0042/design/mockups.html", "<html><script>alert(1)</script></html>")
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0043/product/spec.md", "---\nparent: FTR.FMS.CAR-0042\n---\n# Fix of tariffs\n")
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0050/product/spec.md", "---\nparent: FTR.FMS.CAR-0999\n---\n# Orphan fix\n")
	repo.set("specs/FMS/CAR/FMS.CAR-0011/product/spec.md", "# Old\n")
	repo.set("specs/LOG/DLV/FTR.LOG.DLV-0003/product/spec.md", "# Deliveries\n")
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0060/notes.md", "no spec.md: not a candidate")
	svc := specindex.NewService(db, repo.provider(t), nil, specindex.Config{DefaultBranch: "main"})

	runCheck := func() {
		t.Helper()
		_, err := svc.Enqueue(ctx, db, specindex.TriggerManual, nil)
		must(t, err)
		ran, err := svc.RunOnce(ctx)
		must(t, err)
		if !ran {
			t.Fatal("the check did not run")
		}
	}
	runCheck()

	// SCN-04: the run is recorded.
	var status string
	var found, indexed, issues int
	must(t, db.QueryRow(ctx, `SELECT status, found, indexed, issues FROM spec_scan_runs ORDER BY created_at DESC LIMIT 1`).Scan(&status, &found, &indexed, &issues))
	if status != "succeeded" || found != 6 || indexed != 2 || issues != 3 {
		t.Fatalf("run %s found %d indexed %d issues %d", status, found, indexed, issues)
	}
	// IDX-01, IDX-07, IDX-11: indexed as implemented, parents first, counter raised.
	var phase, source, title, parent string
	must(t, db.QueryRow(ctx, `SELECT f.phase::text, f.source, f.title, COALESCE(p.unique_id, '') FROM features f
		LEFT JOIN features p ON p.id = f.parent_id WHERE f.unique_id = 'FTR.FMS.CAR-0043'`).Scan(&phase, &source, &title, &parent))
	if phase != "indexed" || source != "repository" || title != "Fix of tariffs" || parent != "FTR.FMS.CAR-0042" {
		t.Fatalf("fix %s %s %q parent %q", phase, source, title, parent)
	}
	var last int
	must(t, db.QueryRow(ctx, `SELECT last_number FROM systems WHERE key = 'CAR'`).Scan(&last))
	if last != 43 {
		t.Fatalf("last_number %d", last)
	}
	// IDX-04, IDX-12, CAT-01: problems with their details.
	kinds := openIssues(t, db)
	if kinds["specs/FMS/CAR/FMS.CAR-0011/"] != "old_format" || kinds["specs/FMS/CAR/FTR.FMS.CAR-0050/"] != "missing_parent" ||
		kinds["specs/LOG/DLV/FTR.LOG.DLV-0003/"] != "missing_domain" || len(kinds) != 3 {
		t.Fatalf("issues %v", kinds)
	}
	focus, err := svc.Focus(ctx)
	must(t, err)
	if len(focus) != 3 {
		t.Fatalf("focus %+v", focus)
	}

	// NAV-02: the tree — fixes nested in the parent; no unknown folders.
	tree, err := svc.Tree(ctx, "", "")
	must(t, err)
	if len(tree.Domains) != 1 || len(tree.Domains[0].Systems[0].Features) != 2 {
		b, _ := json.Marshal(tree)
		t.Fatalf("tree %s", b)
	}
	f42 := tree.Domains[0].Systems[0].Features[1]
	if f42.Key != "FTR.FMS.CAR-0042" || len(f42.Fixes) != 1 || f42.Fixes[0].Key != "FTR.FMS.CAR-0043" || strings.Join(f42.Areas, ",") != "product" {
		t.Fatalf("feature %+v", f42)
	}
	// NAV-15: an indexed specification has no issues and no PRs; the released one shows its MVP spec PR.
	doc, err := svc.Document(ctx, "FTR.FMS.CAR-0042", "product")
	must(t, err)
	if doc.Feature.Source != "repository" || len(doc.Feature.Issues) != 0 || len(doc.Feature.PullRequests) != 0 ||
		!strings.Contains(doc.HistoryURL, "/commits/main/specs/FMS/CAR/FTR.FMS.CAR-0042/product/spec.md") {
		t.Fatalf("doc %+v", doc)
	}
	doc2, err := svc.Document(ctx, "FTR.FMS.CAR-0002", "product")
	must(t, err)
	if len(doc2.Feature.PullRequests) != 1 || doc2.Feature.PullRequests[0].Kind != "spec" || doc2.Feature.PullRequests[0].Number != 7 {
		t.Fatalf("released doc PRs %+v", doc2.Feature.PullRequests)
	}
	if _, err := svc.Document(ctx, "FTR.FMS.CAR-0050", "product"); err == nil {
		t.Fatal("a folder with a problem is not in the navigator")
	}
	files, err := svc.Files(ctx, "FTR.FMS.CAR-0042", "design")
	must(t, err)
	if len(files) != 1 || files[0].MimeType != "text/html" || !files[0].Previewable {
		t.Fatalf("files %+v", files)
	}

	// SRC-01, SRC-03: text and ID prefix search.
	res, err := svc.Search(ctx, specindex.SearchQuery{Q: "отмена брони"})
	must(t, err)
	if res.Total < 1 || res.Items[0].FeatureKey != "FTR.FMS.CAR-0002" || !strings.Contains(res.Items[0].Snippet, "‹") ||
		res.Items[0].Section != "4. Требования" {
		t.Fatalf("search %+v", res)
	}
	res, err = svc.Search(ctx, specindex.SearchQuery{Q: "FTR.FMS.CAR-004"})
	must(t, err)
	if res.Total < 2 || !strings.HasPrefix(res.Items[0].FeatureKey, "FTR.FMS.CAR-004") {
		t.Fatalf("id search %+v", res)
	}

	// AGT-02, AGT-04, AGT-05: the agent's tools.
	tools := map[string]mcp.Tool{}
	for _, tl := range svc.Tools() {
		tools[tl.Name] = tl
		if !tl.ReadOnly || len(tl.Modes) != 7 { // six scenarios and the personal agent of Nabu
			t.Fatalf("tool %s is offered read-only in every mode", tl.Name)
		}
	}
	call := func(name string, args any) string {
		t.Helper()
		raw, _ := json.Marshal(args)
		out, err := tools[name].Handler(ctx, mcp.Grant{}, raw)
		must(t, err)
		return out
	}
	if out := call("spec_search", map[string]any{"query": "отмена брони"}); !strings.Contains(out, "**") || !strings.Contains(out, "FTR.FMS.CAR-0002") {
		t.Fatalf("spec_search %s", out)
	}
	if out := call("spec_requirements", map[string]any{"featureKey": "FTR.FMS.CAR-0002", "ids": []string{"R3"}}); !strings.Contains(out, "Дано бронь") {
		t.Fatalf("spec_requirements %s", out)
	}
	if out := call("spec_references", map[string]any{"target": "FTR.FMS.CAR-0002-R3"}); !strings.Contains(out, `"sourceKey":"FTR.FMS.CAR-0042"`) {
		t.Fatalf("spec_references %s", out)
	}
	if out := call("spec_tree", map[string]any{}); !strings.Contains(out, `"features":3`) {
		t.Fatalf("spec_tree %s", out)
	}

	// CAT-03: adding the domain and the system queues a check that indexes the waiting specification.
	dom := domains.NewService(db, nopEvents{})
	dom.CatalogChanged = svc.RequestCatalog
	admin := &domain.Principal{GlobalAdmin: true}
	must(t, db.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, agent_name, agent_tone, is_global_admin)
		VALUES ('sx1', 'sx', 'Admin', 'Hammurapi', 'business', true) RETURNING id`).Scan(&admin.UserID))
	must(t, dom.CreateDomain(ctx, admin, "LOG", "Logistics", false))
	must(t, dom.CreateSystem(ctx, admin, "LOG", "DLV", "Deliveries"))
	ran, err := svc.RunOnce(ctx)
	must(t, err)
	var n int
	must(t, db.QueryRow(ctx, `SELECT count(*) FROM features WHERE unique_id = 'FTR.LOG.DLV-0003' AND phase = 'indexed'`).Scan(&n))
	if !ran || n != 1 {
		t.Fatalf("catalog check ran=%v indexed=%d", ran, n)
	}
	if _, ok := openIssues(t, db)["specs/LOG/DLV/FTR.LOG.DLV-0003/"]; ok {
		t.Fatal("the missing_domain problem is not resolved")
	}

	// IDX-17: a check without changes reads no blobs.
	repo.reads = 0
	runCheck()
	if repo.reads != 0 {
		t.Fatalf("%d blobs read without changes", repo.reads)
	}

	// DEL-01, DEL-02: a folder removed from the branch, then returned.
	repo.remove("specs/FMS/CAR/FTR.FMS.CAR-0042/")
	runCheck()
	var deleted bool
	must(t, db.QueryRow(ctx, `SELECT repo_deleted_at IS NOT NULL FROM features WHERE unique_id = 'FTR.FMS.CAR-0042'`).Scan(&deleted))
	if !deleted || openIssues(t, db)["specs/FMS/CAR/FTR.FMS.CAR-0042/"] != "deleted" {
		t.Fatalf("deleted %v issues %v", deleted, openIssues(t, db))
	}
	if _, err := svc.Document(ctx, "FTR.FMS.CAR-0042", "product"); err == nil {
		t.Fatal("a deleted specification left the navigator")
	}
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0042/product/spec.md", "# Weekend tariffs\n")
	runCheck()
	must(t, db.QueryRow(ctx, `SELECT repo_deleted_at IS NOT NULL FROM features WHERE unique_id = 'FTR.FMS.CAR-0042'`).Scan(&deleted))
	if deleted {
		t.Fatal("repo_deleted_at not reset")
	}

	// SCN-05: "Check now" twice gives one queued run; SCN-10: a push only delays a pending push run.
	id1, err := svc.CheckNow(ctx, admin)
	must(t, err)
	id2, err := svc.CheckNow(ctx, admin)
	must(t, err)
	must(t, db.QueryRow(ctx, `SELECT count(*) FROM spec_scan_runs WHERE status = 'queued'`).Scan(&n))
	if id1 != id2 || n != 1 {
		t.Fatalf("runs %s %s queued %d", id1, id2, n)
	}
	ran, err = svc.RunOnce(ctx)
	must(t, err)
	if !ran {
		t.Fatal("the manual run did not run")
	}
	_, err = svc.Enqueue(ctx, db, specindex.TriggerPush, nil)
	must(t, err)
	var notBefore time.Time
	must(t, db.QueryRow(ctx, `SELECT not_before FROM spec_scan_runs WHERE status = 'queued'`).Scan(&notBefore))
	if time.Until(notBefore) < 20*time.Second {
		t.Fatalf("push not delayed: %s", time.Until(notBefore))
	}
	if ran, _ := svc.RunOnce(ctx); ran {
		t.Fatal("a delayed push run ran early")
	}
	// R17 / NAV-11: a push to the default branch updates a document of a known feature at once.
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0002/product/spec.md", "# Booking v2\n\nНовый текст про аренду самокатов.\n")
	must(t, svc.SpecsPushed(ctx, &git.PushEvent{Branch: "main", After: "c2", Commits: []git.PushCommit{
		{Modified: []string{"specs/FMS/CAR/FTR.FMS.CAR-0002/product/spec.md"}}}}))
	doc2, err = svc.Document(ctx, "FTR.FMS.CAR-0002", "product")
	must(t, err)
	if doc2.Title != "Booking v2" {
		t.Fatalf("push did not update the document: %q", doc2.Title)
	}
	if res, _ := svc.Search(ctx, specindex.SearchQuery{Q: "самокатов"}); res.Total != 1 {
		t.Fatalf("search after push %+v", res)
	}

	// SCN-03 / SCN-06: settings.
	if _, err := svc.PutSettings(ctx, admin, specindex.Settings{Interval: "10m"}); err == nil {
		t.Fatal("10m is not an allowed interval")
	}
	if _, err := svc.Settings(ctx, &domain.Principal{}); err == nil {
		t.Fatal("settings are for global administrators")
	}
}

func openIssues(t *testing.T, db *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := db.Query(context.Background(), `SELECT path, kind FROM spec_index_issues WHERE resolved_at IS NULL`)
	must(t, err)
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, k string
		must(t, rows.Scan(&p, &k))
		out[p] = k
	}
	return out
}
