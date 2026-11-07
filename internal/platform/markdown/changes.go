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
