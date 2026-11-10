// Package rules implements editing of document templates (rules) through a
// branch and PR/MR, applied after approval by another admin of the same area.
package rules

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GreenOnGrey/speclint/v2/contract"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/markdown"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// File names in the API and in the database.
const (
	FileTemplate    = "template"
	FileFixTemplate = "fix-template"
)

func dbFile(f string) (string, bool) {
	switch f {
	case FileTemplate:
		return "template", true
	case FileFixTemplate:
		return "fix_template", true
	}
	return "", false
}

func apiFile(db string) string {
	if db == "fix_template" {
		return FileFixTemplate
	}
	return FileTemplate
}

// RuleFile is a template in the default branch.
type RuleFile struct {
	File    string `json:"file"`
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA     string `json:"sha"`
}

// Change is a proposed rules change.
type Change struct {
	ID         uuid.UUID   `json:"id"`
	Area       domain.Area `json:"area"`
	File       string      `json:"file"`
	Branch     string      `json:"branch"`
	PRNumber   int         `json:"prNumber"`
	PRURL      string      `json:"prUrl"`
	Comment    *string     `json:"comment"`
	Status     string      `json:"status"`
	AuthorID   uuid.UUID   `json:"authorId"`
	Author     string      `json:"author"`
	ApprovedBy *string     `json:"approvedBy"`
	CreatedAt  time.Time   `json:"createdAt"`
	ClosedAt   *time.Time  `json:"closedAt"`
}

// Service implements rules use cases.
type Service struct {
	pool          *pgxpool.Pool
	git           git.Provider
	tokens        git.TokenSource
	defaultBranch string
	// Lang is the language of the documents: of the starter rules and of the
	// description of the contract (FTR.HMR.DEV-0003 R23).
	Lang contract.Lang
}

// NewService creates the service.
func NewService(pool *pgxpool.Pool, provider git.Provider, tokens git.TokenSource, defaultBranch string) *Service {
	return &Service{pool: pool, git: provider, tokens: tokens, defaultBranch: defaultBranch}
}

// CanView reports read access: area admins and global admins.
func CanView(p *domain.Principal, a domain.Area) bool {
	return p.GlobalAdmin || p.IsAreaAdmin(a)
}

// CanChange reports write access: only area admins (a global admin without the
// area role can only read).
func CanChange(p *domain.Principal, a domain.Area) bool { return p.IsAreaAdmin(a) }

// Current reads both templates of an area.
func (s *Service) Current(ctx context.Context, p *domain.Principal, area domain.Area) ([]RuleFile, error) {
	if !CanView(p, area) {
		return nil, apperr.NoRole("admin", string(area))
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	out := []RuleFile{}
	for _, f := range []string{FileTemplate, FileFixTemplate} {
		path := git.RulePath(string(area), f == FileFixTemplate)
		rf := RuleFile{File: f, Path: path}
		file, err := s.git.GetFile(ctx, token, s.defaultBranch, path)
		if err != nil && !errors.Is(err, git.ErrNotFound) {
			return nil, auth.MapGitError(err)
		}
		if file != nil {
			rf.Content, rf.SHA = string(file.Content), git.BlobSHA(file.Content)
		}
		out = append(out, rf)
	}
	return out, nil
}

// ProposeInput is the body of POST /rules/{area}/changes.
type ProposeInput struct {
	File    string `json:"file"`
	Content string `json:"content"`
	BaseSHA string `json:"baseSha"`
	Comment string `json:"comment"`
	// SkipBaseCheck is set by archive imports.
	SkipBaseCheck bool `json:"-"`
}

// Propose creates a branch rules/<area>/<id> and a PR/MR on behalf of the author.
func (s *Service) Propose(ctx context.Context, p *domain.Principal, area domain.Area, in ProposeInput) (*Change, error) {
	if !CanChange(p, area) {
		return nil, apperr.NoRole("admin", string(area))
	}
	dbf, ok := dbFile(in.File)
	if !ok {
		return nil, apperr.Unprocessable("invalid_file", "file must be template or fix-template")
	}
	if err := CheckContent("propose", area, in.File, in.Content); err != nil { // FTR.HMR.DEV-0003 R19
		return nil, err
	}
	var open int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM rule_changes WHERE area = $1 AND file = $2 AND status = 'open'`, area, dbf).Scan(&open); err != nil {
		return nil, err
	}
	if open > 0 {
		return nil, apperr.Conflict("change_already_open", "there is already an open change for this file")
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	path := git.RulePath(string(area), in.File == FileFixTemplate)
	cur, err := s.git.GetFile(ctx, token, s.defaultBranch, path)
	if err != nil && !errors.Is(err, git.ErrNotFound) {
		return nil, auth.MapGitError(err)
	}
	curSHA := ""
	if cur != nil {
		curSHA = git.BlobSHA(cur.Content)
	}
	if !in.SkipBaseCheck && in.BaseSHA != curSHA {
		return nil, apperr.Conflict("stale_rules", "the rules changed since you opened them").With("currentSha", curSHA)
	}
	if git.BlobSHA([]byte(in.Content)) == curSHA {
		return nil, apperr.Unprocessable("no_changes", "the content is identical to the current rules")
	}
	id := uuid.New()
	branch := fmt.Sprintf("rules/%s/%s", area, id.String()[:8])
	head, err := s.git.BranchHead(ctx, token, s.defaultBranch)
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	if err := s.git.CreateBranch(ctx, token, branch, head); err != nil {
		return nil, auth.MapGitError(err)
	}
	fail := func(cause error) (*Change, error) {
		_ = s.git.DeleteBranch(context.WithoutCancel(ctx), token, branch)
		return nil, auth.MapGitError(cause)
	}
	msg := git.Trailers{Area: string(area)}.Message(fmt.Sprintf("rules(%s): update %s", area, in.File))
	if _, err := s.git.Commit(ctx, token, branch, msg, []git.FileChange{{Path: path, Content: []byte(in.Content)}}); err != nil {
		return fail(err)
	}
	body := strings.TrimSpace(in.Comment)
	if body == "" {
		body = "Rules change proposed in Hammurapi."
	}
	pr, err := s.git.CreatePR(ctx, token, branch, s.defaultBranch, fmt.Sprintf("Rules %s: %s", area, in.File), body)
	if err != nil {
		return fail(err)
	}
	var comment *string
	if c := strings.TrimSpace(in.Comment); c != "" {
		comment = &c
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO rule_changes (id, area, file, branch_name, pr_number, pr_url, comment, author_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id, area, dbf, branch, pr.Number, pr.URL, comment, p.UserID)
	if err != nil {
		_ = s.git.ClosePR(context.WithoutCancel(ctx), token, pr.Number)
		_ = s.git.DeleteBranch(context.WithoutCancel(ctx), token, branch)
		if postgres.IsUniqueViolation(err) {
			return nil, apperr.Conflict("change_already_open", "there is already an open change for this file")
		}
		return nil, err
	}
	return s.get(ctx, id)
}

const changeSelect = `SELECT c.id, c.area, c.file, c.branch_name, c.pr_number, c.pr_url, c.comment, c.status,
	c.author_id, au.display_name, ap.display_name, c.created_at, c.closed_at
	FROM rule_changes c JOIN users au ON au.id = c.author_id LEFT JOIN users ap ON ap.id = c.approved_by`

func scanChange(row interface{ Scan(...any) error }) (*Change, error) {
	var c Change
	var file string
	if err := row.Scan(&c.ID, &c.Area, &file, &c.Branch, &c.PRNumber, &c.PRURL, &c.Comment, &c.Status,
		&c.AuthorID, &c.Author, &c.ApprovedBy, &c.CreatedAt, &c.ClosedAt); err != nil {
		return nil, err
	}
	c.File = apiFile(file)
	return &c, nil
}

func (s *Service) get(ctx context.Context, id uuid.UUID) (*Change, error) {
	c, err := scanChange(s.pool.QueryRow(ctx, changeSelect+` WHERE c.id = $1`, id))
	if postgres.IsNoRows(err) {
		return nil, apperr.NotFound("change_not_found", "rules change not found")
	}
	return c, err
}

// List returns changes in areas the user may view.
func (s *Service) List(ctx context.Context, p *domain.Principal, area, status string, page httpx.Page) (httpx.List[Change], error) {
	var areas []domain.Area
	for _, a := range domain.Areas {
		if CanView(p, a) && (area == "" || string(a) == area) {
			areas = append(areas, a)
		}
	}
	if len(areas) == 0 {
		if area != "" || !p.IsAnyAdmin() {
			return httpx.List[Change]{}, apperr.Forbidden("forbidden", "administrator role required")
		}
	}
	names := make([]string, len(areas))
	for i, a := range areas {
		names[i] = string(a)
	}
	args := []any{names, page.Limit + 1}
	cond := ` WHERE c.area::text = ANY($1::text[])`
	if status != "" {
		args = append(args, status)
		cond += fmt.Sprintf(" AND c.status = $%d", len(args))
	}
	if c := page.Cursor; c != nil {
		args = append(args, c.T, c.ID)
		cond += fmt.Sprintf(" AND (c.created_at, c.id::text) < ($%d, $%d)", len(args)-1, len(args))
	}
	rows, err := s.pool.Query(ctx, changeSelect+cond+` ORDER BY c.created_at DESC, c.id::text DESC LIMIT $2`, args...)
	if err != nil {
		return httpx.List[Change]{}, err
	}
	defer rows.Close()
	var out []Change
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return httpx.List[Change]{}, err
		}
		out = append(out, *c)
	}
	if err := rows.Err(); err != nil {
		return httpx.List[Change]{}, err
	}
	return httpx.NewList(out, page.Limit, func(c Change) (time.Time, string) { return c.CreatedAt, c.ID.String() }), nil
}

// ChangeWithDiff is a change and its diff against the current rules.
type ChangeWithDiff struct {
	Change
	Lines []markdown.DiffLine `json:"lines"`
}

// Get returns a change with its diff.
func (s *Service) Get(ctx context.Context, p *domain.Principal, id uuid.UUID) (*ChangeWithDiff, error) {
	c, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !CanView(p, c.Area) {
		return nil, apperr.NoRole("admin", string(c.Area))
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	path := git.RulePath(string(c.Area), c.File == FileFixTemplate)
	oldC, err := s.read(ctx, token, s.defaultBranch, path)
	if err != nil {
		return nil, err
	}
	newC := oldC
	if c.Status == "open" {
		if newC, err = s.read(ctx, token, c.Branch, path); err != nil {
			return nil, err
		}
	}
	return &ChangeWithDiff{Change: *c, Lines: markdown.Diff(oldC, newC)}, nil
}

func (s *Service) read(ctx context.Context, token, ref, path string) (string, error) {
	f, err := s.git.GetFile(ctx, token, ref, path)
	if errors.Is(err, git.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", auth.MapGitError(err)
	}
	return string(f.Content), nil
}

// Approve approves and merges a change on behalf of a different area admin.
func (s *Service) Approve(ctx context.Context, p *domain.Principal, id uuid.UUID) (*Change, error) {
	c, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !CanChange(p, c.Area) {
		return nil, apperr.NoRole("admin", string(c.Area))
	}
	if c.AuthorID == p.UserID {
		return nil, apperr.Forbidden("own_change", "the author cannot approve their own change")
	}
	if c.Status != "open" {
		return nil, apperr.Conflict("change_closed", "the change is not open")
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	// FTR.HMR.DEV-0003 R19: the file of the change is checked again; the change replaces
	// the whole file, so the file of its branch is what the merge brings
	content, err := s.read(ctx, token, c.Branch, git.RulePath(string(c.Area), c.File == FileFixTemplate))
	if err != nil {
		return nil, err
	}
	if err := CheckContent("approve", c.Area, c.File, content); err != nil {
		return nil, err
	}
	if err := s.git.ApprovePR(ctx, token, c.PRNumber); err != nil {
		// Approval rules may be disabled in the provider; the merge below is authoritative.
		slog.WarnContext(ctx, "provider approval failed", "pr", c.PRNumber, "err", err)
	}
	if err := s.git.MergePR(ctx, token, c.PRNumber, fmt.Sprintf("Rules %s: %s", c.Area, c.File)); err != nil {
		return nil, auth.MapGitError(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE rule_changes SET status = 'merged', approved_by = $2, closed_at = now() WHERE id = $1`, id, p.UserID); err != nil {
		return nil, err
	}
	if err := s.git.DeleteBranch(ctx, token, c.Branch); err != nil {
		slog.WarnContext(ctx, "delete rules branch", "branch", c.Branch, "err", err)
	}
	return s.get(ctx, id)
}

// Withdraw closes the author's own change.
func (s *Service) Withdraw(ctx context.Context, p *domain.Principal, id uuid.UUID) (*Change, error) {
	c, err := s.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.AuthorID != p.UserID {
		return nil, apperr.Forbidden("not_author", "only the author can withdraw a change")
	}
	if c.Status != "open" {
		return nil, apperr.Conflict("change_closed", "the change is not open")
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	if err := s.git.ClosePR(ctx, token, c.PRNumber); err != nil && !errors.Is(err, git.ErrNotFound) {
		return nil, auth.MapGitError(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE rule_changes SET status = 'withdrawn', closed_at = now() WHERE id = $1`, id); err != nil {
		return nil, err
	}
	if err := s.git.DeleteBranch(ctx, token, c.Branch); err != nil {
		slog.WarnContext(ctx, "delete rules branch", "branch", c.Branch, "err", err)
	}
	return s.get(ctx, id)
}

// Routes mounts /admin/api/v1/rules.
func (s *Service) Routes(r chi.Router) {
	// Static paths first so "changes" is not taken as an area.
	s.contractRoutes(r)
	r.Get("/rules/changes", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		page, err := httpx.ParsePage(r)
		if err != nil {
			return err
		}
		q := r.URL.Query()
		l, err := s.List(r.Context(), p, q.Get("area"), q.Get("status"), page)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, l)
		return nil
	}))
	withChange := func(fn func(context.Context, *domain.Principal, uuid.UUID) (any, error)) http.HandlerFunc {
		return httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			p, err := httpx.MustPrincipal(r)
			if err != nil {
				return err
			}
			id, err := httpx.ParamUUID(r, "id")
			if err != nil {
				return err
			}
			res, err := fn(r.Context(), p, id)
			if err != nil {
				return err
			}
			httpx.JSON(w, 200, res)
			return nil
		})
	}
	r.Get("/rules/changes/{id}", withChange(func(ctx context.Context, p *domain.Principal, id uuid.UUID) (any, error) { return s.Get(ctx, p, id) }))
	r.Post("/rules/changes/{id}/approve", withChange(func(ctx context.Context, p *domain.Principal, id uuid.UUID) (any, error) {
		return s.Approve(ctx, p, id)
	}))
	r.Post("/rules/changes/{id}/withdraw", withChange(func(ctx context.Context, p *domain.Principal, id uuid.UUID) (any, error) {
		return s.Withdraw(ctx, p, id)
	}))
	r.Get("/rules/{area}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		a, err := httpx.ParamArea(r)
		if err != nil {
			return err
		}
		files, err := s.Current(r.Context(), p, a)
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, map[string]any{"area": a, "files": files, "canChange": CanChange(p, a)})
		return nil
	}))
	r.Post("/rules/{area}/changes", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		a, err := httpx.ParamArea(r)
		if err != nil {
			return err
		}
		var in ProposeInput
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		c, err := s.Propose(r.Context(), p, a, in)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusCreated, map[string]any{"id": c.ID, "prUrl": c.PRURL})
		return nil
	}))
}
