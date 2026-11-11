package specindex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

func logWarn(ctx context.Context, msg string, err error) {
	slog.WarnContext(ctx, msg, "err", err)
}

// folder is a feature folder of the default branch.
type folder struct {
	dir                    string // specs/<d>/<s>/<id>/
	domain, system, folder string
	docs                   map[string]git.TreeEntry // area → spec.md
	files                  []fileEntry              // other files of the areas
}

type fileEntry struct {
	loc   Loc
	entry git.TreeEntry
}

// known is a feature of the database.
type known struct {
	id          uuid.UUID
	key, phase  string
	repoDeleted bool
	dir         string
}

// problem is an open indexing problem found by this run.
type problem struct {
	path, key, kind string
	details         map[string]any
}

// result of a run.
type result struct {
	commit                 string
	found, indexed, issues int
	problemsChanged        bool
}

// folders groups the tree of specs/ by feature folder.
func folders(entries []git.TreeEntry) map[string]*folder {
	out := map[string]*folder{}
	for _, e := range entries {
		l, ok := ParsePath(e.Path)
		if !ok {
			continue
		}
		f := out[l.Dir()]
		if f == nil {
			f = &folder{dir: l.Dir(), domain: l.Domain, system: l.System, folder: l.Folder, docs: map[string]git.TreeEntry{}}
			out[l.Dir()] = f
		}
		if l.IsDocument() {
			f.docs[l.Area] = e
		} else {
			f.files = append(f.files, fileEntry{loc: l, entry: e})
		}
	}
	// A folder is a candidate only with at least one <area>/spec.md (IDX-03).
	for k, f := range out {
		if len(f.docs) == 0 {
			delete(out, k)
		}
	}
	return out
}

// titleArea is the document the title and the parent are read from: the
// product specification, otherwise the first area found (R4).
func (f *folder) titleArea() string {
	if _, ok := f.docs["product"]; ok {
		return "product"
	}
	for _, a := range Areas {
		if _, ok := f.docs[a]; ok {
			return a
		}
	}
	return ""
}

// scan runs one check and records its result.
func (s *Service) scan(ctx context.Context, runID uuid.UUID, trigger string) {
	start := s.now()
	res, err := s.check(ctx)
	status, msg := "succeeded", ""
	if err != nil {
		status, msg = "failed", git.Reason(err)
		if msg == "" {
			msg = err.Error()
		}
		slog.WarnContext(ctx, "spec scan failed", "run", runID, "trigger", trigger, "err", err)
	} else {
		slog.InfoContext(ctx, "spec scan", "run", runID, "trigger", trigger, "commit", res.commit, "found", res.found,
			"indexed", res.indexed, "issues", res.issues, "duration", s.now().Sub(start).String())
	}
	bg := context.WithoutCancel(ctx)
	if _, uerr := s.pool.Exec(bg, `UPDATE spec_scan_runs SET status = $2, finished_at = now(), commit_sha = NULLIF($3, ''),
		found = $4, indexed = $5, issues = $6, error = NULLIF($7, '') WHERE id = $1`,
		runID, status, res.commit, res.found, res.indexed, res.issues, msg); uerr != nil {
		slog.ErrorContext(bg, "record spec scan result", "err", uerr)
	}
	metrics.SpecScanRuns.WithLabelValues(trigger, status).Inc()
	metrics.SpecScanDuration.Observe(s.now().Sub(start).Seconds())
	s.refreshGauges(bg)
	if s.events != nil {
		s.events.Publish(bg, events.Event{Type: events.FocusChanged, Data: map[string]string{"group": "spec"}})
		s.events.Publish(bg, events.Event{Type: EventIndexUpdated, Data: map[string]any{"commit": res.commit}})
	}
}

// EventIndexUpdated tells the navigator to reload (SSE).
const EventIndexUpdated = "spec.index_updated"

func (s *Service) check(ctx context.Context) (result, error) {
	var res result
	token, err := s.git.BotToken(ctx)
	if err != nil {
		return res, err
	}
	head, err := s.git.BranchHead(ctx, token, s.cfg.DefaultBranch)
	if err != nil {
		return res, err
	}
	res.commit = head
	entries, err := s.git.Tree(ctx, token, head, "specs")
	if err != nil {
		return res, err
	}
	dirs := folders(entries)
	res.found = len(dirs)

	knownByKey, err := s.knownFeatures(ctx)
	if err != nil {
		return res, err
	}
	domains, systems, err := s.catalog(ctx)
	if err != nil {
		return res, err
	}

	var problems []problem
	type candidate struct {
		f      *folder
		number int
		parent string
		title  string
	}
	cands := map[string]*candidate{} // key → candidate
	// A folder waiting for its parent is not read again while its document is
	// the same blob and the parent has not appeared (IDX-17).
	waiting, err := s.waitingForParent(ctx)
	if err != nil {
		return res, err
	}
	type deferred struct {
		f      *folder
		number int
		parent string
	}
	var later []deferred
	for _, f := range dirs {
		if _, ok := knownByKey[f.folder]; ok {
			continue // known: changes are processed by webhooks (R8)
		}
		n, p := CheckID(f.folder, f.domain, f.system)
		if p != nil {
			problems = append(problems, problem{path: f.dir, key: f.folder, kind: p.Kind, details: p.Details})
			continue
		}
		if !domains[f.domain] {
			problems = append(problems, problem{path: f.dir, key: f.folder, kind: KindMissingDomain, details: map[string]any{"domain": f.domain, "system": f.system}})
			continue
		}
		if _, ok := systems[f.domain+"/"+f.system]; !ok {
			problems = append(problems, problem{path: f.dir, key: f.folder, kind: KindMissingSystem, details: map[string]any{"domain": f.domain, "system": f.system}})
			continue
		}
		if w, ok := waiting[f.dir]; ok && w.sha == f.docs[f.titleArea()].SHA {
			later = append(later, deferred{f: f, number: n, parent: w.parent})
			continue
		}
		c, err := s.readCandidate(ctx, token, f)
		if err != nil {
			return res, err
		}
		cands[f.folder] = &candidate{f: f, number: n, parent: c.parent, title: c.title}
	}
	for _, d := range later {
		_, isKnown := knownByKey[d.parent]
		if _, isNew := cands[d.parent]; !isKnown && !isNew {
			problems = append(problems, problem{path: d.f.dir, key: d.f.folder, kind: KindMissingParent,
				details: map[string]any{"parent": d.parent, "blobSha": d.f.docs[d.f.titleArea()].SHA}})
			continue
		}
		c, err := s.readCandidate(ctx, token, d.f)
		if err != nil {
			return res, err
		}
		cands[d.f.folder] = &candidate{f: d.f, number: d.number, parent: c.parent, title: c.title}
	}

	// Parents first (IDX-11); a parent that is neither known nor indexed by
	// this run is a problem, retried by the next runs (R5).
	order := make([]string, 0, len(cands))
	state := map[string]int{} // 0 new, 1 visiting, 2 done, 3 failed
	var visit func(k string) bool
	visit = func(k string) bool {
		switch state[k] {
		case 2:
			return true
		case 1, 3:
			return false // a cycle or a failed parent
		}
		state[k] = 1
		c := cands[k]
		if c.parent != "" {
			if _, ok := knownByKey[c.parent]; !ok {
				if _, ok := cands[c.parent]; !ok || !visit(c.parent) {
					state[k] = 3
					problems = append(problems, problem{path: c.f.dir, key: k, kind: KindMissingParent,
						details: map[string]any{"parent": c.parent, "blobSha": c.f.docs[c.f.titleArea()].SHA}})
					return false
				}
			}
		}
		state[k] = 2
		order = append(order, k)
		return true
	}
	keys := make([]string, 0, len(cands))
	for k := range cands {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		visit(k)
	}

	for _, k := range order {
		c := cands[k]
		var parentID *uuid.UUID
		if c.parent != "" {
			if kf, ok := knownByKey[c.parent]; ok {
				id := kf.id
				parentID = &id
			}
		}
		id, err := s.indexFeature(ctx, c.f, systems[c.f.domain+"/"+c.f.system], c.number, c.title, parentID)
		if err != nil {
			if postgres.IsUniqueViolation(err) { // created through the interface meanwhile (IDX-15)
				slog.InfoContext(ctx, "spec appeared in Hammurapi during the check", "key", k)
				continue
			}
			return res, err
		}
		res.indexed++
		knownByKey[k] = &known{id: id, key: k, phase: "indexed", dir: c.f.dir}
	}

	// Deleted from the repository (R7) and returned folders.
	for _, kf := range knownByKey {
		if kf.phase != "indexed" && kf.phase != "released" {
			continue
		}
		_, present := dirs[kf.dir]
		switch {
		case !present:
			problems = append(problems, problem{path: kf.dir, key: kf.key, kind: KindDeleted, details: map[string]any{"phase": kf.phase}})
			if !kf.repoDeleted {
				if _, err := s.pool.Exec(ctx, `UPDATE features SET repo_deleted_at = now() WHERE id = $1 AND repo_deleted_at IS NULL`, kf.id); err != nil {
					return res, err
				}
			}
		case kf.repoDeleted:
			if _, err := s.pool.Exec(ctx, `UPDATE features SET repo_deleted_at = NULL WHERE id = $1`, kf.id); err != nil {
				return res, err
			}
		}
	}

	changed, err := s.saveProblems(ctx, problems)
	if err != nil {
		return res, err
	}
	res.issues, res.problemsChanged = len(problems), changed

	// The index of documents of known features present in the branch.
	indexable := map[string]*folder{}
	for _, f := range dirs {
		if kf, ok := knownByKey[f.folder]; ok && kf.phase != "deleted" {
			indexable[f.dir] = f
		}
	}
	if err := s.syncDocuments(ctx, token, head, indexable); err != nil {
		return res, err
	}
	// FTR.HMR.SPEC-0002 R15: the test cases and services of indexed features, from the documents just synced
	if err := s.projectIndexed(ctx); err != nil {
		return res, err
	}
	return res, nil
}

type readResult struct{ title, parent string }

// readCandidate reads the title and the parent of a new specification (R4, R5).
func (s *Service) readCandidate(ctx context.Context, token string, f *folder) (readResult, error) {
	area := f.titleArea()
	content, err := s.git.Blob(ctx, token, f.docs[area].SHA)
	if err != nil {
		return readResult{}, fmt.Errorf("read %s%s/spec.md: %w", f.dir, area, err)
	}
	md := string(content)
	parent, perr := Parent(md)
	if perr != nil {
		slog.WarnContext(ctx, "spec front matter", "path", f.dir+area+"/spec.md", "err", perr)
	}
	title := Title(md)
	if title == "" {
		title = f.folder
	}
	return readResult{title: title, parent: parent}, nil
}

type waitingParent struct{ parent, sha string }

// waitingForParent lists open missing_parent problems by folder with the blob they were found in.
func (s *Service) waitingForParent(ctx context.Context) (map[string]waitingParent, error) {
	rows, err := s.pool.Query(ctx, `SELECT path, COALESCE(details->>'parent', ''), COALESCE(details->>'blobSha', '')
		FROM spec_index_issues WHERE resolved_at IS NULL AND kind = 'missing_parent'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]waitingParent{}
	for rows.Next() {
		var p string
		var w waitingParent
		if err := rows.Scan(&p, &w.parent, &w.sha); err != nil {
			return nil, err
		}
		if w.sha != "" {
			out[p] = w
		}
	}
	return out, rows.Err()
}

func (s *Service) knownFeatures(ctx context.Context) (map[string]*known, error) {
	rows, err := s.pool.Query(ctx, `SELECT f.id, f.unique_id, f.phase::text, f.repo_deleted_at IS NOT NULL, d.key, sy.key
		FROM features f JOIN systems sy ON sy.id = f.system_id JOIN domains d ON d.id = sy.domain_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*known{}
	for rows.Next() {
		var k known
		var dk, sk string
		if err := rows.Scan(&k.id, &k.key, &k.phase, &k.repoDeleted, &dk, &sk); err != nil {
			return nil, err
		}
		k.dir = "specs/" + dk + "/" + sk + "/" + k.key + "/"
		out[k.key] = &k
	}
	return out, rows.Err()
}

func (s *Service) catalog(ctx context.Context) (map[string]bool, map[string]uuid.UUID, error) {
	domains := map[string]bool{}
	systems := map[string]uuid.UUID{}
	rows, err := s.pool.Query(ctx, `SELECT d.key, sy.id, sy.key FROM domains d LEFT JOIN systems sy ON sy.domain_id = d.id`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var dk string
		var sid *uuid.UUID
		var sk *string
		if err := rows.Scan(&dk, &sid, &sk); err != nil {
			return nil, nil, err
		}
		domains[dk] = true
		if sid != nil && sk != nil {
			systems[dk+"/"+*sk] = *sid
		}
	}
	return domains, systems, rows.Err()
}

// indexFeature creates an implemented feature from the repository (R4) in one
// transaction: the feature, the system's counter, its problems resolved.
func (s *Service) indexFeature(ctx context.Context, f *folder, systemID uuid.UUID, number int, title string, parent *uuid.UUID) (uuid.UUID, error) {
	var id uuid.UUID
	err := postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO features (unique_id, system_id, number, title, branch_name, pr_number, pr_url,
				phase, source, indexed_at, parent_id)
			VALUES ($1, $2, $3, $4, '', 0, '', 'indexed', 'repository', now(), $5) RETURNING id`,
			f.folder, systemID, number, title, parent).Scan(&id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE systems SET last_number = GREATEST(last_number, $2) WHERE id = $1`, systemID, number); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE spec_index_issues SET resolved_at = now() WHERE path = $1 AND resolved_at IS NULL`, f.dir)
		return err
	})
	return id, err
}

// saveProblems upserts the problems of this run and resolves open ones it did
// not meet (FOC-02, FOC-03). It reports whether the open set changed.
func (s *Service) saveProblems(ctx context.Context, problems []problem) (bool, error) {
	changed := false
	err := postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		seen := map[string]bool{}
		for _, p := range problems {
			seen[p.path+"\x00"+p.kind] = true
			details, _ := json.Marshal(p.details)
			var inserted bool
			if err := tx.QueryRow(ctx, `INSERT INTO spec_index_issues (path, kind, feature_key, details) VALUES ($1, $2, $3, $4)
				ON CONFLICT (path, kind) WHERE resolved_at IS NULL
				DO UPDATE SET last_seen_at = now(), details = EXCLUDED.details
				RETURNING (xmax = 0)`, p.path, p.kind, p.key, details).Scan(&inserted); err != nil {
				return err
			}
			changed = changed || inserted
		}
		rows, err := tx.Query(ctx, `SELECT id, path, kind FROM spec_index_issues WHERE resolved_at IS NULL`)
		if err != nil {
			return err
		}
		var stale []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			var path, kind string
			if err := rows.Scan(&id, &path, &kind); err != nil {
				rows.Close()
				return err
			}
			if !seen[path+"\x00"+kind] {
				stale = append(stale, id)
			}
		}
		rows.Close()
		if len(stale) > 0 {
			changed = true
			_, err = tx.Exec(ctx, `UPDATE spec_index_issues SET resolved_at = now() WHERE id = ANY($1)`, stale)
		}
		return err
	})
	return changed, err
}

func (s *Service) refreshGauges(ctx context.Context) {
	rows, err := s.pool.Query(ctx, `SELECT kind, count(*) FROM spec_index_issues WHERE resolved_at IS NULL GROUP BY kind`)
	if err == nil {
		metrics.SpecIndexIssuesOpen.Reset()
		for rows.Next() {
			var k string
			var n float64
			if rows.Scan(&k, &n) == nil {
				metrics.SpecIndexIssuesOpen.WithLabelValues(k).Set(n)
			}
		}
		rows.Close()
	}
	var docs, indexed float64
	if s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM spec_documents), (SELECT count(*) FROM features WHERE source = 'repository')`).Scan(&docs, &indexed) == nil {
		metrics.SpecDocuments.Set(docs)
		metrics.SpecIndexedFeatures.Set(indexed)
	}
}
