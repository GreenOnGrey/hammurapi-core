package markdown

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Traceability parsers (FTR.HMR.CMN-0002 R15): requirements of the product spec,
// test cases of the qa spec, services of the tech spec and the rollout order
// of the arch spec. They follow the structure of the rules templates; when a
// document does not match, callers fall back to the agent.

// Req is a requirement with its ID (R3) and text.
type Req struct {
	ID   string
	Text string
}

// TC is a test case row.
type TC struct {
	ID     string
	Level  string
	ReqIDs []string
	Title  string
}

// ServiceReqs is a row of the tech spec's service table.
type ServiceReqs struct {
	Service string
	ReqIDs  []string
}

var (
	reqRe = regexp.MustCompile(`\*\*(?:FTR\.[A-Z0-9.]+-\d+-)?(R\d+)\.?\*\*\.?\s*(.*)`)
	// reqTitledRe is the same ID with a title inside the bold: **R1. Title.** Text.
	reqTitledRe = regexp.MustCompile(`^\*\*(?:FTR\.[A-Z0-9.]+-\d+-)?(R\d+)\.\s+([^*]+)\*\*\s*(.*)$`)
	reqRefRe    = regexp.MustCompile(`R(\d+)(?:\s*[–—-]\s*R(\d+))?`)
	tcIDRe      = regexp.MustCompile(`^[A-Z][A-Z0-9]*-\d+$`)
	headingRe   = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	listItemRe  = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+(.*)$`)
)

// ParseRequirements extracts **R<n>.** requirements (also <feature key>-R<n>
// and **R<n>. Title.**).
func ParseRequirements(doc string) []Req {
	_, body, _ := SplitFrontMatter(doc)
	seen := map[string]bool{}
	var out []Req
	for _, line := range strings.Split(stripCode(body), "\n") {
		t := strings.TrimSpace(line)
		if lm := listItemRe.FindStringSubmatch(t); lm != nil && !strings.HasPrefix(t, "**") {
			t = strings.TrimSpace(lm[1])
		}
		if !strings.HasPrefix(t, "**") {
			continue
		}
		id, text := "", ""
		if m := reqTitledRe.FindStringSubmatch(t); m != nil {
			id, text = m[1], strings.TrimSpace(m[2]+" "+m[3])
		} else if m := reqRe.FindStringSubmatch(t); m != nil {
			id, text = m[1], strings.TrimSpace(m[2])
		} else {
			continue
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, Req{ID: id, Text: text})
	}
	return out
}

// UnnumberedRequirements lists top-level items of requirement sections that
// have no R-ID (acceptance criteria "Given…/Дано…" are ignored). Items under a
// requirement paragraph are its details, as in the product template.
func UnnumberedRequirements(doc string) []string {
	_, body, _ := SplitFrontMatter(doc)
	var out []string
	inReqs := false
	level := 0
	underReq := false
	for _, line := range strings.Split(stripCode(body), "\n") {
		if m := headingRe.FindStringSubmatch(line); m != nil {
			underReq = false
			l := len(m[1])
			title := strings.ToLower(m[2])
			if strings.Contains(title, "требован") || strings.Contains(title, "requirement") || strings.Contains(title, "anforderung") ||
				strings.Contains(title, "requisito") || strings.Contains(title, "需求") {
				inReqs, level = true, l
			} else if inReqs && l <= level {
				inReqs = false
			}
			continue
		}
		if !inReqs || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		m := listItemRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			if t := strings.TrimSpace(line); strings.HasPrefix(t, "**") && isRequirement(t) {
				underReq = true
			}
			continue
		}
		item := strings.TrimSpace(m[1])
		if item != "" && isRequirement(item) {
			underReq = false
			continue
		}
		if item == "" || underReq || isAcceptance(item) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func isRequirement(s string) bool {
	return reqRe.MatchString(s) || reqTitledRe.MatchString(s)
}

func isAcceptance(s string) bool {
	low := strings.ToLower(s)
	for _, p := range []string{"дано", "given", "когда", "when", "тогда", "then", "angenommen", "dado", "假设"} {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

// table is a parsed markdown table.
type table struct {
	header []string
	rows   [][]string
}

func tables(doc string) []table {
	var out []table
	var cur *table
	for _, line := range strings.Split(stripFences(doc), "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "|") {
			if cur != nil {
				out = append(out, *cur)
				cur = nil
			}
			continue
		}
		cells := splitRow(t)
		if cur == nil {
			cur = &table{header: cells}
			continue
		}
		if isSeparator(cells) {
			continue
		}
		cur.rows = append(cur.rows, cells)
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

func splitRow(t string) []string {
	t = strings.TrimSuffix(strings.TrimPrefix(t, "|"), "|")
	parts := strings.Split(t, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func isSeparator(cells []string) bool {
	for _, c := range cells {
		if strings.Trim(c, ":- ") != "" {
			return false
		}
	}
	return true
}

func col(header []string, keys ...string) int {
	for i, h := range header {
		low := strings.ToLower(h)
		for _, k := range keys {
			if strings.Contains(low, k) {
				return i
			}
		}
	}
	return -1
}

func cell(row []string, i int) string {
	if i < 0 || i >= len(row) {
		return ""
	}
	return row[i]
}

// ParseReqRefs expands "R1, R3–R5" into [R1 R3 R4 R5].
func ParseReqRefs(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range reqRefRe.FindAllStringSubmatch(s, -1) {
		a, _ := strconv.Atoi(m[1])
		b := a
		if m[2] != "" {
			b, _ = strconv.Atoi(m[2])
		}
		if b < a || b-a > 200 {
			b = a
		}
		for n := a; n <= b; n++ {
			id := "R" + strconv.Itoa(n)
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// ParseTestCases extracts test case rows: the first column is the ID (QA-03);
// columns "Requirements", "Level" and "Scenario" are recognized in RU/EN/DE/ES/ZH.
func ParseTestCases(doc string) []TC {
	var out []TC
	seen := map[string]bool{}
	for _, t := range tables(doc) {
		reqCol := col(t.header, "требован", "requirement", "anforderung", "requisito", "需求")
		lvlCol := col(t.header, "ур", "level", "ebene", "nivel", "级别")
		titleCol := col(t.header, "сценарий", "scenario", "szenario", "escenario", "场景")
		if reqCol < 0 {
			continue
		}
		for _, r := range t.rows {
			id := strings.Trim(cell(r, 0), "`* ")
			if !tcIDRe.MatchString(id) || seen[id] {
				continue
			}
			seen[id] = true
			reqs := ParseReqRefs(cell(r, reqCol))
			if reqs == nil {
				reqs = []string{}
			}
			out = append(out, TC{ID: id, Level: strings.Trim(cell(r, lvlCol), "` "), ReqIDs: reqs, Title: cell(r, titleCol)})
		}
	}
	return out
}

// ParseTechServices extracts the service table of the tech spec: a table with
// a "Service" column and a "Requirements" column.
func ParseTechServices(doc string) []ServiceReqs {
	var out []ServiceReqs
	seen := map[string]int{}
	for _, t := range tables(doc) {
		svcCol := col(t.header, "сервис", "service", "dienst", "servicio", "服务")
		reqCol := col(t.header, "требован", "requirement", "anforderung", "requisito", "需求")
		if svcCol < 0 || reqCol < 0 {
			continue
		}
		for _, r := range t.rows {
			svc := strings.Trim(cell(r, svcCol), "`* ")
			if svc == "" || svc == "—" || svc == "-" {
				continue
			}
			reqs := ParseReqRefs(cell(r, reqCol))
			if i, ok := seen[svc]; ok {
				out[i].ReqIDs = append(out[i].ReqIDs, reqs...)
				continue
			}
			seen[svc] = len(out)
			out = append(out, ServiceReqs{Service: svc, ReqIDs: reqs})
		}
	}
	for i := range out {
		sort.Strings(out[i].ReqIDs)
		out[i].ReqIDs = dedupe(out[i].ReqIDs)
		if out[i].ReqIDs == nil {
			out[i].ReqIDs = []string{}
		}
	}
	return out
}

// ParseRolloutOrder orders services by their first mention in the arch spec's
// "Rollout order" section ("Порядок выката"). Services not mentioned keep their
// relative order at the end.
func ParseRolloutOrder(arch string, services []string) []string {
	section := sectionText(arch, "порядок выката", "rollout order", "rollout", "deployment order", "reihenfolge", "orden de despliegue", "发布顺序")
	type pos struct {
		svc string
		at  int
	}
	var ps []pos
	for i, s := range services {
		at := -1
		if section != "" {
			if m := regexp.MustCompile(`(?i)(^|[^a-z0-9_-])` + regexp.QuoteMeta(s) + `($|[^a-z0-9_-])`).FindStringIndex(section); m != nil {
				at = m[0]
			}
		}
		if at < 0 {
			at = 1_000_000 + i
		}
		ps = append(ps, pos{s, at})
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].at < ps[j].at })
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.svc
	}
	return out
}

func sectionText(doc string, titles ...string) string {
	lines := strings.Split(doc, "\n")
	var b strings.Builder
	in := false
	level := 0
	for _, line := range lines {
		if m := headingRe.FindStringSubmatch(line); m != nil {
			low := strings.ToLower(m[2])
			match := false
			for _, t := range titles {
				if strings.Contains(low, t) {
					match = true
				}
			}
			if match {
				in, level = true, len(m[1])
				continue
			}
			if in && len(m[1]) <= level {
				break
			}
		}
		if in {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func dedupe(xs []string) []string {
	var out []string
	for i, x := range xs {
		if i == 0 || x != xs[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// stripFences removes fenced code blocks but keeps inline code.
func stripFences(s string) string {
	var out []string
	inFence := false
	for _, l := range strings.Split(s, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// FillSection puts content right after the first heading whose text contains
// one of titles (lowercase), replacing the placeholder text of the template
// section; if there is no such heading, a "## <fallback>" section is appended.
func FillSection(doc, fallback, content string, titles ...string) string {
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		m := headingRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		low := strings.ToLower(m[2])
		for _, t := range titles {
			if !strings.Contains(low, t) {
				continue
			}
			level := len(m[1])
			end := len(lines)
			for j := i + 1; j < len(lines); j++ {
				if h := headingRe.FindStringSubmatch(lines[j]); h != nil && len(h[1]) <= level {
					end = j
					break
				}
			}
			out := append([]string{}, lines[:i+1]...)
			out = append(out, "", strings.TrimRight(content, "\n"), "")
			return strings.Join(append(out, lines[end:]...), "\n")
		}
	}
	return strings.TrimRight(doc, "\n") + "\n\n## " + fallback + "\n\n" + strings.TrimRight(content, "\n") + "\n"
}
