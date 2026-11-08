//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// FTR.HMR.DEV-0002 R13–R18: links of features in the index, in the header of a
// document, in the graph and in the agent's tool spec_links.
func TestSpecLinks(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "speclinks")
	must(t, err)
	defer drop()
	db, err := postgres.Connect(ctx, url)
	must(t, err)
	defer db.Close()
	must(t, postgres.Migrate(ctx, url))
	_, err = db.Exec(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('FMS', 'Fleet'), ('LOG', 'Logistics') RETURNING id, key)
		INSERT INTO systems (domain_id, key, name) SELECT id, CASE key WHEN 'FMS' THEN 'CAR' ELSE 'DLV' END, key FROM d`)
	must(t, err)

	const tariffs = "# Weekend tariffs\n\n**Версия:** 1.0\n**Опирается на:** `FTR.FMS.CAR-0002` (бронь), `FTR.FMS.CAR-0077`, `FTR.LOG.DLV-0003`\n\n---\n\n" +
		"## 4. Требования\n\n**R1.** Тариф выходного дня, см. `FTR.FMS.CAR-0002` R13.\n\n" +
		"## Изменения в других решениях\n\n| Решение | Требование | Что изменилось |\n|---|---|---|\n| `FTR.FMS.CAR-0002` | R13 | Отмена платная. |\n"
	repo := &fakeRepo{files: map[string]string{}, blobs: map[string]string{}}
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0002/product/spec.md", "# Booking\n\n## 4. Требования\n\n**R13.** Отмена бесплатна.\n")
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0042/product/spec.md", tariffs)
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0043/product/spec.md", "---\nparent: FTR.FMS.CAR-0042\n---\n# Fix of tariffs\n")
	repo.set("specs/LOG/DLV/FTR.LOG.DLV-0003/product/spec.md", "# Deliveries\n")
	svc := specindex.NewService(db, repo.provider(t), nil, specindex.Config{DefaultBranch: "main"})
	runCheck := func() {
		t.Helper()
		_, err := svc.Enqueue(ctx, db, specindex.TriggerManual, nil)
		must(t, err)
		if ran, err := svc.RunOnce(ctx); err != nil || !ran {
			t.Fatalf("the check ran=%v err=%v", ran, err)
		}
	}
	runCheck()

	links := func() []string {
		t.Helper()
		rows, err := db.Query(ctx, `SELECT source_key || ' ' || kind || ' ' || target_key || ' ' || array_to_string(requirements, ',')
			FROM spec_links ORDER BY 1`)
		must(t, err)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			must(t, rows.Scan(&s))
			out = append(out, strings.TrimSpace(s))
		}
		return out
	}
	want := []string{
		"FTR.FMS.CAR-0042 changes FTR.FMS.CAR-0002 R13",
		"FTR.FMS.CAR-0042 extends FTR.FMS.CAR-0002",
		"FTR.FMS.CAR-0042 extends FTR.FMS.CAR-0077",
		"FTR.FMS.CAR-0042 extends FTR.LOG.DLV-0003",
		"FTR.FMS.CAR-0043 changes FTR.FMS.CAR-0042",
	}
	if got := links(); !reflect.DeepEqual(got, want) {
		t.Fatalf("links %q", got)
	}

	// NAV-01, LINK-07: four lists by key; a key outside the index has no title.
	doc, err := svc.Document(ctx, "FTR.FMS.CAR-0042", "product")
	must(t, err)
	l := doc.Feature.Links
	if len(l.Extends) != 3 || l.Extends[1].Key != "FTR.FMS.CAR-0077" || l.Extends[1].Indexed || l.Extends[1].Title != nil ||
		!l.Extends[0].Indexed || l.Extends[0].Title == nil || *l.Extends[0].Title != "Booking" ||
		len(l.Changes) != 1 || strings.Join(l.Changes[0].Requirements, ",") != "R13" ||
		len(l.ExtendedBy) != 0 || len(l.ChangedBy) != 1 || l.ChangedBy[0].Key != "FTR.FMS.CAR-0043" {
		b, _ := json.Marshal(l)
		t.Fatalf("links of 0042 %s", b)
	}
	doc, err = svc.Document(ctx, "FTR.FMS.CAR-0002", "product")
	must(t, err)
	l = doc.Feature.Links
	if len(l.Extends) != 0 || len(l.Changes) != 0 || len(l.ExtendedBy) != 1 || len(l.ChangedBy) != 1 ||
		l.ChangedBy[0].Key != "FTR.FMS.CAR-0042" || strings.Join(l.ChangedBy[0].Requirements, ",") != "R13" {
		b, _ := json.Marshal(l)
		t.Fatalf("links of 0002 %s", b)
	}

	// LINK-09…LINK-12: the agent's tool spec_links (R18), read-only in every mode.
	var linksTool mcp.Tool
	for _, tl := range svc.Tools() {
		if tl.Name == "spec_links" {
			linksTool = tl
		}
	}
	allModes := []string{mcp.ModeGeneral, mcp.ModeSpec, mcp.ModeDiscovery, mcp.ModeGenerate, mcp.ModeCheck, mcp.ModeTask, mcp.ModeNabu}
	if linksTool.Handler == nil || !linksTool.ReadOnly || !reflect.DeepEqual(linksTool.Modes, allModes) {
		t.Fatalf("spec_links is offered read-only in every mode with Nabu: %+v", linksTool.Modes)
	}
	callLinks := func(args map[string]any) (string, error) {
		raw, _ := json.Marshal(args)
		return linksTool.Handler(ctx, mcp.Grant{}, raw)
	}
	linksOf := func(args map[string]any) specindex.LinksResult {
		t.Helper()
		out, err := callLinks(args)
		must(t, err)
		var r specindex.LinksResult
		must(t, json.Unmarshal([]byte(out), &r))
		return r
	}
	keys := func(refs []specindex.LinkRef) string {
		var out []string
		for _, r := range refs {
			out = append(out, r.Key+" "+strings.Join(r.Requirements, ","))
		}
		return strings.TrimSpace(strings.Join(out, ";"))
	}
	// LINK-09: by key, any case; the feature's own title and the four lists
	r9 := linksOf(map[string]any{"featureKey": "ftr.fms.car-0002"})
	if r9.FeatureKey != "FTR.FMS.CAR-0002" || !r9.Indexed || r9.Title == nil || *r9.Title != "Booking" ||
		keys(r9.ExtendedBy) != "FTR.FMS.CAR-0042" || keys(r9.ChangedBy) != "FTR.FMS.CAR-0042 R13" ||
		r9.ChangedBy[0].Title == nil || *r9.ChangedBy[0].Title != "Weekend tariffs" || len(r9.Extends)+len(r9.Changes) != 0 {
		t.Fatalf("spec_links of 0002 %+v", r9)
	}
	// LINK-10: one kind
	r10 := linksOf(map[string]any{"featureKey": "FTR.FMS.CAR-0042", "kind": "changes"})
	if keys(r10.Changes) != "FTR.FMS.CAR-0002 R13" || len(r10.Extends) != 0 || keys(r10.ChangedBy) != "FTR.FMS.CAR-0043" {
		t.Fatalf("spec_links of 0042, changes only %+v", r10)
	}
	// LINK-11: outside the index but referred to; an unknown key
	r11 := linksOf(map[string]any{"featureKey": "FTR.FMS.CAR-0077"})
	if r11.Indexed || r11.Title != nil || keys(r11.ExtendedBy) != "FTR.FMS.CAR-0042" {
		t.Fatalf("spec_links of a key outside the index %+v", r11)
	}
	if _, err := callLinks(map[string]any{"featureKey": "FTR.FMS.CAR-0099"}); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("an unknown key is not_found: %v", err)
	}
	if _, err := callLinks(map[string]any{"featureKey": "FTR.FMS.CAR-0002", "domain": "LOG"}); err == nil {
		t.Fatal("a key with a filter of the graph is refused")
	}
	// LINK-12: the graph narrowed by domain; an unknown kind
	out, err := callLinks(map[string]any{"domain": "log"})
	must(t, err)
	var graph struct {
		Features []struct {
			Key     string `json:"key"`
			Context bool   `json:"context"`
		} `json:"features"`
		Edges []specindex.GraphEdge `json:"edges"`
	}
	must(t, json.Unmarshal([]byte(out), &graph))
	if len(graph.Edges) != 1 || graph.Edges[0].Source != "FTR.FMS.CAR-0042" || graph.Edges[0].Target != "FTR.LOG.DLV-0003" ||
		len(graph.Features) != 2 || graph.Features[0].Key != "FTR.FMS.CAR-0042" || !graph.Features[0].Context || graph.Features[1].Context {
		t.Fatalf("spec_links graph of LOG %s", out)
	}
	if _, err := callLinks(map[string]any{"kind": "mentions"}); err == nil {
		t.Fatal("an unknown kind is refused")
	}

	// GRAPH-01: the domain LOG and the features linked to it as context.
	g, err := svc.SpecGraph(ctx, specindex.GraphQuery{Domain: "LOG"})
	must(t, err)
	nodes := map[string]specindex.GraphNode{}
	for _, n := range g.Nodes {
		nodes[n.Key] = n
	}
	if len(g.Nodes) != 2 || nodes["FTR.LOG.DLV-0003"].Context || !nodes["FTR.FMS.CAR-0042"].Context ||
		len(g.Edges) != 1 || g.Edges[0].Source != "FTR.FMS.CAR-0042" {
		t.Fatalf("graph LOG %+v", g)
	}
	// GRAPH-02: only "changes"; LINK-07: the whole graph has the key outside the index as a node.
	g, err = svc.SpecGraph(ctx, specindex.GraphQuery{Kind: "changes"})
	must(t, err)
	for _, e := range g.Edges {
		if e.Kind != "changes" {
			t.Fatalf("edge %+v", e)
		}
	}
	if len(g.Edges) != 2 || len(g.Nodes) != 4 {
		t.Fatalf("graph changes %+v", g)
	}
	g, err = svc.SpecGraph(ctx, specindex.GraphQuery{})
	must(t, err)
	if len(g.Edges) != 5 || len(g.Nodes) != 5 {
		t.Fatalf("whole graph %+v", g)
	}
	for _, n := range g.Nodes {
		if n.Key == "FTR.FMS.CAR-0077" && (n.Indexed || n.Domain != "FMS" || n.System != "CAR" || n.Context) {
			t.Fatalf("node outside the index %+v", n)
		}
	}
	if _, err := svc.SpecGraph(ctx, specindex.GraphQuery{Kind: "mentions"}); err == nil {
		t.Fatal("an unknown kind is refused")
	}

	// GRAPH-03: the same version of the index answers 304.
	r := chi.NewRouter()
	r.Route("/api/v1", svc.Routes)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v1/spec/graph", nil))
	etag := w.Header().Get("ETag")
	if w.Code != http.StatusOK || etag == "" || !strings.Contains(w.Body.String(), `"edges":[`) {
		t.Fatalf("graph %d %q", w.Code, w.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/spec/graph?domain=FMS", nil)
	req.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotModified {
		t.Fatalf("graph with If-None-Match %d", w.Code)
	}

	// LINK-06: the backfill fills an empty table with the same links, and once only.
	_, err = db.Exec(ctx, `DELETE FROM spec_links`)
	must(t, err)
	n, err := svc.BackfillLinks(ctx)
	must(t, err)
	if got := links(); n != 4 || !reflect.DeepEqual(got, want) {
		t.Fatalf("backfill %d %q", n, got)
	}
	if n, err := svc.BackfillLinks(ctx); err != nil || n != 0 {
		t.Fatalf("second backfill %d %v", n, err)
	}

	// LINK-05: a key removed from «Опирается на» leaves the index with the next indexing.
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0042/product/spec.md", strings.Replace(tariffs, ", `FTR.LOG.DLV-0003`", "", 1))
	runCheck()
	if got := links(); !reflect.DeepEqual(got, []string{want[0], want[1], want[2], want[4]}) {
		t.Fatalf("links after the edit %q", got)
	}
	// A feature that leaves the index takes its links with it; links to it stay (R17).
	repo.remove("specs/FMS/CAR/FTR.FMS.CAR-0042/")
	runCheck()
	if got := links(); !reflect.DeepEqual(got, []string{want[4]}) {
		t.Fatalf("links after the removal %q", got)
	}
}
