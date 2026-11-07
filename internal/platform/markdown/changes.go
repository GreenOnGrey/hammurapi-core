package markdown

import (
	"regexp"
	"strings"
)

// FeatureKey is the pattern of a feature key: FTR.<DOMAIN>.<SYSTEM>-NNNN.
const FeatureKey = `FTR\.[A-Z][A-Z0-9]{1,9}\.[A-Z][A-Z0-9]{1,9}-\d{4}`

var featureKeyRe = regexp.MustCompile(FeatureKey)

// Change is a row of the section "Изменения в других решениях" (FTR.HMR.DEV-0002 R1):
// the changed feature, the requirement (empty for the whole feature) and what changed.
type Change struct {
	Feature string
	Req     string
	Note    string
}

// changesTitles are the headings of the section, lower case.
var changesTitles = []string{"изменения в других решениях", "changes to other features"}

// ParseChanges reads the table of the section "Изменения в других решениях"
// (FTR.HMR.DEV-0002 tech §4.1). A row without a feature key is skipped; the
// requirement is the cell as written, empty for "всё решение" or a dash: a
// wrong cell is reported by whoever checks the rows.
func ParseChanges(doc string) []Change {
	_, body, _ := SplitFrontMatter(doc)
	section := sectionText(stripFences(body), changesTitles...) // an example in a code block is not the section
	if section == "" {
		return nil
	}
	var out []Change
	for _, t := range tables(section) {
		keyCol := col(t.header, "решени", "feature")
		reqCol := col(t.header, "требован", "requirement")
		noteCol := col(t.header, "что измен", "what changed", "изменил", "change")
		if keyCol < 0 || reqCol < 0 {
			continue
		}
		if noteCol == keyCol || noteCol == reqCol {
			noteCol = -1 // the note is optional: the links need only the key and the requirement
		}
		for _, r := range t.rows {
			key := featureKeyRe.FindString(cell(r, keyCol))
			if key == "" {
				continue
			}
			out = append(out, Change{Feature: key, Req: changeReq(cell(r, reqCol)), Note: strings.TrimSpace(cell(r, noteCol))})
		}
	}
	return out
}

// changeReq is the requirement of a row: "" for the whole feature.
func changeReq(s string) string {
	s = strings.TrimSpace(strings.Trim(s, "`* "))
	switch strings.ToLower(s) {
	case "", "—", "–", "-", "всё решение", "все решение", "whole feature":
		return ""
	}
	return s
}

// Note is a mark put at a requirement of a changed feature (FTR.HMR.DEV-0002 R4):
// Req is the requirement of the target ("" for the whole feature), SourceReq
// the requirement of the source that describes the change ("" if none), Text
// what changed.
type Note struct {
	Req, SourceReq, Text string
}

// notePrefix starts every note of a source: the lines ApplyNotes owns.
func notePrefix(source string) string { return "*Изменено в `" + source + "`" }

// NoteLine is the line of a note: *Изменено в `<source>` R<n>: <text>.*
func NoteLine(source string, n Note) string {
	b := notePrefix(source)
	if n.SourceReq != "" {
		b += " " + n.SourceReq
	}
	text := strings.TrimSpace(strings.Trim(strings.TrimSpace(n.Text), "*"))
	if text == "" {
		return b + ".*"
	}
	if r := text[len(text)-1]; r != '.' && r != '!' && r != '?' {
		text += "."
	}
	return b + ": " + text + "*"
}

var (
	// noteReqRe is the first line of a requirement as the index reads it (FTR.HMR.CMN-0005 tech §4.3).
	noteReqRe = regexp.MustCompile(`^\*\*(` + ReqID + `)(?:\.\*\*|\.\s+[^*]+\*\*)`)
	// headerLineRe is a line of the header of a document: **Версия:** 1.0
	headerLineRe = regexp.MustCompile(`^\*\*[^*]+:\*\*`)
)

// ApplyNotes puts the notes of source into a document of another feature
// (FTR.HMR.DEV-0002 arch §3.1): the notes of source already there are removed,
// then each note goes right after the first line of its requirement, after
// notes already there; a note of the whole feature goes after the header lines
// **…:**. Notes of other features and every other line stay as they are, a
// requirement missing in the document gets no note, and applying the same
// notes again gives the same text.
func ApplyNotes(doc, source string, notes []Note) string {
	prefix := notePrefix(source)
	lines := strings.Split(doc, "\n")
	kept := make([]string, 0, len(lines))
	code := make([]bool, 0, len(lines))
	fence := ""
	for _, l := range lines {
		t := strings.TrimSpace(l)
		inCode := fence != ""
		if fence == "" && (strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")) {
			fence, inCode = t[:3], true
		} else if fence != "" && strings.HasPrefix(t, fence) {
			fence = ""
		}
		if !inCode && strings.HasPrefix(t, prefix) {
			continue
		}
		kept = append(kept, l)
		code = append(code, inCode)
	}
	at := func(req string) int { // the index of the line after which the note goes, -1 if none
		if req == "" {
			last, from := -1, 0
			if len(kept) > 0 && strings.TrimSpace(kept[0]) == "---" { // front matter of a fix: not the header
				for j := 1; j < len(kept); j++ {
					if strings.TrimSpace(kept[j]) == "---" {
						from = j + 1
						break
					}
				}
			}
			for i := from; i < len(kept); i++ {
				l := kept[i]
				t := strings.TrimSpace(l)
				if code[i] {
					continue
				}
				if t == "---" || strings.HasPrefix(t, "## ") {
					break
				}
				if headerLineRe.MatchString(t) || strings.HasPrefix(t, "# ") && last < 0 {
					last = i
				}
			}
			return last
		}
		for i, l := range kept {
			if m := noteReqRe.FindStringSubmatch(strings.TrimSpace(l)); m != nil && !code[i] && m[1] == req {
				return i
			}
		}
		return -1
	}
	insert := map[int][]string{}
	for _, n := range notes {
		if i := at(n.Req); i >= 0 {
			insert[i] = append(insert[i], NoteLine(source, n))
		}
	}
	if len(insert) == 0 {
		return strings.Join(kept, "\n")
	}
	out := make([]string, 0, len(kept)+len(notes))
	for i := 0; i < len(kept); i++ {
		out = append(out, kept[i])
		add, ok := insert[i]
		if !ok {
			continue
		}
		for i+1 < len(kept) && strings.HasPrefix(strings.TrimSpace(kept[i+1]), "*Изменено в `") { // after the notes of other features
			i++
			out = append(out, kept[i])
		}
		out = append(out, add...)
	}
	return strings.Join(out, "\n")
}
