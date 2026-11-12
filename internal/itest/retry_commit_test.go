//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/codegen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/jobs/cleaner"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// memStore is an object store in memory.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (m *memStore) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	b, err := io.ReadAll(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = b
	return err
}

func (m *memStore) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[key]
	if !ok {
		return nil, errors.New("no object")
	}
	return io.NopCloser(strings.NewReader(string(b))), nil
}

func (m *memStore) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

// FTR.HMR.DEV-0004 qa RTC-02 (the workflow), RTC-04, RTC-07, RTC-11: POST …/retry-commit, the state retry_commit and the cleaner.
func TestRetryCommit(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "retrycommit")
	must(t, err)
	defer drop()
	pool, err := postgres.Connect(ctx, url)
	must(t, err)
	defer pool.Close()
	must(t, postgres.Migrate(ctx, url))

	var user, sys, svc uuid.UUID
	must(t, pool.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, agent_name, agent_tone)
		VALUES ('rc1', 'ira', 'Ira', 'Hammurapi', 'business') RETURNING id`).Scan(&user))
	must(t, pool.QueryRow(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('FMS', 'Fleet') RETURNING id)
		INSERT INTO systems (domain_id, key, name) SELECT id, 'CAR', 'Cars' FROM d RETURNING id`).Scan(&sys))
	must(t, pool.QueryRow(ctx, `INSERT INTO services (key, name, repo, source) VALUES ('booking', 'booking', 'fms/booking', 'manual') RETURNING id`).Scan(&svc))
	store := specdata.NewPG(pool)
	f := &specdata.Feature{UniqueID: "FTR.FMS.CAR-0005", SystemID: sys, Number: 5, Title: "t", Branch: "feature/FTR.FMS.CAR-0005", PRNumber: 1, PRURL: "u",
		Phase: domain.PhaseCodegen, CreatedBy: user}
	must(t, store.InsertFeature(ctx, f))

	runNabu := uuid.New()
	newTask := func(class string, until time.Time) uuid.UUID {
		id, err := codegen.StartTask(ctx, pool, codegen.TaskSpec{Type: codegen.TaskImplement, FeatureID: &f.ID, ServiceID: svc})
		must(t, err)
		_, err = pool.Exec(ctx, `UPDATE agent_tasks SET status = 'failed', error_class = $2, nabu_run_id = $3, changes_key = $4, changes_until = $5,
			result = '{"steps":[{"step":"commit","status":"failed"}],"branchHead":"base"}' WHERE id = $1`,
			id, class, runNabu, "codegen/changes/"+id.String()+".tar.gz", until)
		must(t, err)
		_, err = pool.Exec(ctx, `UPDATE workflow_runs SET state = 'failed' WHERE subject_id = $1`, id)
		must(t, err)
		return id
	}
	expert := domain.NewPrincipal(user, "ira", "Ira", false)
	expert.Experts["FMS"] = map[domain.ExpertKind]bool{"product": true}
	reader := domain.NewPrincipal(uuid.New(), "oleg", "Oleg", false)
	svcCode := codegen.NewService(store, nopEvents{})
	code := func(err error) string {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			return ae.Code
		}
		return ""
	}

	// RTC-04: changes past their term — no retry of the commit
	old := newTask("git", time.Now().Add(-time.Hour))
	if err := svcCode.RetryCommit(ctx, expert, f.UniqueID, old); code(err) != "retry_commit_unavailable" {
		t.Fatalf("RTC-04 %v", err)
	}
	tk, err := cycledata.New(pool).TaskByID(ctx, old)
	must(t, err)
	if tk.Retry != "agent" {
		t.Fatalf("RTC-04 retry %q", tk.Retry)
	}
	// RTC-11: not an expert of the domain
	live := newTask("git", time.Now().Add(time.Hour))
	if err := svcCode.RetryCommit(ctx, reader, f.UniqueID, live); code(err) != "forbidden" {
		t.Fatalf("RTC-11 %v", err)
	}
	// RTC-02: the same task runs again, its new workflow applies the kept changes without a run of Nabu
	must(t, svcCode.RetryCommit(ctx, expert, f.UniqueID, live))
	tk, err = cycledata.New(pool).TaskByID(ctx, live)
	must(t, err)
	if tk.Status != "running" || tk.ErrorClass != nil || tk.NabuRunID == nil || *tk.NabuRunID != runNabu.String() {
		t.Fatalf("RTC-02 task %+v", tk)
	}
	engine := workflows.New(pool, nopEvents{}, workflows.Config{MaxAttempts: 3, Lease: time.Minute})
	engine.Register(codegen.TaskMachine{Limits: codegen.Limits{MaxParallel: 5}})
	var applied []codegen.RunFinished
	engine.Handle(codegen.EffectApply, workflows.EffectHandler{Do: func(_ context.Context, _ workflows.RunRef, p json.RawMessage) ([]workflows.NewEvent, error) {
		var rf codegen.RunFinished
		_ = json.Unmarshal(p, &rf)
		applied = append(applied, rf)
		return nil, nil
	}})
	for i := 0; i < 5; i++ {
		_, _ = engine.ProcessOnce(ctx)
		_, _ = engine.DispatchOnce(ctx)
	}
	if len(applied) != 1 || !applied[0].RetryCommit || applied[0].TaskID != live || applied[0].RunID != runNabu.String() {
		t.Fatalf("RTC-02 the effect %+v", applied)
	}
	var state string
	must(t, pool.QueryRow(ctx, `SELECT w.state FROM workflow_runs w JOIN agent_tasks t ON t.workflow_run_id = w.id WHERE t.id = $1`, live).Scan(&state))
	if state != "applying" {
		t.Fatalf("RTC-02 state %s", state)
	}

	// RTC-07: the cleaner deletes the archives no longer needed and keeps a live one
	s3 := &memStore{objects: map[string][]byte{}}
	done := newTask("git", time.Now().Add(time.Hour))
	keep := newTask("git", time.Now().Add(time.Hour))
	_, err = pool.Exec(ctx, `UPDATE agent_tasks SET status = 'succeeded' WHERE id = $1`, done)
	must(t, err)
	_, err = pool.Exec(ctx, `UPDATE agent_tasks SET created_at = now() + interval '1 hour' WHERE id = $1`, keep) // the newest of the service
	must(t, err)
	for _, id := range []uuid.UUID{old, done, keep} {
		s3.objects["codegen/changes/"+id.String()+".tar.gz"] = []byte("x")
	}
	rep, err := (&cleaner.Cleaner{Pool: pool, S3: s3, Store: store}).Run(ctx)
	if err != nil && !strings.Contains(err.Error(), "branch") {
		must(t, err)
	}
	if _, ok := s3.objects["codegen/changes/"+keep.String()+".tar.gz"]; !ok || len(s3.objects) != 1 || rep.Archives < 2 {
		t.Fatalf("RTC-07 left %v, cleaned %d", s3.objects, rep.Archives)
	}
}
