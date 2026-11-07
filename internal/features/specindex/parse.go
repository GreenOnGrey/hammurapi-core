// Package specindex indexes the specification repository (FTR.HMR.CMN-0005): a
// periodic check of the default branch finds specifications added outside
// Hammurapi and indexes them as implemented features; an index of documents
// serves the "Specification" navigator, search and the agent's spec_* tools.
package specindex

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/markdown"
)

// Areas of a feature folder that are indexed; other folder names are ignored.
var Areas = []string{"product", "design", "arch", "tech", "qa"}

func validArea(a string) bool {
	for _, x := range Areas {
		if x == a {
			return true
		}
	}
	return false
}

var pathRe = regexp.MustCompile(`^specs/([A-Z][A-Z0-9]{1,9})/([A-Z][A-Z0-9]{1,9})/([^/]+)/([a-z]+)/(.+)$`)

// Loc is a file of a feature folder: specs/<domain>/<system>/<folder>/<area>/<rest>.
type Loc struct {
	Domain, System, Folder, Area, Rest string
}

// Dir is the feature folder with a trailing slash.
func (l Loc) Dir() string {
	return "specs/" + l.Domain + "/" + l.System + "/" + l.Folder + "/"
}

// IsDocument reports whether the file is the area's spec.md.
func (l Loc) IsDocument() bool { return l.Rest == "spec.md" }

// ParsePath parses a repository path; ok is false outside feature folders and
// for unknown areas.
func ParsePath(p string) (Loc, bool) {
	m := pathRe.FindStringSubmatch(p)
	if m == nil || !validArea(m[4]) {
		return Loc{}, false
	}
	return Loc{Domain: m[1], System: m[2], Folder: m[3], Area: m[4], Rest: m[5]}, true
}

// Kinds of indexing problems (tech spec §4.2).
const (
	KindOldFormat       = "old_format"
	KindBadFormat       = "bad_format"
	KindKeyPathMismatch = "key_path_mismatch"
	KindMissingDomain   = "missing_domain"
	KindMissingSystem   = "missing_system"
	KindMissingParent   = "missing_parent"
	KindDeleted         = "deleted"
)

var (
	oldIDRe = regexp.MustCompile(`^([A-Z][A-Z0-9]{1,9})\.([A-Z][A-Z0-9]{1,9})-(\d{4})$`)
	newIDRe = regexp.MustCompile(`^FTR\.([A-Z][A-Z0-9]{1,9})\.([A-Z][A-Z0-9]{1,9})-(\d{4})$`)
)

// Problem is why a folder is not indexed.
type Problem struct {
	Kind    string
	Details map[string]any
}

// CheckID validates the folder name of a feature against its path (R3) and
// returns the feature number.
func CheckID(folder, domain, system string) (int, *Problem) {
	if m := oldIDRe.FindStringSubmatch(folder); m != nil {
		return 0, &Problem{Kind: KindOldFormat, Details: map[string]any{"suggested": "FTR." + folder}}
	}
	m := newIDRe.FindStringSubmatch(folder)
	if m == nil {
		return 0, &Problem{Kind: KindBadFormat, Details: map[string]any{"expected": "FTR.<DOMAIN>.<SYSTEM>-NNNN"}}
	}
	if m[1] != domain || m[2] != system {
		return 0, &Problem{Kind: KindKeyPathMismatch, Details: map[string]any{"id": []string{m[1], m[2]}, "path": []string{domain, system}}}
	}
	n, _ := strconv.Atoi(m[3])
	return n, nil
}

// splitFrontMatter returns the YAML front matter (without the --- lines) and
// the body after it.
func splitFrontMatter(md string) (string, string) {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	if !strings.HasPrefix(md, "---\n") {
		return "", md
	}
	rest := md[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return "", md
	}
	after := rest[end+4:]
	if i := strings.IndexByte(after, '\n'); i >= 0 {
		after = after[i+1:]
	} else {
		after = ""
	}
	return rest[:end], after
}

// Parent reads "parent" from the front matter. A broken front matter is
// reported as an error and treated as no parent (IDX-13).
func Parent(md string) (string, error) {
	fm, _ := splitFrontMatter(md)
	if fm == "" {
		return "", nil
	}
	var v struct {
		Parent string `yaml:"parent"`
	}
	if err := yaml.Unmarshal([]byte(fm), &v); err != nil {
		return "", fmt.Errorf("front matter: %w", err)
	}
	return strings.TrimSpace(v.Parent), nil
}

// line is a line of the body with whether it is inside a code block.
type line struct {
	text string
	code bool
}

func lines(body string) []line {
	var out []line
	fence := ""
	for _, l := range strings.Split(body, "\n") {
		t := strings.TrimSpace(l)
		if fence == "" && (strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")) {
			fence = t[:3]
			out = append(out, line{l, true})
			continue
		}
		if fence != "" {
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
			out = append(out, line{l, true})
			continue
		}
		out = append(out, line{l, false})
	}
	return out
}

var headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*\s*$`)

// Title is the first level-1 heading outside code blocks.
func Title(md string) string {
	_, body := splitFrontMatter(md)
	for _, l := range lines(body) {
		if l.code {
			continue
		}
		if m := headingRe.FindStringSubmatch(l.text); m != nil && len(m[1]) == 1 {
			return plain(m[2])
		}
	}
	return ""
}

// Heading is an entry of the table of contents.
type Heading struct {
	Level int    `json:"level"`
	Text  string `json:"text"`
	Slug  string `json:"slug"`
	line  int
}

// TOC lists the headings of levels 2–3 outside code blocks with unique slugs.
func TOC(md string) []Heading {
	_, body := splitFrontMatter(md)
	return toc(lines(body))
}

func toc(ls []line) []Heading {
	seen := map[string]int{}
	out := []Heading{}
	for i, l := range ls {
		if l.code {
			continue
		}
		m := headingRe.FindStringSubmatch(l.text)
		if m == nil || len(m[1]) < 2 || len(m[1]) > 3 {
			continue
		}
		text := plain(m[2])
		s := Slug(text)
		seen[s]++
		if n := seen[s]; n > 1 {
			s = fmt.Sprintf("%s-%d", s, n)
		}
		out = append(out, Heading{Level: len(m[1]), Text: text, Slug: s, line: i})
	}
	return out
}

// Slug is the anchor of a heading: lower case, spaces as "-", only letters
// (any script), digits and "-".
func Slug(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(text)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-':
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteByte('-')
		}
	}
	return b.String()
}

var (
	linkRe   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	markRe   = regexp.MustCompile("[*_`~]+")
	htmlRe   = regexp.MustCompile(`<[^>]+>`)
	spacesRe = regexp.MustCompile(`[ \t]+`)
)

// plain removes inline markup from a fragment of markdown.
func plain(s string) string {
	s = linkRe.ReplaceAllString(s, "$1")
	s = markRe.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// SearchText is the document without markup syntax; the content of code
// blocks is kept.
func SearchText(md string) string {
	_, body := splitFrontMatter(md)
	var b strings.Builder
	for _, l := range lines(body) {
		t := l.text
		if l.code {
			if s := strings.TrimSpace(t); strings.HasPrefix(s, "```") || strings.HasPrefix(s, "~~~") {
				continue
			}
			b.WriteString(t)
			b.WriteByte('\n')
			continue
		}
		t = strings.TrimSpace(t)
		if strings.Trim(t, "|-: ") == "" && strings.Contains(t, "-") && strings.Contains(t, "|") {
			continue // table delimiter row
		}
		t = strings.TrimLeft(t, "#>")
		t = strings.TrimPrefix(strings.TrimSpace(t), "- ")
		t = strings.ReplaceAll(t, "|", " ")
		t = htmlRe.ReplaceAllString(t, " ")
		t = plain(t)
		t = spacesRe.ReplaceAllString(t, " ")
		if t != "" {
			b.WriteString(t)
			b.WriteByte('\n')
		}
	}
	return strings.TrimSpace(b.String())
}

// Requirement is an R<n> of a product specification with its acceptance criteria.
type Requirement struct {
	ID       string   `json:"id"`
	Text     string   `json:"text"`
	Criteria []string `json:"criteria"`
	Section  string   `json:"section"`
}

var (
	reqRe       = regexp.MustCompile(`^\*\*(` + markdown.ReqID + `)\.\*\*\s*(.*)$`)
	reqTitledRe = regexp.MustCompile(`^\*\*(` + markdown.ReqID + `)\.\s+([^*]+)\*\*\s*(.*)$`) // **R1. Title.** Text.
	listItemRe  = regexp.MustCompile(`^\s*(?:[-*+]|\d+[.)])\s+(.*)$`)
	criterionRe = regexp.MustCompile(`^(?i)(Дано|Given)[\s,:]`) // \b is ASCII-only in RE2
)

func reqLine(s string) (id, text string, ok bool) {
	if m := reqRe.FindStringSubmatch(s); m != nil {
		return m[1], m[2], true
	}
	if m := reqTitledRe.FindStringSubmatch(s); m != nil {
		return m[1], strings.TrimSpace(m[2] + " " + m[3]), true
	}
	return "", "", false
}

// Requirements extracts **R<n>.** and **R<n>. Title.** paragraphs: the text up to the first list or
// the next requirement, and the criteria — list items that start with "Дано"
// (or "Given") up to the next requirement or heading. Details of the requirement,
// a list or a paragraph, may come before them (FTR.HMR.CMN-0005 tech §4.3).
func Requirements(md string) []Requirement {
	_, body := splitFrontMatter(md)
	ls := lines(body)
	heads := toc(ls)
	section := func(i int) string {
		s := ""
		for _, h := range heads {
			if h.line < i {
				s = h.Text
			}
		}
		return s
	}
	var out []Requirement
	for i := 0; i < len(ls); i++ {
		if ls[i].code {
			continue
		}
		id, first, ok := reqLine(strings.TrimSpace(ls[i].text))
		if !ok {
			continue
		}
		r := Requirement{ID: id, Criteria: []string{}, Section: section(i)}
		text := []string{first}
		j := i + 1
		for ; j < len(ls); j++ {
			t := strings.TrimSpace(ls[j].text)
			if ls[j].code || t == "" || listItemRe.MatchString(ls[j].text) || reqRe.MatchString(t) || reqTitledRe.MatchString(t) || headingRe.MatchString(t) {
				break
			}
			text = append(text, t)
		}
		for ; j < len(ls); j++ {
			if ls[j].code {
				continue
			}
			t := strings.TrimSpace(ls[j].text)
			if reqRe.MatchString(t) || reqTitledRe.MatchString(t) || headingRe.MatchString(t) {
				break
			}
			lm := listItemRe.FindStringSubmatch(ls[j].text)
			if lm == nil {
				continue
			}
			if item := plain(lm[1]); criterionRe.MatchString(item) {
				r.Criteria = append(r.Criteria, item)
			}
		}
		r.Text = plain(strings.Join(text, " "))
		out = append(out, r)
		i = j - 1
	}
	return out
}

// Reference is a mention of a feature or a requirement in a document.
type Reference struct {
	Target  string // FTR.FMS.CAR-0002
	Req     string // R3 or ""
	Section string
	Snippet string
}

var refRe = regexp.MustCompile(markdown.FeatureKey + `(?:-` + markdown.ReqID + `)?`)

// References lists mentions of features and requirements outside code blocks;
// mentions of the document's own feature are skipped.
func References(md, self string) []Reference {
	_, body := splitFrontMatter(md)
	ls := lines(body)
	heads := toc(ls)
	var out []Reference
	for i, l := range ls {
		if l.code {
			continue
		}
		for _, m := range refRe.FindAllStringIndex(l.text, -1) {
			id := l.text[m[0]:m[1]]
			target, req := id, ""
			if k := strings.LastIndex(id, "-R"); k > 0 && strings.Count(id, "-") == 2 {
				target, req = id[:k], id[k+1:]
			}
			if target == self {
				continue
			}
			sec := ""
			for _, h := range heads {
				if h.line <= i {
					sec = h.Text
				}
			}
			out = append(out, Reference{Target: target, Req: req, Section: sec, Snippet: snippetAround(l.text, m[0], m[1])})
		}
	}
	return out
}

func snippetAround(s string, from, to int) string {
	const pad = 80
	a, b := from-pad, to+pad
	pre, post := "…", "…"
	if a <= 0 {
		a, pre = 0, ""
	}
	if b >= len(s) {
		b, post = len(s), ""
	}
	// Do not cut a UTF-8 rune.
	for a > 0 && !utf8Start(s[a]) {
		a--
	}
	for b < len(s) && !utf8Start(s[b]) {
		b++
	}
	return pre + strings.TrimSpace(plain(s[a:b])) + post
}

func utf8Start(c byte) bool { return c&0xC0 != 0x80 }

// Section is a part of a document from a heading of level 2–3 to the next one.
type Section struct {
	Heading Heading
	Text    string
}

// Sections splits a document: the part before the first heading of level 2–3
// (title, preamble) and one section per heading.
func Sections(md string) (string, []Section) {
	_, body := splitFrontMatter(md)
	ls := lines(body)
	heads := toc(ls)
	raw := strings.Split(body, "\n")
	if len(heads) == 0 {
		return body, nil
	}
	head := strings.Join(raw[:heads[0].line], "\n")
	var out []Section
	for i, h := range heads {
		end := len(raw)
		if i+1 < len(heads) {
			end = heads[i+1].line
		}
		out = append(out, Section{Heading: h, Text: strings.Join(raw[h.line:end], "\n")})
	}
	return head, out
}
