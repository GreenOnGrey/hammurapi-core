package gategen

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// The questions of the writer by gate (FTR.HMR.DEV-0001 R10–R14): the mechanics
// of the questions of Discovery; a question about skipping a gate exists before
// the gate, so they live in their own table.

// GateQuestions are the open questions of a gate.
type GateQuestions struct {
	Questions []cycledata.Question
	Round     int
	seq       int
}

// RoundsLeft is how many new rounds the writer has.
func (q GateQuestions) RoundsLeft() int { return max(cycledata.MaxRounds-q.Round, 0) }

// LoadQuestions reads the open questions of a gate.
func LoadQuestions(ctx context.Context, q postgres.Querier, featureID uuid.UUID, area domain.Area) (GateQuestions, error) {
	var out GateQuestions
	var raw []byte
	err := q.QueryRow(ctx, `SELECT questions, question_seq, question_round FROM spec_gate_questions WHERE feature_id = $1 AND area = $2`, featureID, area).
		Scan(&raw, &out.seq, &out.Round)
	if postgres.IsNoRows(err) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	_ = json.Unmarshal(raw, &out.Questions)
	return out, nil
}

func saveQuestions(ctx context.Context, q postgres.Querier, featureID uuid.UUID, area domain.Area, g GateQuestions) error {
	if g.Questions == nil {
		g.Questions = []cycledata.Question{}
	}
	raw, _ := json.Marshal(g.Questions)
	_, err := q.Exec(ctx, `INSERT INTO spec_gate_questions (feature_id, area, questions, question_seq, question_round) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (feature_id, area) DO UPDATE SET questions = $3, question_seq = $4, question_round = $5, updated_at = now()`,
		featureID, area, raw, g.seq, g.Round)
	return err
}

// updateQuestions closes the questions answered by the instructions of the run
// and numbers the new round of every gate the writer submitted.
func updateQuestions(ctx context.Context, tx pgx.Tx, f *specdata.Feature, req Request, reqs []SpecRequest, g Generated) error {
	answered := map[domain.Area]map[string]bool{}
	for _, r := range reqs {
		for _, a := range r.Answers {
			area := domain.Area(a.Area)
			if answered[area] == nil {
				answered[area] = map[string]bool{}
			}
			answered[area][a.QuestionID] = true
		}
	}
	for _, area := range domain.Areas {
		if area.Generated() {
			continue
		}
		s, submitted := g.Results[area]
		if !submitted && len(answered[area]) == 0 {
			continue
		}
		cur, err := LoadQuestions(ctx, tx, f.ID, area)
		if err != nil {
			return err
		}
		open := slices.DeleteFunc(cur.Questions, func(q cycledata.Question) bool { return answered[area][q.ID] })
		next := GateQuestions{Questions: open, Round: cur.Round, seq: cur.seq}
		if submitted && req.mode() == ModeWrite {
			inputs := make([]cycledata.QuestionInput, 0, len(s.Questions))
			for _, qi := range s.Questions {
				if qi.SkipArea != "" { // R6: what an answer does is decided by the server
					qi.SkipArea = string(area)
					qi.Options = []string{OptionSkip, OptionDo}
					qi.Recommended = OptionSkip
				}
				inputs = append(inputs, qi)
			}
			var counters cycledata.QuestionCounters
			var dropped int
			next.Questions, counters, dropped = cycledata.NumberQuestions(open, cycledata.QuestionCounters{Seq: cur.seq, Round: cur.Round}, inputs)
			next.seq, next.Round = counters.Seq, counters.Round
			if dropped > 0 {
				metrics.DiscoveryQuestionsDropped.Add(float64(dropped))
				slog.WarnContext(ctx, "questions of the writer dropped", "feature", f.UniqueID, "area", area, "dropped", dropped)
			}
		}
		if err := saveQuestions(ctx, tx, f.ID, area, next); err != nil {
			return err
		}
	}
	return nil
}

// OpenQuestions lists the open questions of a gate; it implements gates.Generator.
func (g Generator) OpenQuestions(ctx context.Context, featureID uuid.UUID, area domain.Area) ([]cycledata.Question, error) {
	q, err := LoadQuestions(ctx, g.Q, featureID, area)
	return q.Questions, err
}

// OpenCounts counts the open questions by gate.
func (g Generator) OpenCounts(ctx context.Context, featureID uuid.UUID) (map[domain.Area]int, error) {
	rows, err := g.Q.Query(ctx, `SELECT area, jsonb_array_length(questions) FROM spec_gate_questions WHERE feature_id = $1`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[domain.Area]int{}
	for rows.Next() {
		var a domain.Area
		var n int
		if err := rows.Scan(&a, &n); err != nil {
			return nil, err
		}
		if n > 0 {
			out[a] = n
		}
	}
	return out, rows.Err()
}

func audienceOf(area domain.Area, q cycledata.Question) domain.ExpertKind {
	switch domain.ExpertKind(q.Audience) {
	case domain.ExpertProduct:
		return domain.ExpertProduct
	case domain.ExpertTechnical:
		return domain.ExpertTechnical
	}
	return area.ApproverKind()
}

func findQuestion(open []cycledata.Question, id string) (cycledata.Question, bool) {
	id = cycledata.QuestionID(id)
	for _, q := range open {
		if q.ID == id {
			return q, true
		}
	}
	return cycledata.Question{}, false
}

func unknownQuestion(area domain.Area, id string, open []cycledata.Question) string {
	if len(open) == 0 {
		return fmt.Sprintf("unknown question %s/%s: the gate has no open questions", area, id)
	}
	ids := make([]string, len(open))
	for i, q := range open {
		ids[i] = q.ID
	}
	return fmt.Sprintf("unknown question %s/%s: open questions are %s", area, id, strings.Join(ids, ", "))
}

// QuestionView is an open question for the current user.
type QuestionView struct {
	cycledata.Question
	Audience  domain.ExpertKind `json:"audience"`
	CanAnswer bool              `json:"canAnswer"`
	// ForOthers: the question is for experts other than those who approve the gate (R11).
	ForOthers bool `json:"forOthers"`
}

// GateQuestionsView is the block of the questions of a gate.
type GateQuestionsView struct {
	Area       domain.Area    `json:"area"`
	Round      int            `json:"round"`
	RoundsLeft int            `json:"roundsLeft"`
	Questions  []QuestionView `json:"questions"`
}

// QuestionsView is GET /features/{uniqueId}/spec/questions.
func QuestionsView(ctx context.Context, q postgres.Querier, p *domain.Principal, f *specdata.Feature) ([]GateQuestionsView, error) {
	out := []GateQuestionsView{}
	for _, area := range domain.Areas {
		g, err := LoadQuestions(ctx, q, f.ID, area)
		if err != nil {
			return nil, err
		}
		if len(g.Questions) == 0 {
			continue
		}
		v := GateQuestionsView{Area: area, Round: g.Round, RoundsLeft: g.RoundsLeft(), Questions: []QuestionView{}}
		for _, x := range g.Questions {
			kind := audienceOf(area, x)
			v.Questions = append(v.Questions, QuestionView{Question: x, Audience: kind,
				ForOthers: kind != area.ApproverKind(),
				CanAnswer: f.Phase == domain.PhaseSpec && p.HasExpert(f.DomainKey, kind)})
		}
		out = append(out, v)
	}
	return out, nil
}

// QuestionsText is the answer of read_spec_questions.
func QuestionsText(ctx context.Context, q postgres.Querier, p *domain.Principal, f *specdata.Feature) (string, error) {
	items, err := QuestionsView(ctx, q, p, f)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "The specification of " + f.UniqueID + " has no open questions.", nil
	}
	var b strings.Builder
	for _, g := range items {
		fmt.Fprintf(&b, "--- %s: round %d, %d rounds left ---\n", g.Area, g.Round, g.RoundsLeft)
		for _, x := range g.Questions {
			fmt.Fprintf(&b, "%s. %s\n%s\nRecommended: %s (%s)\n", x.ID, x.Title, x.Body, x.Recommended, x.Why)
			if len(x.Options) > 0 {
				fmt.Fprintf(&b, "Options: %s\n", strings.Join(x.Options, "; "))
			}
			fmt.Fprintf(&b, "For the %s experts", x.Audience)
			if !x.CanAnswer {
				b.WriteString(": this user cannot answer it")
			}
			b.WriteString("\n")
			if len(x.Touches) > 0 {
				fmt.Fprintf(&b, "The answer may change: %s. Set allowTouches only if the user agrees to that change\n", strings.Join(x.Touches, ", "))
			}
		}
	}
	return b.String(), nil
}
