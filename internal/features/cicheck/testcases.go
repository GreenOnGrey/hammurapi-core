package cicheck

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GreenOnGrey/speclint/contract"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
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
}

// requiredPhases need the tests in the code: code generation and later; a rolled back feature does not (tech §5, 5).
var requiredPhases = map[string]bool{"codegen": true, "validation": true, "in_release": true, "released": true}

// testCases reads the features of the service by feature_services and their test cases: the projection of the
// qa document of the feature, from its branch while in work and from the default branch after the merge.
func (s *Service) testCases(ctx context.Context, serviceID uuid.UUID, key string) (*TestCases, error) {
	out := &TestCases{ContractVersion: contract.Version, Service: key, Features: []CaseFeature{}, Cases: []Case{}}
	rows, err := s.pool.Query(ctx, `SELECT f.unique_id, f.phase::text FROM feature_services fs JOIN features f ON f.id = fs.feature_id
		WHERE fs.service_id = $1 AND f.phase <> 'deleted' AND f.deleted_at IS NULL ORDER BY f.unique_id`, serviceID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f CaseFeature
		if err := rows.Scan(&f.Key, &f.Phase); err != nil {
			rows.Close()
			return nil, err
		}
		f.Required = requiredPhases[f.Phase]
		out.Features = append(out.Features, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT t.tc_id, f.unique_id, t.level FROM feature_services fs JOIN features f ON f.id = fs.feature_id
		JOIN test_cases t ON t.feature_id = f.id
		WHERE fs.service_id = $1 AND f.phase <> 'deleted' AND f.deleted_at IS NULL ORDER BY f.unique_id, t.tc_id`, serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c Case
		if err := rows.Scan(&c.ID, &c.Feature, &c.Level); err != nil {
			return nil, err
		}
		out.Cases = append(out.Cases, c)
	}
	return out, rows.Err()
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

// clientIP is the address of the client: the remote address, which the ingress sets.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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

// bearer is the token of Authorization: Bearer hmr_svc_…, or "".
func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, tokenPrefix) {
		return ""
	}
	return token
}

// CIRoutes mounts the API of CI (arch A4): only the token of a service opens it, and only for its service; a
// session of a user does not. It goes outside the session middleware.
func (s *Service) CIRoutes(r chi.Router) {
	r.Get("/ci/services/{service}/test-cases", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		res, err := s.serveTestCases(w, r)
		metrics.CITestCaseRequests.WithLabelValues(res).Inc()
		return err
	}))
}

func (s *Service) serveTestCases(w http.ResponseWriter, r *http.Request) (string, error) {
	ctx := r.Context()
	// failed attempts are limited by the address: a guess of tokens costs a query each
	failed := func() (string, error) {
		if ok, wait := s.failures.allow(clientIP(r)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			return "rate_limited", apperr.New(http.StatusTooManyRequests, "rate_limited", "too many requests with unknown tokens")
		}
		return "unauthorized", apperr.Unauthorized("token_invalid", "unknown token")
	}
	token := bearer(r)
	if token == "" {
		return failed()
	}
	if ok, _ := s.failures.peek(clientIP(r)); !ok {
		return failed()
	}
	o, err := lookupToken(ctx, s.pool, token)
	if err != nil {
		return "error", err
	}
	if o == nil {
		return failed()
	}
	if o.revoked {
		return "revoked", apperr.Unauthorized("token_revoked", "the token is revoked")
	}
	if ok, wait := s.limits.allow(o.id.String()); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		return "rate_limited", apperr.New(http.StatusTooManyRequests, "rate_limited", "at most 60 requests a minute per token")
	}
	// the scope before the catalog: a token does not tell which other services exist
	if chi.URLParam(r, "service") != o.serviceKey {
		return "forbidden", apperr.Forbidden("token_scope", "the token is of another service")
	}
	svc, err := s.service(ctx, s.pool, o.serviceKey)
	var ae *apperr.Error
	switch {
	case errors.As(err, &ae):
		return "not_found", err
	case err != nil:
		return "error", err
	case svc.DeletedInCatalog:
		return "not_found", apperr.NotFound("service_not_found", "the service is deleted in the catalog")
	}
	if o.lastUsedAt == nil || time.Since(*o.lastUsedAt) > time.Minute {
		_, _ = s.pool.Exec(context.WithoutCancel(ctx), `UPDATE service_tokens SET last_used_at = now() WHERE id = $1`, o.id)
	}
	out, err := s.testCases(ctx, svc.ID, svc.Key)
	if err != nil {
		return "error", err
	}
	httpx.JSON(w, http.StatusOK, out)
	return "ok", nil
}
