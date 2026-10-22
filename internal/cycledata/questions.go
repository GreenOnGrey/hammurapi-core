package cycledata

import (
	"regexp"
	"strings"
)

// openQuestionsRe is the heading of the open questions of an analysis.
var openQuestionsRe = regexp.MustCompile(`(?i)^(?:\d+(?:\.\d+)*\.?\s+)?(?:открытые вопросы|open questions|offene fragen|questions ouvertes|preguntas abiertas|开放问题|未决问题|待解决问题)\s*[:：]?$`)

// OpenQuestionsLast moves the section «Открытые вопросы» with its subsections
// to the end of an analysis (FTR.HMR.DISC-0001 design §3.6): the experts read
// what is left to settle last, wherever the agent put it.
func OpenQuestionsLast(md string) string {
	lines := strings.Split(md, "\n")
	start, end, level := -1, len(lines), 0
	fence := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			fence = !fence
			continue
		}
		if fence {
			continue
		}
		n := headingLevel(t)
		if n == 0 {
			continue
		}
		if start < 0 {
			if openQuestionsRe.MatchString(strings.TrimSpace(t[n:])) {
				start, level = i, n
			}
		} else if n <= level {
			end = i
			break
		}
	}
	if start < 0 || end == len(lines) {
		return md
	}
	section := strings.TrimRight(strings.Join(lines[start:end], "\n"), "\n ")
	rest := strings.TrimRight(strings.Join(append(append([]string{}, lines[:start]...), lines[end:]...), "\n"), "\n ")
	return rest + "\n\n" + section + "\n"
}

// headingLevel is the level of an ATX heading, 0 — not a heading.
func headingLevel(t string) int {
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || (n < len(t) && t[n] != ' ') {
		return 0
	}
	return n
}
