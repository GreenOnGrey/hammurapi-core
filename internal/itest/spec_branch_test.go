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
	spec := specindex.NewService(db, idx.provider(t), nil, specindex.Config{DefaultBranch: "main"})
	_, err = spec.Enqueue(ctx, db, specindex.TriggerManual, nil)
	must(t, err)
	if ran, err := spec.RunOnce(ctx); err != nil || !ran {
		t.Fatalf("index ran=%v err=%v", ran, err)
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
