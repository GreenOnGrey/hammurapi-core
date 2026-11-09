//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/GreenOnGrey/speclint/contract"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/approvals"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gategen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gates"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specchanges"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
)

const (
	pathBookingArch = "specs/FMS/CAR/FTR.FMS.CAR-0002/arch/spec.md"
	pathTariffsArch = "specs/FMS/CAR/FTR.FMS.CAR-0042/arch/spec.md"
	bookingArch     = "# Бронь — архитектура\n\n**Опирается на:** `FTR.FMS.CAR-0001` (A1)\n\n---\n\n## Принятые решения\n\n" +
		"| A | Решение | Обоснование |\n|---|---|---|\n| A1 | Бронь в Postgres | Транзакции |\n| A2 | Отмена — событие Kafka | Подписчики |\n"
	noteA2 = "- *A2 — изменено в `FTR.FMS.CAR-0042` A1: Отмена — команда, а не событие.*"
)

// tariffsArch is the arch document of FTR.FMS.CAR-0042 with rows of its section of changes.
func tariffsArch(decisions string, rows ...string) string {
	if decisions == "" {
		decisions = "| A1 | Отмена — команда сервису тарифов вместо события `FTR.FMS.CAR-0002-A2` | Нужен ответ с ценой |\n"
	}
	return "# Тарифы — архитектура\n\n**Опирается на:** `FTR.FMS.CAR-0002` (A1)\n\n---\n\n" +
		"## Изменения в других решениях\n\n| Решение | Архитектурное решение | Что изменилось |\n|---|---|---|\n" + strings.Join(rows, "\n") + "\n\n" +
		"## Принятые решения\n\n| A | Решение | Обоснование |\n|---|---|---|\n" + decisions
}

const (
	rowA2    = "| `FTR.FMS.CAR-0002` | A2 | Отмена — команда, а не событие. |"
	rowA99   = "| `FTR.FMS.CAR-0002` | A99 | Нет такого. |"
	rowArchR = "| `FTR.FMS.CAR-0002` | R13 | Требование в arch. |"
)

// FTR.HMR.DEV-0003 R10–R16: architecture decisions A<n> — the index, the notes
// under the table of decisions, the block of changes of arch, the checks of the
// submission and of the writer, the layer of links.
func TestSpecArchChanges(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "specarch")
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
	idx.set(pathCatalog, catalog)
	spec := specindex.NewService(db, idx.provider(t), nil, specindex.Config{DefaultBranch: "main", AgentReadMaxChars: 40000})
	index := func() {
		t.Helper()
		_, err := spec.Enqueue(ctx, db, specindex.TriggerManual, nil)
		must(t, err)
		if ran, err := spec.RunOnce(ctx); err != nil || !ran {
			t.Fatalf("index ran=%v err=%v", ran, err)
		}
	}
	index()

	// ARCH-01: the decisions of arch are in the index beside the requirements, with their kind.
	var kinds string
	must(t, db.QueryRow(ctx, `SELECT string_agg(req_id || ':' || kind, ',' ORDER BY kind, req_id) FROM spec_requirements
		WHERE feature_key = 'FTR.FMS.CAR-0002'`).Scan(&kinds))
	if kinds != "A1:decision,A2:decision,R13:requirement,R5:requirement" {
		t.Fatalf("items %s", kinds)
	}

	expert := tu.User("expert:FMS:arch", "expert:FMS:product")
	must(t, db.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, agent_name, agent_tone)
		VALUES ('arch1', 'oleg', 'Oleg', 'Hammurapi', 'business') RETURNING id`).Scan(&expert.UserID))
	store := specdata.NewPG(db)
	var sysID uuid.UUID
	must(t, db.QueryRow(ctx, `SELECT id FROM systems WHERE key = 'CAR'`).Scan(&sysID))
	nf := &specdata.Feature{UniqueID: "FTR.FMS.CAR-0042", SystemID: sysID, Number: 42, Title: "Тарифы", Branch: "feature/FTR.FMS.CAR-0042",
		PRNumber: 7, PRURL: "http://pr", CreatedBy: expert.UserID}
	must(t, store.InsertFeature(ctx, nf))
	for _, a := range []domain.Area{domain.AreaProduct, domain.AreaArch} {
		must(t, store.InsertGate(ctx, &specdata.Gate{FeatureID: nf.ID, Area: a, Status: domain.GateDraft, HeadCommit: "head", CreatedBy: expert.UserID}))
	}
	f, err := store.FeatureByID(ctx, nf.ID)
	must(t, err)

	repo := newBranchRepo(map[string]string{pathBooking: booking, pathBookingArch: bookingArch, pathCatalog: catalog})
	repo.branch(f.Branch, f.PRNumber)
	repo.set(f.Branch, pathTariffsArch, tariffsArch(""))
	gp := repo.provider(t)
	tokens := mocks.NewMockTokenSource(gomock.NewController(t))
	tokens.EXPECT().Token(gomock.Any(), gomock.Any()).Return("tok", nil).AnyTimes()
	changes := &specchanges.Service{Q: db, Git: gp, Lang: contract.RU}
	gs := gates.NewService(store, gp, tokens, nopEvents{}, nil, "main")
	gs.Changes = changes
	items := func() map[string]specchanges.Item {
		t.Helper()
		list, err := gs.SpecChanges(ctx, expert, f.UniqueID, domain.AreaArch)
		must(t, err)
		out := map[string]specchanges.Item{}
		for _, it := range list {
			k := it.Feature
			if it.Item != nil {
				k += " " + *it.Item
			}
			out[k] = it
		}
		return out
	}

	// ARCH-10, ARCH-14: one commit with arch and the note right under the table of decisions of the target;
	// the table and the version of the target are the same.
	_, err = gs.SaveDocument(ctx, expert, f.UniqueID, domain.AreaArch, gates.SaveInput{Content: tariffsArch("", rowA2)})
	must(t, err)
	cs := repo.commitsTo(f.Branch)
	if len(cs) != 1 || strings.Join(cs[0].paths, ",") != pathTariffsArch+","+pathBookingArch {
		t.Fatalf("commits %+v", cs)
	}
	noted := repo.file(f.Branch, pathBookingArch)
	if !strings.Contains(noted, "| A2 | Отмена — событие Kafka | Подписчики |\n\n"+noteA2+"\n") || !strings.HasPrefix(noted, bookingArch[:len(bookingArch)-1]) {
		t.Fatalf("target\n%s", noted)
	}
	// ARCH-19: the block of arch: the row is marked, with a link to the decision.
	if it := items()["FTR.FMS.CAR-0002 A2"]; it.State != "marked" || it.Kind != "decision" || it.URL != "/spec/FTR.FMS.CAR-0002/arch#a2" {
		t.Fatalf("item %+v", it)
	}

	// LANG-03: a note in Russian is marked when the language of the documents is English.
	gs.Changes = &specchanges.Service{Q: db, Git: gp, Lang: contract.EN}
	if it := items()["FTR.FMS.CAR-0002 A2"]; it.State != "marked" {
		t.Fatalf("a Russian note with English documents %+v", it)
	}
	gs.Changes = changes

	// ARCH-06, ARCH-07: R<n> in arch is of the wrong kind, A99 does not exist; the submission is refused.
	must(t, store.ReleaseLock(ctx, f.ID, expert.UserID))
	_, err = gs.SaveDocument(ctx, expert, f.UniqueID, domain.AreaArch, gates.SaveInput{Content: tariffsArch("", rowA2, rowA99, rowArchR)})
	must(t, err)
	got := items()
	if got["FTR.FMS.CAR-0002 A99"].Error == nil || got["FTR.FMS.CAR-0002 A99"].Error.Reason != "unknown_decision" ||
		got["FTR.FMS.CAR-0002 R13"].Error == nil || got["FTR.FMS.CAR-0002 R13"].Error.Reason != "wrong_kind" {
		b, _ := json.Marshal(got)
		t.Fatalf("items %s", b)
	}
	as := approvals.NewService(store, approvals.NewRepository(db), gp, tokens, nopEvents{}, gategen.Generator{Q: db})
	as.Changes = changes
	_, err = as.Submit(ctx, expert, f.UniqueID, domain.AreaArch, true, true)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != "changes_invalid" {
		t.Fatalf("submit with wrong rows %v", err)
	}

	// ARCH-20: decisions without A<n> are refused unless forced.
	repo.set(f.Branch, pathTariffsArch, tariffsArch("| 1 | Без номера | — |\n", rowA2))
	_, err = as.Submit(ctx, expert, f.UniqueID, domain.AreaArch, false, true)
	if !errors.As(err, &ae) || ae.Code != "decisions_without_id" {
		t.Fatalf("submit with an unnumbered decision %v", err)
	}

	// CTR-12, ARCH-07: the writer's arch without A<n> and with a row A99 is not taken.
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
	sunk := 0
	grant := mcp.Grant{Feature: f.UniqueID, Writer: &mcp.WriterScope{Mode: "write", Areas: []domain.Area{domain.AreaArch}},
		Sink: func(string, json.RawMessage) error { sunk++; return nil }}
	var te *mcp.ToolError
	for _, doc := range []string{tariffsArch("| 1 | Без номера | — |\n", rowA2), tariffsArch("", rowA99)} {
		raw, _ := json.Marshal(map[string]any{"area": "arch", "content": doc})
		if _, err := tools["submit_gate"].Handler(ctx, grant, raw); !errors.As(err, &te) || sunk != 0 {
			t.Fatalf("submit_gate %v, sunk %d", err, sunk)
		}
	}
	raw, _ := json.Marshal(map[string]any{"area": "arch", "content": tariffsArch("", rowA2)})
	if _, err := tools["submit_gate"].Handler(ctx, grant, raw); err != nil || sunk != 1 {
		t.Fatalf("a right arch is taken: %v, sunk %d", err, sunk)
	}

	// ARCH-15, ARCH-16, ARCH-18, ARCH-01: after the merge the links of arch are of the layer arch.
	idx.set(pathTariffsArch, tariffsArch("", rowA2))
	idx.set(pathBookingArch, noted)
	index()
	d, err := spec.Document(ctx, "FTR.FMS.CAR-0002", "arch")
	must(t, err)
	if len(d.Feature.Links.ChangedBy) != 1 || d.Feature.Links.ChangedBy[0].Key != "FTR.FMS.CAR-0042" ||
		strings.Join(d.Feature.Links.ChangedBy[0].Items, ",") != "A2" || d.Feature.Links.ChangedBy[0].Layer != "arch" {
		t.Fatalf("links of arch %+v", d.Feature.Links)
	}
	d, err = spec.Document(ctx, "FTR.FMS.CAR-0002", "product")
	must(t, err)
	if len(d.Feature.Links.ChangedBy) != 0 {
		t.Fatalf("links of product %+v", d.Feature.Links)
	}
	g, err := spec.SpecGraph(ctx, specindex.GraphQuery{})
	must(t, err)
	if len(g.Edges) != 0 {
		t.Fatalf("product graph %+v", g.Edges)
	}
	g, err = spec.SpecGraph(ctx, specindex.GraphQuery{Layer: "arch"})
	must(t, err)
	edges := []string{}
	for _, e := range g.Edges {
		edges = append(edges, fmt.Sprintf("%s %s %s %s %v", e.Source, e.Kind, e.Target, e.Layer, e.Items))
	}
	if strings.Join(edges, "; ") != "FTR.FMS.CAR-0002 extends FTR.FMS.CAR-0001 arch [A1]; FTR.FMS.CAR-0042 changes FTR.FMS.CAR-0002 arch [A2]; FTR.FMS.CAR-0042 extends FTR.FMS.CAR-0002 arch [A1]" {
		t.Fatalf("arch graph %s", strings.Join(edges, "; "))
	}
	l, err := spec.LinksOf(ctx, "FTR.FMS.CAR-0042", "", "arch")
	must(t, err)
	if len(l.Changes) != 1 || l.Changes[0].Layer != "arch" {
		t.Fatalf("spec_links %+v", l)
	}
	reqs, err := spec.FeatureRequirements(ctx, "FTR.FMS.CAR-0002", nil, "decision")
	must(t, err)
	if len(reqs) != 2 || reqs[0].ID != "A1" {
		t.Fatalf("decisions %+v", reqs)
	}
	// MIG-02: spec_references finds a reference to a decision by its key.
	refs, err := spec.References(ctx, "FTR.FMS.CAR-0002-A2", 1)
	must(t, err)
	if b, _ := json.Marshal(refs); !strings.Contains(string(b), "FTR.FMS.CAR-0042") {
		t.Fatalf("references %s", b)
	}
}
