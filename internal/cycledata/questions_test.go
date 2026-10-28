package cycledata

import (
	"strconv"
	"strings"
	"testing"
)

// OQ-01: the open questions close the analysis wherever the agent put them;
// a heading in a code block and a document without them stay as they are.
func TestOpenQuestionsLast(t *testing.T) {
	for in, want := range map[string]string{
		"## Проблема\n\nп\n\n## Открытые вопросы\n\n- в1\n\n### Детали\n\nд\n\n## Риски\n\nр\n": "## Проблема\n\nп\n\n## Риски\n\nр\n\n## Открытые вопросы\n\n- в1\n\n### Детали\n\nд\n",
		"## 4. Open questions:\n- q\n## Risks\n- r":                  "## Risks\n- r\n\n## 4. Open questions:\n- q\n",
		"## Проблема\n\n## Открытые вопросы\n\n- в\n":                "## Проблема\n\n## Открытые вопросы\n\n- в\n",
		"## Проблема\n\n```\n## Открытые вопросы\n```\n\n## Риски\n": "## Проблема\n\n```\n## Открытые вопросы\n```\n\n## Риски\n",
		"## Проблема\n\nвопросы\n":                                   "## Проблема\n\nвопросы\n",
	} {
		if got := OpenQuestionsLast(in); got != want {
			t.Errorf("%q\n got %q\nwant %q", in, got, want)
		}
	}
}

// QS-01…QS-04: the server numbers the questions through all rounds, keeps
// the number and round of a question asked again, drops what is over the
// limit or has no recommendation and cuts long fields instead of refusing.
func TestNumberQuestions(t *testing.T) {
	q := func(id, title string) QuestionInput {
		return QuestionInput{ID: id, QuestionText: QuestionText{Title: title, Body: "b", Recommended: "r"}}
	}
	ids := func(qs []Question) string {
		out := []string{}
		for _, x := range qs {
			out = append(out, x.ID+"/"+strconv.Itoa(x.Round))
		}
		return strings.Join(out, " ")
	}
	first, c1, dropped := NumberQuestions(nil, QuestionCounters{}, []QuestionInput{q("", "a"), q("Q9", "b"), q("", "c")})
	if ids(first) != "Q1/1 Q2/1 Q3/1" || c1 != (QuestionCounters{Seq: 3, Round: 1}) || dropped != 0 {
		t.Fatalf("first round: %s %+v", ids(first), c1)
	}
	second, c2, _ := NumberQuestions(first, c1, []QuestionInput{q("q2 ", "b again"), q("", "d"), q("", "e")})
	if ids(second) != "Q2/1 Q4/2 Q5/2" || c2 != (QuestionCounters{Seq: 5, Round: 2}) {
		t.Fatalf("second round: %s %+v", ids(second), c2)
	}
	// only questions asked again: no new round begins
	if _, again, _ := NumberQuestions(second, c2, []QuestionInput{q("Q4", "d")}); again != c2 {
		t.Fatalf("asked again: %+v", again)
	}
	// every question answered: the next round still gets the next number
	if next, _, _ := NumberQuestions(nil, c2, []QuestionInput{q("", "f")}); ids(next) != "Q6/3" {
		t.Fatalf("after all answered: %s", ids(next))
	}
	// QS-01: over the limit; a question without a recommendation; the same number twice
	many := []QuestionInput{q("Q2", "x"), q("Q2", "y"), {QuestionText: QuestionText{Title: "no rec"}}, q("", "1"), q("", "2"), q("", "3"), q("", "4")}
	if got, _, dropped := NumberQuestions(second, c2, many); ids(got) != "Q2/1 Q6/3 Q7/3 Q8/3 Q9/3" || dropped != 2 {
		t.Fatalf("limit: %s dropped %d", ids(got), dropped)
	}
	// QS-04: long fields are cut, empty options and the fifth one dropped
	long := QuestionInput{QuestionText: QuestionText{Title: strings.Repeat("т", 300), Recommended: "r", Options: []string{"a", " ", "b", "c", "d", "e"}}}
	got, _, _ := NumberQuestions(nil, QuestionCounters{}, []QuestionInput{long})
	if c := got[0]; len([]rune(c.Title)) != maxQuestionTitle || !strings.HasSuffix(c.Title, "…") || len(c.Options) != maxQuestionOptions || c.Options[1] != "b" {
		t.Fatalf("clip: %q %v", c.Title, c.Options)
	}
}
