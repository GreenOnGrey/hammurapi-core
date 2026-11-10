// Package specchanges keeps the notes «Изменено в …» in the documents of the
// features a feature changes (FTR.HMR.DEV-0002): requirements R<n> of product
// and decisions A<n> of arch (FTR.HMR.DEV-0003 R12, R14); one function for every
// path that saves a product or arch document.
package specchanges

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/GreenOnGrey/speclint/v2/contract"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Reasons a row of the section cannot be applied (tech §2.4).
const (
	ReasonUnknownTarget      = "unknown_target"
	ReasonTargetInWork       = "target_in_work"
	ReasonUnknownRequirement = "unknown_requirement"
	ReasonUnknownDecision    = "unknown_decision" // FTR.HMR.DEV-0003 R12
	ReasonWrongKind          = "wrong_kind"       // R<n> in arch, A<n> in product
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
	case ReasonUnknownDecision:
		return where + ": no such architecture decision in the default branch."
	case ReasonWrongKind:
		return where + ": product changes requirements R<n>, arch changes decisions A<n> — move the row to the section of the other document."
	}
	return e.Feature + ": no such feature in the default branch."
}

// Areas are the areas whose documents have the section of changes and get
// notes (FTR.HMR.DEV-0003 R12): product with R<n>, arch with A<n>.
var Areas = []string{"product", "arch"}

// Supported tells whether a document of the area has the section of changes.
func Supported(area string) bool { return area == Areas[0] || area == Areas[1] }

// lang is the language the notes are written in: English when unset (FTR.HMR.DEV-0003 R23).
func (s *Service) lang() contract.Lang { return contract.ParseLang(string(s.Lang)) }

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
	Q    postgres.Querier
	Git  git.Provider
	Lang contract.Lang // the language of the documents: the notes are written in it (FTR.HMR.DEV-0003 R22)
}

// row is a row of the section with what Align needs.
type row struct {
	Feature, Req, Note string
	path               string // the document of the target in the default branch; "" when the row is wrong
	err                *RowError
}

// check validates the rows of a product or arch document (R3, FTR.HMR.DEV-0003
// R12): the target has the document of the same area in the index of the
// default branch, and the item of the kind of the area.
func (s *Service) check(ctx context.Context, self, area, doc string) ([]row, []RowError, error) {
	kind := contract.KindOf(contract.Area(area))
	paths := map[string]string{}
	items := map[string]map[string]bool{}
	var rows []row
	var errs []RowError
	for _, c := range contract.Parse(doc, contract.Area(area)).Changes {
		r := row{Feature: c.Key, Req: c.Item, Note: c.Text}
		path, ok := paths[c.Key]
		if !ok {
			err := s.Q.QueryRow(ctx, `SELECT path FROM spec_documents WHERE feature_key = $1 AND area = $2`, c.Key, area).Scan(&path)
			if err != nil && !postgres.IsNoRows(err) {
				return nil, nil, err
			}
			paths[c.Key] = path
		}
		reason := ""
		switch {
		case c.Key == self:
			reason = ReasonUnknownTarget
		case path == "":
			var known bool
			if err := s.Q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM features WHERE unique_id = $1 AND deleted_at IS NULL)`, c.Key).Scan(&known); err != nil {
				return nil, nil, err
			}
			reason = ReasonUnknownTarget
			if known {
				reason = ReasonTargetInWork
			}
		case c.Item != "" && contract.ItemKind(c.Item) != "" && contract.ItemKind(c.Item) != kind:
			reason = ReasonWrongKind
		case c.Item != "":
			ids, ok := items[c.Key]
			if !ok {
				var err error
				if ids, err = s.items(ctx, c.Key, kind); err != nil {
					return nil, nil, err
				}
				items[c.Key] = ids
			}
			if !ids[c.Item] {
				reason = ReasonUnknownRequirement
				if kind == contract.KindDecision {
					reason = ReasonUnknownDecision
				}
			}
		}
		if reason != "" {
			e := RowError{Feature: c.Key, Reason: reason}
			if c.Item != "" {
				req := c.Item
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

// Check lists the rows of a product or arch document that cannot be applied (R3).
func (s *Service) Check(ctx context.Context, self, area, doc string) ([]RowError, error) {
	if !Supported(area) {
		return nil, nil
	}
	_, errs, err := s.check(ctx, self, area, doc)
	return errs, err
}

// Refusal is the reason a submission of the writer is refused with, "" — none
// (tech §3.1); a refusal is counted.
func (s *Service) Refusal(ctx context.Context, self, area, doc string) (string, error) {
	errs, err := s.Check(ctx, self, area, doc)
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

func (s *Service) items(ctx context.Context, key, kind string) (map[string]bool, error) {
	rs, err := s.Q.Query(ctx, `SELECT req_id FROM spec_requirements WHERE feature_key = $1 AND kind = $2`, key, kind)
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

// target is a document to align: the default-branch path and the notes it should have.
type target struct {
	path  string
	notes []contract.Note
}

// plan groups the valid rows by target and adds the targets where the branch
// already has notes of the source (a removed row, R4).
func (s *Service) plan(ctx context.Context, token string, f *specdata.Feature, area, doc string, rows []row) ([]string, map[string]*target, error) {
	src := contract.Parse(doc, contract.Area(area))
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
		t.notes = append(t.notes, contract.Note{Target: r.Req, SourceItem: contract.SourceItem(src, r.Feature), Text: r.Note})
	}
	if f.PRNumber > 0 { // documents of the area of other features changed in the branch: notes of rows since removed
		files, err := s.Git.PRChangedFiles(ctx, token, f.PRNumber)
		if err != nil && !errors.Is(err, git.ErrNotFound) {
			return nil, nil, err
		}
		for _, cf := range files {
			loc, ok := specindex.ParsePath(cf.Path)
			if !ok || loc.Folder == f.UniqueID || loc.Area != area || !loc.IsDocument() || cf.Status == "removed" {
				continue
			}
			add(loc.Folder, cf.Path)
		}
	}
	return order, byKey, nil
}

// Align computes the notes of a product or arch document in the branch of the
// feature (tech §5.1, FTR.HMR.DEV-0003 R14): the changed documents of other
// features and the rows that got no note. Other areas have nothing to align.
func (s *Service) Align(ctx context.Context, token string, f *specdata.Feature, area, doc string) ([]git.FileChange, []RowError, error) {
	if !Supported(area) {
		return nil, nil, nil
	}
	rows, errs, err := s.check(ctx, f.UniqueID, area, doc)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range errs {
		req := ""
		if e.Requirement != nil {
			req = *e.Requirement
		}
		slog.InfoContext(ctx, "spec change row rejected", "feature", f.UniqueID, "area", area, "target", e.Feature, "item", req, "reason", e.Reason)
	}
	order, targets, err := s.plan(ctx, token, f, area, doc, rows)
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
		next := contract.ApplyNotes(string(file.Content), contract.Area(area), f.UniqueID, t.notes, s.lang())
		if next != string(file.Content) {
			out = append(out, git.FileChange{Path: t.path, Content: []byte(next)})
		}
	}
	return out, errs, nil
}

// Item is a row of the block «Изменения в других решениях» (tech §2.1,
// FTR.HMR.DEV-0003 tech §2.5): Requirement is the old name of Item, kept for
// one version of the web.
type Item struct {
	Feature     string    `json:"feature"`
	Item        *string   `json:"item"`
	Kind        string    `json:"kind"` // requirement | decision
	Requirement *string   `json:"requirement"`
	Note        string    `json:"note"`
	State       string    `json:"state"` // marked | pending | error
	URL         string    `json:"url"`
	Error       *RowError `json:"error,omitempty"`
}

// Items are the rows of the section of a product or arch document with their
// state in the branch (R7, FTR.HMR.DEV-0003 R16).
func (s *Service) Items(ctx context.Context, token string, f *specdata.Feature, area, doc string) ([]Item, error) {
	if !Supported(area) {
		return []Item{}, nil
	}
	rows, _, err := s.check(ctx, f.UniqueID, area, doc)
	if err != nil {
		return nil, err
	}
	src := contract.Parse(doc, contract.Area(area))
	kind := contract.KindOf(contract.Area(area))
	docs := map[string]string{}
	out := make([]Item, 0, len(rows))
	for _, r := range rows {
		it := Item{Feature: r.Feature, Kind: kind, Note: r.Note, State: "error", Error: r.err}
		if r.Req != "" {
			req := r.Req
			it.Item, it.Requirement = &req, &req
		}
		it.URL = "/spec/" + r.Feature + "/" + area // the route of the navigator
		if r.Req != "" {
			it.URL += "#" + strings.ToLower(r.Req)
		}
		if r.err == nil {
			target, ok := docs[r.path]
			if !ok {
				file, err := s.Git.GetFile(ctx, token, f.Branch, r.path)
				if err != nil && !errors.Is(err, git.ErrNotFound) {
					return nil, err
				}
				if file != nil {
					target = string(file.Content)
				}
				docs[r.path] = target
			}
			// marked: the target has the note of this row, in any language of the contract (R22)
			want := contract.Note{Target: r.Req, Source: f.UniqueID, SourceItem: contract.SourceItem(src, r.Feature), Text: noteText(r.Note)}
			it.State = "pending"
			for _, n := range contract.Parse(target, contract.Area(area)).Notes {
				if n.Target == want.Target && n.Source == want.Source && n.SourceItem == want.SourceItem && noteText(n.Text) == want.Text {
					it.State = "marked"
					break
				}
			}
		}
		out = append(out, it)
	}
	return out, nil
}

// noteText is the text of a note as the contract writes it: without the stars
// and the final full stop.
func noteText(s string) string {
	return strings.TrimRight(strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "*")), ".")
}

// ErrInvalid is 409 changes_invalid with the wrong rows (tech §2.4).
func ErrInvalid(rows []RowError) error {
	return apperr.Conflict("changes_invalid", "some rows of «Изменения в других решениях» cannot be applied: fix them first").With("rows", rows)
}

// Verify refuses a product or arch document whose section has wrong rows (R3):
// 409 changes_invalid, before anything is committed.
func (s *Service) Verify(ctx context.Context, token string, f *specdata.Feature, area string) error {
	doc, err := s.document(ctx, token, f, area)
	if doc == nil || err != nil {
		return err
	}
	errs, err := s.Check(ctx, f.UniqueID, area, *doc)
	if err != nil {
		return err
	}
	if len(errs) > 0 {
		metrics.SpecChangeNotes.WithLabelValues("rejected").Inc()
		return ErrInvalid(errs)
	}
	return nil
}

// Ensure aligns the notes before a product or arch gate moves on (R3, R6): the
// document as it is in the branch, wrong rows refuse with 409 changes_invalid,
// changed notes are committed with the trailer Generated. A feature without
// the document has nothing to align.
func (s *Service) Ensure(ctx context.Context, token string, f *specdata.Feature, area string) error {
	if !Supported(area) {
		return nil
	}
	doc, err := s.document(ctx, token, f, area)
	if doc == nil || err != nil {
		return err
	}
	changes, errs, err := s.Align(ctx, token, f, area, *doc)
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

// document is a document of the feature in its branch, nil — none.
func (s *Service) document(ctx context.Context, token string, f *specdata.Feature, area string) (*string, error) {
	if !Supported(area) {
		return nil, nil
	}
	file, err := s.Git.GetFile(ctx, token, f.Branch, git.SpecPath(f.DomainKey, f.SystemKey, f.UniqueID, area))
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
