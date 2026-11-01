//go:build integration

// Package itest runs repository SQL against a real Postgres (dockertest).
// Run with: go test -tags integration ./internal/itest/
package itest

import (
	"context"
	"fmt"
	neturl "net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ory/dockertest/v4"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/admin"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/approvals"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/attachments"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/domains"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/profile"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
)

var pool *pgxpool.Pool

// adminURL is the server the tests create their databases on: a ready
// Postgres from HMR_TEST_DATABASE_URL (local runs without Docker) or the
// dockertest container.
var adminURL string

func TestMain(m *testing.M) {
	ctx := context.Background()
	if url := os.Getenv("HMR_TEST_DATABASE_URL"); url != "" {
		adminURL = url
		os.Exit(runOn(ctx, m, nil))
	}
	dp, err := dockertest.NewPool(ctx, "", dockertest.WithMaxWait(2*time.Minute))
	if err != nil {
		fmt.Println("docker unavailable:", err)
		os.Exit(1)
	}
	res, err := dp.Run(ctx, "postgres", dockertest.WithTag("16"), dockertest.WithoutReuse(),
		dockertest.WithEnv([]string{"POSTGRES_PASSWORD=pw", "POSTGRES_DB=hammurapi"}))
	if err != nil {
		fmt.Println("start postgres:", err)
		_ = dp.Close(ctx)
		os.Exit(1)
	}
	adminURL = fmt.Sprintf("postgres://postgres:pw@%s/postgres?sslmode=disable", res.GetHostPort("5432/tcp"))
	err = dp.Retry(ctx, time.Minute, func() error {
		p, err := postgres.Connect(ctx, adminURL)
		if err != nil {
			return err
		}
		defer p.Close()
		return p.Ping(ctx)
	})
	if err != nil {
		fmt.Println("postgres:", err)
		_ = dp.Close(ctx)
		os.Exit(1)
	}
	code := runOn(ctx, m, nil)
	_ = dp.Close(ctx)
	os.Exit(code)
}

// runOn migrates a fresh database, runs the tests on it and drops it.
func runOn(ctx context.Context, m *testing.M, _ any) int {
	url, drop, err := freshDB(ctx, "itest")
	if err == nil {
		if pool, err = postgres.Connect(ctx, url); err == nil {
			err = postgres.Migrate(ctx, url)
		}
	}
	if err != nil {
		fmt.Println("postgres:", err)
		return 1
	}
	code := m.Run()
	pool.Close()
	drop()
	return code
}

// freshDB creates an empty database on the test server.
func freshDB(ctx context.Context, prefix string) (string, func(), error) {
	admin, err := postgres.Connect(ctx, adminURL)
	if err != nil {
		return "", nil, err
	}
	name := fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		return "", nil, err
	}
	u, err := neturl.Parse(adminURL)
	if err != nil {
		admin.Close()
		return "", nil, err
	}
	u.Path = "/" + name
	drop := func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close()
	}
	return u.String(), drop, nil
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestRepositories walks a feature through the projection with every query.
func TestRepositories(t *testing.T) {
	ctx := context.Background()
	authRepo := auth.NewRepository(pool)

	// AUTH-02/03: users are created on first login; the second call is an update.
	u, created, err := authRepo.UpsertUser(ctx, auth.UserRow{ProviderUID: "1", Username: "anna", DisplayName: "Anna K.",
		Language: "en", AgentName: "Codex", AgentTone: domain.ToneBusiness, GlobalAdmin: true})
	must(t, err)
	if !created || !u.GlobalAdmin {
		t.Fatalf("first login: %+v %v", u, created)
	}
	_, created, err = authRepo.UpsertUser(ctx, auth.UserRow{ProviderUID: "1", Username: "anna", DisplayName: "Anna", Language: "en", AgentName: "X", AgentTone: domain.ToneMentor})
	must(t, err)
	if created {
		t.Fatal("second login created a user")
	}
	must(t, authRepo.SaveToken(ctx, u.ID, auth.EncryptedToken{Access: []byte{1}, ExpiresAt: time.Now().Add(time.Hour)}))
	tok, err := authRepo.Token(ctx, u.ID)
	must(t, err)
	if tok == nil {
		t.Fatal("token not stored")
	}
	sid, err := authRepo.CreateSession(ctx, u.ID, "csrf", time.Hour)
	must(t, err)
	sess, err := authRepo.Session(ctx, sid)
	must(t, err)
	must(t, authRepo.TouchSession(ctx, sid, time.Hour))
	if sess == nil || sess.CSRFToken != "csrf" {
		t.Fatalf("session %+v", sess)
	}

	// Admin: roles without editor/approver (ROLE-01), last global admin (ADM-11), user list.
	adm := admin.NewService(pool)
	must(t, adm.SetRoles(ctx, u.ID, admin.RolesInput{GlobalAdmin: true, AreaAdmin: []domain.Area{domain.AreaProduct}}))
	tu.Code(t, adm.SetRoles(ctx, u.ID, admin.RolesInput{GlobalAdmin: false}), 409, "last_global_admin")
	p, err := authRepo.Principal(ctx, u.ID)
	must(t, err)
	if !p.IsAreaAdmin(domain.AreaProduct) || !p.GlobalAdmin {
		t.Fatalf("principal %+v", p)
	}
	days, err := admin.RetentionDays(ctx, pool)
	must(t, err)
	if days != 90 {
		t.Fatalf("retention %d", days)
	}

	// Dictionary (NOAPR-01/02/03, ADM-03).
	ds := domains.NewService(pool, nopEvents{})
	must(t, ds.CreateDomain(ctx, p, "FMS", "Fleet", true))
	must(t, ds.CreateDomain(ctx, p, "CRM", "Clients", false))
	tu.Code(t, ds.CreateDomain(ctx, p, "FMS", "Again", true), 409, "key_exists")
	must(t, ds.CreateSystem(ctx, p, "FMS", "CAR", "Cars"))
	no := false
	must(t, ds.PatchDomain(ctx, p, "FMS", nil, &no))
	n, err := ds.SetApprovalAll(ctx, p, true)
	must(t, err)
	if n != 2 {
		t.Fatalf("changed %d, want 2", n)
	}
	// Experts per domain and kind (CAT-08: allowed with Backstage too).
	must(t, ds.SetExperts(ctx, p, "FMS", domains.ExpertsInput{Product: []uuid.UUID{u.ID}, Technical: []uuid.UUID{u.ID}}))
	list, err := domains.NewRepository(pool).List(ctx)
	must(t, err)
	var fms *domains.Domain
	for i := range list {
		if list[i].Key == "FMS" {
			fms = &list[i]
		}
	}
	if fms == nil || len(fms.Systems) != 1 || len(fms.Experts.Product) != 1 || fms.Source != "manual" {
		t.Fatalf("domains %+v", list)
	}
	p, err = authRepo.Principal(ctx, u.ID)
	must(t, err)
	if !p.HasExpert("FMS", domain.ExpertTechnical) || !p.CanApprove("FMS", domain.AreaQA) {
		t.Fatalf("principal %+v", p)
	}
	users, err := adm.Users(ctx, "ann", httpx.Page{Limit: 10})
	must(t, err)
	if len(users) != 1 || len(users[0].AreaAdmin) != 1 || len(users[0].Experts) != 1 {
		t.Fatalf("users %+v", users)
	}

	// Profile: my domains drive the "mine" filter (PROF-03).
	prof := profile.NewService(pool)
	doms := []string{"FMS"}
	lang := "de"
	pr, err := prof.Update(ctx, u.ID, profile.Patch{Domains: &doms, Language: &lang})
	must(t, err)
	if pr.Language != "de" || len(pr.Domains) != 1 {
		t.Fatalf("profile %+v", pr)
	}

	// Feature + gates + history.
	store := specdata.NewPG(pool)
	sys, err := store.SystemByKeys(ctx, "FMS", "CAR")
	must(t, err)
	var f *specdata.Feature
	must(t, store.InTx(ctx, func(tx specdata.Store) error {
		num, err := tx.NextNumber(ctx, sys.ID)
		if err != nil {
			return err
		}
		f = &specdata.Feature{UniqueID: domain.FeatureKey("FMS", "CAR", num), SystemID: sys.ID, Number: num, Title: "Weekend booking",
			Branch: "feature/FTR.FMS.CAR-0001", PRNumber: 1, PRURL: "u", Phase: domain.PhaseSpec, CreatedBy: u.ID}
		if err := tx.InsertFeature(ctx, f); err != nil {
			return err
		}
		now := time.Now()
		g := &specdata.Gate{FeatureID: f.ID, Area: domain.AreaProduct, Status: domain.GateInReview, HeadCommit: "c1", SubmittedAt: &now, CreatedBy: u.ID}
		if err := tx.InsertGate(ctx, g); err != nil {
			return err
		}
		if err := tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventSubmitted, ActorID: &u.ID}); err != nil {
			return err
		}
		return tx.InsertEvent(ctx, &specdata.GateEvent{GateID: g.ID, Type: domain.EventCreated, ActorID: &u.ID, CommitSHA: &g.HeadCommit})
	}))
	got, err := store.FeatureByUniqueID(ctx, "FTR.FMS.CAR-0001")
	must(t, err)
	if got.DomainKey != "FMS" || !got.ApprovalRequired || got.CreatedByName != "Anna" {
		t.Fatalf("feature %+v", got)
	}
	for _, filter := range []specdata.ListFilter{
		{UserID: u.ID, Domain: "mine", Status: "active", Page: httpx.Page{Limit: 10}},
		{UserID: u.ID, Domain: "FMS", Status: "all", Phase: "spec", Query: "weekend", Page: httpx.Page{Limit: 10}},
		{UserID: u.ID, Domain: "all", Status: "all", Query: "FTR.FMS.CAR", Page: httpx.Page{Limit: 10, Cursor: &httpx.Cursor{T: time.Now().Add(time.Hour), ID: "z"}}},
	} {
		items, err := store.ListFeatures(ctx, filter)
		must(t, err)
		if len(items) != 1 || len(items[0].Gates) != 1 {
			t.Fatalf("list %+v: %+v", filter, items)
		}
	}
	hist, err := store.History(ctx, f.ID, domain.AreaProduct, httpx.Page{Limit: 10})
	must(t, err)
	if len(hist) != 2 {
		t.Fatalf("history %+v", hist)
	}
	subID, subName, err := store.LastSubmitter(ctx, hist[0].GateID)
	must(t, err)
	if subID == nil || *subName != "Anna" {
		t.Fatal("submitter")
	}
	fixes, err := store.Fixes(ctx, f.ID)
	must(t, err)
	if len(fixes) != 0 {
		t.Fatal("fixes")
	}

	// Approvals queue (ROLE-03, HOME-04) respects approval_required (NOAPR-07).
	ar := approvals.NewRepository(pool)
	pending, err := ar.Pending(ctx, u.ID.String(), httpx.Page{Limit: 10})
	must(t, err)
	if len(pending) != 1 || pending[0].SubmittedBy == nil {
		t.Fatalf("pending %+v", pending)
	}
	must(t, ds.PatchDomain(ctx, p, "FMS", nil, &no))
	pending, err = ar.Pending(ctx, u.ID.String(), httpx.Page{Limit: 10})
	must(t, err)
	if len(pending) != 0 {
		t.Fatal("queue shows a domain without approval")
	}

	// Webhook idempotency (HOOK-02) and soft deletion (DEL-07/13).
	fresh, err := store.MarkWebhookProcessed(ctx, "evt")
	must(t, err)
	again, err := store.MarkWebhookProcessed(ctx, "evt")
	must(t, err)
	if !fresh || again {
		t.Fatal("webhook idempotency")
	}
	must(t, store.MarkDeleted(ctx, f.ID, u.ID, true))
	pend, err := store.FeaturesPendingCleanup(ctx)
	must(t, err)
	if len(pend) != 1 {
		t.Fatal("cleanup queue")
	}
	items, err := store.ListFeatures(ctx, specdata.ListFilter{UserID: u.ID, Domain: "FMS", Status: "all", Page: httpx.Page{Limit: 10}})
	must(t, err)
	if len(items) != 0 {
		t.Fatal("deleted feature listed")
	}
	num, err := store.NextNumber(ctx, sys.ID)
	must(t, err)
	if num != 2 {
		t.Fatalf("number reused: %d", num)
	}

	// Attachments (the chat of the operator is gone, FTR.HMR.CMN-0007 R3).
	att := attachments.NewService(pool, nil, 1<<20, nil)
	if _, err := pool.Exec(ctx, `INSERT INTO attachments (user_id, file_name, mime_type, size_bytes, s3_key) VALUES ($1,'a.png','image/png',1,'k1')`, u.ID); err != nil {
		t.Fatal(err)
	}
	al, err := att.List(ctx, u.ID, httpx.Page{Limit: 10})
	must(t, err)
	if len(al.Items) != 1 {
		t.Fatalf("attachments %+v", al)
	}
	// ATT-05: someone else's attachment.
	_, err = att.Get(ctx, uuid.New(), al.Items[0].ID)
	tu.Code(t, err, 403, "forbidden")
}

type nopEvents struct{}

func (nopEvents) Publish(context.Context, events.Event) {}
