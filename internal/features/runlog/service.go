package runlog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Events reads the events of a run of Nabu (nabu.Client.StreamEvents).
type Events interface {
	StreamEvents(ctx context.Context, runID string, after int, onEvent func(nabu.RunEvent), onReconnect func()) error
}

// Service serves GET /api/v1/features/{uniqueId}/runs/{runId}/log.
type Service struct {
	Pool   *pgxpool.Pool
	Nabu   Events
	Config Config
	// Flush is how often the text of an open message goes out (live runs).
	Flush time.Duration
	// StepsWait is how long the end of a code task waits for the steps of Hammurapi (arch §4.3).
	StepsWait time.Duration
	now       func() time.Time

	mu     sync.Mutex
	open   int
	byUser map[uuid.UUID]int
}

// Config are the limits of the streams (tech §«Конфигурация»).
type Config struct {
	PerUser   int
	Max       int
	Retention time.Duration
}

func (s *Service) defaults() {
	if s.Config.PerUser <= 0 {
		s.Config.PerUser = 3
	}
	if s.Config.Max <= 0 {
		s.Config.Max = 100
	}
	if s.Config.Retention <= 0 {
		s.Config.Retention = 30 * 24 * time.Hour
	}
	if s.Flush <= 0 {
		s.Flush = 500 * time.Millisecond
	}
	if s.StepsWait <= 0 {
		s.StepsWait = 60 * time.Second
	}
	if s.now == nil {
		s.now = time.Now
	}
}

// acquire takes a place for a stream of the user; false — over a limit.
func (s *Service) acquire(user uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byUser == nil {
		s.byUser = map[uuid.UUID]int{}
	}
	if s.open >= s.Config.Max || s.byUser[user] >= s.Config.PerUser {
		return false
	}
	s.open++
	s.byUser[user]++
	metrics.RunLogStreams.Set(float64(s.open))
	return true
}

func (s *Service) release(user uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open--
	if s.byUser[user]--; s.byUser[user] <= 0 {
		delete(s.byUser, user)
	}
	metrics.RunLogStreams.Set(float64(s.open))
}

// run is a run of a feature: of a code task or of a row of its usage.
type run struct {
	task     *uuid.UUID
	started  time.Time
	finished bool
}

// findRun checks that runID is a run of the feature (R10: a run of another one is not found).
func (s *Service) findRun(ctx context.Context, featureID, runID uuid.UUID) (*run, error) {
	r := &run{}
	var task uuid.UUID
	var status string
	err := s.Pool.QueryRow(ctx, `SELECT id, COALESCE(started_at, created_at), status::text FROM agent_tasks
		WHERE feature_id = $1 AND nabu_run_id = $2 ORDER BY created_at DESC LIMIT 1`, featureID, runID).Scan(&task, &r.started, &status)
	if err == nil {
		r.task, r.finished = &task, status != "queued" && status != "running"
		return r, nil
	}
	if !postgres.IsNoRows(err) {
		return nil, err
	}
	err = s.Pool.QueryRow(ctx, `SELECT created_at FROM agent_usage WHERE feature_id = $1 AND nabu_run_id = $2 ORDER BY created_at LIMIT 1`,
		featureID, runID).Scan(&r.started)
	if postgres.IsNoRows(err) {
		return nil, apperr.NotFound("run_not_found", "the run is not a run of this feature")
	}
	r.finished = true
	return r, err
}

// steps reads the steps of Hammurapi of a code task once it is not running.
func (s *Service) steps(ctx context.Context, task uuid.UUID) ([]Entry, error) {
	deadline := s.now().Add(s.StepsWait)
	for {
		var status string
		var raw []byte
		if err := s.Pool.QueryRow(ctx, `SELECT status::text, result FROM agent_tasks WHERE id = $1`, task).Scan(&status, &raw); err != nil {
			return nil, err
		}
		if (status != "running" && status != "queued") || !s.now().Before(deadline) {
			var res struct {
				Steps []struct {
					Step   string    `json:"step"`
					Status string    `json:"status"`
					Detail string    `json:"detail"`
					At     time.Time `json:"at"`
					Retry  bool      `json:"retry"`
				} `json:"steps"`
			}
			_ = json.Unmarshal(raw, &res)
			out := make([]Entry, 0, len(res.Steps))
			for i, st := range res.Steps {
				out = append(out, Entry{ID: "s" + strconv.Itoa(i+1), Kind: KindStep, At: st.At.UTC().Format(time.RFC3339),
					Step: st.Step, Status: st.Status, Detail: mask(st.Detail), Retry: st.Retry})
			}
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Routes mounts the log under /api/v1.
func (s *Service) Routes(r chi.Router) {
	s.defaults()
	r.Get("/features/{uniqueId}/runs/{runId}/log", func(w http.ResponseWriter, r *http.Request) {
		if err := s.serve(w, r); err != nil {
			httpx.Error(w, r, err)
		}
	})
}

func (s *Service) serve(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	runID, err := uuid.Parse(chi.URLParam(r, "runId"))
	if err != nil {
		return apperr.NotFound("run_not_found", "the run is not a run of this feature")
	}
	var featureID uuid.UUID
	var domainKey string
	err = s.Pool.QueryRow(ctx, `SELECT f.id, d.key FROM features f JOIN systems sy ON sy.id = f.system_id
		JOIN domains d ON d.id = sy.domain_id WHERE f.unique_id = $1 AND f.deleted_at IS NULL`, chi.URLParam(r, "uniqueId")).Scan(&featureID, &domainKey)
	if postgres.IsNoRows(err) {
		return apperr.NotFound("feature_not_found", "feature not found")
	}
	if err != nil {
		return err
	}
	if !CanView(p, domainKey) { // R10
		metrics.RunLogErrors.WithLabelValues("forbidden").Inc()
		return apperr.Forbidden("forbidden", "the log of a run is for the experts of the domain and the administrators")
	}
	rn, err := s.findRun(ctx, featureID, runID)
	if err != nil {
		return err
	}
	if !s.acquire(p.UserID) {
		return apperr.New(http.StatusTooManyRequests, "too_many_streams", "too many open logs")
	}
	defer s.release(p.UserID)

	after := 0
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		head, _, _ := strings.Cut(v, ".")
		after, _ = strconv.Atoi(head)
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming is not supported")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var wmu sync.Mutex
	write := func(event, id string, v any) {
		b, _ := json.Marshal(v)
		wmu.Lock()
		defer wmu.Unlock()
		if id != "" {
			fmt.Fprintf(w, "id: %s\n", id)
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		fl.Flush()
	}
	send := func(outs []Out) {
		for _, o := range outs {
			if o.Event == "delta" {
				write("delta", o.ID, map[string]string{"id": o.Entry.ID, "text": o.Entry.Text})
				continue
			}
			write("entry", o.ID, o.Entry)
		}
	}

	// R10: a run older than the events of Nabu has an empty log
	expired := func() {
		metrics.RunLogErrors.WithLabelValues("expired").Inc()
		write("entry", "", Entry{ID: "expired", Kind: KindExpired, Data: json.RawMessage(fmt.Sprintf(`{"days":%d}`, int(s.Config.Retention.Hours()/24)))})
	}
	if rn.finished && s.now().Sub(rn.started) > s.Config.Retention {
		expired()
		return nil
	}

	a := &assembler{live: !rn.finished, now: s.now, lastSeq: after}
	events := make(chan nabu.RunEvent, 64)
	reconnects := make(chan struct{}, 1)
	errc := make(chan error, 1)
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		errc <- s.Nabu.StreamEvents(sctx, runID.String(), after, func(e nabu.RunEvent) {
			select {
			case events <- e:
			case <-sctx.Done():
			}
		}, func() {
			select {
			case reconnects <- struct{}{}:
			default:
			}
		})
	}()
	tick := time.NewTicker(s.Flush)
	defer tick.Stop()
	got := 0
	for done := false; !done; {
		select {
		case <-ctx.Done():
			return nil
		case e := <-events:
			got++
			send(a.add(e))
			done = e.Type == "completed"
		case <-reconnects:
			write("reconnect", "", map[string]any{})
		case <-tick.C:
			send(a.flush())
		case err := <-errc:
			// drain what came before the end of the stream
			for len(events) > 0 {
				e := <-events
				got++
				send(a.add(e))
			}
			send(a.close())
			// a reader that resumes after the end of a finished run gets nothing, not «expired»
			if err != nil && nabu.IsNotFound(err) || err == nil && got == 0 && rn.finished && after == 0 {
				expired()
				return nil
			}
			if err != nil {
				metrics.RunLogErrors.WithLabelValues("nabu_unreachable").Inc()
				write("error", "", map[string]string{"code": "nabu_unavailable"})
				return nil
			}
			done = true
		}
	}
	send(a.close())
	if rn.task != nil { // R8: the steps of Hammurapi after the agent
		steps, err := s.steps(ctx, *rn.task)
		if err != nil {
			return nil
		}
		for i, st := range steps {
			write("entry", fmt.Sprintf("%d.%d", a.lastSeq, i+1), st)
		}
	}
	return nil
}

// CanView tells who sees the log of a run: the experts of the domain of the feature and the administrators (R10).
func CanView(p *domain.Principal, domainKey string) bool {
	return p != nil && (p.GlobalAdmin || p.IsExpertOf(domainKey))
}
