//go:build integration

package itest

import (
	"context"
	"strings"
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// FTR.HMR.SPEC-0002 R15: the check of the specification repository projects the
// test cases and services of indexed features; features of Hammurapi keep theirs.
func TestIndexedTrace(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "spectrace")
	must(t, err)
	defer drop()
	db, err := postgres.Connect(ctx, url)
	must(t, err)
	defer db.Close()
	must(t, postgres.Migrate(ctx, url))
	_, err = db.Exec(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('FMS', 'Fleet') RETURNING id)
		INSERT INTO systems (domain_id, key, name, last_number) SELECT id, 'CAR', 'Cars', 1 FROM d`)
	must(t, err)
	_, err = db.Exec(ctx, `INSERT INTO services (key, name, repo) VALUES ('booking', 'Booking', 'o/booking')`)
	must(t, err)
	// A feature released through Hammurapi with the projection of its branch.
	_, err = db.Exec(ctx, `INSERT INTO features (unique_id, system_id, number, title, branch_name, pr_number, pr_url, phase)
		SELECT 'FTR.FMS.CAR-0001', id, 1, 'Booking', 'feature/FTR.FMS.CAR-0001', 7, 'https://git.example/pr/7', 'released' FROM systems WHERE key = 'CAR'`)
	must(t, err)
	_, err = db.Exec(ctx, `INSERT INTO test_cases (feature_id, tc_id, level, req_ids, title)
		SELECT id, 'BOOK-01', 'U', '{R1}', 'Бронь' FROM features WHERE unique_id = 'FTR.FMS.CAR-0001'`)
	must(t, err)

	const qaHead = "# Тесты\n\n| ID | Требования | Сценарий | Ожидаемый результат | Ур. |\n|---|---|---|---|---|\n"
	const imported = "specs/FMS/CAR/FTR.FMS.CAR-0042/"
	repo := &fakeRepo{files: map[string]string{}, blobs: map[string]string{}}
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0001/product/spec.md", "# Booking\n\n**R1.** Бронь.\n")
	repo.set("specs/FMS/CAR/FTR.FMS.CAR-0001/qa/spec.md", qaHead+"| BOOK-01 | R1 | Бронь | Да | U |\n| BOOK-02 | R1 | Отмена | Да | U |\n")
	repo.set(imported+"product/spec.md", "# Weekend tariffs\n\n**R1.** Тариф выходного дня.\n")
	repo.set(imported+"tech/spec.md", "# Техспецификация\n\n## 1. Изменения по сервисам\n\n| Сервис | Что меняется | Требования |\n|---|---|---|\n| `booking` | Тариф | R1 |\n| `ghost` | Нет в каталоге | R1 |\n")
	repo.set(imported+"qa/spec.md", qaHead+"| WKND-10 | R1 | Тариф | Да | U |\n| WKND-11 | R1 | Скидка | Да | U |\n")
	svc := specindex.NewService(db, repo.provider(t), nil, specindex.Config{DefaultBranch: "main"})
	runCheck := func() {
		t.Helper()
		_, err := svc.Enqueue(ctx, db, specindex.TriggerManual, nil)
		must(t, err)
		if ran, err := svc.RunOnce(ctx); err != nil || !ran {
			t.Fatalf("check ran=%v err=%v", ran, err)
		}
	}
	casesOf := func(key string) string {
		t.Helper()
		var ids []string
		must(t, db.QueryRow(ctx, `SELECT COALESCE(array_agg(t.tc_id ORDER BY t.tc_id), '{}') FROM test_cases t
			JOIN features f ON f.id = t.feature_id WHERE f.unique_id = $1`, key).Scan(&ids))
		return strings.Join(ids, ",")
	}

	// SVC-12, SVC-14: the first check projects the indexed feature: its cases and its services of the catalog.
	runCheck()
	var phase, source, services string
	must(t, db.QueryRow(ctx, `SELECT f.phase::text, COALESCE(f.trace_source, ''),
		COALESCE((SELECT string_agg(s.key, ',') FROM feature_services fs JOIN services s ON s.id = fs.service_id WHERE fs.feature_id = f.id), '')
		FROM features f WHERE f.unique_id = 'FTR.FMS.CAR-0042'`).Scan(&phase, &source, &services))
	if phase != "indexed" || source == "" || services != "booking" || casesOf("FTR.FMS.CAR-0042") != "WKND-10,WKND-11" {
		t.Fatalf("indexed: phase %s source %q services %q cases %q", phase, source, services, casesOf("FTR.FMS.CAR-0042"))
	}
	// SVC-15: the released feature keeps the projection of its branch.
	if got := casesOf("FTR.FMS.CAR-0001"); got != "BOOK-01" {
		t.Fatalf("released feature re-projected: %s", got)
	}

	// SVC-13: without a change of the documents the projection is not written again.
	_, err = db.Exec(ctx, `DELETE FROM test_cases WHERE tc_id = 'WKND-11'`)
	must(t, err)
	runCheck()
	if got := casesOf("FTR.FMS.CAR-0042"); got != "WKND-10" {
		t.Fatalf("projected again without a change: %s", got)
	}
	// A new case in qa of the default branch reaches the projection.
	repo.set(imported+"qa/spec.md", qaHead+"| WKND-10 | R1 | Тариф | Да | U |\n| WKND-11 | R1 | Скидка | Да | U |\n| WKND-12 | R1 | Возврат | Да | I |\n")
	runCheck()
	var source2 string
	must(t, db.QueryRow(ctx, `SELECT trace_source FROM features WHERE unique_id = 'FTR.FMS.CAR-0042'`).Scan(&source2))
	if got := casesOf("FTR.FMS.CAR-0042"); got != "WKND-10,WKND-11,WKND-12" || source2 == source {
		t.Fatalf("after the change: cases %s, source %q → %q", got, source, source2)
	}
	// A service added to the catalog later links the feature that names it.
	_, err = db.Exec(ctx, `INSERT INTO services (key, name, repo) VALUES ('ghost', 'Ghost', 'o/ghost')`)
	must(t, err)
	runCheck()
	must(t, db.QueryRow(ctx, `SELECT string_agg(s.key, ',' ORDER BY s.key) FROM feature_services fs JOIN services s ON s.id = fs.service_id
		JOIN features f ON f.id = fs.feature_id WHERE f.unique_id = 'FTR.FMS.CAR-0042'`).Scan(&services))
	if services != "booking,ghost" {
		t.Fatalf("services after the catalog changed: %s", services)
	}
	// A removed qa clears the test cases.
	repo.mu.Lock()
	delete(repo.files, imported+"qa/spec.md")
	repo.mu.Unlock()
	runCheck()
	if got := casesOf("FTR.FMS.CAR-0042"); got != "" {
		t.Fatalf("cases after qa was removed: %s", got)
	}
}
