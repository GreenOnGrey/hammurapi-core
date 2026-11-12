//go:build integration

package itest

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/runlog"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// fakeEvents is the stream of events of runs of Nabu.
type fakeEvents struct {
	mu     sync.Mutex
	runs   map[string][]nabu.RunEvent
	hold   map[string]chan struct{} // a run that is still going: the stream waits
	broken map[string]bool          // the stream breaks once before the events
}

func (f *fakeEvents) StreamEvents(ctx context.Context, runID string, after int, onEvent func(nabu.RunEvent), onReconnect func()) error {
	f.mu.Lock()
	evs, ok := f.runs[runID]
	hold := f.hold[runID]
	broken := f.broken[runID]
	delete(f.broken, runID)
	f.mu.Unlock()
	if !ok {
		return &nabu.APIError{Status: 404, Code: "run_not_found"}
	}
	if broken {
		onReconnect()
	}
	for _, e := range evs {
		if e.Seq > after {
			onEvent(e)
		}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
		}
	}
	return ctx.Err()
}

type sseEvent struct {
	ID, Event string
	Data      json.RawMessage
}

// FTR.HMR.DEV-0004 qa LOG-03, LOG-05…LOG-12, LOG-22: GET /api/v1/features/{id}/runs/{runId}/log.
func TestRunLog(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "runlog")
	must(t, err)
	defer drop()
	db, err := postgres.Connect(ctx, url)
	must(t, err)
	defer db.Close()
	must(t, postgres.Migrate(ctx, url))

	var user, feature, other, svc uuid.UUID
	must(t, db.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, agent_name, agent_tone)
		VALUES ('rl1', 'ira', 'Ira', 'Hammurapi', 'business') RETURNING id`).Scan(&user))
	must(t, db.QueryRow(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('FMS', 'Fleet') RETURNING id)
		INSERT INTO systems (domain_id, key, name) SELECT id, 'CAR', 'Cars' FROM d RETURNING id`).Scan(&svc))
	newFeature := func(n int) uuid.UUID {
		var id uuid.UUID
		must(t, db.QueryRow(ctx, `INSERT INTO features (unique_id, system_id, number, title, branch_name, pr_number, pr_url, phase, created_by)
			VALUES ($1, $2, $3, 'F', 'b', 1, 'http://pr', 'codegen', $4) RETURNING id`, "FTR.FMS.CAR-000"+string(rune('0'+n)), svc, n, user).Scan(&id))
		return id
	}
	feature, other = newFeature(5), newFeature(6)
	must(t, db.QueryRow(ctx, `INSERT INTO services (key, name, repo, source) VALUES ('booking', 'booking', 'fms/booking', 'manual') RETURNING id`).Scan(&svc))
	task := func(f uuid.UUID, run uuid.UUID, status string, started time.Time, result string) {
		_, err := db.Exec(ctx, `INSERT INTO agent_tasks (type, status, feature_id, service_id, workflow_run_id, input, nabu_run_id, started_at, result)
			VALUES ('implement', $1, $2, $3, gen_random_uuid(), '{}', $4, $5, $6)`, status, f, svc, run, started, result)
		must(t, err)
	}
	codeRun, oldRun, writerRun, otherRun, liveRun := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	task(feature, codeRun, "failed", time.Now().Add(-time.Hour),
		`{"steps":[{"step":"branch","status":"ok","detail":"created b","at":"2026-11-11T07:34:20Z"},{"step":"commit","status":"failed","detail":"commit to b: create the tree: git provider: not found (HTTP 404)","at":"2026-11-11T07:34:28Z"}]}`)
	task(feature, oldRun, "failed", time.Now().AddDate(0, 0, -40), `{}`)
	task(feature, liveRun, "running", time.Now(), `{}`)
	task(other, otherRun, "succeeded", time.Now(), `{}`)
	_, err = db.Exec(ctx, `INSERT INTO agent_usage (context, scenario, feature_id, nabu_run_id, areas, tokens_in, tokens_out)
		VALUES ('gate', 'gate_generation', $1, $2, '{tech,qa}', 1, 1)`, feature, writerRun)
	must(t, err)

	events := &fakeEvents{runs: map[string][]nabu.RunEvent{
		codeRun.String(): {
			{Seq: 1, Type: "started", Data: json.RawMessage(`{}`)},
			{Seq: 2, Type: "text_delta", Data: json.RawMessage(`{"delta":"Реализую "}`)},
			{Seq: 3, Type: "text_delta", Data: json.RawMessage(`{"delta":"миграции"}`)},
			{Seq: 4, Type: "tool_call", Data: json.RawMessage(`{"id":"a","name":"bash","args":{"command":"go test ./..."}}`)},
			{Seq: 5, Type: "tool_result", Data: json.RawMessage(`{"id":"a","name":"bash","isError":false,"summary":"ok"}`)},
			{Seq: 6, Type: "completed", Data: json.RawMessage(`{"status":"succeeded"}`)},
		},
		writerRun.String(): {
			{Seq: 1, Type: "tool_call", Data: json.RawMessage(`{"id":"s","name":"mcp__hammurapi__spec_read","args":{"featureKey":"FTR.FMS.CAR-0005"}}`)},
			{Seq: 2, Type: "tool_call", Data: json.RawMessage(`{"id":"g","name":"mcp__hammurapi__submit_gate","args":{"area":"qa"}}`)},
			{Seq: 3, Type: "completed", Data: json.RawMessage(`{"status":"succeeded"}`)},
		},
		liveRun.String():  {{Seq: 1, Type: "text_delta", Data: json.RawMessage(`{"delta":"идёт"}`)}},
		otherRun.String(): {{Seq: 1, Type: "completed", Data: json.RawMessage(`{}`)}},
	}, hold: map[string]chan struct{}{liveRun.String(): make(chan struct{})}, broken: map[string]bool{writerRun.String(): true}}

	svcLog := &runlog.Service{Pool: db, Nabu: events, Config: runlog.Config{PerUser: 1}, Flush: 20 * time.Millisecond, StepsWait: time.Second}
	r := chi.NewRouter()
	svcLog.Routes(r)
	expert := domain.NewPrincipal(user, "ira", "Ira", false)
	expert.Experts["FMS"] = map[domain.ExpertKind]bool{"product": true}
	reader := domain.NewPrincipal(uuid.New(), "oleg", "Oleg", false)
	open := func(p *domain.Principal, run uuid.UUID, lastID string) (*http.Response, func()) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			r.ServeHTTP(w, req.WithContext(httpx.WithPrincipal(req.Context(), p)))
		}))
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/features/FTR.FMS.CAR-0005/runs/"+run.String()+"/log", nil)
		if lastID != "" {
			req.Header.Set("Last-Event-ID", lastID)
		}
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		return resp, func() { resp.Body.Close(); srv.Close() }
	}
	read := func(resp *http.Response, stop func(sseEvent) bool) []sseEvent {
		var out []sseEvent
		sc := bufio.NewScanner(resp.Body)
		var e sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "id: "):
				e.ID = line[4:]
			case strings.HasPrefix(line, "event: "):
				e.Event = line[7:]
			case strings.HasPrefix(line, "data: "):
				e.Data = json.RawMessage(line[6:])
			case line == "":
				out = append(out, e)
				if stop != nil && stop(e) {
					return out
				}
				e = sseEvent{}
			}
		}
		return out
	}
	entry := func(e sseEvent) runlog.Entry {
		var x runlog.Entry
		_ = json.Unmarshal(e.Data, &x)
		return x
	}

	// LOG-01…LOG-03: the message, the tool, the end, then the steps of Hammurapi
	resp, done := open(expert, codeRun, "")
	evs := read(resp, nil)
	done()
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, entry(e).Kind+":"+entry(e).Step)
	}
	if got := strings.Join(kinds, " "); got != "message: tool: tool: done: step:branch step:commit" {
		t.Fatalf("LOG-03 entries %s", got)
	}
	if m := entry(evs[0]); m.Text != "Реализую миграции" {
		t.Fatalf("LOG-01 message %q", m.Text)
	}
	if st := entry(evs[5]); st.Status != "failed" || !strings.Contains(st.Detail, "create the tree") || evs[5].ID != "6.2" {
		t.Fatalf("LOG-03 the failed step %+v id %s", st, evs[5].ID)
	}
	// LOG-06: resume after the event 4 — no message, no call again
	resp, done = open(expert, codeRun, "4")
	evs = read(resp, nil)
	done()
	if len(evs) != 4 || entry(evs[0]).Kind != runlog.KindTool || entry(evs[0]).Result == nil {
		t.Fatalf("LOG-06 resumed %+v", evs)
	}
	// LOG-07, LOG-12: the run of the writer — a broken stream is told, no steps of Hammurapi
	resp, done = open(expert, writerRun, "")
	evs = read(resp, nil)
	done()
	if evs[0].Event != "reconnect" || len(evs) != 4 || entry(evs[1]).Tool != "mcp__hammurapi__spec_read" || entry(evs[3]).Kind != runlog.KindDone {
		t.Fatalf("LOG-07, LOG-12 %+v", evs)
	}
	// LOG-08: a run of another feature
	if resp, done := open(expert, otherRun, ""); resp.StatusCode != 404 {
		t.Fatalf("LOG-08 %d", resp.StatusCode)
	} else {
		done()
	}
	// LOG-22: a user who is not an expert of the domain
	if resp, done := open(reader, codeRun, ""); resp.StatusCode != 403 {
		t.Fatalf("LOG-22 %d", resp.StatusCode)
	} else {
		done()
	}
	// LOG-10: a run older than the events of Nabu
	resp, done = open(expert, oldRun, "")
	evs = read(resp, nil)
	done()
	if len(evs) != 1 || entry(evs[0]).Kind != runlog.KindExpired {
		t.Fatalf("LOG-10 %+v", evs)
	}
	// LOG-09 (live), LOG-11: a live run sends its text at the flush; a second stream of the user is refused
	resp, done = open(expert, liveRun, "")
	first := read(resp, func(sseEvent) bool { return true })
	if len(first) != 1 || entry(first[0]).Text != "идёт" || entry(first[0]).At == "" {
		t.Fatalf("LOG-09 live %+v", first)
	}
	if resp2, done2 := open(expert, codeRun, ""); resp2.StatusCode != 429 {
		t.Fatalf("LOG-11 %d", resp2.StatusCode)
	} else {
		done2()
	}
	close(events.hold[liveRun.String()])
	done()
}
