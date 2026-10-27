package specindex

import (
	"errors"
	"strings"
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
)

func longDoc() string {
	var b strings.Builder
	b.WriteString("# Big\nintro\n")
	for i := 0; i < 12; i++ {
		b.WriteString("## Section " + string(rune('A'+i)) + "\n")
		b.WriteString(strings.Repeat("Lorem ipsum dolor sit amet. ", 400) + "\n\n")
	}
	return b.String()
}

// AGT-08: a long document gives its beginning up to a section boundary, the
// whole table of contents and the sections left out.
func TestReadLongDocument(t *testing.T) {
	md := longDoc()
	r, err := readDoc("FTR.A.B-0001", "product", md, "", 40000)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || len(r.Markdown) > 40000 || len(r.TOC) != 12 || len(r.OmittedSections) == 0 {
		t.Fatalf("truncated=%v len=%d toc=%d omitted=%v", r.Truncated, len(r.Markdown), len(r.TOC), r.OmittedSections)
	}
	shown := strings.Count(r.Markdown, "\n## ")
	if shown+len(r.OmittedSections) != 12 || !strings.HasPrefix(r.OmittedSections[0], "Section ") {
		t.Fatalf("shown %d omitted %v", shown, r.OmittedSections)
	}
	if r, _ := readDoc("K", "product", "# Small\ntext\n", "", 40000); r.Truncated || r.Markdown != "# Small\ntext\n" {
		t.Fatalf("small %+v", r)
	}
}

// AGT-09, AGT-10: a section by slug or by heading text; an unknown section
// is not_found with the sections to choose from.
func TestReadSection(t *testing.T) {
	md := "# T\n## 1. Проблема\nтекст проблемы\n## 2. Решение\nтекст решения\n"
	for _, sec := range []string{"2-решение", "2. Решение"} {
		r, err := readDoc("K", "product", md, sec, 40000)
		if err != nil || r.Section != "2. Решение" || !strings.Contains(r.Markdown, "текст решения") || strings.Contains(r.Markdown, "проблемы") {
			t.Fatalf("%q: %+v %v", sec, r, err)
		}
	}
	_, err := readDoc("K", "product", md, "Решен", 40000)
	var te *mcp.ToolError
	if !errors.As(err, &te) || !strings.Contains(te.Msg, "not_found") || !strings.Contains(te.Msg, "2. Решение") {
		t.Fatalf("%v", err)
	}
}

// CAT-04: catalog-info.yaml for Backstage with hammurapi/key and the domain of the system.
func TestCatalogInfoExample(t *testing.T) {
	y := CatalogInfoExample("LOG", "DLV", true)
	for _, want := range []string{"kind: Domain", "name: log", "hammurapi/key: LOG", "kind: System", "name: dlv", "hammurapi/key: DLV", "domain: log", "owner: team-log"} {
		if !strings.Contains(y, want) {
			t.Errorf("%q not in\n%s", want, y)
		}
	}
	if strings.Contains(CatalogInfoExample("FMS", "INS", false), "kind: Domain") {
		t.Error("an existing domain is not repeated")
	}
}

// AGT-09: agents refer to a section by its number, as the specifications do:
// «3.3», «§7», «7. Аудит» find the section; a missing number does not.
func TestReadSectionByNumber(t *testing.T) {
	md := "# Спека\n\n## 3. Подтверждение\n\nтекст 3\n\n### 3.3 Подтверждение изменяющих вызовов\n\nтекст 3.3\n\n## 7. Аудит и журнал\n\nтекст 7\n"
	for query, want := range map[string]string{"3.3": "3.3 Подтверждение изменяющих вызовов", "§7": "7. Аудит и журнал", "7. Аудит": "7. Аудит и журнал", "3": "3. Подтверждение"} {
		r, err := readDoc("FTR.NAB.CMN-0002", "arch", md, query, 10000)
		if err != nil || r.Section != want || r.Note != "" {
			t.Fatalf("%q: %+v %v", query, r, err)
		}
	}
	// the number wins over a different title, with a note for the agent
	r, err := readDoc("FTR.NAB.CMN-0002", "arch", md, "7. Песочницы", 10000)
	if err != nil || r.Section != "7. Аудит и журнал" || !strings.Contains(r.Note, "found by number 7") {
		t.Fatalf("mismatch: %+v %v", r, err)
	}
	if _, err := readDoc("FTR.NAB.CMN-0002", "arch", md, "9", 10000); err == nil {
		t.Fatal("a missing number is found")
	}
	if sectionNumber("Подтверждение") != "" || sectionNumber("6.1a Тема") != "6.1a" {
		t.Fatal("sectionNumber")
	}
}
