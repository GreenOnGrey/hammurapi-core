package rules

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/GreenOnGrey/speclint/v2/contract"
	"github.com/GreenOnGrey/speclint/v2/starter"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// The document contract of the rules (FTR.HMR.DEV-0003 R17–R20, R24): a
// template keeps the elements Hammurapi reads in every document.

// Violation is an element of the contract a template breaks.
type Violation struct {
	Element string `json:"element"`
	Message string `json:"message"`
}

// FileViolations are the violations of a template of the default branch.
type FileViolations struct {
	Area       domain.Area `json:"area"`
	File       string      `json:"file"`
	SHA        string      `json:"sha"`
	Violations []Violation `json:"violations"`
}

func violations(area domain.Area, file, content string) []Violation {
	out := []Violation{}
	for _, v := range contract.CheckTemplate(contract.Area(area), file, content) {
		out = append(out, Violation{Element: v.Element, Message: v.Message})
	}
	return out
}

// TemplateViolations are the violations of the contract in a template (R19).
func TemplateViolations(area domain.Area, file, content string) []Violation {
	return violations(area, file, content)
}

// CheckContent refuses a template that breaks the contract with 422
// rules_contract (R19, tech §2.6) and counts the refusal by stage.
func CheckContent(stage string, area domain.Area, file, content string) error {
	v := violations(area, file, content)
	if len(v) == 0 {
		return nil
	}
	metrics.RulesContractRejections.WithLabelValues(stage).Inc()
	return apperr.Unprocessable("rules_contract", "the template breaks the document contract").
		With("file", file).With("violations", v)
}

// Describe is GET /rules/{area}/contract: the elements of both files of the area
// in the language of the documents, and the languages read too (R18).
func (s *Service) Describe(p *domain.Principal, area domain.Area) (map[string]any, error) {
	if !CanView(p, area) {
		return nil, apperr.NoRole("admin", string(area))
	}
	also := []string{}
	for _, l := range contract.Langs {
		if l != s.lang() {
			also = append(also, string(l))
		}
	}
	elems := func(file string) []contract.Element {
		out := contract.Describe(contract.Area(area), file, s.lang())
		if out == nil {
			out = []contract.Element{}
		}
		return out
	}
	return map[string]any{"lang": s.lang(), "alsoRead": also, "template": elems(FileTemplate), "fixTemplate": elems(FileFixTemplate)}, nil
}

func (s *Service) lang() contract.Lang { return contract.ParseLang(string(s.Lang)) }

// Start runs at the start of the worker (R20, R24): an empty repository — no
// template at all — gets the starter rules in the language of the documents,
// under a lock so that replicas do not seed twice; then every template is
// checked. Only here does the bot commit to the default branch past a PR: a
// push that deletes rules/ later is not undone.
func (s *Service) Start(ctx context.Context) error {
	token, err := s.git.BotToken(ctx)
	if err != nil {
		return err
	}
	all, err := s.readAll(ctx, token, nil)
	if err != nil {
		return err
	}
	empty := true
	for _, e := range all {
		empty = empty && e.f == nil
	}
	if empty {
		return postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
			var locked bool
			if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext('hammurapi.rules_seed'))`).Scan(&locked); err != nil || !locked {
				return err // another replica seeds
			}
			return s.seed(ctx, token) // the starter rules keep the contract (a test of speclint): nothing to record
		})
	}
	return s.record(ctx, all)
}

// RulesPushed checks the templates a push to the default branch touched (R20);
// a path "rules/" checks all of them (the provider truncated the list of files).
func (s *Service) RulesPushed(ctx context.Context, branch string, paths []string) error {
	if branch != s.defaultBranch {
		return nil
	}
	touched := map[string]bool{}
	for _, p := range paths {
		if strings.HasPrefix(p, "rules/") {
			touched[p] = true
		}
	}
	if len(touched) == 0 {
		return nil
	}
	if touched["rules/"] {
		touched = nil
	}
	token, err := s.git.BotToken(ctx)
	if err != nil {
		return err
	}
	all, err := s.readAll(ctx, token, touched)
	if err != nil {
		return err
	}
	return s.record(ctx, all)
}

// ruleFile is a template of the default branch, f nil when it is missing.
type ruleFile struct {
	area domain.Area
	file string
	f    *git.File
}

// readAll reads the templates of every area; only the paths of only when it is set.
func (s *Service) readAll(ctx context.Context, token string, only map[string]bool) ([]ruleFile, error) {
	var out []ruleFile
	for _, a := range domain.Areas {
		for _, file := range []string{FileTemplate, FileFixTemplate} {
			path := git.RulePath(string(a), file == FileFixTemplate)
			if only != nil && !only[path] {
				continue
			}
			f, err := s.git.GetFile(ctx, token, s.defaultBranch, path)
			if errors.Is(err, git.ErrNotFound) {
				f, err = nil, nil
			}
			if err != nil {
				return nil, err
			}
			out = append(out, ruleFile{a, file, f})
		}
	}
	return out, nil
}

// record keeps a row of rules_violations per template that breaks the contract
// or is missing, and removes the rows of the templates that keep it.
func (s *Service) record(ctx context.Context, files []ruleFile) error {
	for _, e := range files {
		v, sha := []Violation{{Element: "file", Message: "the template is missing"}}, ""
		if e.f != nil {
			v, sha = violations(e.area, e.file, string(e.f.Content)), git.BlobSHA(e.f.Content)
		}
		if len(v) == 0 {
			if _, err := s.pool.Exec(ctx, `DELETE FROM rules_violations WHERE area = $1 AND file = $2`, e.area, e.file); err != nil {
				return err
			}
			continue
		}
		b, _ := json.Marshal(v)
		if _, err := s.pool.Exec(ctx, `INSERT INTO rules_violations (area, file, sha, violations, checked_at) VALUES ($1, $2, $3, $4, now())
			ON CONFLICT (area, file) DO UPDATE SET sha = EXCLUDED.sha, violations = EXCLUDED.violations, checked_at = now()`,
			e.area, e.file, sha, b); err != nil {
			return err
		}
		slog.WarnContext(ctx, "rules template breaks the document contract", "area", e.area, "file", e.file, "violations", len(v))
	}
	return nil
}

// seed commits the starter rules into a repository without rules/ (R24): the
// only commit of the bot to the default branch past a PR.
func (s *Service) seed(ctx context.Context, token string) error {
	files := starter.Files(s.lang())
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	changes := make([]git.FileChange, 0, len(paths))
	for _, p := range paths {
		changes = append(changes, git.FileChange{Path: p, Content: files[p]})
	}
	msg := git.Trailers{Generated: true}.Message("Add the starter rules (FTR.HMR.DEV-0003)")
	if _, err := s.git.Commit(ctx, token, s.defaultBranch, msg, changes); err != nil {
		return err
	}
	slog.InfoContext(ctx, "starter rules added to the specification repository", "lang", s.lang(), "files", len(changes))
	return nil
}

// Violations lists the templates of the default branch that break the contract,
// in the areas the user administers; a global administrator sees all (R20).
func (s *Service) Violations(ctx context.Context, p *domain.Principal) ([]FileViolations, error) {
	rows, err := s.pool.Query(ctx, `SELECT area, file, sha, violations FROM rules_violations ORDER BY area, file`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FileViolations{}
	for rows.Next() {
		var fv FileViolations
		var b []byte
		if err := rows.Scan(&fv.Area, &fv.File, &fv.SHA, &b); err != nil {
			return nil, err
		}
		if !CanView(p, fv.Area) {
			continue
		}
		_ = json.Unmarshal(b, &fv.Violations)
		out = append(out, fv)
	}
	return out, rows.Err()
}

// contractRoutes are GET /rules/violations and GET /rules/{area}/contract.
func (s *Service) contractRoutes(r chi.Router) {
	r.Get("/rules/violations", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		v, err := s.Violations(r.Context(), p)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusOK, map[string]any{"items": v})
		return nil
	}))
	r.Get("/rules/{area}/contract", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		a, err := httpx.ParamArea(r)
		if err != nil {
			return err
		}
		d, err := s.Describe(p, a)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusOK, d)
		return nil
	}))
}
