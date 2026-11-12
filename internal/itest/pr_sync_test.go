//go:build integration

package itest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	dto "github.com/prometheus/client_model/go"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/prsync"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	gmocks "github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

type eventLog struct {
	mu  sync.Mutex
	all []events.Event
}

func (l *eventLog) Publish(_ context.Context, e events.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = append(l.all, e)
}

func (l *eventLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.all)
}

func prSyncCount(t *testing.T, result string) float64 {
	var m dto.Metric
	must(t, metrics.PRSync.WithLabelValues(result).Write(&m))
	return m.GetCounter().GetValue()
}

// FTR.HMR.DEV-0004 qa PCI-04…PCI-07: the CI status, heads and titles of open PRs from the provider.
func TestPRSync(t *testing.T) {
	ctx := context.Background()
	url, drop, err := freshDB(ctx, "prsync")
	must(t, err)
	defer drop()
	db, err := postgres.Connect(ctx, url)
	must(t, err)
	defer db.Close()
	must(t, postgres.Migrate(ctx, url))

	var user, system, feature uuid.UUID
	must(t, db.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, agent_name, agent_tone)
		VALUES ('ps1', 'ira', 'Ira', 'Hammurapi', 'business') RETURNING id`).Scan(&user))
	must(t, db.QueryRow(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ('FMS', 'Fleet') RETURNING id)
		INSERT INTO systems (domain_id, key, name) SELECT id, 'CAR', 'Cars' FROM d RETURNING id`).Scan(&system))
	must(t, db.QueryRow(ctx, `INSERT INTO features (unique_id, system_id, number, title, branch_name, pr_number, pr_url, phase, created_by)
		VALUES ('FTR.FMS.CAR-0005', $1, 5, 'F', 'b', 1, 'http://pr', 'codegen', $2) RETURNING id`, system, user).Scan(&feature))
	cd := cycledata.New(db)
	mkPR := func(repo string, number int, title, head string, ci *string) cycledata.PR {
		var svc uuid.UUID
		err := db.QueryRow(ctx, `SELECT id FROM services WHERE repo = $1`, repo).Scan(&svc)
		if err != nil {
			must(t, db.QueryRow(ctx, `INSERT INTO services (key, name, repo, source) VALUES ($1, $1, $2, 'manual') RETURNING id`,
				repo[len("o/"):], repo).Scan(&svc))
		}
		pr := &cycledata.PR{Repo: repo, Number: number, URL: "u", Title: title, Branch: "hammurapi/FTR.FMS.CAR-0005/x", Kind: "service",
			FeatureID: feature, ServiceID: &svc, ByAgent: true, State: "open", Review: "none", HeadSHA: head}
		must(t, cd.UpsertPR(ctx, pr))
		if ci != nil {
			must(t, cd.SetPRCI(ctx, pr.ID, *ci))
		}
		got, err := cd.PRByRepoNumber(ctx, repo, number)
		must(t, err)
		return *got
	}
	get := func(repo string, number int) cycledata.PR {
		got, err := cd.PRByRepoNumber(ctx, repo, number)
		must(t, err)
		return *got
	}
	ciOf := func(pr cycledata.PR) string {
		if pr.CIStatus == nil {
			return "<nil>"
		}
		return *pr.CIStatus
	}
	str := func(s string) *string { return &s }

	ctrl := gomock.NewController(t)
	gp := gmocks.NewMockProvider(ctrl)
	web, core := gmocks.NewMockProvider(ctrl), gmocks.NewMockProvider(ctrl)
	gp.EXPECT().ForRepo("o/web").Return(web).AnyTimes()
	gp.EXPECT().ForRepo("o/core").Return(core).AnyTimes()
	web.EXPECT().BotToken(gomock.Any()).Return("tw", nil).AnyTimes()
	core.EXPECT().BotToken(gomock.Any()).Return("tc", nil).AnyTimes()
	evs := &eventLog{}
	svc := &prsync.Service{Pool: db, Git: gp, Events: evs}

	// PCI-04: a check event of the head of an open PR; a PR with another head stays as it was
	mkPR("o/web", 15, "#15 web", "h15", nil)
	mkPR("o/web", 16, "#16 web", "h16", str("pending"))
	web.EXPECT().CommitCI(gomock.Any(), "tw", "h15").Return(git.CISuccess, nil)
	must(t, svc.CommitChanged(ctx, &git.CIEvent{Repo: "o/web", SHA: "h15"}))
	if ciOf(get("o/web", 15)) != "success" || ciOf(get("o/web", 16)) != "pending" || evs.count() != 1 {
		t.Fatalf("PCI-04: %s %s, %d events", ciOf(get("o/web", 15)), ciOf(get("o/web", 16)), evs.count())
	}
	// the same status again changes nothing and tells nobody
	web.EXPECT().CommitCI(gomock.Any(), "tw", "h15").Return(git.CISuccess, nil)
	must(t, svc.CommitChanged(ctx, &git.CIEvent{Repo: "o/web", SHA: "h15"}))
	if evs.count() != 1 {
		t.Fatalf("PCI-04 unchanged: %d events", evs.count())
	}

	// PCI-05, PCI-07: the sync — the title of the agent, an old head and no status; a PR that matches
	// with a final status (no call for its checks); a 403
	mkPR("o/core", 30, "I'll start by exploring the repository", "old", nil)
	core.EXPECT().GetPR(gomock.Any(), "tc", 30).Return(&git.PRInfo{Number: 30, Title: "FTR.FMS.CAR-0005 core: Tariffs", State: "open", HeadSHA: "new"}, nil)
	core.EXPECT().CommitCI(gomock.Any(), "tc", "new").Return(git.CIFailure, nil)
	web.EXPECT().GetPR(gomock.Any(), "tw", 15).Return(&git.PRInfo{Number: 15, Title: "#15 web", State: "open", HeadSHA: "h15"}, nil)
	web.EXPECT().GetPR(gomock.Any(), "tw", 16).Return(&git.PRInfo{Number: 16, Title: "#16 web", State: "open", HeadSHA: "h16"}, nil)
	web.EXPECT().CommitCI(gomock.Any(), "tw", "h16").Return("", errors.New("git provider: forbidden (HTTP 403)"))
	failed := prSyncCount(t, "failed")
	before := evs.count()
	must(t, svc.Pass(ctx))
	pr := get("o/core", 30)
	if pr.Title != "FTR.FMS.CAR-0005 core: Tariffs" || pr.HeadSHA != "new" || ciOf(pr) != "failure" {
		t.Fatalf("PCI-05: %+v ci %s", pr, ciOf(pr))
	}
	if evs.count() != before+1 {
		t.Fatalf("PCI-05: one PR changed, %d events", evs.count()-before)
	}
	if got := prSyncCount(t, "failed"); got != failed+1 {
		t.Fatalf("PCI-07: failed %v → %v", failed, got)
	}
	// every PR of the pass goes to the end of the queue, the failed one too
	var unsynced int
	must(t, db.QueryRow(ctx, `SELECT count(*) FROM pull_requests WHERE synced_at IS NULL`).Scan(&unsynced))
	if unsynced != 0 {
		t.Fatalf("PCI-05: %d PRs not marked synced", unsynced)
	}

	// a push that came in after the PR was read wins: neither the old head nor its status comes back
	stale := get("o/core", 30)
	_, err = db.Exec(ctx, `UPDATE pull_requests SET head_sha = 'pushed', ci_status = NULL WHERE id = $1`, stale.ID)
	must(t, err)
	changed, err := svc.Apply(ctx, stale, prsync.State{Head: "new", CI: git.CISuccess})
	must(t, err)
	if pr := get("o/core", 30); changed || pr.HeadSHA != "pushed" || ciOf(pr) != "<nil>" {
		t.Fatalf("a late push: changed %v, head %s, ci %s", changed, pr.HeadSHA, ciOf(pr))
	}
	changed, err = svc.Apply(ctx, stale, prsync.State{Head: "older", CI: git.CISuccess})
	must(t, err)
	if pr := get("o/core", 30); changed || pr.HeadSHA != "pushed" {
		t.Fatalf("a late push and a stale head of the provider: changed %v, head %s", changed, pr.HeadSHA)
	}

	// PCI-06: a JUnit report does not overwrite the status of the provider, it sets a PR without checks
	none := mkPR("o/web", 17, "#17 web", "h17", str("none"))
	must(t, cd.SetPRCIFromReport(ctx, get("o/web", 15).ID, "h15", "failure"))
	must(t, cd.SetPRCIFromReport(ctx, none.ID, "h17", "failure"))
	if ciOf(get("o/web", 15)) != "success" || ciOf(get("o/web", 17)) != "failure" {
		t.Fatalf("PCI-06: %s %s", ciOf(get("o/web", 15)), ciOf(get("o/web", 17)))
	}
	// and «no checks» of the provider does not take the place of the status of the report
	changed, err = svc.Apply(ctx, get("o/web", 17), prsync.State{Head: "h17", CI: git.CINone})
	must(t, err)
	if changed || ciOf(get("o/web", 17)) != "failure" {
		t.Fatalf("PCI-06 none over a report: changed %v, %s", changed, ciOf(get("o/web", 17)))
	}
}
