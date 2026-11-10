// Package imports implements importing ready specifications from a zip
// archive: synchronous validation and preview in api, asynchronous execution
// in worker.
package imports

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/gabriel-vasile/mimetype"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/rules"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/kafka"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/markdown"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/storage"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Issue is a preview error or warning; the UI localizes it by code.
type Issue struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// FeaturePreview is the preview of one archive feature.
type FeaturePreview struct {
	ArchiveID string   `json:"archiveId"`
	NewID     *string  `json:"newId"` // predicted; reserved only on execution
	Domain    string   `json:"domain"`
	System    string   `json:"system"`
	Title     string   `json:"title"`
	Areas     []string `json:"areas"`
	Files     int      `json:"files"`
	FileNames []string `json:"fileNames"`
	Parent    *string  `json:"parent"`
	Errors    []Issue  `json:"errors"`
	Warnings  []Issue  `json:"warnings"`
}

// RulePreview is the plan for a rules file of the archive.
type RulePreview struct {
	Area   string `json:"area"`
	File   string `json:"file"`
	Action string `json:"action"` // propose | skip
	Reason string `json:"reason,omitempty"`
	// Violations of the document contract when Reason is rules_contract (FTR.HMR.DEV-0003 R19).
	Violations []rules.Violation `json:"violations,omitempty"`
	Result     *string           `json:"result,omitempty"`
	PRURL      *string           `json:"prUrl,omitempty"`
}

// Preview is stored in imports.preview.
type Preview struct {
	FileName string           `json:"fileName"`
	Size     int              `json:"size"`
	Features []FeaturePreview `json:"features"`
	Rules    []RulePreview    `json:"rules"`
	// Admins can create missing domains; shown to users who cannot.
	Admins []string `json:"admins"`
}

// ItemResult is the execution result of one feature.
type ItemResult struct {
	ArchiveID string  `json:"archiveId"`
	Status    string  `json:"status"`
	NewID     *string `json:"newId"`
	PRURL     *string `json:"prUrl"`
	Gates     int     `json:"gates"`
	Error     *string `json:"error"`
}

// Job is an import job.
type Job struct {
	ID         uuid.UUID  `json:"id"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt"`
	Preview
	Results []ItemResult `json:"results"`
	UserID  uuid.UUID    `json:"-"`
	S3Key   string       `json:"-"`
}

// Config configures imports.
type Config struct {
	MaxBytes      int64
	Limits        Limits
	AllowedAssets []string
	DefaultBranch string
}

// Service implements imports.
type Service struct {
	pool      *pgxpool.Pool
	store     specdata.Store
	s3        storage.Storage
	bus       kafka.Publisher
	git       git.Provider
	tokens    git.TokenSource
	events    events.Publisher
	rules     *rules.Service
	principal func(ctx context.Context, id uuid.UUID) (*domain.Principal, error)
	cfg       Config
	allowed   map[string]bool
}

// NewService creates the service.
func NewService(pool *pgxpool.Pool, store specdata.Store, s3 storage.Storage, bus kafka.Publisher, provider git.Provider,
	tokens git.TokenSource, ev events.Publisher, rs *rules.Service,
	principal func(ctx context.Context, id uuid.UUID) (*domain.Principal, error), cfg Config) *Service {
	allowed := map[string]bool{}
	for _, t := range cfg.AllowedAssets {
		allowed[strings.ToLower(t)] = true
	}
	return &Service{pool: pool, store: store, s3: s3, bus: bus, git: provider, tokens: tokens, events: ev, rules: rs,
		principal: principal, cfg: cfg, allowed: allowed}
}

// Upload stores and validates an archive, creating a job with a preview.
func (s *Service) Upload(ctx context.Context, p *domain.Principal, fileName string, data []byte) (*Job, error) {
	if !p.IsAnyExpert() {
		return nil, apperr.Forbidden("forbidden", "domain expert role required")
	}
	if int64(len(data)) > s.cfg.MaxBytes {
		return nil, apperr.TooLarge("archive_too_large", "the archive is too large").With("maxBytes", s.cfg.MaxBytes)
	}
	arc, err := ParseArchive(data, s.cfg.Limits)
	if err != nil {
		return nil, err
	}
	if len(arc.Features) == 0 && len(arc.Rules) == 0 {
		return nil, apperr.Unprocessable("archive_empty", "the archive contains no specifications")
	}
	pv, err := s.validate(ctx, p, arc)
	if err != nil {
		return nil, err
	}
	pv.FileName, pv.Size = fileName, len(data)
	id := uuid.New()
	key := fmt.Sprintf("imports/%s/%s.zip", p.UserID, id)
	if err := s.s3.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "application/zip"); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(pv)
	if _, err := s.pool.Exec(ctx, `INSERT INTO imports (id, user_id, s3_key, status, preview) VALUES ($1,$2,$3,'validated',$4)`,
		id, p.UserID, key, raw); err != nil {
		_ = s.s3.Delete(context.WithoutCancel(ctx), key)
		return nil, err
	}
	return s.Get(ctx, p, id)
}

func (s *Service) validate(ctx context.Context, p *domain.Principal, arc *Archive) (*Preview, error) {
	pv := &Preview{Features: []FeaturePreview{}, Rules: []RulePreview{}}
	next := map[uuid.UUID]int{} // predicted numbers per system
	for _, f := range arc.Features {
		fp := FeaturePreview{ArchiveID: f.ArchiveID, Domain: f.Domain, System: f.System, Areas: []string{}, Errors: []Issue{}, Warnings: []Issue{}, FileNames: []string{}}
		for name := range f.Files {
			fp.FileNames = append(fp.FileNames, name)
		}
		sort.Strings(fp.FileNames)
		fp.Files = len(f.Files)
		for _, stray := range f.Stray {
			fp.Errors = append(fp.Errors, Issue{Code: "unexpected_path", Params: map[string]any{"path": stray}})
		}
		// Areas, spec.md and assets.
		var areas []domain.Area
		for _, a := range f.Areas() {
			area, err := domain.ParseArea(a)
			if err != nil {
				fp.Errors = append(fp.Errors, Issue{Code: "unknown_area", Params: map[string]any{"area": a}})
				continue
			}
			areas = append(areas, area)
			if _, ok := f.Files[a+"/spec.md"]; !ok {
				fp.Errors = append(fp.Errors, Issue{Code: "missing_spec", Params: map[string]any{"area": a}})
			}
		}
		domain.SortAreas(areas)
		for _, a := range areas {
			fp.Areas = append(fp.Areas, string(a))
		}
		if len(areas) == 0 {
			fp.Errors = append(fp.Errors, Issue{Code: "no_areas"})
		}
		// Parent from front matter; title from the first heading.
		var parent string
		for _, a := range areas {
			doc, ok := f.Files[string(a)+"/spec.md"]
			if !ok {
				continue
			}
			front, _, hasFront := markdown.SplitFrontMatter(string(doc))
			if hasFront && front["parent"] != "" && parent == "" {
				parent = front["parent"]
			}
			if fp.Title == "" {
				fp.Title = markdown.FirstHeading(string(doc))
			}
		}
		if fp.Title == "" {
			fp.Title = f.ArchiveID
		}
		for _, name := range fp.FileNames {
			data := f.Files[name]
			if strings.HasSuffix(name, "/spec.md") {
				for _, w := range markdown.CheckSubset(string(data), parent != "") {
					fp.Warnings = append(fp.Warnings, Issue{Code: "markdown_" + w, Params: map[string]any{"file": name}})
				}
				continue
			}
			if !s.assetAllowed(data) {
				fp.Errors = append(fp.Errors, Issue{Code: "asset_type_not_allowed", Params: map[string]any{"file": name, "type": mimetype.Detect(data).String()}})
			}
		}
		// Mentions of the old id are not rewritten.
		mentions := 0
		for name, data := range f.Files {
			if strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".html") {
				mentions += strings.Count(string(data), f.ArchiveID)
			}
		}
		if mentions > 0 {
			fp.Warnings = append(fp.Warnings, Issue{Code: "old_id_mentioned", Params: map[string]any{"count": mentions}})
		}
		// Roles: experts import into their own domains.
		if !p.IsExpertOf(f.Domain) {
			fp.Errors = append(fp.Errors, Issue{Code: "not_domain_expert", Params: map[string]any{"domain": f.Domain}})
		}
		// Dictionary.
		sys, err := s.store.SystemByKeys(ctx, f.Domain, f.System)
		switch {
		case errors.Is(err, specdata.ErrNotFound):
			code := "unknown_system"
			var n int
			if e := s.pool.QueryRow(ctx, `SELECT count(*) FROM domains WHERE key = $1`, f.Domain).Scan(&n); e == nil && n == 0 {
				code = "unknown_domain"
			}
			fp.Errors = append(fp.Errors, Issue{Code: code, Params: map[string]any{"domain": f.Domain, "system": f.System}})
		case err != nil:
			return nil, err
		default:
			if _, ok := next[sys.ID]; !ok {
				n, err := s.store.PeekNumber(ctx, sys.ID)
				if err != nil {
					return nil, err
				}
				next[sys.ID] = n
			}
		}
		if parent != "" {
			fp.Parent = &parent
			pf, err := s.store.FeatureByUniqueID(ctx, parent)
			if errors.Is(err, specdata.ErrNotFound) || (err == nil && pf.Phase == domain.PhaseDeleted) {
				fp.Errors = append(fp.Errors, Issue{Code: "parent_invalid", Params: map[string]any{"parent": parent}})
			} else if err != nil {
				return nil, err
			}
		}
		if len(fp.Errors) == 0 && sys != nil {
			next[sys.ID]++
			id := domain.FeatureKey(sys.DomainKey, sys.Key, next[sys.ID])
			fp.NewID = &id
		}
		pv.Features = append(pv.Features, fp)
	}
	for _, r := range arc.Rules {
		rp := RulePreview{Area: r.Area, File: r.File, Action: "skip"}
		area, err := domain.ParseArea(r.Area)
		switch {
		case err != nil:
			rp.Reason = "unknown_area"
		case r.File != "template.md" && r.File != "fix-template.md":
			rp.Reason = "unknown_file"
		case !rules.CanChange(p, area):
			rp.Reason = "not_area_admin"
		case len(rules.TemplateViolations(area, strings.TrimSuffix(r.File, ".md"), string(r.Content))) > 0:
			rp.Reason = "rules_contract" // FTR.HMR.DEV-0003 R19: the template breaks the document contract
			rp.Violations = rules.TemplateViolations(area, strings.TrimSuffix(r.File, ".md"), string(r.Content))
		default:
			rp.Action = "propose"
		}
		pv.Rules = append(pv.Rules, rp)
	}
	admins, err := s.adminNames(ctx)
	if err != nil {
		return nil, err
	}
	pv.Admins = admins
	return pv, nil
}

func (s *Service) assetAllowed(data []byte) bool {
	for m := mimetype.Detect(data); m != nil; m = m.Parent() {
		for t := range s.allowed {
			if m.Is(t) {
				return true
			}
		}
	}
	return false
}

func (s *Service) adminNames(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT u.display_name FROM users u LEFT JOIN area_admins r ON r.user_id = u.id
		WHERE u.is_global_admin OR r.user_id IS NOT NULL ORDER BY 1 LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Service) load(ctx context.Context, id uuid.UUID) (*Job, error) {
	var j Job
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT id, user_id, s3_key, status, preview, created_at, started_at, finished_at FROM imports WHERE id = $1`, id).
		Scan(&j.ID, &j.UserID, &j.S3Key, &j.Status, &raw, &j.CreatedAt, &j.StartedAt, &j.FinishedAt)
	if postgres.IsNoRows(err) {
		return nil, apperr.NotFound("import_not_found", "import not found")
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &j.Preview); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT i.archive_id, i.status, f.unique_id, f.pr_url, i.error,
		(SELECT count(*) FROM gates g WHERE g.feature_id = f.id AND g.deleted_at IS NULL)
		FROM import_items i LEFT JOIN features f ON f.id = i.feature_id WHERE i.import_id = $1 ORDER BY i.archive_id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	j.Results = []ItemResult{}
	for rows.Next() {
		var r ItemResult
		var gates *int
		if err := rows.Scan(&r.ArchiveID, &r.Status, &r.NewID, &r.PRURL, &r.Error, &gates); err != nil {
			return nil, err
		}
		if gates != nil {
			r.Gates = *gates
		}
		j.Results = append(j.Results, r)
	}
	return &j, rows.Err()
}

// Get returns a job of its author.
func (s *Service) Get(ctx context.Context, p *domain.Principal, id uuid.UUID) (*Job, error) {
	j, err := s.load(ctx, id)
	if err != nil {
		return nil, err
	}
	if j.UserID != p.UserID {
		return nil, apperr.Forbidden("forbidden", "only the author can access the import")
	}
	return j, nil
}

func (s *Service) readArchive(ctx context.Context, key string) (*Archive, error) {
	rc, err := s.s3.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, s.cfg.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	return ParseArchive(data, s.cfg.Limits)
}

// Revalidate re-checks the stored archive (e.g. after domains were created).
func (s *Service) Revalidate(ctx context.Context, p *domain.Principal, id uuid.UUID) (*Job, error) {
	j, err := s.Get(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if j.Status != "validated" {
		return nil, apperr.Conflict("import_started", "the import was already started")
	}
	arc, err := s.readArchive(ctx, j.S3Key)
	if err != nil {
		return nil, err
	}
	pv, err := s.validate(ctx, p, arc)
	if err != nil {
		return nil, err
	}
	pv.FileName, pv.Size = j.FileName, j.Size
	raw, _ := json.Marshal(pv)
	if _, err := s.pool.Exec(ctx, `UPDATE imports SET preview = $2 WHERE id = $1`, id, raw); err != nil {
		return nil, err
	}
	return s.Get(ctx, p, id)
}

// Confirm starts the import of all features without errors.
func (s *Service) Confirm(ctx context.Context, p *domain.Principal, id uuid.UUID) (*Job, error) {
	j, err := s.Get(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if j.Status != "validated" {
		return nil, apperr.Conflict("import_started", "the import was already started")
	}
	ok := 0
	for _, f := range j.Features {
		if len(f.Errors) == 0 {
			ok++
		}
	}
	proposals := 0
	for _, r := range j.Rules {
		if r.Action == "propose" {
			proposals++
		}
	}
	if ok == 0 && proposals == 0 {
		return nil, apperr.Conflict("nothing_to_import", "there is nothing to import")
	}
	tag, err := s.pool.Exec(ctx, `UPDATE imports SET status = 'running', started_at = now() WHERE id = $1 AND status = 'validated'`, id)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return nil, apperr.Conflict("import_started", "the import was already started")
	}
	for _, f := range j.Features {
		st := "pending"
		if len(f.Errors) > 0 {
			st = "skipped"
		}
		if _, err := s.pool.Exec(ctx, `INSERT INTO import_items (import_id, archive_id, status) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`,
			id, f.Domain+"/"+f.System+"/"+f.ArchiveID, st); err != nil {
			return nil, err
		}
	}
	msg, _ := json.Marshal(map[string]string{"importId": id.String()})
	if err := s.bus.Publish(ctx, kafka.TopicImports, id.String(), msg); err != nil {
		_, _ = s.pool.Exec(ctx, `UPDATE imports SET status = 'validated', started_at = NULL WHERE id = $1`, id)
		_, _ = s.pool.Exec(ctx, `DELETE FROM import_items WHERE import_id = $1`, id)
		return nil, apperr.Unavailable("queue_unavailable", "could not start the import, try again")
	}
	return s.Get(ctx, p, id)
}

// Cancel cancels a job that was not confirmed.
func (s *Service) Cancel(ctx context.Context, p *domain.Principal, id uuid.UUID) error {
	j, err := s.Get(ctx, p, id)
	if err != nil {
		return err
	}
	if j.Status != "validated" {
		return apperr.Conflict("import_started", "the import was already started")
	}
	if _, err := s.pool.Exec(ctx, `UPDATE imports SET status = 'cancelled', finished_at = now() WHERE id = $1`, id); err != nil {
		return err
	}
	if err := s.s3.Delete(ctx, j.S3Key); err != nil {
		slog.WarnContext(ctx, "delete import archive", "key", j.S3Key, "err", err)
	}
	return nil
}

// Handle is the worker's Kafka handler for hammurapi.imports.
func (s *Service) Handle(ctx context.Context, _, value []byte) error {
	var m struct {
		ImportID uuid.UUID `json:"importId"`
	}
	if err := json.Unmarshal(value, &m); err != nil {
		slog.ErrorContext(ctx, "drop malformed import message", "err", err)
		return nil
	}
	return s.Execute(ctx, m.ImportID)
}

// Execute runs an import job. Re-delivery is safe: items already imported or
// failed are skipped.
func (s *Service) Execute(ctx context.Context, id uuid.UUID) error {
	j, err := s.load(ctx, id)
	if err != nil {
		if e, ok := apperr.As(err); ok && e.Status == 404 {
			return nil
		}
		return err
	}
	if j.Status != "running" {
		return nil
	}
	p, err := s.principal(ctx, j.UserID)
	if err != nil {
		return err
	}
	if p == nil {
		return s.finish(ctx, j, "failed")
	}
	arc, err := s.readArchive(ctx, j.S3Key)
	if err != nil {
		return fmt.Errorf("read archive: %w", err)
	}
	byKey := map[string]*ArchiveFeature{}
	for _, f := range arc.Features {
		byKey[f.Domain+"/"+f.System+"/"+f.ArchiveID] = f
	}
	previewByKey := map[string]FeaturePreview{}
	for _, f := range j.Features {
		previewByKey[f.Domain+"/"+f.System+"/"+f.ArchiveID] = f
	}
	token, tokenErr := s.tokens.Token(ctx, j.UserID)
	total := 0
	for _, r := range j.Results {
		if r.Status != "skipped" {
			total++
		}
	}
	done := 0
	imported := 0
	for _, r := range j.Results {
		switch r.Status {
		case "imported":
			done++
			imported++
			continue
		case "failed":
			done++
			continue
		case "skipped":
			continue
		}
		s.progress(ctx, j, done, total, r.ArchiveID, nil)
		var itemErr error
		if tokenErr != nil {
			itemErr = tokenErr
		} else if af := byKey[r.ArchiveID]; af == nil {
			itemErr = errors.New("feature is missing from the archive")
		} else {
			itemErr = s.importFeature(ctx, p, token, j, af, previewByKey[r.ArchiveID])
		}
		done++
		if itemErr != nil {
			msg := git.Reason(itemErr)
			if e, ok := apperr.As(itemErr); ok {
				msg = e.Message
			}
			slog.WarnContext(ctx, "feature import failed", "import", j.ID, "feature", r.ArchiveID, "err", itemErr)
			if err := s.store.SetImportItem(ctx, j.ID, r.ArchiveID, "failed", nil, &msg); err != nil {
				return err
			}
		} else {
			imported++
		}
		s.progress(ctx, j, done, total, r.ArchiveID, itemErr)
	}
	s.proposeRules(ctx, p, j, arc)
	status := "done"
	if total > 0 && imported == 0 {
		status = "failed"
	}
	return s.finish(ctx, j, status)
}

func (s *Service) finish(ctx context.Context, j *Job, status string) error {
	raw, _ := json.Marshal(j.Preview)
	if _, err := s.pool.Exec(ctx, `UPDATE imports SET status = $2, finished_at = now(), preview = $3 WHERE id = $1`, j.ID, status, raw); err != nil {
		return err
	}
	if err := s.s3.Delete(ctx, j.S3Key); err != nil {
		slog.WarnContext(ctx, "delete import archive", "key", j.S3Key, "err", err)
	}
	uid := j.UserID
	s.events.Publish(ctx, events.Event{Type: events.ImportProgress, UserID: &uid, Data: map[string]any{"importId": j.ID, "status": status}})
	s.events.Publish(ctx, events.Event{Type: events.ApprovalsChanged, Data: map[string]string{}})
	return nil
}

func (s *Service) progress(ctx context.Context, j *Job, done, total int, current string, err error) {
	uid := j.UserID
	data := map[string]any{"importId": j.ID, "status": "running", "done": done, "total": total, "current": current}
	if err != nil {
		data["error"] = err.Error()
	}
	s.events.Publish(ctx, events.Event{Type: events.ImportProgress, UserID: &uid, Data: data})
}

// importFeature creates one feature from the archive: new number, branch, a
// single commit with all files, PR/MR, and gates awaiting approval (or drafts
// in a domain without approval). On failure the branch is deleted and the
// transaction (with the number) is rolled back.
func (s *Service) importFeature(ctx context.Context, p *domain.Principal, token string, j *Job, af *ArchiveFeature, fp FeaturePreview) error {
	sys, err := s.store.SystemByKeys(ctx, af.Domain, af.System)
	if err != nil {
		return fmt.Errorf("system %s/%s: %w", af.Domain, af.System, err)
	}
	var parent *specdata.Feature
	if fp.Parent != nil {
		pf, err := s.store.FeatureByUniqueID(ctx, *fp.Parent)
		if err != nil || pf.Phase == domain.PhaseDeleted {
			return fmt.Errorf("parent %s is not a live feature", *fp.Parent)
		}
		parent = pf
	}
	var areas []domain.Area
	for _, a := range fp.Areas {
		area, err := domain.ParseArea(a)
		if err != nil {
			return err
		}
		areas = append(areas, area)
	}
	if !p.IsExpertOf(sys.DomainKey) {
		return fmt.Errorf("expert of domain %s required", sys.DomainKey)
	}
	return s.store.InTx(ctx, func(tx specdata.Store) error {
		n, err := tx.NextNumber(ctx, sys.ID)
		if err != nil {
			return err
		}
		uid := domain.FeatureKey(sys.DomainKey, sys.Key, n)
		branch := git.FeatureBranch(uid)
		base, err := s.git.BranchHead(ctx, token, s.cfg.DefaultBranch)
		if err != nil {
			return auth.MapGitError(err)
		}
		if err := s.git.CreateBranch(ctx, token, branch, base); err != nil {
			return auth.MapGitError(err)
		}
		cleanup := func(cause error) error {
			if derr := s.git.DeleteBranch(context.WithoutCancel(ctx), token, branch); derr != nil {
				slog.ErrorContext(ctx, "delete branch after failed import", "branch", branch, "err", derr)
			}
			return cause
		}
		names := make([]string, 0, len(af.Files))
		for name := range af.Files {
			names = append(names, name)
		}
		sort.Strings(names)
		changes := make([]git.FileChange, 0, len(names))
		for _, name := range names {
			area, file, _ := strings.Cut(name, "/")
			changes = append(changes, git.FileChange{Path: git.SpecDir(sys.DomainKey, sys.Key, uid, area) + "/" + file, Content: af.Files[name]})
		}
		msg := git.Trailers{Feature: uid, Import: j.ID.String()}.Message(fmt.Sprintf("%s: import %s from archive", uid, af.ArchiveID))
		sha, err := s.git.Commit(ctx, token, branch, msg, changes)
		if err != nil {
			return cleanup(auth.MapGitError(err))
		}
		pr, err := s.git.CreatePR(ctx, token, branch, s.cfg.DefaultBranch, fmt.Sprintf("%s %s", uid, fp.Title),
			fmt.Sprintf("Imported into Hammurapi from archive (was %s).", af.ArchiveID))
		if err != nil {
			return cleanup(auth.MapGitError(err))
		}
		// Imported features have no Discovery and no issues (R9).
		f := &specdata.Feature{UniqueID: uid, SystemID: sys.ID, Number: n, Title: fp.Title, Branch: branch, PRNumber: pr.Number, PRURL: pr.URL,
			Phase: domain.PhaseSpec, Imported: true, CreatedBy: p.UserID}
		if parent != nil {
			f.ParentID = &parent.ID
		}
		if err := tx.InsertFeature(ctx, f); err != nil {
			return cleanup(err)
		}
		now := time.Now()
		for _, area := range areas {
			g := &specdata.Gate{FeatureID: f.ID, Area: area, Status: domain.GateDraft, Generated: area.Generated(), HeadCommit: sha, CreatedBy: p.UserID}
			if sys.ApprovalRequired {
				g.Status, g.SubmittedAt = domain.GateInReview, &now
			}
			if err := tx.InsertGate(ctx, g); err != nil {
				return cleanup(err)
			}
			if err := tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventCreated, ActorID: &p.UserID, CommitSHA: &sha}); err != nil {
				return cleanup(err)
			}
			if sys.ApprovalRequired {
				if err := tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventSubmitted, ActorID: &p.UserID}); err != nil {
					return cleanup(err)
				}
			}
		}
		if err := tx.SetImportItem(ctx, j.ID, af.Domain+"/"+af.System+"/"+af.ArchiveID, "imported", &f.ID, nil); err != nil {
			return cleanup(err)
		}
		return nil
	})
}

func (s *Service) proposeRules(ctx context.Context, p *domain.Principal, j *Job, arc *Archive) {
	for i, rp := range j.Rules {
		if rp.Action != "propose" || rp.Result != nil {
			continue
		}
		var content []byte
		for _, r := range arc.Rules {
			if r.Area == rp.Area && r.File == rp.File {
				content = r.Content
			}
		}
		area, _ := domain.ParseArea(rp.Area)
		file := rules.FileTemplate
		if rp.File == "fix-template.md" {
			file = rules.FileFixTemplate
		}
		res := "proposed"
		c, err := s.rules.Propose(ctx, p, area, rules.ProposeInput{File: file, Content: string(content), SkipBaseCheck: true,
			Comment: "Imported from archive " + j.FileName})
		if err != nil {
			res = "failed: " + err.Error()
			if e, ok := apperr.As(err); ok {
				res = e.Code
			}
		} else {
			j.Rules[i].PRURL = &c.PRURL
		}
		j.Rules[i].Result = &res
	}
}
