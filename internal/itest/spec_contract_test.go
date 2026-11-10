//go:build integration

package itest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/approvals"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gategen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/speccontract"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/trace"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
)

const (
	ctrDir  = "specs/FMS/CAR/FTR.FMS.CAR-0050/"
	ctrArch = "# Отмена — архитектура\n\n**Опирается на:** `FTR.FMS.CAR-0001` (A1)\n\n" +
		"## Затрагиваемые сервисы и контракты\n\n| Сервис | Роль в фиче | Меняющиеся контракты |\n|---|---|---|\n| `booking` | Бронь | `POST /bookings/{id}/cancel` |\n\n" +
		"## Схема C4\n\n```mermaid\nC4Container\n  Person(client, \"Клиент\")\n  Container(booking, \"Бронь\", \"Go\")\n  Rel(client, booking, \"отменяет\", \"HTTPS\")\n```\n\n" +
		"## Принятые решения\n\n| A | Решение | Обоснование |\n|---|---|---|\n| A1 | Отмена — команда | Ответ с ценой |\n"
	ctrTech = "# Отмена — техническая спецификация\n\n## Изменения по сервисам\n\n| Сервис | Репозиторий | Что меняется | Требования |\n|---|---|---|---|\n" +
		"| `booking` | org/booking | Отмена | R1 |\n"
	ctrProduct = "# Отмена\n\n**R1.** Клиент отменяет бронь.\n\n- Дано бронь, когда клиент отменяет, тогда она отменена.\n"
	ctrYAML    = "openapi: 3.0.3\ninfo:\n  title: booking\n  version: \"1\"\npaths:\n  /bookings/{id}/cancel:\n    post: {}\n"
)

// FTR.HMR.SPEC-0004: the submission of arch and tech checks the services, the C4 diagram and the contract files;
// the projection takes the services of arch; a code task gets the contract of its service.
func TestSpecContract(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "speccontract")
	must(t, err)
	defer drop()
	db, err := postgres.Connect(ctx, url)
	must(t, err)
	defer db.Close()
	must(t, postgres.Migrate(ctx, url))
	_, err = db.Exec(ctx, `WITH d AS (INSERT INTO domains (key, name, approval_required) VALUES ('FMS', 'Fleet', true) RETURNING id)
		INSERT INTO systems (domain_id, key, name) SELECT id, 'CAR', 'Cars' FROM d`)
	must(t, err)
	_, err = db.Exec(ctx, `INSERT INTO services (key, name, repo) VALUES ('booking', 'Booking', 'org/booking'), ('pricing', 'Pricing', 'org/pricing'),
		('legacy', 'Legacy', 'org/legacy')`)
	must(t, err)
	_, err = db.Exec(ctx, `UPDATE services SET deleted_in_catalog = true WHERE key = 'legacy'`)
	must(t, err)

	expert := tu.User("expert:FMS:arch", "expert:FMS:product")
	must(t, db.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, agent_name, agent_tone)
		VALUES ('ctr1', 'olga', 'Olga', 'Hammurapi', 'business') RETURNING id`).Scan(&expert.UserID))
	store := specdata.NewPG(db)
	var sysID uuid.UUID
	must(t, db.QueryRow(ctx, `SELECT id FROM systems WHERE key = 'CAR'`).Scan(&sysID))
	nf := &specdata.Feature{UniqueID: "FTR.FMS.CAR-0050", SystemID: sysID, Number: 50, Title: "Отмена", Branch: "feature/FTR.FMS.CAR-0050",
		PRNumber: 9, PRURL: "http://pr", CreatedBy: expert.UserID}
	must(t, store.InsertFeature(ctx, nf))
	must(t, store.InsertGate(ctx, &specdata.Gate{FeatureID: nf.ID, Area: domain.AreaArch, Status: domain.GateDraft, HeadCommit: "head", CreatedBy: expert.UserID}))
	f, err := store.FeatureByID(ctx, nf.ID)
	must(t, err)

	repo := newBranchRepo(map[string]string{})
	repo.branch(f.Branch, f.PRNumber)
	gp := repo.provider(t)
	gp.EXPECT().Tree(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, _, ref, dir string) ([]git.TreeEntry, error) {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		var out []git.TreeEntry
		for p, c := range repo.files(ref) {
			if strings.HasPrefix(p, dir+"/") {
				out = append(out, git.TreeEntry{Path: p, SHA: git.BlobSHA([]byte(c)), Size: int64(len(c))})
			}
		}
		return out, nil
	}).AnyTimes()
	tokens := mocks.NewMockTokenSource(gomock.NewController(t))
	tokens.EXPECT().Token(gomock.Any(), gomock.Any()).Return("tok", nil).AnyTimes()
	checker := &speccontract.Checker{Git: gp, Pool: db, MaxBytes: 1 << 20}
	violations := func(area domain.Area) []speccontract.Violation {
		t.Helper()
		err := checker.Check(ctx, "tok", f, area)
		if err == nil {
			return nil
		}
		var ae *apperr.Error
		if !errors.As(err, &ae) || ae.Code != "spec_contract" || ae.Status != 422 {
			t.Fatalf("not spec_contract: %v", err)
		}
		vs, _ := ae.Details["violations"].([]speccontract.Violation)
		return vs
	}
	set := func(rel, content string) { repo.set(f.Branch, ctrDir+rel, content) }
	del := func(rel string) {
		repo.mu.Lock()
		defer repo.mu.Unlock()
		delete(repo.files(f.Branch), ctrDir+rel)
	}

	// SVC-03: a key not in the catalog, refused by the submission with the nearest key; a deleted key is unknown too.
	set("arch/spec.md", strings.Replace(ctrArch, "| `booking` | Бронь", "| `bookng` | Бронь", 1))
	as := approvals.NewService(store, approvals.NewRepository(db), gp, tokens, nopEvents{}, gategen.Generator{Q: db})
	as.Contract = checker
	_, err = as.Submit(ctx, expert, f.UniqueID, domain.AreaArch, true, true)
	var ae *apperr.Error
	if !errors.As(err, &ae) || ae.Code != "spec_contract" {
		t.Fatalf("SVC-03 submit %v", err)
	}
	vs, _ := ae.Details["violations"].([]speccontract.Violation)
	if len(vs) != 2 || vs[0].Check != "c4-service-missing" || vs[1].Check != "arch-service-unknown" || vs[1].Suggest != "booking" || vs[1].Line != 9 {
		t.Fatalf("SVC-03 %+v", vs)
	}
	set("arch/spec.md", strings.NewReplacer("| `booking` | Бронь", "| `legacy` | Бронь", "Container(booking", "Container(legacy", "Rel(client, booking", "Rel(client, legacy").Replace(ctrArch))
	if vs := violations(domain.AreaArch); len(vs) != 1 || vs[0].Check != "arch-service-unknown" {
		t.Fatalf("a deleted service %+v", vs)
	}

	// TPL-03: an arch without the diagram is refused with c4-missing.
	set("arch/spec.md", ctrArch[:strings.Index(ctrArch, "## Схема C4")]+ctrArch[strings.Index(ctrArch, "## Принятые решения"):])
	if vs := violations(domain.AreaArch); len(vs) != 1 || vs[0].Check != "c4-missing" {
		t.Fatalf("TPL-03 %+v", vs)
	}

	// SVC-05: arch with booking passes, and the projection links booking while the feature is at the gate arch.
	set("arch/spec.md", ctrArch)
	set("product/spec.md", ctrProduct)
	if vs := violations(domain.AreaArch); vs != nil {
		t.Fatalf("SVC-05 valid arch %+v", vs)
	}
	project := func() []string {
		t.Helper()
		docs, err := trace.Read(ctx, gp, "tok", trace.Feature{ID: f.ID, Key: f.UniqueID, DomainKey: f.DomainKey, SystemKey: f.SystemKey, Ref: f.Branch})
		must(t, err)
		_, err = trace.Project(ctx, db, f.ID, docs)
		must(t, err)
		var keys []string
		rows, err := db.Query(ctx, `SELECT s.key FROM feature_services fs JOIN services s ON s.id = fs.service_id WHERE fs.feature_id = $1 ORDER BY s.key`, f.ID)
		must(t, err)
		defer rows.Close()
		for rows.Next() {
			var k string
			must(t, rows.Scan(&k))
			keys = append(keys, k)
		}
		return keys
	}
	if got := project(); strings.Join(got, ",") != "booking" {
		t.Fatalf("SVC-05 feature_services %v", got)
	}

	// SVC-06: tech names pricing, which arch does not: refused, and the projection keeps the services of arch.
	set("tech/spec.md", ctrTech+"| `pricing` | org/pricing | Цена | R1 |\n")
	if vs := violations(domain.AreaTech); len(vs) != 1 || vs[0].Check != "tech-service-not-in-arch" || vs[0].Line != 8 {
		t.Fatalf("SVC-06 %+v", vs)
	}
	if got := project(); strings.Join(got, ",") != "booking" {
		t.Fatalf("SVC-06 feature_services %v", got)
	}
	var reqSvc string
	must(t, db.QueryRow(ctx, `SELECT string_agg(s.key, ',') FROM requirement_services rs JOIN services s ON s.id = rs.service_id WHERE rs.feature_id = $1`, f.ID).Scan(&reqSvc))
	if reqSvc != "booking" {
		t.Fatalf("SVC-06 requirement services %s", reqSvc)
	}

	// FILE-01: a contract of booking passes; FILE-03: a YAML error names its line; a contract of another service fails.
	set("tech/spec.md", ctrTech)
	set("tech/booking.openapi.yaml", ctrYAML)
	if vs := violations(domain.AreaTech); vs != nil {
		t.Fatalf("FILE-01 %+v", vs)
	}
	set("tech/booking.openapi.yaml", "openapi: 3.0.3\npaths:\n  /a:\n    get: [unclosed\n")
	set("tech/pricing.proto", "syntax = \"proto3\";\nmessage A {}\n")
	vs = violations(domain.AreaTech)
	if len(vs) != 2 || vs[0].Check != "contract-file-invalid" || vs[0].File != "tech/booking.openapi.yaml" || vs[0].Line != 3 ||
		vs[1].Check != "contract-file-service" || vs[1].File != "tech/pricing.proto" {
		t.Fatalf("FILE-03 %+v", vs)
	}
	set("tech/booking.openapi.yaml", "openapi: 3.0.3\npaths: []\n")
	if vs := violations(domain.AreaTech); len(vs) != 2 || vs[0].Message != "paths is not a mapping" {
		t.Fatalf("paths must be a mapping %+v", vs)
	}
	del("tech/pricing.proto")
	set("tech/booking.openapi.yaml", ctrYAML)

	// GEN-02: a code task of booking gets its contract, not those of other services.
	set("tech/booking.proto", "syntax = \"proto3\";\nservice Booking {}\n")
	set("tech/pricing.openapi.json", `{"openapi":"3.1.0","paths":{}}`)
	got, err := speccontract.Contracts(ctx, gp, "tok", f, "booking", 1<<20)
	must(t, err)
	if len(got) != 2 || got["booking.openapi.yaml"] != ctrYAML || got["booking.proto"] == "" {
		t.Fatalf("GEN-02 %v", got)
	}

	// SVC-07: without arch, the services come from tech.
	del("arch/spec.md")
	set("tech/spec.md", ctrTech+"| `pricing` | org/pricing | Цена | R1 |\n")
	if got := project(); strings.Join(got, ",") != "booking,pricing" {
		t.Fatalf("SVC-07 feature_services %v", got)
	}
	del("tech/pricing.openapi.json")
	if vs := violations(domain.AreaTech); vs != nil {
		t.Fatalf("SVC-07 tech without arch %+v", vs)
	}

	// An arch of the contract 1.x (no diagram) does not name the services: the feature keeps those of tech.
	set("arch/spec.md", "# Старая\n\n## Затрагиваемые сервисы и контракты\n\n| Сервис | Роль |\n|---|---|\n| Запуски | x |\n")
	if got := project(); strings.Join(got, ",") != "booking,pricing" {
		t.Fatalf("an arch of 1.x %v", got)
	}
}
