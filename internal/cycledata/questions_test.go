package cycledata

import "testing"

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
