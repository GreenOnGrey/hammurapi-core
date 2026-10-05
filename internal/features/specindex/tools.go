package specindex

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// allModes: the tools are read-only and offered in every scenario (R18).
var allModes = []string{mcp.ModeGeneral, mcp.ModeSpec, mcp.ModeDiscovery, mcp.ModeGenerate, mcp.ModeCheck, mcp.ModeTask, mcp.ModeNabu}

func schema(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

var areaEnum = map[string]any{"type": "string", "enum": Areas}

func asJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// Tools are the agent's read-only tools over the index (tech spec §5a).
func (s *Service) Tools() []mcp.Tool {
	return []mcp.Tool{
		{
			Name: "spec_tree",
			Description: "The specification of the product as merged to the default branch: without filters — domains and systems " +
				"with the number of features; with domain (and system) — the features with titles, phases and areas; fixes are nested in their parent.",
			Modes: allModes, ReadOnly: true,
			InputSchema: schema(map[string]any{"domain": str("Domain key, e.g. FMS"), "system": str("System key, e.g. CAR")}),
			Handler: func(ctx context.Context, _ mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Domain, System string }
				_ = json.Unmarshal(raw, &a)
				t, err := s.Tree(ctx, strings.ToUpper(a.Domain), strings.ToUpper(a.System))
				if err != nil {
					return "", err
				}
				if a.Domain == "" {
					type sys struct {
						Key, Name string
						Features  int `json:"features"`
					}
					type dom struct {
						Key, Name string
						Systems   []sys `json:"systems"`
					}
					out := []dom{}
					for _, d := range t.Domains {
						x := dom{Key: d.Key, Name: d.Name, Systems: []sys{}}
						for _, sy := range d.Systems {
							x.Systems = append(x.Systems, sys{Key: sy.Key, Name: sy.Name, Features: sy.Count})
						}
						out = append(out, x)
					}
					return asJSON(map[string]any{"domains": out})
				}
				return asJSON(t)
			},
		},
		{
			Name: "spec_search",
			Description: "Full-text search in the specification of the default branch (Russian and English word forms) and by feature ID prefix. " +
				"Returns up to 20 documents per page: feature key, title, area, section and a fragment with matches in **…**; hasMore tells whether to ask the next page.",
			Modes: allModes, ReadOnly: true,
			InputSchema: schema(map[string]any{"query": str("Words, a phrase in quotes, or an ID prefix like FTR.FMS.CAR-00"),
				"domain": str("Domain key"), "system": str("System key"), "area": areaEnum,
				"page": map[string]any{"type": "integer", "minimum": 1}}, "query"),
			Handler: func(ctx context.Context, _ mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct {
					Query, Domain, System, Area string
					Page                        int
				}
				if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Query) == "" {
					return "", &mcp.ToolError{Msg: "query is required"}
				}
				a.Page = max(a.Page, 1)
				size := s.cfg.AgentPageSize
				r, err := s.Search(ctx, SearchQuery{Q: a.Query, Domain: strings.ToUpper(a.Domain), System: strings.ToUpper(a.System), Area: a.Area,
					Offset: (a.Page - 1) * size, Limit: size, Start: "**", Stop: "**"})
				if err != nil {
					return "", err
				}
				type item struct {
					Key     string  `json:"featureKey"`
					Title   string  `json:"title"`
					Area    string  `json:"area"`
					Section string  `json:"section"`
					Snippet string  `json:"snippet"`
					Rank    float64 `json:"rank"`
				}
				items := make([]item, 0, len(r.Items))
				for _, it := range r.Items {
					items = append(items, item{Key: it.FeatureKey, Title: it.FeatureTitle, Area: it.Area, Section: it.Section, Snippet: it.Snippet, Rank: it.Rank})
				}
				return asJSON(map[string]any{"total": r.Total, "page": a.Page, "items": items, "hasMore": r.NextCursor != nil})
			},
		},
		{
			Name: "spec_read",
			Description: "Read a document of the default branch: the whole document, or one section by its slug or heading text. " +
				"A long document returns its beginning, the full table of contents and the sections left out — read them by section.",
			Modes: allModes, ReadOnly: true,
			InputSchema: schema(map[string]any{"featureKey": str("FTR.<DOMAIN>.<SYSTEM>-NNNN"), "area": areaEnum,
				"section": str("Slug or heading text of a section (levels 2–3)")}, "featureKey", "area"),
			Handler: func(ctx context.Context, _ mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ FeatureKey, Area, Section string }
				if err := json.Unmarshal(raw, &a); err != nil || a.FeatureKey == "" || a.Area == "" {
					return "", &mcp.ToolError{Msg: "featureKey and area are required"}
				}
				out, err := s.Read(ctx, a.FeatureKey, a.Area, a.Section)
				if err != nil {
					return "", err
				}
				return asJSON(out)
			},
		},
		{
			Name: "spec_requirements",
			Description: "Requirements of a feature from its product specification: id (R1…), text, acceptance criteria (Given…/Дано…) and section. " +
				"ids narrows them, e.g. [\"R3\"].",
			Modes: allModes, ReadOnly: true,
			InputSchema: schema(map[string]any{"featureKey": str("FTR.<DOMAIN>.<SYSTEM>-NNNN"),
				"ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}}, "featureKey"),
			Handler: func(ctx context.Context, _ mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct {
					FeatureKey string   `json:"featureKey"`
					IDs        []string `json:"ids"`
				}
				if err := json.Unmarshal(raw, &a); err != nil || a.FeatureKey == "" {
					return "", &mcp.ToolError{Msg: "featureKey is required"}
				}
				reqs, err := s.FeatureRequirements(ctx, a.FeatureKey, a.IDs)
				if err != nil {
					return "", err
				}
				return asJSON(map[string]any{"featureKey": a.FeatureKey, "requirements": reqs})
			},
		},
		{
			Name: "spec_references",
			Description: "Where a feature (FTR.FMS.CAR-0002) or a requirement (FTR.FMS.CAR-0002-R3) is mentioned in other documents: " +
				"source feature, area, section and the text around the mention; up to 20 per page.",
			Modes: allModes, ReadOnly: true,
			InputSchema: schema(map[string]any{"target": str("Feature key or requirement key"),
				"page": map[string]any{"type": "integer", "minimum": 1}}, "target"),
			Handler: func(ctx context.Context, _ mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct {
					Target string
					Page   int
				}
				if err := json.Unmarshal(raw, &a); err != nil || a.Target == "" {
					return "", &mcp.ToolError{Msg: "target is required"}
				}
				out, err := s.References(ctx, a.Target, max(a.Page, 1))
				if err != nil {
					return "", err
				}
				return asJSON(out)
			},
		},
	}
}

// ReadResult is the answer of spec_read.
type ReadResult struct {
	FeatureKey      string    `json:"featureKey"`
	Area            string    `json:"area"`
	Section         string    `json:"section,omitempty"`
	Markdown        string    `json:"markdown"`
	TOC             []Heading `json:"toc"`
	Truncated       bool      `json:"truncated"`
	OmittedSections []string  `json:"omittedSections,omitempty"`
}

// Read returns a document or one of its sections within the limit (R19).
func (s *Service) Read(ctx context.Context, key, area, section string) (*ReadResult, error) {
	var md string
	err := s.pool.QueryRow(ctx, `SELECT markdown FROM spec_documents WHERE feature_key = $1 AND area = $2`, key, area).Scan(&md)
	if postgres.IsNoRows(err) {
		return nil, s.notFound(ctx, key, area)
	}
	if err != nil {
		return nil, err
	}
	return readDoc(key, area, md, section, s.cfg.AgentReadMaxChars)
}

func readDoc(key, area, md, section string, limit int) (*ReadResult, error) {
	head, secs := Sections(md)
	toc := make([]Heading, 0, len(secs))
	for _, sc := range secs {
		toc = append(toc, sc.Heading)
	}
	r := &ReadResult{FeatureKey: key, Area: area, TOC: toc}
	if section != "" {
		want := strings.ToLower(strings.TrimSpace(section))
		for _, sc := range secs {
			if sc.Heading.Slug == want || strings.ToLower(sc.Heading.Text) == want || Slug(section) == sc.Heading.Slug {
				r.Section = sc.Heading.Text
				r.Markdown, r.Truncated = cutParagraph(sc.Text, limit)
				return r, nil
			}
		}
		var near []string
		for _, sc := range secs {
			if strings.Contains(strings.ToLower(sc.Heading.Text), want) || strings.Contains(sc.Heading.Slug, Slug(section)) {
				near = append(near, sc.Heading.Text)
			}
		}
		if len(near) == 0 {
			for _, sc := range secs {
				near = append(near, sc.Heading.Text)
			}
		}
		return nil, &mcp.ToolError{Msg: fmt.Sprintf("not_found: no section %q in %s/%s; sections: %s", section, key, area, strings.Join(near, "; "))}
	}
	if len(md) <= limit {
		r.Markdown = md
		return r, nil
	}
	// The beginning up to a section boundary, then the list of the rest (AGT-08).
	var b strings.Builder
	b.WriteString(head)
	i := 0
	for ; i < len(secs); i++ {
		if b.Len()+len(secs[i].Text)+1 > limit {
			break
		}
		b.WriteString("\n")
		b.WriteString(secs[i].Text)
	}
	if i == 0 && b.Len() > limit { // even the preamble is too long
		text, _ := cutParagraph(b.String(), limit)
		b.Reset()
		b.WriteString(text)
	}
	r.Markdown, r.Truncated = b.String(), true
	for ; i < len(secs); i++ {
		r.OmittedSections = append(r.OmittedSections, secs[i].Heading.Text+" ("+secs[i].Heading.Slug+")")
	}
	return r, nil
}

// cutParagraph cuts text at the last paragraph boundary within limit.
func cutParagraph(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	cut := text[:limit]
	if i := strings.LastIndex(cut, "\n\n"); i > limit/2 {
		cut = cut[:i]
	} else {
		for len(cut) > 0 && !utf8Start(text[len(cut)]) {
			cut = cut[:len(cut)-1]
		}
	}
	return cut, true
}

func (s *Service) notFound(ctx context.Context, key, area string) error {
	var areas []string
	_ = s.pool.QueryRow(ctx, `SELECT array_agg(area ORDER BY area) FROM spec_documents WHERE feature_key = $1`, key).Scan(&areas)
	if len(areas) > 0 {
		return &mcp.ToolError{Msg: fmt.Sprintf("not_found: %s has no %s document; areas: %s", key, area, strings.Join(areas, ", "))}
	}
	prefix := key
	if len(prefix) > 10 {
		prefix = prefix[:len(prefix)-2]
	}
	var near []string
	_ = s.pool.QueryRow(ctx, `SELECT array_agg(DISTINCT feature_key) FROM (SELECT feature_key FROM spec_documents
		WHERE lower(feature_key) LIKE lower($1) || '%' ORDER BY feature_key LIMIT 10) x`, prefix).Scan(&near)
	msg := fmt.Sprintf("not_found: %s is not in the specification index", key)
	if len(near) > 0 {
		msg += "; similar: " + strings.Join(near, ", ")
	}
	return &mcp.ToolError{Msg: msg}
}

// FeatureRequirements lists the requirements of a feature (AGT-04).
func (s *Service) FeatureRequirements(ctx context.Context, key string, ids []string) ([]Requirement, error) {
	rows, err := s.pool.Query(ctx, `SELECT req_id, text, criteria, COALESCE(section, '') FROM spec_requirements
		WHERE feature_key = $1 AND (cardinality($2::text[]) = 0 OR req_id = ANY($2))
		ORDER BY substring(req_id from 2)::int`, key, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Requirement{}
	for rows.Next() {
		var r Requirement
		var crit []byte
		if err := rows.Scan(&r.ID, &r.Text, &crit, &r.Section); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(crit, &r.Criteria)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		var n int
		_ = s.pool.QueryRow(ctx, `SELECT count(*) FROM spec_documents WHERE feature_key = $1 AND area = 'product'`, key).Scan(&n)
		if n == 0 {
			return nil, s.notFound(ctx, key, "product")
		}
	}
	return out, nil
}

var targetRe = regexp.MustCompile(`^(FTR\.[A-Z][A-Z0-9]{1,9}\.[A-Z][A-Z0-9]{1,9}-\d{4})(?:-(R\d+))?$`)

// References lists mentions of a feature or a requirement (AGT-05, AGT-06).
func (s *Service) References(ctx context.Context, target string, page int) (map[string]any, error) {
	m := targetRe.FindStringSubmatch(strings.TrimSpace(target))
	if m == nil {
		return nil, &mcp.ToolError{Msg: "target is a feature key FTR.<DOMAIN>.<SYSTEM>-NNNN or a requirement key FTR.…-NNNN-R<n>"}
	}
	size := s.cfg.AgentPageSize
	rows, err := s.pool.Query(ctx, `SELECT source_key, area, COALESCE(section, ''), COALESCE(target_req, ''), snippet, count(*) OVER ()
		FROM spec_references WHERE target_key = $1 AND ($2 = '' OR target_req = $2)
		ORDER BY source_key, area, id LIMIT $3 OFFSET $4`, m[1], m[2], size, (page-1)*size)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type item struct {
		SourceKey string `json:"sourceKey"`
		Area      string `json:"area"`
		Section   string `json:"section"`
		Req       string `json:"requirement,omitempty"`
		Snippet   string `json:"snippet"`
	}
	items := []item{}
	total := 0
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.SourceKey, &it.Area, &it.Section, &it.Req, &it.Snippet, &total); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"target": target, "total": total, "page": page, "items": items, "hasMore": page*size < total}, nil
}
