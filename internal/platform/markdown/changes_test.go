package markdown

import (
	"reflect"
	"testing"
)

const changesDoc = "# Тарифы выходного дня\n\n**Версия:** 1.0\n**Опирается на:** `FTR.FMS.CAR-0002`\n\n---\n\n" +
	"## 4. Требования\n\n**R1.** Тариф.\n\n" +
	"```markdown\n## Изменения в других решениях\n\n| Решение | Требование | Что изменилось |\n|---|---|---|\n| `FTR.FMS.CAR-0099` | R1 | Пример. |\n```\n\n" +
	"## Изменения в других решениях\n\n" +
	"| Решение | Требование | Что изменилось |\n|---|---|---|\n" +
	"| `FTR.FMS.CAR-0002` | R13 | Перегенерация после перепроверки. |\n" +
	"| `FTR.FMS.CAR-0002` | `R26a` | Требование больше не действует. |\n" +
	"| `FTR.FMS.CAR-0001` | всё решение | Новая страница решения. |\n" +
	"| `FTR.<D>.<S>-NNNN` | R<n> | Шаблон. |\n\n" +
	"## 5. Скоуп\n\n| Решение | Требование | Что изменилось |\n|---|---|---|\n| `FTR.FMS.CAR-0007` | R1 | Не раздел. |\n"

// CHG-01: three rows, one for the whole feature; examples in code and other sections are not rows.
func TestParseChanges(t *testing.T) {
	got := ParseChanges(changesDoc)
	want := []Change{
		{Feature: "FTR.FMS.CAR-0002", Req: "R13", Note: "Перегенерация после перепроверки."},
		{Feature: "FTR.FMS.CAR-0002", Req: "R26a", Note: "Требование больше не действует."},
		{Feature: "FTR.FMS.CAR-0001", Req: "", Note: "Новая страница решения."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

// CHG-02: no section, no rows.
func TestParseChangesNoSection(t *testing.T) {
	if got := ParseChanges("# Решение\n\n## 4. Требования\n\n**R1.** Текст.\n"); got != nil {
		t.Fatalf("got %+v", got)
	}
}

func TestParseChangesEnglishAndDash(t *testing.T) {
	doc := "# F\n\n## Changes to other features\n\n| Feature | Requirement | What changed |\n|---|---|---|\n| FTR.FMS.CAR-0002 | — | Whole. |\n"
	got := ParseChanges(doc)
	if len(got) != 1 || got[0].Req != "" || got[0].Note != "Whole." {
		t.Fatalf("got %+v", got)
	}
}

// A table without the note column still gives the rows: the links need only the key and the requirement.
func TestParseChangesWithoutNote(t *testing.T) {
	doc := "# F\n\n## Изменения в других решениях\n\n| Решение | Требование |\n|---|---|\n| `FTR.FMS.CAR-0002` | R3 |\n"
	got := ParseChanges(doc)
	if len(got) != 1 || got[0].Req != "R3" || got[0].Note != "" {
		t.Fatalf("got %+v", got)
	}
}
