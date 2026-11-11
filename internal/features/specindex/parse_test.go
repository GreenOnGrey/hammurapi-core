package specindex

import (
	"reflect"
	"strings"
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
)

func TestParsePath(t *testing.T) {
	l, ok := ParsePath("specs/FMS/CAR/FTR.FMS.CAR-0042/product/spec.md")
	if !ok || l.Folder != "FTR.FMS.CAR-0042" || l.Area != "product" || !l.IsDocument() || l.Dir() != "specs/FMS/CAR/FTR.FMS.CAR-0042/" {
		t.Fatalf("%+v %v", l, ok)
	}
	if l, ok := ParsePath("specs/FMS/CAR/FTR.FMS.CAR-0042/design/mockups.html"); !ok || l.IsDocument() || l.Rest != "mockups.html" {
		t.Fatalf("file %+v %v", l, ok)
	}
	for _, p := range []string{"specs/FMS/CAR/FTR.FMS.CAR-0042/notes/spec.md", "rules/x.md", "specs/fms/CAR/X/product/spec.md", "specs/FMS/CAR/X/spec.md"} {
		if _, ok := ParsePath(p); ok {
			t.Errorf("%s parsed", p)
		}
	}
}

// DF-02: the files of discovery/ and the legacy root discovery.md are files of
// the area discovery, never documents; nested folders are not read.
func TestParsePathDiscovery(t *testing.T) {
	dir := "specs/FMS/CAR/FTR.FMS.CAR-0042/"
	for p, rest := range map[string]string{
		dir + "discovery/flow.png":     "flow.png",
		dir + "discovery/discovery.md": "discovery.md",
		dir + "discovery.md":           "discovery.md",
		dir + "discovery/spec.md":      "spec.md",
	} {
		l, ok := ParsePath(p)
		if !ok || l.Area != AreaDiscovery || l.Rest != rest || l.IsDocument() || l.Folder != "FTR.FMS.CAR-0042" || l.Dir() != dir {
			t.Errorf("%s: %+v %v", p, l, ok)
		}
	}
	for _, p := range []string{dir + "discovery/a/b.png", dir + "notes.md", "specs/FMS/CAR/discovery.md"} {
		if l, ok := ParsePath(p); ok {
			t.Errorf("%s parsed: %+v", p, l)
		}
	}
}

// IDX-04, IDX-05, IDX-06.
func TestCheckID(t *testing.T) {
	if n, p := CheckID("FTR.FMS.CAR-0042", "FMS", "CAR"); p != nil || n != 42 {
		t.Fatalf("%d %+v", n, p)
	}
	cases := map[string]string{
		"FMS.CAR-0011":     KindOldFormat,
		"FTR.FMS.CAR-42":   KindBadFormat,
		"FTR.fms.CAR-0042": KindBadFormat,
		"FTR.PAY.REF-0002": KindKeyPathMismatch,
	}
	for id, kind := range cases {
		dom, sys := "FMS", "CAR"
		if strings.Contains(id, "PAY") {
			dom, sys = "PAY", "INV"
		}
		if _, p := CheckID(id, dom, sys); p == nil || p.Kind != kind {
			t.Errorf("%s: %+v, want %s", id, p, kind)
		}
	}
	if _, p := CheckID("FMS.CAR-0011", "FMS", "CAR"); p.Details["suggested"] != "FTR.FMS.CAR-0011" {
		t.Errorf("suggested %v", p.Details)
	}
}

// IDX-09, IDX-13: title and parent from front matter.
func TestTitleAndParent(t *testing.T) {
	md := "---\nparent: FTR.FMS.CAR-0002\n---\n\n```\n# not a title\n```\n# Weekend tariffs\n\ntext\n"
	if got := Title(md); got != "Weekend tariffs" {
		t.Fatalf("title %q", got)
	}
	if p, err := Parent(md); err != nil || p != "FTR.FMS.CAR-0002" {
		t.Fatalf("parent %q %v", p, err)
	}
	if p, err := Parent("---\nparent: [broken\n---\n# T\n"); err == nil || p != "" {
		t.Fatalf("broken front matter: %q %v", p, err)
	}
	if Title("no heading") != "" {
		t.Fatal("title without heading")
	}
}

// NAV-05: repeated and Cyrillic headings get unique slugs.
func TestTOC(t *testing.T) {
	md := "# T\n## 1. Проблема\n### Детали\n## Детали\n```\n## in code\n```\n#### deep\n"
	got := TOC(md)
	var slugs []string
	for _, h := range got {
		slugs = append(slugs, h.Slug)
	}
	if !reflect.DeepEqual(slugs, []string{"1-проблема", "детали", "детали-2"}) || got[1].Level != 3 {
		t.Fatalf("%+v", got)
	}
}

func TestSearchText(t *testing.T) {
	md := "# Title\n\n| A | B |\n| --- | --- |\n| **x** | [link](http://e) |\n\n```go\nfunc main() {}\n```\n"
	got := SearchText(md)
	for _, want := range []string{"Title", "x", "link", "func main() {}"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q not in %q", want, got)
		}
	}
	if strings.Contains(got, "**") || strings.Contains(got, "http://e") || strings.Contains(got, "---") {
		t.Errorf("markup left: %q", got)
	}
}

// AGT-04: requirements with their criteria and section.
func TestRequirementsTitleInBold(t *testing.T) {
	got := Requirements("## Требования\n\n**R1. Заголовок.** Хвост.\n\n- Критерии приёмки:\n  - Дано а, когда б.\n\n**R2. Второе.**\n")
	if len(got) != 2 || got[0].ID != "R1" || got[0].Text != "Заголовок. Хвост." || got[1].Text != "Второе." || len(got[0].Criteria) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestRequirements(t *testing.T) {
	md := `# P

## 4. Требования

**R1.** Weekend tariff applies
on weekends.
- Дано суббота, когда бронь, тогда тариф выходного.
- Дано воскресенье, когда бронь, тогда тариф выходного.
- not a criterion

**R2.** Price shown.

**R3.** Free cancellation within 30 minutes.
- Given a booking, when cancelled in 10 minutes, then it is free.
`
	got := Requirements(md)
	if len(got) != 3 {
		t.Fatalf("%+v", got)
	}
	if got[0].ID != "R1" || got[0].Text != "Weekend tariff applies on weekends." || len(got[0].Criteria) != 2 || got[0].Section != "4. Требования" {
		t.Errorf("R1 %+v", got[0])
	}
	if got[1].ID != "R2" || len(got[1].Criteria) != 0 {
		t.Errorf("R2 %+v", got[1])
	}
	if got[2].ID != "R3" || len(got[2].Criteria) != 1 {
		t.Errorf("R3 %+v", got[2])
	}
}

// AGT-07: mentions in code blocks are not references; own feature is skipped.
func TestReferences(t *testing.T) {
	md := "# T\n## Связи\nСм. FTR.FMS.CAR-0002-R3 и FTR.FMS.CAR-0007.\nСвоя: FTR.FMS.CAR-0042.\n```\nFTR.FMS.CAR-0009\n```\n"
	got := References(md, "FTR.FMS.CAR-0042")
	if len(got) != 2 || got[0].Target != "FTR.FMS.CAR-0002" || got[0].Req != "R3" || got[0].Section != "Связи" ||
		got[1].Target != "FTR.FMS.CAR-0007" || got[1].Req != "" {
		t.Fatalf("%+v", got)
	}
	if !strings.Contains(got[0].Snippet, "FTR.FMS.CAR-0002-R3") {
		t.Fatalf("snippet %q", got[0].Snippet)
	}
}

func TestSections(t *testing.T) {
	head, secs := Sections("# T\nintro\n## A\na text\n### A1\nsub\n## B\nb text\n")
	if !strings.Contains(head, "intro") || len(secs) != 3 || secs[0].Heading.Slug != "a" || !strings.Contains(secs[2].Text, "b text") ||
		strings.Contains(secs[0].Text, "sub") {
		t.Fatalf("%q %+v", head, secs)
	}
}

// FTR.HMR.DEV-0001 WR-24: a document by the templates of rules/ is found by the spec_* tools.
func TestDocumentByTemplate(t *testing.T) {
	md := "---\nparent: FTR.FMS.CAR-0000  # ID родительской фичи\n---\n\n# Продуктовая спецификация: Скидка\n\n## Требования\n\n### Возврат\n\n" +
		"**R1.** Скидка за ранний возврат, см. FTR.FMS.CAR-0002-R3.\n\n- Критерии приёмки:\n  - Дано клиент вернул авто раньше, когда возврат принят, тогда скидка начислена.\n"
	if p, err := Parent(md); err != nil || p != "FTR.FMS.CAR-0000" {
		t.Fatalf("parent %q %v", p, err)
	}
	if _, problem := CheckID("FTR.FMS.CAR-0000", "FMS", "CAR"); problem != nil {
		t.Fatalf("parent of the template: %+v", problem)
	}
	reqs := Requirements(md)
	if len(reqs) != 1 || reqs[0].ID != "R1" || len(reqs[0].Criteria) != 1 || reqs[0].Section != "Возврат" {
		t.Fatalf("requirements %+v", reqs)
	}
	refs := References(md, "FTR.FMS.CAR-0005")
	if len(refs) != 1 || refs[0].Target != "FTR.FMS.CAR-0002" || refs[0].Req != "R3" {
		t.Fatalf("references %+v", refs)
	}
}

// AGT-04: criteria follow details of the requirement — a list and a paragraph —
// and end at the next requirement or heading (FTR.HMR.CMN-0005 tech §4.3).
func TestRequirementsCriteriaAfterDetails(t *testing.T) {
	md := `## Требования

**R3.** Писатель получает весь Discovery:
- ответы экспертов — принятые решения;
- допущения — ещё не решения.

Вопросы, которые Discovery не решает, решаются здесь.
- Критерии приёмки:
  - Дано эксперт ответил, когда писатель пишет, тогда вопроса нет.

### 4.2 Дальше

- Дано под другим заголовком.
`
	got := Requirements(md)
	if len(got) != 1 || len(got[0].Criteria) != 1 || got[0].Text != "Писатель получает весь Discovery:" {
		t.Fatalf("got %+v", got)
	}
}

// AGT-04: an inserted requirement keeps its letter, R26a.
func TestRequirementsWithSuffix(t *testing.T) {
	got := Requirements("**R26a.** Вставлено.\n- Дано а.\n\n**R26b. Ещё.** Текст.\n")
	if len(got) != 2 || got[0].ID != "R26a" || len(got[0].Criteria) != 1 || got[1].ID != "R26b" {
		t.Fatalf("got %+v", got)
	}
	refs := References("см. FTR.NAB.CMN-0001-R26c", "FTR.HMR.CMN-0007")
	if len(refs) != 1 || refs[0].Target != "FTR.NAB.CMN-0001" || refs[0].Req != "R26c" {
		t.Fatalf("refs %+v", refs)
	}
}

// DF-04: a folder with only discovery/ is not a candidate (IDX-03); beside a
// gate its files are files of the folder, not documents.
func TestFoldersDiscovery(t *testing.T) {
	dir := "specs/FMS/CAR/FTR.FMS.CAR-0042/"
	if got := folders([]git.TreeEntry{{Path: dir + "discovery/discovery.md"}, {Path: dir + "discovery/flow.png"}}); len(got) != 0 {
		t.Fatalf("only discovery: %+v", got)
	}
	got := folders([]git.TreeEntry{{Path: dir + "product/spec.md"}, {Path: dir + "discovery/discovery.md"}, {Path: dir + "discovery.md"}})
	f := got[dir]
	if f == nil || len(f.docs) != 1 || len(f.files) != 2 || f.titleArea() != "product" {
		t.Fatalf("with product: %+v", f)
	}
}
