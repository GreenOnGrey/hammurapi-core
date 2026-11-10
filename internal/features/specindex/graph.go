package specindex

import (
	"context"
	"sort"
	"strings"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
)

// ─── links of a feature and the graph (FTR.HMR.DEV-0002 R14–R17) ───

// LinkRef is the other end of a link in the header of a document.
type LinkRef struct {
	Key          string   `json:"key"`
	Title        *string  `json:"title"`        // null outside the index
	Indexed      bool     `json:"indexed"`      // the feature has documents in the index (R17)
	Layer        string   `json:"layer"`        // product | arch (FTR.HMR.DEV-0003 R15)
	Items        []string `json:"items"`        // R<n> of product, A<n> of arch
	Requirements []string `json:"requirements"` // the old name of items, kept for one version of the web
}

// FeatureLinks are the outgoing and incoming links of a feature (tech §2.2).
type FeatureLinks struct {
	Extends    []LinkRef `json:"extends"`
	Changes    []LinkRef `json:"changes"`
	ExtendedBy []LinkRef `json:"extendedBy"`
	ChangedBy  []LinkRef `json:"changedBy"`
}

// featureLinks reads the links of a feature both ways in one query, ordered by
// key; layer narrows them to product or arch, "" keeps both (FTR.HMR.DEV-0003 R15).
func (s *Service) featureLinks(ctx context.Context, key, layer string) (*FeatureLinks, error) {
	out := &FeatureLinks{Extends: []LinkRef{}, Changes: []LinkRef{}, ExtendedBy: []LinkRef{}, ChangedBy: []LinkRef{}}
	rows, err := s.pool.Query(ctx, `SELECT l.kind, l.source_key = $1, o.key, l.requirements, l.layer,
			EXISTS (SELECT 1 FROM spec_documents sd WHERE sd.feature_key = o.key), f.title
		FROM spec_links l
		CROSS JOIN LATERAL (SELECT CASE WHEN l.source_key = $1 THEN l.target_key ELSE l.source_key END AS key) o
		LEFT JOIN features f ON f.unique_id = o.key
		WHERE (l.source_key = $1 OR l.target_key = $1) AND ($2 = '' OR l.layer = $2)
		ORDER BY o.key, l.layer DESC`, key, layer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var outgoing bool
		var r LinkRef
		if err := rows.Scan(&kind, &outgoing, &r.Key, &r.Requirements, &r.Layer, &r.Indexed, &r.Title); err != nil {
			return nil, err
		}
		if !r.Indexed {
			r.Title = nil
		}
		if r.Requirements == nil {
			r.Requirements = []string{}
		}
		r.Items = r.Requirements
		switch {
		case kind == LinkExtends && outgoing:
			out.Extends = append(out.Extends, r)
		case kind == LinkExtends:
			out.ExtendedBy = append(out.ExtendedBy, r)
		case outgoing:
			out.Changes = append(out.Changes, r)
		default:
			out.ChangedBy = append(out.ChangedBy, r)
		}
	}
	return out, rows.Err()
}

// GraphNode is a feature of the graph.
type GraphNode struct {
	Key     string  `json:"key"`
	Title   *string `json:"title"`
	Domain  string  `json:"domain"`
	System  string  `json:"system"`
	Phase   *string `json:"phase"`
	Indexed bool    `json:"indexed"`
	Context bool    `json:"context"` // outside the filter, linked to a feature inside it (R15)
}

// GraphEdge is a link of the graph, from the feature that names it.
type GraphEdge struct {
	Source       string   `json:"source"`
	Target       string   `json:"target"`
	Kind         string   `json:"kind"`
	Layer        string   `json:"layer"` // product | arch (FTR.HMR.DEV-0003 R15)
	Items        []string `json:"items"`
	Requirements []string `json:"requirements"` // the old name of items, kept for one version of the web
}

// Graph is GET /api/v1/spec/graph.
type Graph struct {
	Commit string      `json:"commit"` // the version of the index, as the tree and the ETag
	Nodes  []GraphNode `json:"nodes"`
	Edges  []GraphEdge `json:"edges"`
}

// GraphQuery narrows the graph: domain and system keep the edges with an end
// inside and mark the other end as context; kind keeps one kind of edges;
// layer is product (the default), arch or both (FTR.HMR.DEV-0003 R15).
type GraphQuery struct {
	Domain, System, Kind, Layer string
}

// KeyParts are the domain and the system of a feature key.
func KeyParts(key string) (string, string) {
	parts := strings.SplitN(strings.TrimPrefix(key, "FTR."), ".", 2)
	if len(parts) != 2 {
		return "", ""
	}
	sys, _, _ := strings.Cut(parts[1], "-")
	return parts[0], sys
}

// SpecGraph returns the features of the index and the links between them
// (FTR.HMR.DEV-0002 tech §2.3); keys outside the index are nodes with
// indexed false. Focus and depth are left to the client.
func (s *Service) SpecGraph(ctx context.Context, q GraphQuery) (*Graph, error) {
	if q.Kind != "" && q.Kind != LinkExtends && q.Kind != LinkChanges {
		return nil, apperr.BadRequest("bad_kind", "kind is extends, changes or empty")
	}
	layer := q.Layer
	switch layer {
	case "":
		layer = LayerProduct
	case "both":
		layer = ""
	case LayerProduct, LayerArch:
	default:
		return nil, apperr.BadRequest("bad_layer", "layer is product, arch or both")
	}
	nodes := map[string]*GraphNode{}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT sd.feature_key, f.title, sd.domain, sd.system, f.phase::text
		FROM spec_documents sd LEFT JOIN features f ON f.unique_id = sd.feature_key`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		n := &GraphNode{Indexed: true}
		if err := rows.Scan(&n.Key, &n.Title, &n.Domain, &n.System, &n.Phase); err != nil {
			rows.Close()
			return nil, err
		}
		nodes[n.Key] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT l.source_key, l.target_key, l.kind, l.layer, l.requirements, f.phase::text
		FROM spec_links l LEFT JOIN features f ON f.unique_id = l.target_key
		WHERE ($1 = '' OR l.kind = $1) AND ($2 = '' OR l.layer = $2)
		ORDER BY l.source_key, l.target_key, l.kind, l.layer DESC`, q.Kind, layer)
	if err != nil {
		return nil, err
	}
	var edges []GraphEdge
	for rows.Next() {
		var e GraphEdge
		var phase *string
		if err := rows.Scan(&e.Source, &e.Target, &e.Kind, &e.Layer, &e.Requirements, &phase); err != nil {
			rows.Close()
			return nil, err
		}
		if e.Requirements == nil {
			e.Requirements = []string{}
		}
		e.Items = e.Requirements
		if _, ok := nodes[e.Target]; !ok { // R17: a key outside the index
			d, sy := KeyParts(e.Target)
			nodes[e.Target] = &GraphNode{Key: e.Target, Domain: d, System: sy, Phase: phase}
		}
		if _, ok := nodes[e.Source]; !ok { // the document left the index between the two reads
			d, sy := KeyParts(e.Source)
			nodes[e.Source] = &GraphNode{Key: e.Source, Domain: d, System: sy}
		}
		edges = append(edges, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	inside := func(key string) bool {
		n := nodes[key]
		return (q.Domain == "" || n.Domain == q.Domain) && (q.System == "" || n.System == q.System)
	}
	g := &Graph{Nodes: []GraphNode{}, Edges: []GraphEdge{}}
	keep := map[string]bool{}
	for k := range nodes {
		if inside(k) {
			keep[k] = true
		}
	}
	for _, e := range edges {
		if !inside(e.Source) && !inside(e.Target) {
			continue
		}
		g.Edges = append(g.Edges, e)
		for _, k := range []string{e.Source, e.Target} {
			if !inside(k) {
				nodes[k].Context = true
				keep[k] = true
			}
		}
	}
	for k := range keep {
		g.Nodes = append(g.Nodes, *nodes[k])
	}
	sort.Slice(g.Nodes, func(i, j int) bool { return g.Nodes[i].Key < g.Nodes[j].Key })
	return g, nil
}
