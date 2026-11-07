package specindex

import (
	"reflect"
	"strings"
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/markdown"
)

// LINK-01: «Опирается на» with three keys and explanations in parentheses gives three "extends".
func TestLinksExtends(t *testing.T) {
	md := "# Тарифы\n\n**Версия:** 1.0\n**Решение:** `FTR.FMS.CAR-0042`\n" +
		"**Опирается на:** `FTR.FMS.CAR-0002` (бронь), `FTR.FMS.CAR-0001` (каталог) и `FTR.LOG.DLV-0003` (доставка)\n\n---\n\n## 1. Проблема\n\nТекст.\n"
	got := Links(md, "FTR.FMS.CAR-0042", "")
	want := []Link{
		{Target: "FTR.FMS.CAR-0001", Kind: LinkExtends, Requirements: []string{}},
		{Target: "FTR.FMS.CAR-0002", Kind: LinkExtends, Requirements: []string{}},
		{Target: "FTR.LOG.DLV-0003", Kind: LinkExtends, Requirements: []string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

// LINK-02: two rows for one target with different requirements give one "changes" link.
func TestLinksChangesMerged(t *testing.T) {
	md := "# F\n\n---\n\n## Изменения в других решениях\n\n| Решение | Требование | Что изменилось |\n|---|---|---|\n" +
		"| `FTR.FMS.CAR-0002` | R13 | А. |\n| `FTR.FMS.CAR-0002` | R2 | Б. |\n| `FTR.FMS.CAR-0002` | R13 | В. |\n| `FTR.FMS.CAR-0001` | всё решение | Г. |\n"
	got := Links(md, "FTR.FMS.CAR-0042", "")
	want := []Link{
		{Target: "FTR.FMS.CAR-0001", Kind: LinkChanges, Requirements: []string{}},
		{Target: "FTR.FMS.CAR-0002", Kind: LinkChanges, Requirements: []string{"R2", "R13"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

// LINK-03: a fix changes its parent when the section does not name it; when it does, its requirements stay.
func TestLinksFixParent(t *testing.T) {
	md := "---\nparent: FTR.FMS.CAR-0042\n---\n# Исправление\n"
	got := Links(md, "FTR.FMS.CAR-0043", "FTR.FMS.CAR-0042")
	if !reflect.DeepEqual(got, []Link{{Target: "FTR.FMS.CAR-0042", Kind: LinkChanges, Requirements: []string{}}}) {
		t.Fatalf("got %+v", got)
	}
	md += "\n## Изменения в других решениях\n\n| Решение | Требование | Что изменилось |\n|---|---|---|\n| `FTR.FMS.CAR-0042` | R3 | А. |\n"
	got = Links(md, "FTR.FMS.CAR-0043", "FTR.FMS.CAR-0042")
	if !reflect.DeepEqual(got, []Link{{Target: "FTR.FMS.CAR-0042", Kind: LinkChanges, Requirements: []string{"R3"}}}) {
		t.Fatalf("got %+v", got)
	}
}

// LINK-08: an old-form parent (FMS.CAR-0042) is not a key: no link to a feature that does not exist.
func TestLinksOldFormParent(t *testing.T) {
	if got := Links("# Исправление\n", "FTR.FMS.CAR-0043", "FMS.CAR-0042"); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// LINK-04: a link to itself and keys in the text outside the header and the section are not links.
func TestLinksIgnoresTextAndSelf(t *testing.T) {
	md := "# F\n\n**Опирается на:** `FTR.FMS.CAR-0042`\n\n---\n\n## 1. Проблема\n\n**Опирается на:** `FTR.FMS.CAR-0005`\n\nНапример, `FTR.FMS.CAR-0005` R3.\n\n" +
		"## Изменения в других решениях\n\n| Решение | Требование | Что изменилось |\n|---|---|---|\n| `FTR.FMS.CAR-0042` | R1 | Себя. |\n"
	if got := Links(md, "FTR.FMS.CAR-0042", ""); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestSortReqs(t *testing.T) {
	ids := []string{"R26a", "R13", "R2", "R26"}
	sortReqs(ids)
	if !reflect.DeepEqual(ids, []string{"R2", "R13", "R26", "R26a"}) {
		t.Fatalf("got %v", ids)
	}
}

// CHG-07: notes put by FTR.HMR.DEV-0002 keep the criteria the index reads, also of a lettered requirement with details.
func TestRequirementsKeepCriteriaWithNotes(t *testing.T) {
	doc := "# Бронь\n\n## 4. Требования\n\n**R13.** Отмена бесплатна\nв течение 30 минут.\n- Дано бронь, когда отмена, тогда бесплатно.\n\n" +
		"**R26a. Возврат.** Деньги возвращаются.\n\nУточнение: на карту.\n\n- Дано отмена, когда платёж картой, тогда возврат на карту.\n"
	noted := markdown.ApplyNotes(doc, "FTR.FMS.CAR-0042", []markdown.Note{{Req: "R13", Text: "А"}, {Req: "R26a", SourceReq: "R2", Text: "Б"}})
	before, after := Requirements(doc), Requirements(noted)
	if len(before) != 2 || len(after) != 2 {
		t.Fatalf("requirements %+v / %+v", before, after)
	}
	for i := range before {
		if !reflect.DeepEqual(before[i].Criteria, after[i].Criteria) || len(after[i].Criteria) != 1 {
			t.Fatalf("%s criteria %q → %q", before[i].ID, before[i].Criteria, after[i].Criteria)
		}
	}
	if !strings.Contains(after[0].Text, "Изменено в FTR.FMS.CAR-0042") { // R9: the agent reading the requirement sees the note
		t.Fatalf("text %q", after[0].Text)
	}
}
