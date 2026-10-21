package specindex

import (
	"context"
	"crypto/sha1" //nolint:gosec // an ETag, not a security hash
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// ─── tree (R12) ─────────────────────────────────────────────────────

// TreeFeature is a feature of the navigator with its areas and fixes.
type TreeFeature struct {
	Key    string         `json:"key"`
	Title  string         `json:"title"`
	Phase  string         `json:"phase"`
	Source string         `json:"source"`
	Areas  []string       `json:"areas"`
	Fixes  []*TreeFeature `json:"fixes"`
	parent string
}

// TreeSystem is a system of the navigator.
type TreeSystem struct {
	Key      string         `json:"key"`
	Name     string         `json:"name"`
	Count    int            `json:"count"`
	Features []*TreeFeature `json:"features"`
}

// TreeDomain is a domain of the navigator.
type TreeDomain struct {
	Key     string        `json:"key"`
	Name    string        `json:"name"`
	Systems []*TreeSystem `json:"systems"`
}

// Tree is GET /api/v1/spec/tree.
type Tree struct {
	Commit  string        `json:"commit"`
	Domains []*TreeDomain `json:"domains"`
}

// IndexTag is the version of the index for ETag: it changes with every write.
func (s *Service) IndexTag(ctx context.Context) (string, error) {
	var n int64
	var last *time.Time
	if err := s.pool.QueryRow(ctx, `SELECT count(*), max(updated_at) FROM spec_documents`).Scan(&n, &last); err != nil {
		return "", err
	}
	h := sha1.New() //nolint:gosec
	fmt.Fprintf(h, "%d|", n)
	if last != nil {
		fmt.Fprint(h, last.UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}

// Tree lists the features that have documents in the index, sorted by keys;
// fixes are nested in their parent. domain and system narrow it.
func (s *Service) Tree(ctx context.Context, domainKey, systemKey string) (*Tree, error) {
	rows, err := s.pool.Query(ctx, `SELECT d.key, d.name, sy.key, sy.name, f.unique_id, f.title, f.phase::text, f.source,
			COALESCE(p.unique_id, ''), array_agg(sd.area ORDER BY sd.area)
		FROM spec_documents sd
		JOIN features f ON f.unique_id = sd.feature_key
		JOIN systems sy ON sy.id = f.system_id JOIN domains d ON d.id = sy.domain_id
		LEFT JOIN features p ON p.id = f.parent_id
		WHERE ($1 = '' OR d.key = $1) AND ($2 = '' OR sy.key = $2)
		GROUP BY d.key, d.name, sy.key, sy.name, f.unique_id, f.title, f.phase, f.source, p.unique_id
		ORDER BY d.key, sy.key, f.unique_id`, domainKey, systemKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	t := &Tree{Domains: []*TreeDomain{}}
	var dom *TreeDomain
	var sys *TreeSystem
	byKey := map[string]*TreeFeature{}
	var all []*TreeFeature
	sysOf := map[*TreeFeature]*TreeSystem{}
	for rows.Next() {
		var dk, dn, sk, sn string
		f := &TreeFeature{Fixes: []*TreeFeature{}}
		if err := rows.Scan(&dk, &dn, &sk, &sn, &f.Key, &f.Title, &f.Phase, &f.Source, &f.parent, &f.Areas); err != nil {
			return nil, err
		}
		f.Areas = orderAreas(f.Areas)
		if dom == nil || dom.Key != dk {
			dom = &TreeDomain{Key: dk, Name: dn, Systems: []*TreeSystem{}}
			t.Domains = append(t.Domains, dom)
			sys = nil
		}
		if sys == nil || sys.Key != sk {
			sys = &TreeSystem{Key: sk, Name: sn, Features: []*TreeFeature{}}
			dom.Systems = append(dom.Systems, sys)
		}
		byKey[f.Key] = f
		all = append(all, f)
		sysOf[f] = sys
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, f := range all {
		sysOf[f].Count++
		if p, ok := byKey[f.parent]; ok && f.parent != "" {
			p.Fixes = append(p.Fixes, f)
			continue
		}
		sysOf[f].Features = append(sysOf[f].Features, f)
	}
	tag, err := s.IndexTag(ctx)
	if err != nil {
		return nil, err
	}
	t.Commit = tag
	return t, nil
}

func orderAreas(in []string) []string {
	out := []string{}
	for _, a := range Areas {
		for _, x := range in {
			if x == a {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// ─── document (R13, R15, R20) ───────────────────────────────────────

// IssueRef is a source issue of a feature.
type IssueRef struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// PRRef is a merged PR/MR of the implementation.
type PRRef struct {
	Kind     string     `json:"kind"` // service | spec
	Service  string     `json:"service,omitempty"`
	Repo     string     `json:"repo"`
	Number   int        `json:"number"`
	URL      string     `json:"url"`
	MergedAt *time.Time `json:"mergedAt"`
}

// FeatureInfo is the header of a document: status and links of the feature.
type FeatureInfo struct {
	Phase        string     `json:"phase"`
	Source       string     `json:"source"`
	Title        string     `json:"title"`
	Release      *string    `json:"release"`
	ReleasedAt   *time.Time `json:"releasedAt"`
	IndexedAt    *time.Time `json:"indexedAt"`
	Parent       *string    `json:"parent"`
	Issues       []IssueRef `json:"issues"`
	PullRequests []PRRef    `json:"pullRequests"`
}

// Document is GET /api/v1/spec/documents/{featureKey}/{area}.
type Document struct {
	FeatureKey string      `json:"featureKey"`
	Area       string      `json:"area"`
	Areas      []string    `json:"areas"`
	Path       string      `json:"path"`
	Title      string      `json:"title"`
	Markdown   string      `json:"markdown"`
	TOC        []Heading   `json:"toc"`
	BlobSHA    string      `json:"blobSha"`
	Commit     string      `json:"commit"`
	HistoryURL string      `json:"historyUrl,omitempty"`
	Feature    FeatureInfo `json:"feature"`
}

func notFoundDoc() error {
	return apperr.NotFound("spec_document_not_found", "the document is not in the specification index")
}

// Document returns a document of the index with the links of its feature.
func (s *Service) Document(ctx context.Context, key, area string) (*Document, error) {
	d := &Document{FeatureKey: key, Area: area}
	var title *string
	var toc []byte
	err := s.pool.QueryRow(ctx, `SELECT path, title, toc, markdown, blob_sha, commit_sha FROM spec_documents
		WHERE feature_key = $1 AND area = $2`, key, area).Scan(&d.Path, &title, &toc, &d.Markdown, &d.BlobSHA, &d.Commit)
	if postgres.IsNoRows(err) {
		return nil, notFoundDoc()
	}
	if err != nil {
		return nil, err
	}
	if title != nil {
		d.Title = *title
	}
	_ = json.Unmarshal(toc, &d.TOC)
	if d.TOC == nil {
		d.TOC = []Heading{}
	}
	d.HistoryURL = s.historyURL(d.Path)
	var areas []string
	if err := s.pool.QueryRow(ctx, `SELECT array_agg(area) FROM spec_documents WHERE feature_key = $1`, key).Scan(&areas); err != nil {
		return nil, err
	}
	d.Areas = orderAreas(areas)
	info, err := s.featureInfo(ctx, key)
	if err != nil {
		return nil, err
	}
	d.Feature = *info
	return d, nil
}

// historyURL is the history of a file at the provider: the commit URL of the
// provider with /commit/<sha> replaced by /commits/<branch>/<path>. Empty when
// the path or the form of the commit URL is unknown: no link is better than a
// broken one (FTR.HMR.SPEC-0001 R13).
func (s *Service) historyURL(path string) string {
	return historyURL(s.git.CommitURL(marker), s.cfg.DefaultBranch, path)
}

const marker = "0000000000000000000000000000000000000000"

func historyURL(commitURL, branch, path string) string {
	if path == "" || !strings.Contains(commitURL, "/commit/"+marker) {
		return ""
	}
	return strings.Replace(commitURL, "/commit/"+marker, "/commits/"+branch+"/"+path, 1)
}

func (s *Service) featureInfo(ctx context.Context, key string) (*FeatureInfo, error) {
	fi := &FeatureInfo{Issues: []IssueRef{}, PullRequests: []PRRef{}}
	var id string
	var prNumber int
	var prURL string
	err := s.pool.QueryRow(ctx, `SELECT f.id::text, f.phase::text, f.source, f.title, f.indexed_at, p.unique_id, f.pr_number, f.pr_url,
			r.key, r.confirmed_at
		FROM features f LEFT JOIN features p ON p.id = f.parent_id LEFT JOIN releases r ON r.feature_id = f.id
		WHERE f.unique_id = $1`, key).Scan(&id, &fi.Phase, &fi.Source, &fi.Title, &fi.IndexedAt, &fi.Parent, &prNumber, &prURL,
		&fi.Release, &fi.ReleasedAt)
	if postgres.IsNoRows(err) {
		return nil, notFoundDoc()
	}
	if err != nil {
		return nil, err
	}
	// Issues by their current key (after a move, NAV-17).
	rows, err := s.pool.Query(ctx, `SELECT i.key, i.title FROM feature_issues fi JOIN issues i ON i.id = fi.issue_id
		WHERE fi.feature_id = $1 ORDER BY i.key`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r IssueRef
		if err := rows.Scan(&r.Key, &r.Title); err != nil {
			rows.Close()
			return nil, err
		}
		r.URL = "/issues/" + r.Key
		fi.Issues = append(fi.Issues, r)
	}
	rows.Close()
	// Merged PRs of services and of the specification; no reverts, no closed ones (NAV-16).
	rows, err = s.pool.Query(ctx, `SELECT pr.kind::text, COALESCE(sv.key, ''), pr.repo, pr.number, pr.url, pr.merged_at
		FROM pull_requests pr LEFT JOIN services sv ON sv.id = pr.service_id
		WHERE pr.feature_id = $1 AND pr.state = 'merged' AND pr.kind IN ('service', 'spec')
		ORDER BY (pr.kind = 'spec'), pr.merged_at NULLS LAST, pr.number`, id)
	if err != nil {
		return nil, err
	}
	hasSpec := false
	for rows.Next() {
		var r PRRef
		if err := rows.Scan(&r.Kind, &r.Service, &r.Repo, &r.Number, &r.URL, &r.MergedAt); err != nil {
			rows.Close()
			return nil, err
		}
		hasSpec = hasSpec || r.Kind == "spec"
		fi.PullRequests = append(fi.PullRequests, r)
	}
	rows.Close()
	// The specification PR of an MVP feature is known only from the feature itself.
	if !hasSpec && fi.Phase == "released" && prNumber > 0 && prURL != "" {
		fi.PullRequests = append(fi.PullRequests, PRRef{Kind: "spec", Repo: s.git.Repo(), Number: prNumber, URL: prURL})
	}
	return fi, nil
}

// ─── files (R13) ────────────────────────────────────────────────────

// File is a file of an area besides spec.md.
type File struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	MimeType    string `json:"mimeType"`
	Previewable bool   `json:"previewable"`
	// ViewURL opens the file as a page of its own in an opaque origin
	// (FTR.HMR.SPEC-0001 R6, arch §6); a path of the API, without the host.
	ViewURL string `json:"viewUrl,omitempty"`
	blobSHA string
}

func viewURL(key, area, name string) string {
	return "/api/v1/spec/files/" + url.PathEscape(key) + "/" + url.PathEscape(area) + "/raw?" +
		url.Values{"name": {name}, "mode": {"view"}}.Encode()
}

func previewable(mt string) bool {
	switch mt {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/svg+xml", "text/html":
		return true
	}
	return false
}

// Files lists the files of an area.
func (s *Service) Files(ctx context.Context, key, area string) ([]File, error) {
	rows, err := s.pool.Query(ctx, `SELECT path, name, size_bytes, mime_type, blob_sha FROM spec_files
		WHERE feature_key = $1 AND area = $2 ORDER BY name`, key, area)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []File{}
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Path, &f.Name, &f.Size, &f.MimeType, &f.blobSHA); err != nil {
			return nil, err
		}
		f.Previewable = previewable(f.MimeType) && f.Size <= s.cfg.PreviewMaxBytes
		if f.Previewable {
			f.ViewURL = viewURL(key, area, f.Name)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// RawFile reads a file of an area from git by the SHA of the index.
func (s *Service) RawFile(ctx context.Context, key, area, name string) (*File, []byte, error) {
	var f File
	err := s.pool.QueryRow(ctx, `SELECT path, name, size_bytes, mime_type, blob_sha FROM spec_files
		WHERE feature_key = $1 AND area = $2 AND name = $3`, key, area, name).Scan(&f.Path, &f.Name, &f.Size, &f.MimeType, &f.blobSHA)
	if postgres.IsNoRows(err) {
		return nil, nil, apperr.NotFound("spec_file_not_found", "the file is not in the specification index")
	}
	if err != nil {
		return nil, nil, err
	}
	f.Previewable = previewable(f.MimeType) && f.Size <= s.cfg.PreviewMaxBytes
	token, err := s.git.BotToken(ctx)
	if err != nil {
		return nil, nil, err
	}
	data, err := s.git.Blob(ctx, token, f.blobSHA)
	return &f, data, err
}

// ─── search (R14) ───────────────────────────────────────────────────

// SearchItem is a found document.
type SearchItem struct {
	FeatureKey   string  `json:"featureKey"`
	FeatureTitle string  `json:"featureTitle"`
	Area         string  `json:"area"`
	Path         string  `json:"path"`
	Section      string  `json:"section"`
	Snippet      string  `json:"snippet"`
	Rank         float64 `json:"rank"`
}

// SearchResult is GET /api/v1/spec/search.
type SearchResult struct {
	Total      int          `json:"total"`
	Items      []SearchItem `json:"items"`
	NextCursor *string      `json:"nextCursor"`
}

// SearchQuery are the parameters of a search.
type SearchQuery struct {
	Q, Domain, System, Area string
	Offset, Limit           int
	// Marks frame the matches: ‹› for the interface (escaped there), ** for the agent.
	Start, Stop string
}

var idLikeRe = regexp.MustCompile(`^(?i)(FTR\.)?[A-Z][A-Z0-9]*[.-]`)

// Search finds documents by text and features by ID prefix; matches by ID go
// first (SRC-03). Snippets are built only for the page (tech spec §5).
func (s *Service) Search(ctx context.Context, sq SearchQuery) (*SearchResult, error) {
	q := strings.TrimSpace(sq.Q)
	if sq.Limit <= 0 {
		sq.Limit = 20
	}
	sq.Limit = min(sq.Limit, s.cfg.SearchMaxLimit)
	if sq.Start == "" {
		sq.Start, sq.Stop = "‹", "›"
	}
	res := &SearchResult{Items: []SearchItem{}}
	if q == "" {
		return res, nil
	}
	idLike := idLikeRe.MatchString(q)
	const filters = ` AND ($2 = '' OR d.domain = $2) AND ($3 = '' OR d.system = $3) AND ($4 = '' OR d.area = $4)`
	// Matches by ID (prefix), then full-text matches by rank.
	sql := `WITH q AS (SELECT websearch_to_tsquery('russian', $1) || websearch_to_tsquery('english', $1) AS tsq),
		hits AS (
			SELECT d.path, d.feature_key, d.area, 2.0::float8 AS rank FROM spec_documents d
			WHERE $5 AND lower(d.feature_key) LIKE lower($6) || '%'` + filters + `
			UNION ALL
			SELECT d.path, d.feature_key, d.area, ts_rank(d.search_vector, q.tsq)::float8 FROM spec_documents d, q
			WHERE d.search_vector @@ q.tsq` + filters + `),
		best AS (SELECT path, feature_key, area, max(rank) AS rank FROM hits GROUP BY path, feature_key, area)
		SELECT path, feature_key, area, rank, count(*) OVER () FROM best ORDER BY rank DESC, feature_key, area LIMIT $7 OFFSET $8`
	rows, err := s.pool.Query(ctx, sql, q, sq.Domain, sq.System, sq.Area, idLike, strings.TrimSuffix(q, "*"), sq.Limit, sq.Offset)
	if err != nil {
		return nil, err
	}
	var paths []string
	for rows.Next() {
		var it SearchItem
		if err := rows.Scan(&it.Path, &it.FeatureKey, &it.Area, &it.Rank, &res.Total); err != nil {
			rows.Close()
			return nil, err
		}
		res.Items = append(res.Items, it)
		paths = append(paths, it.Path)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return res, nil
	}
	opts := fmt.Sprintf("StartSel=%s, StopSel=%s, MaxFragments=2, MaxWords=25, MinWords=8, FragmentDelimiter=\" … \"", sq.Start, sq.Stop)
	srows, err := s.pool.Query(ctx, `WITH q AS (SELECT websearch_to_tsquery('russian', $1) || websearch_to_tsquery('english', $1) AS tsq)
		SELECT d.path, COALESCE(f.title, d.title, d.feature_key), ts_headline('russian', d.search_text, q.tsq, $3), d.markdown, d.toc
		FROM spec_documents d LEFT JOIN features f ON f.unique_id = d.feature_key, q WHERE d.path = ANY($2)`, q, paths, opts)
	if err != nil {
		return nil, err
	}
	type extra struct {
		title, snippet, section string
	}
	byPath := map[string]extra{}
	for srows.Next() {
		var p, title, snippet, md string
		var toc []byte
		if err := srows.Scan(&p, &title, &snippet, &md, &toc); err != nil {
			srows.Close()
			return nil, err
		}
		byPath[p] = extra{title: title, snippet: snippet, section: sectionOfMatch(md, toc, snippet, sq.Start, sq.Stop)}
	}
	srows.Close()
	for i := range res.Items {
		e := byPath[res.Items[i].Path]
		res.Items[i].FeatureTitle, res.Items[i].Snippet, res.Items[i].Section = e.title, e.snippet, e.section
		if !strings.Contains(e.snippet, sq.Start) {
			res.Items[i].Snippet = e.snippet // an ID match without a text match
		}
	}
	if next := sq.Offset + len(res.Items); next < res.Total {
		c := strconv.Itoa(next)
		res.NextCursor = &c
	}
	return res, nil
}

// sectionOfMatch is the nearest heading before the first highlighted word.
func sectionOfMatch(md string, rawTOC []byte, snippet, start, stop string) string {
	i := strings.Index(snippet, start)
	if i < 0 {
		return ""
	}
	rest := snippet[i+len(start):]
	j := strings.Index(rest, stop)
	if j <= 0 {
		return ""
	}
	word := strings.ToLower(rest[:j])
	pos := strings.Index(strings.ToLower(md), word)
	if pos < 0 {
		return ""
	}
	var toc []Heading
	_ = json.Unmarshal(rawTOC, &toc)
	// Headings in document order; the last one before pos.
	_, body := splitFrontMatter(md)
	offset := len(md) - len(body)
	section := ""
	for _, h := range toc {
		hp := strings.Index(md[offset:], h.Text)
		if hp < 0 {
			continue
		}
		hp += offset
		if hp > pos {
			break
		}
		section = h.Text
		offset = hp + len(h.Text)
	}
	return section
}
