package issuetopics

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FTR.HMR.DISC-0001 TP-10: the title of a topic is at most 120 characters.
func TestTitle(t *testing.T) {
	if got := Title("ISS.FMS-0007", " Скидка за ранний возврат "); got != "ISS.FMS-0007 — Скидка за ранний возврат" {
		t.Fatalf("%q", got)
	}
	long := Title("ISS.FMS-0007", strings.Repeat("длинное название ", 20))
	if utf8.RuneCountInString(long) > MaxTitle || !strings.HasSuffix(long, "…") || !strings.HasPrefix(long, "ISS.FMS-0007 — ") {
		t.Fatalf("%d %q", utf8.RuneCountInString(long), long)
	}
}
