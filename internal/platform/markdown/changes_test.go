package markdown

import (
	"reflect"
	"strings"
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

const target = "# Бронь\n\n**Версия:** 2.1\n**Решение:** `FTR.FMS.CAR-0002`\n\n---\n\n## 4. Требования\n\n" +
	"**R13.** Отмена брони бесплатна\nв течение 30 минут.\n- Дано бронь, когда отмена через 10 минут, тогда бесплатно.\n\n" +
	"**R26a. Возврат.** Деньги возвращаются.\n\nУточнение: на карту.\n\n- Дано отмена, когда платёж картой, тогда возврат на карту.\n\n" +
	"```markdown\n**R13.** Пример в коде.\n```\n"

// CHG-03: the note goes right after the first line of R13; the rest of the text is the same.
func TestApplyNotes(t *testing.T) {
	got := ApplyNotes(target, "FTR.FMS.CAR-0042", []Note{{Req: "R13", SourceReq: "R4", Text: "Отмена платная после 10 минут."}})
	want := strings.Replace(target, "**R13.** Отмена брони бесплатна\n",
		"**R13.** Отмена брони бесплатна\n*Изменено в `FTR.FMS.CAR-0042` R4: Отмена платная после 10 минут.*\n", 1)
	if got != want {
		t.Fatalf("got\n%s", got)
	}
	// CHG-04: applying the same notes again gives the same text.
	if again := ApplyNotes(got, "FTR.FMS.CAR-0042", []Note{{Req: "R13", SourceReq: "R4", Text: "Отмена платная после 10 минут."}}); again != got {
		t.Fatalf("not idempotent\n%s", again)
	}
	// CHG-08: without the note lines the document is the same, byte for byte.
	if back := ApplyNotes(got, "FTR.FMS.CAR-0042", nil); back != target {
		t.Fatalf("not reversible\n%s", back)
	}
}

// CHG-05: a removed row removes the note of the source; the note of another feature stays, and a new one goes after it.
func TestApplyNotesOtherSources(t *testing.T) {
	doc := ApplyNotes(target, "FTR.FMS.CAR-0042", []Note{{Req: "R13", Text: "А"}})
	doc = ApplyNotes(doc, "FTR.FMS.CAR-0050", []Note{{Req: "R13", Text: "Б"}})
	if !strings.Contains(doc, "бесплатна\n*Изменено в `FTR.FMS.CAR-0042`: А.*\n*Изменено в `FTR.FMS.CAR-0050`: Б.*\nв течение") {
		t.Fatalf("order\n%s", doc)
	}
	doc = ApplyNotes(doc, "FTR.FMS.CAR-0042", nil)
	if strings.Contains(doc, "CAR-0042") || !strings.Contains(doc, "бесплатна\n*Изменено в `FTR.FMS.CAR-0050`: Б.*\nв течение") {
		t.Fatalf("removal\n%s", doc)
	}
}

// CHG-06: a note of the whole feature goes after the header lines, before the rule.
func TestApplyNotesWholeFeature(t *testing.T) {
	got := ApplyNotes(target, "FTR.FMS.CAR-0042", []Note{{Text: "Новая страница решения"}})
	if !strings.Contains(got, "**Решение:** `FTR.FMS.CAR-0002`\n*Изменено в `FTR.FMS.CAR-0042`: Новая страница решения.*\n\n---") {
		t.Fatalf("got\n%s", got)
	}
}

// CHG-07: a lettered requirement with criteria after details: the criteria the index reads stay the same.
// The index test of the same document is in specindex (TestRequirementsKeepCriteriaWithNotes).
func TestApplyNotesLettered(t *testing.T) {
	got := ApplyNotes(target, "FTR.FMS.CAR-0042", []Note{{Req: "R26a", SourceReq: "R2", Text: "Возврат на счёт"}})
	if !strings.Contains(got, "**R26a. Возврат.** Деньги возвращаются.\n*Изменено в `FTR.FMS.CAR-0042` R2: Возврат на счёт.*\n\nУточнение") {
		t.Fatalf("got\n%s", got)
	}
	if strings.Contains(got, "Пример в коде.\n*Изменено") {
		t.Fatal("a requirement in a code block got a note")
	}
}

// CHG-09: a cancelled requirement gets the same note with its text.
func TestApplyNotesCancelled(t *testing.T) {
	got := ApplyNotes(target, "FTR.FMS.CAR-0042", []Note{{Req: "R13", SourceReq: "R7", Text: "требование больше не действует"}})
	if !strings.Contains(got, "*Изменено в `FTR.FMS.CAR-0042` R7: требование больше не действует.*") {
		t.Fatalf("got\n%s", got)
	}
}

func TestApplyNotesMissingRequirement(t *testing.T) {
	if got := ApplyNotes(target, "FTR.FMS.CAR-0042", []Note{{Req: "R99", Text: "А"}}); got != target {
		t.Fatalf("got\n%s", got)
	}
}

// CHG-06 for a fix: the front matter is not the header; the note goes after the header lines below it.
func TestApplyNotesWholeFeatureWithFrontMatter(t *testing.T) {
	doc := "---\nparent: FTR.FMS.CAR-0002\n---\n\n# Исправление\n\n**Версия:** 1.0\n\n---\n\n## 4. Требования\n"
	got := ApplyNotes(doc, "FTR.FMS.CAR-0042", []Note{{Text: "Новая страница"}})
	if !strings.Contains(got, "**Версия:** 1.0\n*Изменено в `FTR.FMS.CAR-0042`: Новая страница.*\n\n---\n\n## 4.") {
		t.Fatalf("got\n%s", got)
	}
}
