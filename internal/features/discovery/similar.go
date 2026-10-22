package discovery

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// The MCP tool similar_features_cost (FTR.HMR.DISC-0001 R16, tech §4.4): the
// released features of the domain of the issue that look like it, with what
// their agents cost by stage and how long their stages took. Hammurapi knows
// nothing of the person-hours of people.

// SimilarNote closes every answer of similar_features_cost.
const SimilarNote = "Hammurapi has no data on person-hours: estimate them yourself."

// SimilarCost is one similar released feature.
type SimilarCost struct {
	Key        string                    `json:"key"`
	Title      string                    `json:"title"`
	Similarity float64                   `json:"similarity"`
	Agent      []cycledata.EstimateAgent `json:"agent"`
	StageDays  map[string]float64        `json:"stageDays"`
	TotalDays  *float64                  `json:"totalDays"`
}

// usageStage maps agent_usage.context to the stages of the estimate.
var usageStage = map[string]string{"gate": "spec", "codegen": "codegen", "check": "checks"}

// queryOf is the OR-query of the words of the issue for the full-text search.
func queryOf(text string) string {
	seen := map[string]bool{}
	var words []string
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) < 3 || seen[w] {
			continue
		}
		seen[w] = true
		words = append(words, w)
		if len(words) == 40 {
			break
		}
	}
	return strings.Join(words, " | ")
}

func round(v float64) float64 { return math.Round(v*10) / 10 }

// SimilarFeaturesCost finds up to limit released similar features of the
// domain of the issue (ES-06…ES-08).
func SimilarFeaturesCost(ctx context.Context, q postgres.Querier, issueID uuid.UUID, limit int) ([]SimilarCost, error) {
	if limit <= 0 || limit > 10 {
		limit = 5
	}
	is, err := cycledata.New(q).IssueByID(ctx, issueID)
	if err != nil {
		return nil, err
	}
	words := queryOf(is.Title + " " + is.Description)
	out := []SimilarCost{}
	if words == "" {
		return out, nil
	}
	rows, err := q.Query(ctx, `WITH c AS (
			SELECT f.id, f.unique_id, f.title, f.created_at,
				to_tsvector('simple', f.title || ' ' || COALESCE(string_agg(i.title || ' ' || COALESCE(dd.content, ''), ' '), '')) AS doc
			FROM features f JOIN systems s ON s.id = f.system_id JOIN domains d ON d.id = s.domain_id
			LEFT JOIN feature_issues fi ON fi.feature_id = f.id LEFT JOIN issues i ON i.id = fi.issue_id
			LEFT JOIN discovery_docs dd ON dd.issue_id = fi.issue_id
			WHERE d.id = $1 AND f.phase = 'released' AND NOT EXISTS (SELECT 1 FROM feature_issues x WHERE x.feature_id = f.id AND x.issue_id = $4)
			GROUP BY f.id)
		SELECT id, unique_id, title, ts_rank(doc, to_tsquery('simple', $2)) AS r FROM c
		WHERE doc @@ to_tsquery('simple', $2) ORDER BY r DESC, created_at DESC LIMIT $3`, is.DomainID, words, limit, is.ID)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		var c SimilarCost
		if err := rows.Scan(&id, &c.Key, &c.Title, &c.Similarity); err != nil {
			rows.Close()
			return nil, err
		}
		c.Similarity = math.Round(c.Similarity*100) / 100
		ids = append(ids, id)
		out = append(out, c)
	}
	rows.Close()
	for i, id := range ids {
		if out[i].Agent, err = featureAgentCost(ctx, q, id); err != nil {
			return nil, err
		}
		if out[i].StageDays, out[i].TotalDays, err = featureStageDays(ctx, q, id); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func featureAgentCost(ctx context.Context, q postgres.Querier, featureID uuid.UUID) ([]cycledata.EstimateAgent, error) {
	rows, err := q.Query(ctx, `SELECT context, COALESCE(sum(cost_usd), 0)::float8, COALESCE(sum(tokens_in + tokens_out), 0)
		FROM agent_usage WHERE feature_id = $1 GROUP BY context`, featureID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	by := map[string]*cycledata.EstimateAgent{}
	for rows.Next() {
		var ctxName string
		var usd float64
		var tokens int64
		if err := rows.Scan(&ctxName, &usd, &tokens); err != nil {
			return nil, err
		}
		stage, ok := usageStage[ctxName]
		if !ok {
			continue // discovery and the chat are not the cost of the work
		}
		a := by[stage]
		if a == nil {
			a = &cycledata.EstimateAgent{Stage: stage}
			by[stage] = a
		}
		a.USD += usd
		a.Tokens += tokens
	}
	out := []cycledata.EstimateAgent{}
	for _, st := range cycledata.AgentStages {
		if a := by[st]; a != nil {
			a.USD = math.Round(a.USD*100) / 100
			out = append(out, *a)
		}
	}
	return out, rows.Err()
}

// featureStageDays measures the stages of a released feature by the history
// of its workflows: the specification until the first code generation, the
// runs of generation and validation, the release until its confirmation.
func featureStageDays(ctx context.Context, q postgres.Querier, featureID uuid.UUID) (map[string]float64, *float64, error) {
	var spec, codegen, validation, release, total *float64
	err := q.QueryRow(ctx, `WITH f AS (SELECT id, created_at FROM features WHERE id = $1),
		cg AS (SELECT min(created_at) AS first, sum(EXTRACT(EPOCH FROM updated_at - created_at)) AS secs FROM workflow_runs WHERE kind = 'codegen' AND subject_id = $1),
		va AS (SELECT sum(EXTRACT(EPOCH FROM updated_at - created_at)) AS secs FROM workflow_runs WHERE kind = 'validation' AND subject_id = $1),
		re AS (SELECT created_at, confirmed_at FROM releases WHERE feature_id = $1)
		SELECT EXTRACT(EPOCH FROM (SELECT first FROM cg) - f.created_at) / 86400,
			(SELECT secs FROM cg) / 86400, (SELECT secs FROM va) / 86400,
			EXTRACT(EPOCH FROM (SELECT confirmed_at - created_at FROM re)) / 86400,
			EXTRACT(EPOCH FROM (SELECT confirmed_at FROM re) - f.created_at) / 86400
		FROM f`, featureID).Scan(&spec, &codegen, &validation, &release, &total)
	if err != nil {
		return nil, nil, err
	}
	days := map[string]float64{}
	for k, v := range map[string]*float64{"spec": spec, "codegen": codegen, "validation": validation, "release": release} {
		if v != nil {
			days[k] = round(*v)
		}
	}
	if total != nil {
		t := round(*total)
		total = &t
	}
	return days, total, nil
}

// SimilarFeaturesCostText is the answer of the tool for the agent.
func SimilarFeaturesCostText(ctx context.Context, q postgres.Querier, issueID uuid.UUID, limit int) (string, error) {
	list, err := SimilarFeaturesCost(ctx, q, issueID, limit)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(map[string]any{"features": list, "note": SimilarNote})
	return string(b), err
}
