// Package specchanges keeps the notes «Изменено в …» in the product documents of
// the features a feature changes (FTR.HMR.DEV-0002): one function for every
// path that saves a product document.
package specchanges

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/markdown"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Reasons a row of the section cannot be applied (tech §2.4).
const (
	ReasonUnknownTarget      = "unknown_target"
	ReasonTargetInWork       = "target_in_work"
	ReasonUnknownRequirement = "unknown_requirement"
)

// RowError is a row of the section that gets no note.
type RowError struct {
	Feature     string  `json:"feature"`
	Requirement *string `json:"requirement,omitempty"`
	Reason      string  `json:"reason"`
}

// Message is the reason for the writer (tech §3.1).
func (e RowError) Message() string {
	where := e.Feature
	if e.Requirement != nil {
		where += " " + *e.Requirement
	}
	switch e.Reason {
	case ReasonTargetInWork:
		return e.Feature + " is still in work: changes to it go through a revision request in its own topic, not this section."
	case ReasonUnknownRequirement:
		return where + ": no such requirement in the default branch."
	}
	return e.Feature + ": no such feature in the default branch."
}

// Messages joins the reasons for a ToolError of submit_gate.
func Messages(errs []RowError) string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Message()
	}
	return strings.Join(out, "\n") + "\nFix the section «Изменения в других решениях» and submit again."
}

// Service checks the rows against the index of the default branch and writes the notes.
type Service struct {
	Q   postgres.Querier
	Git git.Provider
}

// row is a row of the section with what Align needs.
type row struct {
	markdown.Change
	path string // product of the target in the default branch; "" when the row is wrong
	err  *RowError
}

// check validates the rows of a product document (R3): the target is in the
// index of the default branch and has the requirement.
func (s *Service) check(ctx context.Context, self, product string) ([]row, []RowError, error) {
	changes := markdown.ParseChanges(product)
	paths := map[string]string{}
	reqs := map[string]map[string]bool{}
	var rows []row
	var errs []RowError
	for _, c := range changes {
		r := row{Change: c}
		path, ok := paths[c.Feature]
		if !ok {
			err := s.Q.QueryRow(ctx, `SELECT path FROM spec_documents WHERE feature_key = $1 AND area = 'product'`, c.Feature).Scan(&path)
			if err != nil && !postgres.IsNoRows(err) {
				return nil, nil, err
			}
			paths[c.Feature] = path
		}
		reason := ""
		switch {
		case c.Feature == self:
			reason = ReasonUnknownTarget
		case path == "":
			var known bool
			if err := s.Q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM features WHERE unique_id = $1 AND deleted_at IS NULL)`, c.Feature).Scan(&known); err != nil {
				return nil, nil, err
			}
			reason = ReasonUnknownTarget
			if known {
				reason = ReasonTargetInWork
			}
		case c.Req != "":
			ids, ok := reqs[c.Feature]
			if !ok {
				var err error
				if ids, err = s.requirements(ctx, c.Feature); err != nil {
					return nil, nil, err
				}
				reqs[c.Feature] = ids
			}
			if !ids[c.Req] {
				reason = ReasonUnknownRequirement
			}
		}
		if reason != "" {
			e := RowError{Feature: c.Feature, Reason: reason}
			if c.Req != "" {
				req := c.Req
				e.Requirement = &req
			}
			r.err = &e
			errs = append(errs, e)
		} else {
			r.path = path
		}
		rows = append(rows, r)
	}
	return rows, errs, nil
}

// Check lists the rows of a product document that cannot be applied (R3).
func (s *Service) Check(ctx context.Context, self, product string) ([]RowError, error) {
	_, errs, err := s.check(ctx, self, product)
	return errs, err
}

// Refusal is the reason a submission of the writer is refused with, "" — none
// (tech §3.1); a refusal is counted.
func (s *Service) Refusal(ctx context.Context, self, product string) (string, error) {
	errs, err := s.Check(ctx, self, product)
	if err != nil || len(errs) == 0 {
		return "", err
	}
	metrics.SpecChangeNotes.WithLabelValues("rejected").Inc()
	return Messages(errs), nil
}

// Applied counts the documents of other features committed with notes (arch §7).
func Applied(changes []git.FileChange) {
	metrics.SpecChangeNotes.WithLabelValues("applied").Add(float64(len(changes)))
}

func (s *Service) requirements(ctx context.Context, key string) (map[string]bool, error) {
	rs, err := s.Q.Query(ctx, `SELECT req_id FROM spec_requirements WHERE feature_key = $1`, key)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[string]bool{}
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rs.Err()
}

// sourceReq is the first requirement of the source that names the target
// (tech §4.2); "" when none does.
func sourceReq(reqs []specindex.Requirement, target string) string {
	for _, r := range reqs {
		if strings.Contains(r.Text, target) {
			return r.ID
		}
	}
	return ""
}

// target is a document to align: the default-branch path and the notes it should have.
type target struct {
	path  string
	notes []markdown.Note
}

// plan groups the valid rows by target and adds the targets where the branch
// already has notes of the source (a removed row, R4).
func (s *Service) plan(ctx context.Context, token string, f *specdata.Feature, product string, rows []row) ([]string, map[string]*target, error) {
	reqs := specindex.Requirements(product)
	byKey := map[string]*target{}
	var order []string
	add := func(key, path string) *target {
		t, ok := byKey[key]
		if !ok {
			t = &target{path: path}
			byKey[key] = t
			order = append(order, key)
		}
		return t
	}
	for _, r := range rows {
		if r.err != nil {
			continue
		}
		t := add(r.Feature, r.path)
		t.notes = append(t.notes, markdown.Note{Req: r.Req, SourceReq: sourceReq(reqs, r.Feature), Text: r.Note})
	}
	if f.PRNumber > 0 { // product documents of other features changed in the branch: notes of rows since removed
		files, err := s.Git.PRChangedFiles(ctx, token, f.PRNumber)
		if err != nil && !errors.Is(err, git.ErrNotFound) {
			return nil, nil, err
		}
		for _, cf := range files {
			loc, ok := specindex.ParsePath(cf.Path)
			if !ok || loc.Folder == f.UniqueID || loc.Area != "product" || !loc.IsDocument() || cf.Status == "removed" {
				continue
			}
			add(loc.Folder, cf.Path)
		}
	}
	return order, byKey, nil
}

// Align computes the notes of a product document in the branch of the feature
// (tech §5.1): the changed documents of other features and the rows that got
// no note.
func (s *Service) Align(ctx context.Context, token string, f *specdata.Feature, product string) ([]git.FileChange, []RowError, error) {
	rows, errs, err := s.check(ctx, f.UniqueID, product)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range errs {
		req := ""
		if e.Requirement != nil {
			req = *e.Requirement
		}
		slog.InfoContext(ctx, "spec change row rejected", "feature", f.UniqueID, "target", e.Feature, "requirement", req, "reason", e.Reason)
	}
	order, targets, err := s.plan(ctx, token, f, product, rows)
	if err != nil {
		return nil, nil, err
	}
	var out []git.FileChange
	for _, key := range order {
		t := targets[key]
		file, err := s.Git.GetFile(ctx, token, f.Branch, t.path)
		if errors.Is(err, git.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		next := markdown.ApplyNotes(string(file.Content), f.UniqueID, t.notes)
		if next != string(file.Content) {
			out = append(out, git.FileChange{Path: t.path, Content: []byte(next)})
		}
	}
	return out, errs, nil
}

// Item is a row of the block «Изменения в других решениях» (tech §2.1).
type Item struct {
	Feature     string    `json:"feature"`
	Requirement *string   `json:"requirement"`
	Note        string    `json:"note"`
	State       string    `json:"state"` // marked | pending | error
	URL         string    `json:"url"`
	Error       *RowError `json:"error,omitempty"`
}

// Items are the rows of the section with their state in the branch (R7).
func (s *Service) Items(ctx context.Context, token string, f *specdata.Feature, product string) ([]Item, error) {
	rows, _, err := s.check(ctx, f.UniqueID, product)
	if err != nil {
		return nil, err
	}
	reqs := specindex.Requirements(product)
	docs := map[string]string{}
	out := make([]Item, 0, len(rows))
	for _, r := range rows {
		it := Item{Feature: r.Feature, Note: r.Note, State: "error", Error: r.err}
		if r.Req != "" {
			req := r.Req
			it.Requirement = &req
		}
		it.URL = "/spec/" + r.Feature + "/product" // the route of the navigator
		if r.Req != "" {
			it.URL += "#" + strings.ToLower(r.Req)
		}
		if r.err == nil {
			doc, ok := docs[r.path]
			if !ok {
				file, err := s.Git.GetFile(ctx, token, f.Branch, r.path)
				if err != nil && !errors.Is(err, git.ErrNotFound) {
					return nil, err
				}
				if file != nil {
					doc = string(file.Content)
				}
				docs[r.path] = doc
			}
			line := markdown.NoteLine(f.UniqueID, markdown.Note{Req: r.Req, SourceReq: sourceReq(reqs, r.Feature), Text: r.Note})
			it.State = "pending"
			if hasLine(doc, line) {
				it.State = "marked"
			}
		}
		out = append(out, it)
	}
	return out, nil
}

func hasLine(doc, line string) bool {
	for _, l := range strings.Split(doc, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}

// ErrInvalid is 409 changes_invalid with the wrong rows (tech §2.4).
func ErrInvalid(rows []RowError) error {
	return apperr.Conflict("changes_invalid", "some rows of «Изменения в других решениях» cannot be applied: fix them first").With("rows", rows)
}

// Verify refuses a product document whose section has wrong rows (R3): 409
// changes_invalid, before anything is committed.
func (s *Service) Verify(ctx context.Context, token string, f *specdata.Feature) error {
	doc, err := s.product(ctx, token, f)
	if doc == nil || err != nil {
		return err
	}
	errs, err := s.Check(ctx, f.UniqueID, *doc)
	if err != nil {
		return err
	}
	if len(errs) > 0 {
		metrics.SpecChangeNotes.WithLabelValues("rejected").Inc()
		return ErrInvalid(errs)
	}
	return nil
}

// Ensure aligns the notes before the product gate moves on (R3, R6): the
// product document as it is in the branch, wrong rows refuse with 409
// changes_invalid, changed notes are committed with the trailer Generated.
// A feature without a product document has nothing to align.
func (s *Service) Ensure(ctx context.Context, token string, f *specdata.Feature) error {
	doc, err := s.product(ctx, token, f)
	if doc == nil || err != nil {
		return err
	}
	changes, errs, err := s.Align(ctx, token, f, *doc)
	if err != nil {
		return err
	}
	if len(errs) > 0 {
		metrics.SpecChangeNotes.WithLabelValues("rejected").Inc()
		return ErrInvalid(errs)
	}
	if len(changes) == 0 {
		return nil
	}
	_, err = s.Git.Commit(ctx, token, f.Branch, CommitMessage(f.UniqueID), changes)
	if errors.Is(err, git.ErrConflict) {
		return apperr.Conflict("stale_document", "the branch changed concurrently, retry")
	}
	if err == nil {
		Applied(changes)
	}
	return err
}

// product is the product document of the feature in its branch, nil — none.
func (s *Service) product(ctx context.Context, token string, f *specdata.Feature) (*string, error) {
	file, err := s.Git.GetFile(ctx, token, f.Branch, git.SpecPath(f.DomainKey, f.SystemKey, f.UniqueID, "product"))
	if errors.Is(err, git.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	doc := string(file.Content)
	return &doc, nil
}

// CommitMessage is the message of a commit with notes only.
func CommitMessage(key string) string {
	return git.Trailers{Feature: key, Generated: true}.Message(key + ": notes in changed features")
}
