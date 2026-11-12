package specindex

// FTR.HMR.SPEC-0006: spec_read, spec_requirements and spec_links answer for a
// feature in work from its branch, which the index of the default branch does
// not hold until the merge.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/GreenOnGrey/speclint/v2/contract"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/markdown"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Sources of an answer of the spec tools (SPEC-0006 tech §API).
const (
	SourceIndex  = "index"
	SourceBranch = "branch"
)

// branchCacheTTL keeps a document of a branch for a series of calls of an agent (SPEC-0006 A2).
const branchCacheTTL = 30 * time.Second

// errNotInWork: the key is not a feature in work, the index answers as before.
var errNotInWork = errors.New("not a feature in work")

// inWork is a feature whose documents live in its branch.
type inWork struct {
	key, domain, system, branch string
	skipped                     []string
	heads                       map[string]string // area → head commit of the gate: the key of the cache
}

type branchEntry struct {
	md  string
	at  time.Time
	sha string
}

type branchCache struct {
	mu sync.Mutex
	m  map[string]branchEntry
}

func (c *branchCache) get(k, sha string, now time.Time) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || e.sha != sha || now.Sub(e.at) > branchCacheTTL {
		return "", false
	}
	return e.md, true
}

func (c *branchCache) put(k, sha, md string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 256 {
		c.m = map[string]branchEntry{}
	}
	c.m[k] = branchEntry{md: md, at: now, sha: sha}
}

// featureInWork reads a feature that is not merged yet: in a phase of the cycle
// before the release, not deleted. Released, indexed, rolled back and deleted
// features are not read from a branch.
func (s *Service) featureInWork(ctx context.Context, key string) (*inWork, error) {
	f := &inWork{key: key, heads: map[string]string{}}
	err := s.pool.QueryRow(ctx, `SELECT d.key, s.key, f.branch_name, f.skipped_areas
		FROM features f JOIN systems s ON s.id = f.system_id JOIN domains d ON d.id = s.domain_id
		WHERE f.unique_id = $1 AND f.deleted_at IS NULL AND f.branch_name <> ''
		  AND f.phase IN ('spec', 'codegen', 'validation', 'in_release')`, key).Scan(&f.domain, &f.system, &f.branch, &f.skipped)
	if postgres.IsNoRows(err) {
		return nil, errNotInWork
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT g.area::text, g.head_commit FROM gates g JOIN features f ON f.id = g.feature_id
		WHERE f.unique_id = $1 AND g.deleted_at IS NULL`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var area, head string
		if err := rows.Scan(&area, &head); err != nil {
			return nil, err
		}
		f.heads[area] = head
	}
	return f, rows.Err()
}

// branchDocument reads one document of a feature in work with the token of the
// bot: agents read as the bot, a person sees every feature (SPEC-0006 R2).
// "" without an error — the area has no document in the branch.
func (s *Service) branchDocument(ctx context.Context, f *inWork, area string) (string, error) {
	if slices.Contains(f.skipped, area) {
		return "", nil
	}
	ck := f.key + "/" + area
	if md, ok := s.branchDocs.get(ck, f.heads[area], s.now()); ok {
		return md, nil
	}
	token, err := s.git.BotToken(ctx)
	if err != nil {
		return "", err
	}
	file, err := s.git.GetFile(ctx, token, f.branch, git.SpecPath(f.domain, f.system, f.key, area))
	if errors.Is(err, git.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	md := string(file.Content)
	s.branchDocs.put(ck, f.heads[area], md, s.now())
	return md, nil
}

// readBranch is spec_read of a feature in work.
func (s *Service) readBranch(ctx context.Context, key, area, section string) (*ReadResult, error) {
	f, err := s.featureInWork(ctx, key)
	if err != nil {
		return nil, err
	}
	md, err := s.branchDocument(ctx, f, area)
	if err != nil {
		return nil, err
	}
	if md == "" {
		return nil, &mcp.ToolError{Msg: fmt.Sprintf("not_found: %s is in work and has no %s document in its branch %s", key, area, f.branch)}
	}
	r, err := readDoc(key, area, md, section, s.cfg.AgentReadMaxChars)
	if err != nil {
		return nil, err
	}
	r.Source, r.Branch = SourceBranch, f.branch
	return r, nil
}

// requirementsBranch is spec_requirements of a feature in work: R<n> of its
// product, A<n> of its arch, as the index would store them.
func (s *Service) requirementsBranch(ctx context.Context, key string, ids []string, kind string) ([]Requirement, string, error) {
	f, err := s.featureInWork(ctx, key)
	if err != nil {
		return nil, "", err
	}
	out := []Requirement{}
	found := false
	for _, area := range []string{"arch", "product"} { // decisions first, as ORDER BY kind DESC of the index
		if (kind == "requirement" && area == "arch") || (kind == "decision" && area == "product") {
			continue
		}
		md, err := s.branchDocument(ctx, f, area)
		if err != nil {
			return nil, "", err
		}
		if md == "" {
			continue
		}
		found = true
		items := Requirements(md)
		if area == "arch" {
			items = Decisions(md)
		}
		for _, r := range items {
			if len(ids) == 0 || slices.Contains(ids, r.ID) {
				out = append(out, r)
			}
		}
	}
	if !found {
		area := "product"
		if kind == "decision" {
			area = "arch"
		}
		return nil, "", &mcp.ToolError{Msg: fmt.Sprintf("not_found: %s is in work and has no %s document in its branch %s", key, area, f.branch)}
	}
	return out, f.branch, nil
}

// linksBranch adds the outgoing links of the product and arch of a feature in
// work to the links of the index, which know only who refers to it (SPEC-0006 R1,
// FTR.HMR.DEV-0003 R6).
func (s *Service) linksBranch(ctx context.Context, key, layer string, out *FeatureLinks) (string, error) {
	f, err := s.featureInWork(ctx, key)
	if err != nil {
		return "", err
	}
	for _, area := range []string{"product", "arch"} {
		l := LayerProduct
		if area == "arch" {
			l = LayerArch
		}
		if layer != "" && layer != l {
			continue
		}
		md, err := s.branchDocument(ctx, f, area)
		if err != nil {
			return "", err
		}
		if md == "" {
			continue
		}
		parent, _ := Parent(md)
		for _, lk := range Links(md, area, key, parent) {
			ref := LinkRef{Key: lk.Target, Layer: lk.Layer, Items: lk.Requirements, Requirements: lk.Requirements}
			if lk.Kind == LinkExtends {
				out.Extends = append(out.Extends, ref)
			} else {
				out.Changes = append(out.Changes, ref)
			}
		}
	}
	if err := s.fillRefs(ctx, out.Extends); err != nil {
		return "", err
	}
	if err := s.fillRefs(ctx, out.Changes); err != nil {
		return "", err
	}
	return f.branch, nil
}

// fillRefs fills the title and the presence in the index of linked features in one query.
func (s *Service) fillRefs(ctx context.Context, refs []LinkRef) error {
	if len(refs) == 0 {
		return nil
	}
	keys := make([]string, len(refs))
	for i, r := range refs {
		keys[i] = r.Key
	}
	rows, err := s.pool.Query(ctx, `SELECT k, EXISTS (SELECT 1 FROM spec_documents WHERE feature_key = k),
		(SELECT title FROM features WHERE unique_id = k) FROM unnest($1::text[]) AS k`, keys)
	if err != nil {
		return err
	}
	defer rows.Close()
	type info struct {
		indexed bool
		title   *string
	}
	got := map[string]info{}
	for rows.Next() {
		var k string
		var i info
		if err := rows.Scan(&k, &i.indexed, &i.title); err != nil {
			return err
		}
		got[k] = i
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range refs {
		in := got[refs[i].Key]
		refs[i].Indexed = in.indexed
		if in.indexed {
			refs[i].Title = in.title
		}
	}
	return nil
}

// countSource counts an answer of a spec tool by its source (SPEC-0006 arch §8).
func countSource(tool, source string) {
	metrics.SpecToolCalls.WithLabelValues(tool, source).Inc()
}

// QAServiceRefusal checks the column «Сервисы» of a qa a writer submits against
// the services of the feature: the table of arch of its branch, or of tech when
// arch is skipped (FTR.HMR.SPEC-0005 R2). "" — the document may be taken. Without
// a table of services in the branch there is nothing to check against.
func (s *Service) QAServiceRefusal(ctx context.Context, featureKey, qa string) (string, error) {
	var used []string
	for _, tc := range markdown.ParseTestCases(qa) {
		for _, k := range tc.Services {
			if !slices.Contains(used, k) {
				used = append(used, k)
			}
		}
	}
	if len(used) == 0 {
		return "", nil
	}
	f, err := s.featureInWork(ctx, featureKey)
	if errors.Is(err, errNotInWork) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var known []string
	for _, area := range []string{"arch", "tech"} {
		md, err := s.branchDocument(ctx, f, area)
		if err != nil {
			return "", err
		}
		if md == "" {
			continue
		}
		d := contract.Parse(md, contract.Area(area))
		for _, sv := range d.Services {
			known = append(known, sv.Key)
		}
		if d.HasServices {
			break
		}
	}
	if len(known) == 0 {
		return "", nil
	}
	var bad []string
	for _, k := range used {
		if !slices.Contains(known, k) {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return "", nil
	}
	return fmt.Sprintf("qa_unknown_service: %s — not among the services of the feature (%s); the column «Сервисы» names keys of the table of services of arch",
		strings.Join(bad, ", "), strings.Join(known, ", ")), nil
}
