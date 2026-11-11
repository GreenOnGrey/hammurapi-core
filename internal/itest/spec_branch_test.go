//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GreenOnGrey/speclint/v2/contract"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gates"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specchanges"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
)

// FTR.HMR.DEV-0003 R1–R9: the files of a gate in the branch of the feature,
// the links of the feature in work and the text of an item.
func TestSpecFeatureBranch(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "specbranch")
	must(t, err)
	defer drop()
	db, err := postgres.Connect(ctx, url)
	must(t, err)
	defer db.Close()
	must(t, postgres.Migrate(ctx, url))
	_, err = db.Exec(ctx, `WITH d AS (INSERT INTO domains (key, name, approval_required) VALUES ('FMS', 'Fleet', true) RETURNING id)
		INSERT INTO systems (domain_id, key, name) SELECT id, 'CAR', 'Cars' FROM d`)
	must(t, err)

	idx := &fakeRepo{files: map[string]string{}, blobs: map[string]string{}}
	idx.set(pathBooking, booking)
	idx.set(pathBookingArch, bookingArch)
	// FTR.HMR.DISC-0001 R30: the materials of the research of the indexed feature, with the legacy document too
	const bookingDisc = "specs/FMS/CAR/FTR.FMS.CAR-0002/discovery/"
	idx.set(bookingDisc+"discovery.md", "# Анализ брони\n\n## Риски\n")
	idx.set(bookingDisc+"flow.png", "png-booking")
	idx.set("specs/FMS/CAR/FTR.FMS.CAR-0002/discovery.md", "# Старый анализ\n")
	idx.set(bookingDisc+"old/nested.md", "nested")
	spec := specindex.NewService(db, idx.provider(t), nil, specindex.Config{DefaultBranch: "main"})
	_, err = spec.Enqueue(ctx, db, specindex.TriggerManual, nil)
	must(t, err)
	if ran, err := spec.RunOnce(ctx); err != nil || !ran {
		t.Fatalf("index ran=%v err=%v", ran, err)
	}

	// DF-03: the files of discovery are files of the index, never documents; nested folders are not read.
	var discFiles, discDocs int
	must(t, db.QueryRow(ctx, `SELECT (SELECT count(*) FROM spec_files WHERE area = 'discovery' AND feature_key = 'FTR.FMS.CAR-0002'),
		(SELECT count(*) FROM spec_documents WHERE area = 'discovery')`).Scan(&discFiles, &discDocs))
	if discFiles != 3 || discDocs != 0 {
		t.Fatalf("discovery files %d, documents %d", discFiles, discDocs)
	}
	// DF-06, DF-08: the document of the navigator — the new path before the legacy one, from git by the SHA of the index.
	dd, err := spec.Document(ctx, "FTR.FMS.CAR-0002", specindex.AreaDiscovery)
	must(t, err)
	if dd.Markdown != "# Анализ брони\n\n## Риски\n" || dd.Path != bookingDisc+"discovery.md" || len(dd.Areas) == 0 || dd.Areas[0] != "discovery" ||
		dd.Title == "" || len(dd.TOC) != 1 {
		t.Fatalf("discovery document %+v", dd)
	}
	if pd, err := spec.Document(ctx, "FTR.FMS.CAR-0002", "product"); err != nil || pd.Areas[0] != "discovery" || pd.Areas[1] != "product" {
		t.Fatalf("areas of product %+v %v", pd, err)
	}
	// DF-07: the files of the area without its document.
	if df, err := spec.Files(ctx, "FTR.FMS.CAR-0002", specindex.AreaDiscovery); err != nil || len(df) != 1 || df[0].Name != "flow.png" || df[0].ViewURL == "" {
		t.Fatalf("discovery files %+v %v", df, err)
	}
	_, err = db.Exec(ctx, `DELETE FROM spec_files WHERE path = $1`, bookingDisc+"discovery.md")
	must(t, err)
	if dd, err := spec.Document(ctx, "FTR.FMS.CAR-0002", specindex.AreaDiscovery); err != nil || dd.Markdown != "# Старый анализ\n" {
		t.Fatalf("legacy discovery document %+v %v", dd, err)
	}

	user := tu.User()
	must(t, db.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, agent_name, agent_tone)
		VALUES ('br1', 'ira', 'Ira', 'Hammurapi', 'business') RETURNING id`).Scan(&user.UserID))
	store := specdata.NewPG(db)
	var sysID uuid.UUID
	must(t, db.QueryRow(ctx, `SELECT id FROM systems WHERE key = 'CAR'`).Scan(&sysID))
	nf := &specdata.Feature{UniqueID: "FTR.FMS.CAR-0042", SystemID: sysID, Number: 42, Title: "Тарифы", Branch: "feature/FTR.FMS.CAR-0042",
		PRNumber: 7, PRURL: "http://pr", CreatedBy: user.UserID}
	must(t, store.InsertFeature(ctx, nf))
	must(t, store.InsertGate(ctx, &specdata.Gate{FeatureID: nf.ID, Area: domain.AreaProduct, Status: domain.GateApproved, HeadCommit: "h", CreatedBy: user.UserID}))
	must(t, store.InsertGate(ctx, &specdata.Gate{FeatureID: nf.ID, Area: domain.AreaArch, Status: domain.GateDraft, HeadCommit: "h", CreatedBy: user.UserID}))
	must(t, store.InsertGate(ctx, &specdata.Gate{FeatureID: nf.ID, Area: domain.AreaDesign, Status: domain.GateDraft, HeadCommit: "h", CreatedBy: user.UserID}))
	f, err := store.FeatureByID(ctx, nf.ID)
	must(t, err)

	const design = "specs/FMS/CAR/FTR.FMS.CAR-0042/design/"
	repo := newBranchRepo(map[string]string{pathBooking: booking, pathBookingArch: bookingArch})
	repo.branch(f.Branch, f.PRNumber)
	repo.set(f.Branch, pathTariffs, tariffs(rowR13))
	repo.set(f.Branch, pathTariffsArch, tariffsArch("", rowA2))
	repo.set(f.Branch, design+"spec.md", "# Дизайн\n")
	repo.set(f.Branch, design+"mockups.html", "<!doctype html><title>m</title><script>fetch('/api/v1/me')</script>")
	repo.set(f.Branch, design+"flow.png", "png")
	repo.set(f.Branch, design+"big.html", strings.Repeat("x", 2048))
	gp := repo.provider(t)
	gp.EXPECT().Tree(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _, ref, dir string) ([]git.TreeEntry, error) {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		files := repo.files(ref)
		if files == nil {
			return nil, git.ErrNotFound
		}
		var out []git.TreeEntry
		for p, c := range files {
			if strings.HasPrefix(p, dir+"/") {
				out = append(out, git.TreeEntry{Path: p, SHA: git.BlobSHA([]byte(c)), Size: int64(len(c))})
			}
		}
		return out, nil
	}).AnyTimes()
	gp.EXPECT().Blob(gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _, sha string) ([]byte, error) {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		for _, c := range repo.files(f.Branch) {
			if git.BlobSHA([]byte(c)) == sha {
				return []byte(c), nil
			}
		}
		return nil, git.ErrNotFound
	}).AnyTimes()
	tokens := mocks.NewMockTokenSource(gomock.NewController(t))
	tokens.EXPECT().Token(gomock.Any(), gomock.Any()).Return("tok", nil).AnyTimes()
	gs := gates.NewService(store, gp, tokens, nopEvents{}, nil, "main")
	gs.Changes = &specchanges.Service{Q: db, Git: gp, Lang: contract.RU}
	gs.PreviewMaxBytes = 1024

	// MOCK-01, MOCK-02, MOCK-04: the files of the folder of the gate in the branch, without spec.md; a file over the limit is not previewable.
	files, err := gs.Files(ctx, user, f.UniqueID, domain.AreaDesign)
	must(t, err)
	names := []string{}
	for _, x := range files.Files {
		names = append(names, x.Name)
	}
	if strings.Join(names, ",") != "big.html,flow.png,mockups.html" || files.Files[0].Previewable || !files.Files[2].Previewable ||
		!strings.Contains(files.Files[2].ViewURL, "mode=view") || files.Ref != f.Branch {
		t.Fatalf("files %+v", files)
	}
	// MOCK-03: product has no files besides spec.md.
	if pf, err := gs.Files(ctx, user, f.UniqueID, domain.AreaProduct); err != nil || len(pf.Files) != 0 {
		t.Fatalf("product files %+v %v", pf, err)
	}

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(httpx.WithPrincipal(req.Context(), user)))
		})
	})
	r.Route("/api/v1", func(r chi.Router) {
		gates.NewHandlers(gs).Routes(r)
		spec.Routes(r)
	})
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	// MOCK-15: the mockup of the branch is a page of an opaque origin.
	w := get("/api/v1/features/FTR.FMS.CAR-0042/gates/design/files/raw?name=mockups.html&mode=view")
	if w.Code != http.StatusOK || w.Header().Get("Content-Security-Policy") != "sandbox allow-scripts" || !strings.Contains(w.Body.String(), "fetch(") {
		t.Fatalf("view %d %v", w.Code, w.Header())
	}
	// The version of the blob in the address is cached, without it the file of the branch is revalidated.
	if w.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("cache without v: %q", w.Header().Get("Cache-Control"))
	}
	if w := get(files.Files[2].ViewURL); w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "private, max-age=300" {
		t.Fatalf("cache with v: %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	// MOCK-04: over the limit — 413 for viewing.
	if w := get("/api/v1/features/FTR.FMS.CAR-0042/gates/design/files/raw?name=big.html&mode=view"); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("big %d", w.Code)
	}
	// MOCK-17: a path is not a name.
	_, _, err = gs.File(ctx, user, f.UniqueID, domain.AreaDesign, "../product/spec.md")
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != "invalid_name" {
		t.Fatalf("a path %v", err)
	}
	if _, _, err := gs.File(ctx, user, f.UniqueID, domain.AreaDesign, "spec.md"); !errors.As(err, &ae) || ae.Code != "invalid_name" {
		t.Fatalf("spec.md is not a file of the gate: %v", err)
	}

	// FTR.HMR.SPEC-0004 qa FILE-05: the files beside a document through MCP, with the rights and the limit of the viewer.
	tools := map[string]mcp.Tool{}
	for _, tl := range agent.Tools(agent.ToolDeps{Gates: gs, Principal: func(context.Context, uuid.UUID) (*domain.Principal, error) { return user, nil }}) {
		tools[tl.Name] = tl
	}
	call := func(g mcp.Grant, name string, args map[string]any) (string, error) {
		raw, _ := json.Marshal(args)
		return tools[name].Handler(ctx, g, raw)
	}
	personal := mcp.Grant{Mode: mcp.ModeNabu, UserID: user.UserID}
	out, err := call(personal, "list_gate_files", map[string]any{"uniqueId": f.UniqueID, "area": "design"})
	if err != nil || !strings.Contains(out, "mockups.html\ttext/html") || strings.Contains(out, "spec.md") {
		t.Fatalf("list_gate_files %q %v", out, err)
	}
	if out, err = call(personal, "read_gate_file", map[string]any{"uniqueId": f.UniqueID, "area": "design", "name": "mockups.html"}); err != nil ||
		!strings.Contains(out, "<title>m</title>") {
		t.Fatalf("read_gate_file %q %v", out, err)
	}
	var te *mcp.ToolError
	for _, c := range []struct {
		g    mcp.Grant
		args map[string]any
	}{
		{personal, map[string]any{"uniqueId": f.UniqueID, "area": "design", "name": "flow.png"}},           // not text
		{personal, map[string]any{"uniqueId": f.UniqueID, "area": "design", "name": "big.html"}},           // over the limit
		{personal, map[string]any{"uniqueId": f.UniqueID, "area": "design", "name": "../product/spec.md"}}, // a path
		{mcp.Grant{Mode: mcp.ModeTask, Feature: "FTR.FMS.CAR-0001", UserID: user.UserID}, map[string]any{"uniqueId": f.UniqueID, "area": "design", "name": "mockups.html"}},
	} {
		if _, err := call(c.g, "read_gate_file", c.args); !errors.As(err, &te) {
			t.Fatalf("read_gate_file %v: %v", c.args, err)
		}
	}

	// FTR.HMR.DISC-0001 R30: the materials of the research in the branch of the feature.
	disc := "specs/FMS/CAR/FTR.FMS.CAR-0042/discovery/"
	repo.set(f.Branch, disc+"discovery.md", "# Анализ\n")
	repo.set(f.Branch, disc+"flow.png", "png")
	repo.set(f.Branch, disc+"deck.pptx", "pptx")
	repo.set(f.Branch, disc+"notes.md", "заметки эксперта")
	repo.set(f.Branch, disc+"a/b.md", "nested")
	// DF-09: the document and the files by name, without nested folders; a presentation is not previewable.
	df, err := gs.Discovery(ctx, user, f.UniqueID)
	must(t, err)
	names = names[:0]
	for _, x := range df.Files {
		names = append(names, x.Name)
	}
	if df.Document == nil || df.Document.Markdown != "# Анализ\n" || df.Ref != f.Branch || strings.Join(names, ",") != "deck.pptx,flow.png,notes.md" ||
		df.Files[0].ViewURL != "" || !strings.Contains(df.Files[1].ViewURL, "/discovery/files/raw?") {
		t.Fatalf("discovery %+v %+v", df, df.Document)
	}
	if w := get("/api/v1/features/FTR.FMS.CAR-0042/discovery"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"deck.pptx"`) {
		t.Fatalf("GET discovery %d %s", w.Code, w.Body.String())
	}
	if w := get(df.Files[1].ViewURL); w.Code != http.StatusOK || w.Body.String() != "png" {
		t.Fatalf("view flow.png %d", w.Code)
	}
	// DF-10: a path is not a name.
	if _, _, err := gs.DiscoveryFile(ctx, user, f.UniqueID, "../product/spec.md"); !errors.As(err, &ae) || ae.Code != "invalid_name" {
		t.Fatalf("discovery path %v", err)
	}
	// DF-11: the agent lists the materials with the document and reads only the text ones; a task reads only its feature.
	out, err = call(personal, "list_gate_files", map[string]any{"uniqueId": f.UniqueID, "area": "discovery"})
	if err != nil || !strings.Contains(out, "discovery.md\ttext/markdown") || !strings.Contains(out, "deck.pptx") || strings.Contains(out, "b.md") {
		t.Fatalf("list_gate_files discovery %q %v", out, err)
	}
	for name, want := range map[string]string{"notes.md": "заметки эксперта", "discovery.md": "# Анализ"} {
		if out, err := call(personal, "read_gate_file", map[string]any{"uniqueId": f.UniqueID, "area": "discovery", "name": name}); err != nil || !strings.Contains(out, want) {
			t.Fatalf("read_gate_file discovery %s %q %v", name, out, err)
		}
	}
	for _, c := range []struct {
		g    mcp.Grant
		name string
	}{
		{personal, "flow.png"},
		{mcp.Grant{Mode: mcp.ModeTask, Feature: "FTR.FMS.CAR-0001", UserID: user.UserID}, "notes.md"},
	} {
		if _, err := call(c.g, "read_gate_file", map[string]any{"uniqueId": f.UniqueID, "area": "discovery", "name": c.name}); !errors.As(err, &te) {
			t.Fatalf("read_gate_file discovery %s: %v", c.name, err)
		}
	}
	// DF-08 in the branch: without discovery/discovery.md the legacy document in the root of the folder.
	repo.mu.Lock()
	delete(repo.files(f.Branch), disc+"discovery.md")
	repo.mu.Unlock()
	repo.set(f.Branch, "specs/FMS/CAR/FTR.FMS.CAR-0042/discovery.md", "# Старый\n")
	if df, err := gs.Discovery(ctx, user, f.UniqueID); err != nil || df.Document == nil || df.Document.Markdown != "# Старый\n" {
		t.Fatalf("legacy in the branch %+v %v", df, err)
	}
	// DF-10: neither folder nor document — an empty answer, not an error.
	repo.mu.Lock()
	for p := range repo.files(f.Branch) {
		if strings.HasPrefix(p, disc) || strings.HasSuffix(p, "/discovery.md") {
			delete(repo.files(f.Branch), p)
		}
	}
	repo.mu.Unlock()
	gp.EXPECT().BranchHead(gomock.Any(), gomock.Any(), f.Branch).Return("h", nil).AnyTimes()
	if df, err := gs.Discovery(ctx, user, f.UniqueID); err != nil || df.Document != nil || len(df.Files) != 0 {
		t.Fatalf("empty discovery %+v %v", df, err)
	}

	// DRAFT-01, DRAFT-02: the links of the feature in work from its branch, by layer, with the state of the gate.
	layers, err := gs.DraftLinks(ctx, user, f.UniqueID)
	must(t, err)
	if len(layers) != 2 || layers[0].Layer != "product" || layers[0].State != "approved" || layers[1].Layer != "arch" || layers[1].State != "draft" {
		t.Fatalf("layers %+v", layers)
	}
	if len(layers[0].Changes) != 1 || layers[0].Changes[0].Key != "FTR.FMS.CAR-0002" || strings.Join(layers[0].Changes[0].Items, ",") != "R13" ||
		!layers[0].Changes[0].Indexed || len(layers[1].Changes) != 1 || strings.Join(layers[1].Changes[0].Items, ",") != "A2" {
		t.Fatalf("links %+v", layers)
	}
	// DRAFT-04: the links of the feature in work are not in the index.
	var n int
	must(t, db.QueryRow(ctx, `SELECT count(*) FROM spec_links WHERE source_key = 'FTR.FMS.CAR-0042'`).Scan(&n))
	if n != 0 {
		t.Fatalf("draft links in the index: %d", n)
	}
	// DRAFT-03: a skipped arch has no links.
	_, err = db.Exec(ctx, `UPDATE features SET skipped_areas = '{arch}' WHERE id = $1`, f.ID)
	must(t, err)
	layers, err = gs.DraftLinks(ctx, user, f.UniqueID)
	must(t, err)
	if layers[1].State != "skipped" || len(layers[1].Changes) != 0 {
		t.Fatalf("skipped %+v", layers[1])
	}

	// DRAFT-10: the text of the requirement of the index with its criteria; a decision with its row.
	if w := get("/api/v1/spec/items/FTR.FMS.CAR-0002/r13"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Дано бронь") {
		t.Fatalf("item R13 %d %s", w.Code, w.Body.String())
	}
	if w := get("/api/v1/spec/items/FTR.FMS.CAR-0002/A2"); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Отмена — событие Kafka") {
		t.Fatalf("item A2 %d %s", w.Code, w.Body.String())
	}
	if w := get("/api/v1/spec/items/FTR.FMS.CAR-0002/A9"); w.Code != http.StatusNotFound {
		t.Fatalf("item A9 %d", w.Code)
	}
}
