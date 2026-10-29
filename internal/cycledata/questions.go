package cycledata

import (
	"regexp"
	"strconv"
	"strings"
)

// The open questions of the researcher to the experts (FTR.HMR.DISC-0001 R26,
// tech §5.6): one round at a time, numbered by the server through all rounds.

// MaxQuestions bounds the open questions of a round and MaxRounds the rounds
// of an issue; the rest are dropped, not refused: a refusal made the agent
// write the whole document again. Without the bound of rounds the researcher
// went deeper and deeper (18 rounds on ISS.NAB-0009).
const (
	MaxQuestions = 10
	MaxRounds    = 5
)

// Bounds of the fields of a question; a longer field is cut with an ellipsis.
const (
	maxQuestionTitle   = 200
	maxQuestionBody    = 2000
	maxQuestionOptions = 4
	maxQuestionOption  = 200
	maxRecommended     = 500
	maxWhy             = 1000
)

// QuestionText is what a question asks and recommends.
type QuestionText struct {
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Options     []string `json:"options"`
	Recommended string   `json:"recommended"`
	Why         string   `json:"why"`
}

// Question is an open question of the researcher to the experts.
type Question struct {
	ID    string `json:"id"`
	Round int    `json:"round"`
	QuestionText
}

// QuestionInput is a question as save_discovery gives it: ID only for a
// question asked again.
type QuestionInput struct {
	ID string `json:"id"`
	QuestionText
}

// QuestionCounters are the last number and the last round given out.
type QuestionCounters struct {
	Seq, Round int
}

// QuestionID is the number of a question as the agents and experts may
// write it: «q3 » is Q3.
func QuestionID(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }

// AnswerSource is where an answer comes from (R27, R28): an expert on the
// page of the issue, the user in the chat, the personal agent from what it
// knows of the user, confirmed by the user.
type AnswerSource string

// Sources of an answer.
const (
	AnswerExpert AnswerSource = "expert"
	AnswerUser   AnswerSource = "user"
	AnswerMemory AnswerSource = "memory"
)

// OfBrief tells a source a brief of the personal agent may give.
func (s AnswerSource) OfBrief() bool { return s == AnswerUser || s == AnswerMemory }

// Answer is an answer to an open question in an instruction to the
// researcher; Question is the title of the question, set by the server.
type Answer struct {
	QuestionID string       `json:"questionId"`
	Question   string       `json:"question"`
	Answer     string       `json:"answer"`
	Source     AnswerSource `json:"source"`
}

// NumberQuestions numbers the questions of a new revision: a question asked
// again keeps its number and round, a new one gets the next number and the
// next round; numbers are never reused. New questions after MaxRounds are
// dropped, those asked again stay. dropped counts the questions over the
// bounds or without a title or a recommendation.
func NumberQuestions(prev []Question, c QuestionCounters, in []QuestionInput) (open []Question, next QuestionCounters, dropped int) {
	asked := map[string]Question{}
	for _, q := range prev {
		asked[q.ID] = q
	}
	open, next = []Question{}, c
	begun := false
	for _, qi := range in {
		q := Question{QuestionText: QuestionText{Title: clip(qi.Title, maxQuestionTitle), Body: clip(qi.Body, maxQuestionBody),
			Recommended: clip(qi.Recommended, maxRecommended), Why: clip(qi.Why, maxWhy), Options: []string{}}}
		for _, o := range qi.Options {
			if o = clip(o, maxQuestionOption); o != "" && len(q.Options) < maxQuestionOptions {
				q.Options = append(q.Options, o)
			}
		}
		if q.Title == "" || q.Recommended == "" || len(open) == MaxQuestions {
			dropped++
			continue
		}
		id := QuestionID(qi.ID)
		if p, ok := asked[id]; ok {
			q.ID, q.Round = p.ID, p.Round
			delete(asked, id) // a second question with the number is new
		} else {
			if !begun && next.Round >= MaxRounds {
				dropped++
				continue
			}
			if !begun {
				begun = true
				next.Round++
			}
			next.Seq++
			q.ID, q.Round = "Q"+strconv.Itoa(next.Seq), next.Round
		}
		open = append(open, q)
	}
	return open, next, dropped
}

// LastRound is the latest round of the open questions, 0 — none.
func LastRound(qs []Question) int {
	n := 0
	for _, q := range qs {
		n = max(n, q.Round)
	}
	return n
}

// clip trims s and cuts it to n characters with an ellipsis.
func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

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
