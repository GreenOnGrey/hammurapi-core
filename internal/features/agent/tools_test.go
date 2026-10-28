package agent

import (
	"strings"
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
)

// QS-18, QS-19: the brief is prepared in the order of R28, and no
// description of a tool names a skill (R29).
func TestRevisionToolDescription(t *testing.T) {
	for _, want := range []string{"read_issue", "from what you know about the user", "confirm each such answer",
		"a few at a time", "show the whole brief", "only what concerns this issue", "never write it yourself", "Skills or instructions you have may refine"} {
		if !strings.Contains(RevisionToolDescription, want) {
			t.Fatalf("no %q in the description", want)
		}
	}
	for _, tl := range Tools(ToolDeps{}) {
		for _, skill := range []string{"grill", "skill \"", "load the skill"} {
			if strings.Contains(strings.ToLower(tl.Description), skill) {
				t.Fatalf("%s names a skill: %s", tl.Name, tl.Description)
			}
		}
	}
}

// QS-17: read_issue shows the open questions with numbers, options and
// recommendations.
func TestWriteQuestions(t *testing.T) {
	var b strings.Builder
	writeQuestions(&b, []cycledata.Question{
		{ID: "Q2", Round: 1, QuestionText: cycledata.QuestionText{Title: "Порог", Body: "Час или доля?", Recommended: "2 часа", Why: "проще"}},
		{ID: "Q5", Round: 2, QuestionText: cycledata.QuestionText{Title: "Источник метрики", Options: []string{"VictoriaMetrics", "ClickHouse"}, Recommended: "VictoriaMetrics"}},
	})
	want := "\n--- Open questions (round 2) ---\nQ2 — Порог\nЧас или доля?\nrecommended: 2 часа — проще\n" +
		"Q5 — Источник метрики\noptions: VictoriaMetrics | ClickHouse\nrecommended: VictoriaMetrics\n"
	if b.String() != want {
		t.Fatalf("got %q", b.String())
	}
	b.Reset()
	if writeQuestions(&b, nil); b.Len() != 0 {
		t.Fatal("no questions — no block")
	}
}
