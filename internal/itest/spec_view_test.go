//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// FTR.HMR.SPEC-0001: files of an area open as pages of their own in an opaque
// origin; the list of files says which of them can.
func TestSpecFileView(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "specview")
	must(t, err)
	defer drop()
	db, err := postgres.Connect(ctx, url)
	must(t, err)
	defer db.Close()
	must(t, postgres.Migrate(ctx, url))
	_, err = db.Exec(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('FMS', 'Fleet') RETURNING id)
		INSERT INTO systems (domain_id, key, name) SELECT id, 'CAR', 'Cars' FROM d`)
	must(t, err)

	const page = "<html><script>fetch('/api/v1/me')</script></html>"
	repo := &fakeRepo{files: map[string]string{}, blobs: map[string]string{}}
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0042/product/spec.md", "# Weekend tariffs\n")
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0042/design/mockups.html", page)
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0042/design/logo.png", "\x89PNG")
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0042/design/flows.pdf", "%PDF-1.7")
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0042/design/big & wide.html", "<html>"+strings.Repeat("x", 200)+"</html>")
	svc := specindex.NewService(db, repo.provider(t), nil, specindex.Config{DefaultBranch: "main", PreviewMaxBytes: 128})
	_, err = svc.Enqueue(ctx, db, specindex.TriggerManual, nil)
	must(t, err)
	if _, err := svc.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Route("/api/v1", svc.Routes)
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}

	// API-04: viewUrl only for files that can be viewed; the name is escaped.
	w := get("/api/v1/spec/files/FTR.FMS.CAR-0042/design")
	var list struct {
		Files []map[string]any `json:"files"`
	}
	must(t, json.Unmarshal(w.Body.Bytes(), &list))
	views := map[string]string{}
	for _, f := range list.Files {
		v, _ := f["viewUrl"].(string)
		views[f["name"].(string)] = v
	}
	if views["mockups.html"] != "/api/v1/spec/files/FTR.FMS.CAR-0042/design/raw?mode=view&name=mockups.html" ||
		views["logo.png"] == "" || views["flows.pdf"] != "" || views["big & wide.html"] != "" || len(views) != 4 {
		t.Fatalf("views %v", views)
	}

	// API-05: HTML as a page sandboxed into an opaque origin.
	w = get(views["mockups.html"])
	h := w.Header()
	if w.Code != http.StatusOK || h.Get("Content-Type") != "text/html; charset=utf-8" ||
		h.Get("Content-Security-Policy") != "sandbox allow-scripts" || h.Get("Referrer-Policy") != "no-referrer" ||
		h.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(h.Get("Content-Disposition"), "inline") ||
		w.Body.String() != page {
		t.Fatalf("view html %d %v %q", w.Code, h, w.Body.String())
	}

	// API-06: an image in view mode is served as in preview mode.
	w = get(views["logo.png"])
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "image/png" ||
		w.Header().Get("Content-Security-Policy") != "default-src 'none'; style-src 'unsafe-inline'" {
		t.Fatalf("view png %d %v", w.Code, w.Header())
	}

	// API-07: over the limit — 413.
	w = get("/api/v1/spec/files/FTR.FMS.CAR-0042/design/raw?mode=view&name=big+%26+wide.html")
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "spec_file_too_large") {
		t.Fatalf("too large %d %s", w.Code, w.Body.String())
	}

	// API-08: other types are downloads.
	w = get("/api/v1/spec/files/FTR.FMS.CAR-0042/design/raw?mode=view&name=flows.pdf")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/octet-stream" ||
		!strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("view pdf %d %v", w.Code, w.Header())
	}

	// API-10: preview mode for HTML is unchanged for one release.
	w = get("/api/v1/spec/files/FTR.FMS.CAR-0042/design/raw?mode=preview&name=mockups.html")
	var preview map[string]string
	must(t, json.Unmarshal(w.Body.Bytes(), &preview))
	if w.Code != http.StatusOK || preview["html"] != page {
		t.Fatalf("preview %d %s", w.Code, w.Body.String())
	}
}
