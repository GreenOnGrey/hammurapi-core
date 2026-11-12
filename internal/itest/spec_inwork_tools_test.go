//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// FTR.HMR.SPEC-0006 qa SPB-01…SPB-09: spec_read, spec_requirements and
// spec_links answer for a feature in work from its branch.
func TestSpecToolsFeatureInWork(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "specinwork")
	must(t, err)
	defer drop()
	db, err := postgres.Connect(ctx, url)
	must(t, err)
	defer db.Close()
	must(t, postgres.Migrate(ctx, url))
	_, err = db.Exec(ctx, `WITH d AS (INSERT INTO domains (key, name, approval_required) VALUES ('FMS', 'Fleet', true) RETURNING id)
		INSERT INTO systems (domain_id, key, name) SELECT id, 'CAR', 'Cars' FROM d`)
	must(t, err)
	var user uuid.UUID
	must(t, db.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, agent_name, agent_tone)
		VALUES ('iw1', 'ira', 'Ira', 'Hammurapi', 'business') RETURNING id`).Scan(&user))
	var sysID uuid.UUID
	must(t, db.QueryRow(ctx, `SELECT id FROM systems WHERE key = 'CAR'`).Scan(&sysID))
	store := specdata.NewPG(db)
	nf := &specdata.Feature{UniqueID: "FTR.FMS.CAR-0042", SystemID: sysID, Number: 42, Title: "Тарифы", Branch: "feature/FTR.FMS.CAR-0042",
		PRNumber: 7, PRURL: "http://pr", CreatedBy: user}
	must(t, store.InsertFeature(ctx, nf))
	must(t, store.InsertGate(ctx, &specdata.Gate{FeatureID: nf.ID, Area: domain.AreaProduct, Status: domain.GateApproved, HeadCommit: "h1", CreatedBy: user}))
	must(t, store.InsertGate(ctx, &specdata.Gate{FeatureID: nf.ID, Area: domain.AreaArch, Status: domain.GateDraft, HeadCommit: "h1", CreatedBy: user}))
	// FTR.FMS.CAR-0002 is in the index of the default branch.
	_, err = db.Exec(ctx, `INSERT INTO spec_documents (path, feature_key, domain, system, area, blob_sha, commit_sha, title, markdown, search_text, search_vector)
		VALUES ($1, 'FTR.FMS.CAR-0002', 'FMS', 'CAR', 'product', 'b', 'c', 'Бронь', $2, '', ''::tsvector)`, pathBooking, booking)
	must(t, err)

	repo := newBranchRepo(map[string]string{pathBooking: booking})
	repo.branch(nf.Branch, nf.PRNumber)
	repo.set(nf.Branch, pathTariffs, tariffs(rowR13))
	repo.set(nf.Branch, pathTariffsArch, tariffsArch("", rowA2))
	gp := repo.provider(t)
	gp.EXPECT().BotToken(gomock.Any()).Return("bot", nil).AnyTimes()
	svc := specindex.NewService(db, gp, nil, specindex.Config{DefaultBranch: "main"})

	tools := map[string]mcp.Tool{}
	for _, tl := range svc.Tools() {
		tools[tl.Name] = tl
	}
	call := func(name string, args any) (map[string]any, error) {
		t.Helper()
		raw, _ := json.Marshal(args)
		out, err := tools[name].Handler(ctx, mcp.Grant{Mode: mcp.ModeTask, Feature: "FTR.FMS.CAR-0042"}, raw)
		if err != nil {
			return nil, err
		}
		var v map[string]any
		must(t, json.Unmarshal([]byte(out), &v))
		return v, nil
	}

	// SPB-01: the whole document of the branch, marked as such.
	r, err := call("spec_read", map[string]any{"featureKey": "FTR.FMS.CAR-0042", "area": "product"})
	must(t, err)
	if r["source"] != specindex.SourceBranch || r["branch"] != nf.Branch || !strings.Contains(r["markdown"].(string), "Тарифы выходного дня") {
		t.Fatalf("SPB-01 %+v", r)
	}
	// SPB-02: one section, as from the index.
	r, err = call("spec_read", map[string]any{"featureKey": "FTR.FMS.CAR-0042", "area": "product", "section": "Изменения в других решениях"})
	must(t, err)
	if md := r["markdown"].(string); !strings.Contains(md, "R13") || strings.Contains(md, "**R4.**") {
		t.Fatalf("SPB-02 section %q", md)
	}
	// SPB-03: requirements of product; decisions of arch with kind decision.
	r, err = call("spec_requirements", map[string]any{"featureKey": "FTR.FMS.CAR-0042"})
	must(t, err)
	reqs, _ := r["requirements"].([]any)
	if r["source"] != specindex.SourceBranch || len(reqs) != 1 || reqs[0].(map[string]any)["id"] != "R4" {
		t.Fatalf("SPB-03 requirements %+v", r)
	}
	r, err = call("spec_requirements", map[string]any{"featureKey": "FTR.FMS.CAR-0042", "kind": "decision"})
	must(t, err)
	if reqs, _ := r["requirements"].([]any); len(reqs) != 1 || reqs[0].(map[string]any)["id"] != "A1" {
		t.Fatalf("SPB-03 decisions %+v", r)
	}
	// SPB-04: the links of the documents of the branch, both layers, the linked feature indexed.
	r, err = call("spec_links", map[string]any{"featureKey": "FTR.FMS.CAR-0042"})
	must(t, err)
	changes, _ := r["changes"].([]any)
	extends, _ := r["extends"].([]any)
	if r["source"] != specindex.SourceBranch || len(changes) != 2 || len(extends) != 2 {
		t.Fatalf("SPB-04 links %+v", r)
	}
	if c := changes[0].(map[string]any); c["key"] != "FTR.FMS.CAR-0002" || c["indexed"] != true {
		t.Fatalf("SPB-04 change %+v", c)
	}
	r, err = call("spec_links", map[string]any{"featureKey": "FTR.FMS.CAR-0042", "layer": "arch"})
	must(t, err)
	if changes, _ := r["changes"].([]any); len(changes) != 1 || changes[0].(map[string]any)["layer"] != "arch" {
		t.Fatalf("SPB-04 arch layer %+v", r)
	}
	// SPB-05: the personal agent of any user reads a feature in work too (product 1.1 R2).
	raw, _ := json.Marshal(map[string]any{"featureKey": "FTR.FMS.CAR-0042", "area": "arch"})
	out, err := tools["spec_read"].Handler(ctx, mcp.Grant{Mode: mcp.ModeNabu, UserID: user}, raw)
	must(t, err)
	if !strings.Contains(out, `"source":"branch"`) || !strings.Contains(out, "Тарифы — архитектура") {
		t.Fatalf("SPB-05 %s", out)
	}
	// SPB-08: the search knows only the default branch.
	r, err = call("spec_search", map[string]any{"query": "FTR.FMS.CAR-004"})
	must(t, err)
	if r["total"].(float64) != 0 {
		t.Fatalf("SPB-08 a feature in work in the search: %+v", r)
	}
	// SPB-06: a key that is neither in the index nor a feature answers as before.
	if _, err := call("spec_read", map[string]any{"featureKey": "FTR.FMS.CAR-0099", "area": "product"}); err == nil ||
		!strings.Contains(err.Error(), "is not in the specification index") {
		t.Fatalf("SPB-06 %v", err)
	}
	// SPB-07: an indexed feature answers from the index.
	r, err = call("spec_read", map[string]any{"featureKey": "FTR.FMS.CAR-0002", "area": "product"})
	must(t, err)
	if r["source"] != specindex.SourceIndex || r["branch"] != nil {
		t.Fatalf("SPB-07 %+v", r)
	}
	// SPB-09: a new revision (another head of the gate) is read again; the same one comes from the cache.
	repo.set(nf.Branch, pathTariffs, strings.Replace(tariffs(rowR13), "Тарифы выходного дня", "Тарифы праздников", 1))
	r, err = call("spec_read", map[string]any{"featureKey": "FTR.FMS.CAR-0042", "area": "product"})
	must(t, err)
	if !strings.Contains(r["markdown"].(string), "Тарифы выходного дня") {
		t.Fatalf("SPB-09 the same revision is cached: %q", r["markdown"])
	}
	_, err = db.Exec(ctx, `UPDATE gates SET head_commit = 'h2' WHERE feature_id = $1 AND area = 'product'`, nf.ID)
	must(t, err)
	r, err = call("spec_read", map[string]any{"featureKey": "FTR.FMS.CAR-0042", "area": "product"})
	must(t, err)
	if !strings.Contains(r["markdown"].(string), "Тарифы праздников") {
		t.Fatalf("SPB-09 a new revision is read: %q", r["markdown"])
	}
	// A released feature is not read from its branch.
	_, err = db.Exec(ctx, `UPDATE features SET phase = 'released' WHERE id = $1`, nf.ID)
	must(t, err)
	if _, err := call("spec_read", map[string]any{"featureKey": "FTR.FMS.CAR-0042", "area": "product"}); err == nil {
		t.Fatal("a released feature is read from its branch")
	}
}
