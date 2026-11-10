package specindex

import (
	"reflect"
	"strings"
	"testing"

	"github.com/GreenOnGrey/speclint/contract"
)

// LINK-01: «Опирается на» with three keys and explanations in parentheses gives three "extends".
func TestLinksExtends(t *testing.T) {
	md := "# Тарифы\n\n**Версия:** 1.0\n**Решение:** `FTR.FMS.CAR-0042`\n" +
		"**Опирается на:** `FTR.FMS.CAR-0002` (бронь), `FTR.FMS.CAR-0001` (каталог) и `FTR.LOG.DLV-0003` (доставка)\n\n---\n\n## 1. Проблема\n\nТекст.\n"
	got := Links(md, "product", "FTR.FMS.CAR-0042", "")
	want := []Link{
		{Target: "FTR.FMS.CAR-0001", Kind: LinkExtends, Layer: LayerProduct, Requirements: []string{}},
		{Target: "FTR.FMS.CAR-0002", Kind: LinkExtends, Layer: LayerProduct, Requirements: []string{}},
		{Target: "FTR.LOG.DLV-0003", Kind: LinkExtends, Layer: LayerProduct, Requirements: []string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

// LINK-02: two rows for one target with different requirements give one "changes" link.
func TestLinksChangesMerged(t *testing.T) {
	md := "# F\n\n---\n\n## Изменения в других решениях\n\n| Решение | Требование | Что изменилось |\n|---|---|---|\n" +
		"| `FTR.FMS.CAR-0002` | R13 | А. |\n| `FTR.FMS.CAR-0002` | R2 | Б. |\n| `FTR.FMS.CAR-0002` | R13 | В. |\n| `FTR.FMS.CAR-0001` | всё решение | Г. |\n"
	got := Links(md, "product", "FTR.FMS.CAR-0042", "")
	want := []Link{
		{Target: "FTR.FMS.CAR-0001", Kind: LinkChanges, Layer: LayerProduct, Requirements: []string{}},
		{Target: "FTR.FMS.CAR-0002", Kind: LinkChanges, Layer: LayerProduct, Requirements: []string{"R2", "R13"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

// LINK-03: a fix changes its parent when the section does not name it; when it does, its requirements stay.
func TestLinksFixParent(t *testing.T) {
	md := "---\nparent: FTR.FMS.CAR-0042\n---\n# Исправление\n"
	got := Links(md, "product", "FTR.FMS.CAR-0043", "FTR.FMS.CAR-0042")
	if !reflect.DeepEqual(got, []Link{{Target: "FTR.FMS.CAR-0042", Kind: LinkChanges, Layer: LayerProduct, Requirements: []string{}}}) {
		t.Fatalf("got %+v", got)
	}
	md += "\n## Изменения в других решениях\n\n| Решение | Требование | Что изменилось |\n|---|---|---|\n| `FTR.FMS.CAR-0042` | R3 | А. |\n"
	got = Links(md, "product", "FTR.FMS.CAR-0043", "FTR.FMS.CAR-0042")
	if !reflect.DeepEqual(got, []Link{{Target: "FTR.FMS.CAR-0042", Kind: LinkChanges, Layer: LayerProduct, Requirements: []string{"R3"}}}) {
		t.Fatalf("got %+v", got)
	}
}

// LINK-08: an old-form parent (FMS.CAR-0042) is not a key: no link to a feature that does not exist.
func TestLinksOldFormParent(t *testing.T) {
	if got := Links("# Исправление\n", "product", "FTR.FMS.CAR-0043", "FMS.CAR-0042"); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// LINK-04: a link to itself and keys in the text outside the header and the section are not links.
func TestLinksIgnoresTextAndSelf(t *testing.T) {
	md := "# F\n\n**Опирается на:** `FTR.FMS.CAR-0042`\n\n---\n\n## 1. Проблема\n\n**Опирается на:** `FTR.FMS.CAR-0005`\n\nНапример, `FTR.FMS.CAR-0005` R3.\n\n" +
		"## Изменения в других решениях\n\n| Решение | Требование | Что изменилось |\n|---|---|---|\n| `FTR.FMS.CAR-0042` | R1 | Себя. |\n"
	if got := Links(md, "product", "FTR.FMS.CAR-0042", ""); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// CHG-07: notes put by FTR.HMR.DEV-0002 keep the criteria the index reads, also of a lettered requirement with details.
func TestRequirementsKeepCriteriaWithNotes(t *testing.T) {
	doc := "# Бронь\n\n## 4. Требования\n\n**R13.** Отмена бесплатна\nв течение 30 минут.\n- Дано бронь, когда отмена, тогда бесплатно.\n\n" +
		"**R26a. Возврат.** Деньги возвращаются.\n\nУточнение: на карту.\n\n- Дано отмена, когда платёж картой, тогда возврат на карту.\n"
	noted := contract.ApplyNotes(doc, contract.Product, "FTR.FMS.CAR-0042", []contract.Note{{Target: "R13", Text: "А"}, {Target: "R26a", SourceItem: "R2", Text: "Б"}}, contract.RU)
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

// ARCH-04, ARCH-05: «Опирается на» of arch with decisions, and the section of arch, give links of the layer «arch».
func TestLinksArch(t *testing.T) {
	md := "# Архитектура\n\n**Опирается на:** `FTR.HMR.CMN-0001` (A3, A5)\n\n## Изменения в других решениях\n\n" +
		"| Решение | Архитектурное решение | Что изменилось |\n|---|---|---|\n| `FTR.HMR.CMN-0002` | A10 | Слой. |\n\n" +
		"## Принятые решения\n\n| A | Решение | Обоснование |\n|---|---|---|\n| A1 | Один | Потому что `FTR.HMR.CMN-0002` |\n"
	got := Links(md, "arch", "FTR.HMR.DEV-0003", "")
	want := []Link{
		{Target: "FTR.HMR.CMN-0001", Kind: LinkExtends, Layer: LayerArch, Requirements: []string{"A3", "A5"}},
		{Target: "FTR.HMR.CMN-0002", Kind: LinkChanges, Layer: LayerArch, Requirements: []string{"A10"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	ds := Decisions(md)
	if len(ds) != 1 || ds[0].ID != "A1" || ds[0].Section != "Принятые решения" || !strings.Contains(ds[0].Text, "Один") {
		t.Fatalf("decisions %+v", ds)
	}
}

// ARCH-01: a reference to a decision is a reference with its item.
func TestReferencesToDecisions(t *testing.T) {
	refs := References("# F\n\nСм. FTR.HMR.CMN-0002-A2 и FTR.HMR.CMN-0002-R3.\n", "FTR.HMR.DEV-0003")
	if len(refs) != 2 || refs[0].Req != "A2" || refs[1].Req != "R3" || refs[0].Target != "FTR.HMR.CMN-0002" {
		t.Fatalf("got %+v", refs)
	}
}
