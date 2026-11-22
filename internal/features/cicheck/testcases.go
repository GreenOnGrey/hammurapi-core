package cicheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GreenOnGrey/speclint/v2/contract"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/speccontract"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// TestCases is the answer of GET /api/v1/ci/services/{key}/test-cases (tech §3.1): the features of the service,
// their phases and the IDs of their test cases; no text of the specifications.
type TestCases struct {
	ContractVersion string        `json:"contractVersion"`
	Service         string        `json:"service"`
	Features        []CaseFeature `json:"features"`
	Cases           []Case        `json:"cases"`
}

// CaseFeature is a feature of the service; its automated test cases must be in the code when required.
type CaseFeature struct {
	Key      string `json:"key"`
	Phase    string `json:"phase"`
	Required bool   `json:"required"`
}

// Case is a test case of a feature.
type Case struct {
	ID      string `json:"id"`
	Feature string `json:"feature"`
	Level   string `json:"level"`
	// Services: the services the case is marked with, empty — every service of the feature (FTR.HMR.SPEC-0005 R1).
	Services []string `json:"services"`
}

// required tells whether the automated test cases of a feature must be in the checked revision (tech §3.1):
// the code is already in the default branch of the service — its PR is merged or the feature is released — or
// this run checks the pull request of the feature itself. Nothing is merged to the default branch during
// development (FTR.HMR.CMN-0002 §4), so the phase alone does not make a feature required.
func required(phase domain.FeaturePhase, merged, atHead bool) bool {
	switch phase {
	case domain.PhaseReleased:
		return true
	case domain.PhaseCodegen, domain.PhaseValidation, domain.PhaseInRelease:
		return merged || atHead
	}
	return false
}

// clip keeps a query parameter within reason: the branch and the commit of a pull request are short.
func clip(v string) string {
	if v = strings.TrimSpace(v); len(v) > 255 {
		return v[:255]
	}
	return v
}

// testCases reads the features of the service by feature_services and their test cases: the projection of the
// qa document of the feature, from its branch while in work and from the default branch after the merge. head
// and sha name the pull request under check, empty outside a pull request.
func (s *Service) testCases(ctx context.Context, serviceID uuid.UUID, key, head, sha string) (*TestCases, error) {
	out := &TestCases{ContractVersion: contract.Version, Service: key, Features: []CaseFeature{}, Cases: []Case{}}
	rows, err := s.pool.Query(ctx, `SELECT f.unique_id, f.phase::text,
		EXISTS (SELECT 1 FROM pull_requests p WHERE p.feature_id = f.id AND p.service_id = $1
			AND p.kind = 'service' AND p.state = 'merged') AS merged,
		EXISTS (SELECT 1 FROM pull_requests p WHERE p.feature_id = f.id AND p.service_id = $1
			AND p.kind = 'service' AND p.state <> 'closed'
			AND (($2 <> '' AND p.head_sha = $2) OR ($3 <> '' AND p.branch = $3))) AS at_head
		FROM feature_services fs JOIN features f ON f.id = fs.feature_id
		WHERE fs.service_id = $1 AND f.phase <> 'deleted' AND f.deleted_at IS NULL ORDER BY f.unique_id`, serviceID, sha, head)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f CaseFeature
		var merged, atHead bool
		if err := rows.Scan(&f.Key, &f.Phase, &merged, &atHead); err != nil {
			rows.Close()
			return nil, err
		}
		f.Required = required(domain.FeaturePhase(f.Phase), merged, atHead)
		out.Features = append(out.Features, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// FTR.HMR.SPEC-0005 R3: a case marked with other services is not a case of this one (A1: the filter is here,
	// so speclint of any 2.x version in the CI of a service gets only its cases).
	rows, err = s.pool.Query(ctx, `SELECT t.tc_id, f.unique_id, t.level, t.services FROM feature_services fs JOIN features f ON f.id = fs.feature_id
		JOIN test_cases t ON t.feature_id = f.id
		WHERE fs.service_id = $1 AND f.phase <> 'deleted' AND f.deleted_at IS NULL ORDER BY f.unique_id, t.tc_id`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	filtered := 0
	for rows.Next() {
		var c Case
		if err := rows.Scan(&c.ID, &c.Feature, &c.Level, &c.Services); err != nil {
			return nil, err
		}
		if len(c.Services) > 0 && !slices.Contains(c.Services, key) {
			filtered++
			continue
		}
		out.Cases = append(out.Cases, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	metrics.CITestCasesFiltered.WithLabelValues(key).Add(float64(filtered))
	return out, nil
}

// limiter allows 60 requests a minute per token (arch §3.5), and as many failed attempts per address.
type limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	seen   map[string]*bucket
	now    func() time.Time
}

type bucket struct {
	start time.Time
	n     int
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{limit: limit, window: window, seen: map[string]*bucket{}, now: time.Now}
}

// peek tells whether a request of id would pass, without counting it.
func (l *limiter) peek(id string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.seen[id]
	if b == nil || l.now().Sub(b.start) >= l.window || b.n < l.limit {
		return true, 0
	}
	return false, b.start.Add(l.window).Sub(l.now())
}

// allow counts a request; when the limit is reached it returns the time until the window ends.
func (l *limiter) allow(id string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if len(l.seen) > 10000 { // tokens of services are few; forget the old windows
		for k, b := range l.seen {
			if now.Sub(b.start) >= l.window {
				delete(l.seen, k)
			}
		}
	}
	b := l.seen[id]
	if b == nil || now.Sub(b.start) >= l.window {
		l.seen[id] = &bucket{start: now, n: 1}
		return true, 0
	}
	if b.n >= l.limit {
		return false, b.start.Add(l.window).Sub(now)
	}
	b.n++
	return true, 0
}

// rawBearer is the token of Authorization: Bearer, whatever it is.
func rawBearer(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// bearer is the installation token or a JWT; "" for anything else.
func bearer(r *http.Request) string {
	if t := rawBearer(r); strings.HasPrefix(t, tokenPrefix) || strings.Count(t, ".") == 2 {
		return t
	}
	return ""
}

func (s *Service) service(ctx context.Context, q postgres.Querier, key string) (*cycledata.Service, error) {
	svc, err := cycledata.New(q).ServiceByKey(ctx, key)
	if errors.Is(err, cycledata.ErrNotFound) {
		return nil, apperr.NotFound("service_not_found", "service not found")
	}
	return svc, err
}

// CIRoutes mounts the API of CI (FTR.HMR.SPEC-0002 arch A4, FTR.HMR.SPEC-0003 arch §3.1): the OIDC token of a CI
// job opens the services of its repository, the installation token every service; a session of a user does not
// open it. It goes outside the session middleware.
func (s *Service) CIRoutes(r chi.Router) {
	r.Get("/ci/services/{service}/test-cases", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		res, auth, err := s.serveTestCases(w, r)
		metrics.CITestCaseRequests.WithLabelValues(res, auth).Inc()
		return err
	}))
	r.Get("/ci/services", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		res, auth, err := s.serveServices(w, r)
		metrics.CIServicesRequests.WithLabelValues(res, auth).Inc()
		return err
	}))
}

// repoService is a service of the catalog with its repository.
type repoService struct {
	id      uuid.UUID
	key     string
	deleted bool
}

// repoServices are the services of a repository, the deleted ones included.
func (s *Service) repoServices(ctx context.Context, repo string) ([]repoService, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, key, deleted_in_catalog FROM services WHERE lower(repo) = lower($1) ORDER BY key`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []repoService
	for rows.Next() {
		var r repoService
		if err := rows.Scan(&r.id, &r.key, &r.deleted); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// clientIP is the address of the client: the first of X-Forwarded-For behind the ingress, else the remote address.
func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		first, _, _ := strings.Cut(f, ",")
		if ip := strings.TrimSpace(first); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// caller is who presents a token of CI: the installation, or the repository of an OIDC token.
type caller struct {
	installation bool
	repository   string
}

// authCI checks the token of a request of CI (FTR.HMR.SPEC-0003 tech §2): the installation token or the OIDC token
// of a job; result and auth are the labels of the metric.
func (s *Service) authCI(w http.ResponseWriter, r *http.Request) (c *caller, result, auth string, err error) {
	ctx := r.Context()
	auth = "none"
	limited := func(wait time.Duration, msg string) (*caller, string, string, error) {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		return nil, "rate_limited", auth, apperr.New(http.StatusTooManyRequests, "rate_limited", msg)
	}
	// failed attempts are limited by the address: a guess of tokens costs a query or a signature each
	failed := func(msg string) (*caller, string, string, error) {
		if ok, wait := s.failures.allow(clientIP(r)); !ok {
			return limited(wait, "too many requests with tokens that are not accepted")
		}
		return nil, "unauthorized", auth, apperr.Unauthorized("token_invalid", msg)
	}
	if ok, _ := s.failures.peek(clientIP(r)); !ok {
		return failed("too many requests with tokens that are not accepted")
	}
	if t := rawBearer(r); strings.HasPrefix(t, "hmr_svc_") { // a file of 1.1.0: no guess, no count against the address
		return nil, "unauthorized", auth, apperr.Unauthorized("token_invalid",
			"tokens of services are gone: the job presents the OIDC token of its CI; replace the file of the check (FTR.HMR.SPEC-0003)")
	}
	token := bearer(r)
	switch {
	case token == "":
		return failed("no token: the OIDC token of the CI job or the installation token")
	case strings.HasPrefix(token, tokenPrefix):
		auth = "installation"
		o, err := lookupToken(ctx, s.pool, token)
		switch {
		case err != nil:
			return nil, "error", auth, err
		case o == nil:
			return failed("unknown installation token")
		case o.revoked:
			return nil, "revoked", auth, apperr.Unauthorized("token_revoked", "the installation token is revoked")
		}
		if ok, wait := s.limits.allow("installation:" + o.id.String()); !ok {
			return limited(wait, "at most 60 requests a minute per token")
		}
		if o.lastUsedAt == nil || time.Since(*o.lastUsedAt) > time.Minute {
			_, _ = s.pool.Exec(context.WithoutCancel(ctx), `UPDATE ci_tokens SET last_used_at = now() WHERE id = $1`, o.id)
		}
		return &caller{installation: true}, "", auth, nil
	default:
		auth = "oidc"
		if s.OIDC == nil {
			return failed("OIDC tokens of CI are not configured")
		}
		id, err := s.OIDC.Verify(ctx, token)
		if err != nil {
			if errors.Is(err, errToken) {
				return failed(err.Error())
			}
			return nil, "error", auth, err // the keys of the issuer are not reachable: not the fault of the token
		}
		auth = "oidc_" + id.Provider
		if ok, wait := s.limits.allow("repo:" + strings.ToLower(id.Repository)); !ok {
			return limited(wait, "at most 60 requests a minute per repository")
		}
		return &caller{repository: id.Repository}, "", auth, nil
	}
}

func (s *Service) serveTestCases(w http.ResponseWriter, r *http.Request) (result, auth string, err error) {
	ctx := r.Context()
	c, result, auth, err := s.authCI(w, r)
	if c == nil {
		return result, auth, err
	}
	key := chi.URLParam(r, "service")
	var svcID uuid.UUID
	if c.installation {
		svc, err := s.service(ctx, s.pool, key)
		var ae *apperr.Error
		switch {
		case errors.As(err, &ae):
			return "not_found", auth, err
		case err != nil:
			return "error", auth, err
		case svc.DeletedInCatalog:
			return "not_found", auth, apperr.NotFound("service_not_found", "the service is deleted in the catalog")
		}
		svcID = svc.ID
	} else {
		services, err := s.repoServices(ctx, c.repository)
		if err != nil {
			return "error", auth, err
		}
		if len(services) == 0 {
			slog.InfoContext(ctx, "ci oidc refused", "repository", c.repository, "reason", "repo_not_in_catalog")
			return "forbidden", auth, apperr.Forbidden("repo_not_in_catalog", fmt.Sprintf("repository %s is not the repository of any service in Hammurapi", c.repository)).With("repository", c.repository)
		}
		i := slices.IndexFunc(services, func(rs repoService) bool { return rs.key == key })
		switch {
		case i < 0: // the scope before the catalog: a token does not tell which other services exist
			slog.InfoContext(ctx, "ci oidc refused", "repository", c.repository, "service", key, "reason", "token_scope")
			return "forbidden", auth, apperr.Forbidden("token_scope", fmt.Sprintf("the token is of repository %s, service %s is not in it", c.repository, key)).With("repository", c.repository)
		case services[i].deleted:
			return "not_found", auth, apperr.NotFound("service_not_found", "the service is deleted in the catalog")
		}
		svcID = services[i].id
	}
	out, err := s.testCases(ctx, svcID, key, clip(r.URL.Query().Get("head")), clip(r.URL.Query().Get("sha")))
	if err != nil {
		return "error", auth, err
	}
	httpx.JSON(w, http.StatusOK, out)
	return "ok", auth, nil
}

// serveServices answers the keys of the catalog to the CI of the repository of specifications (FTR.HMR.SPEC-0004
// tech §3.2): to the installation token, or to the OIDC token of that repository.
func (s *Service) serveServices(w http.ResponseWriter, r *http.Request) (result, auth string, err error) {
	c, result, auth, err := s.authCI(w, r)
	if c == nil {
		return result, auth, err
	}
	if !c.installation && !strings.EqualFold(c.repository, s.git.Repo()) {
		slog.InfoContext(r.Context(), "ci oidc refused", "repository", c.repository, "reason", "token_scope")
		return "forbidden", auth, apperr.Forbidden("token_scope", fmt.Sprintf("the token is of repository %s, not of the repository of specifications", c.repository)).With("repository", c.repository)
	}
	keys, err := speccontract.ServiceKeys(r.Context(), s.pool)
	if err != nil {
		return "error", auth, err
	}
	httpx.JSON(w, http.StatusOK, map[string][]string{"services": keys})
	return "ok", auth, nil
}
