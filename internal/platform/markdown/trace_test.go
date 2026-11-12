package markdown

import (
	"reflect"
	"strings"
	"testing"
)

const product = `# Onboarding

## Сценарии и требования

**R1.** Пользователь создаёт задачу.
- Дано пользователь, когда он создаёт задачу, тогда ключ ISS.FMS-0001.

- **R2.** Эксперт переносит задачу.
- Загрузка документов без ID
- **FTR.FMS.CAR-0007-R3.** С ключом решения.

## Метрики успеха

- Не требование
`

// GEN-05: requirements are projected from **R<n>.** markers.
func TestParseRequirements(t *testing.T) {
	got := ParseRequirements(product)
	want := []Req{{"R1", "Пользователь создаёт задачу."}, {"R2", "Эксперт переносит задачу."}, {"R3", "С ключом решения."}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

// GEN-06: items of the requirements section without an ID are reported.
func TestUnnumberedRequirements(t *testing.T) {
	got := UnnumberedRequirements(product)
	if !reflect.DeepEqual(got, []string{"Загрузка документов без ID"}) {
		t.Fatalf("got %q", got)
	}
}

// The template and a title inside the bold: the items are details, not requirements.
func TestRequirementDetailsAreNotUnnumbered(t *testing.T) {
	doc := "## Требования\n\n" +
		"**R1.** По шаблону.\n\n- Критерии приёмки:\n  - Дано а, когда б, тогда в.\n\n" +
		"### 1. Раздел\n\n" +
		"**R2. Заголовок в жирном.**\n\n- Подробность.\n- Критерии приёмки:\n  - Дано а, когда б, тогда в.\n\n" +
		"**R3. С текстом.** Хвост.\n\n- Подробность.\n"
	if got := UnnumberedRequirements(doc); got != nil {
		t.Fatalf("got %q", got)
	}
	want := []Req{{"R1", "По шаблону."}, {"R2", "Заголовок в жирном."}, {"R3", "С текстом. Хвост."}}
	if got := ParseRequirements(doc); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

// A heading ends the requirement above it: items under a new heading need their own ID.
func TestItemsUnderNewHeadingAreUnnumbered(t *testing.T) {
	doc := "## Требования\n\n**R1.** Первое.\n\n- Подробность.\n\n### Ещё\n\n- Без ID\n"
	if got := UnnumberedRequirements(doc); !reflect.DeepEqual(got, []string{"Без ID"}) {
		t.Fatalf("got %q", got)
	}
}

func TestParseTestCases(t *testing.T) {
	doc := "| ID | Требования | Сценарий | Ожидаемый результат | Ур. |\n|---|---|---|---|---|\n" +
		"| ISS-01 | R1 | Создание | Ключ | I |\n| DSC-02 | R4–R6, R9 | Discovery | Готово | U |\n| — | — | x | y | E |\n"
	got := ParseTestCases(doc)
	if len(got) != 2 || got[0].ID != "ISS-01" || got[0].Level != "I" || got[0].Title != "Создание" {
		t.Fatalf("got %+v", got)
	}
	if !reflect.DeepEqual(got[1].ReqIDs, []string{"R4", "R5", "R6", "R9"}) {
		t.Fatalf("ranges: %v", got[1].ReqIDs)
	}
}

// FTR.HMR.SPEC-0005 qa TCS-01: the column «Сервисы» of the test cases.
func TestParseTestCaseServices(t *testing.T) {
	doc := "| ID | Требования | Сервисы | Сценарий | Ожидаемый результат | Ур. |\n|---|---|---|---|---|---|\n" +
		"| `MIG-01` | R1 | `nabu-core` | Миграция | Колонки | I |\n" +
		"| `TCS-10` | R2 |  | Экран | Видно | E |\n" +
		"| `CTR-01` | R3 | `nabu-core`, `nabu-web` | Контракт | Схемы | I |\n" +
		"| `TCS-11` | R4 | — | Стенд | 5 с | M |\n"
	got := ParseTestCases(doc)
	want := [][]string{{"nabu-core"}, nil, {"nabu-core", "nabu-web"}, nil}
	if len(got) != 4 {
		t.Fatalf("got %+v", got)
	}
	for i, w := range want {
		if !reflect.DeepEqual(got[i].Services, w) {
			t.Fatalf("%s: services %v, want %v", got[i].ID, got[i].Services, w)
		}
	}
	if got[0].Title != "Миграция" || got[0].Level != "I" {
		t.Fatalf("the other columns: %+v", got[0])
	}
}

func TestParseTechServices(t *testing.T) {
	doc := "## Изменения по сервисам\n\n| Сервис | Требования | Изменения |\n|---|---|---|\n| `booking` | R1, R2 | API |\n| pricing | R2 | правила |\n"
	got := ParseTechServices(doc)
	want := []ServiceReqs{{"booking", []string{"R1", "R2"}}, {"pricing", []string{"R2"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestParseRolloutOrder(t *testing.T) {
	arch := "# Arch\n\n## Порядок выката\n\n1. `pricing` — первым\n2. booking\n\n## Другое\n\nbilling"
	got := ParseRolloutOrder(arch, []string{"booking", "billing", "pricing"})
	if !reflect.DeepEqual(got, []string{"pricing", "booking", "billing"}) {
		t.Fatalf("got %v", got)
	}
}

func TestFillSection(t *testing.T) {
	doc := "# T\n\n## Problem\n\nx\n\n## Success metrics\n\nHow we will know.\n\n## Decisions\n"
	got := FillSection(doc, "Success metrics", "| a | b |", "success metrics", "метрики успеха")
	if !strings.Contains(got, "## Success metrics\n\n| a | b |\n\n## Decisions") || strings.Contains(got, "How we will know") {
		t.Fatalf("got %q", got)
	}
	got = FillSection("# T\n", "Success metrics", "m", "success metrics")
	if !strings.HasSuffix(got, "## Success metrics\n\nm\n") {
		t.Fatalf("got %q", got)
	}
}

// GEN-05: an inserted requirement keeps its letter, R26a.
func TestRequirementsWithSuffix(t *testing.T) {
	got := ParseRequirements("**R26a.** Вставлено.\n\n**R26b. Ещё.** Текст.\n")
	if len(got) != 2 || got[0].ID != "R26a" || got[1].ID != "R26b" {
		t.Fatalf("got %+v", got)
	}
	for in, want := range map[string]string{
		"R26a, R3–R5":  "R26a R3 R4 R5",
		"R26a–R26c":    "R26a R26b R26c",
		"R26–R26b":     "R26 R26a R26b",
		"R25–R27a":     "R25 R26 R27 R27a",
		"R12e2e, R1.":  "R12 R1",
		"R5–R3, R7–R7": "R5 R7",
		"PR3 и R4":     "R4",
	} {
		if got := strings.Join(ParseReqRefs(in), " "); got != want {
			t.Errorf("ParseReqRefs(%q) = %q, want %q", in, got, want)
		}
	}
}
