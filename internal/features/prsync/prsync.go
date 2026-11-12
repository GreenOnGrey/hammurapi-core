// Package prsync keeps the PRs of services as the git provider has them: the CI status of the
// head from the checks of the provider, the head and the title (FTR.HMR.DEV-0004 R2, R17, arch §4.8).
// Check events of the webhook and a sync of open PRs every PR_SYNC_INTERVAL both end in Apply.
package prsync

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// lockKey keeps one sync at a time over the workers.
const lockKey = "hashtext('hammurapi.prsync')"

// batch is how many open PRs one pass compares; the next pass takes the next ones (synced_at).
const batch = 100

// State is a PR as the provider has it; an empty field is not known and changes nothing. CI is the
// status of the checks of Head, so a status is never written onto another head.
type State struct {
	Title string
	Head  string
	CI    string // success | failure | pending | none
}

// Service applies the state of the provider to PRs.
type Service struct {
	Pool     *pgxpool.Pool
	Git      git.Provider
	Events   events.Publisher
	Interval time.Duration
}

// final is a status that changes only with a new head or a rerun of a check — the events tell of those.
func final(ci *string) bool {
	return ci != nil && (*ci == git.CISuccess || *ci == git.CIFailure || *ci == git.CINone)
}

// Apply writes what differs between the PR, as it was read, and the provider and tells the
// feature; false — nothing changed. A push that came in after the PR was read wins: the head
// moves only from the head that was read, the status is written only for its own head.
func (s *Service) Apply(ctx context.Context, pr cycledata.PR, st State) (bool, error) {
	changed := false
	err := postgres.InTx(ctx, s.Pool, func(tx pgx.Tx) error {
		cd := cycledata.New(tx)
		head, cur := pr.HeadSHA, pr.CIStatus
		payload := map[string]any{"prId": pr.ID}
		if st.Head != "" && st.Head != pr.HeadSHA {
			moved, err := cd.MovePRHead(ctx, pr.ID, pr.HeadSHA, st.Head)
			if err != nil || !moved {
				return err
			}
			head, cur, changed = st.Head, nil, true
			payload["headSha"] = st.Head // new code: validation clears its signatures
		}
		if st.Title != "" && st.Title != pr.Title {
			if err := cd.SetPRTitle(ctx, pr.ID, st.Title); err != nil {
				return err
			}
			changed = true
		}
		// tech decision 11: «no checks» does not take the place of a status from a JUnit report
		keepReport := st.CI == git.CINone && cur != nil && (*cur == git.CISuccess || *cur == git.CIFailure)
		if st.CI != "" && !keepReport {
			set, err := cd.SetPRHeadCI(ctx, pr.ID, head, st.CI)
			if err != nil {
				return err
			}
			if set {
				changed = true
				payload["ci"] = st.CI
			}
		}
		if !changed {
			return nil
		}
		return workflows.SendToFeature(ctx, tx, pr.FeatureID, "pr_updated", payload)
	})
	if err != nil || !changed {
		return false, err
	}
	if s.Events != nil {
		s.Events.Publish(ctx, events.Event{Type: events.FeatureUpdated, Data: map[string]any{"prId": pr.ID}})
	}
	return true, nil
}

// CommitChanged takes a check event: the open PRs with this head get the status of all the checks
// of the commit (arch A14).
func (s *Service) CommitChanged(ctx context.Context, ev *git.CIEvent) error {
	prs, err := cycledata.New(s.Pool).PRsByHead(ctx, ev.Repo, ev.SHA, "")
	if err != nil {
		return err
	}
	var open []cycledata.PR
	for _, pr := range prs {
		if pr.State == "open" && pr.Kind != "spec" && pr.HeadSHA == ev.SHA {
			open = append(open, pr)
		}
	}
	if len(open) == 0 {
		metrics.PRCIEvents.WithLabelValues("unknown").Inc()
		return nil
	}
	prov := s.Git.ForRepo(ev.Repo)
	token, err := prov.BotToken(ctx)
	if err != nil {
		metrics.PRCIEvents.WithLabelValues("failed").Inc()
		return err
	}
	ci, err := prov.CommitCI(ctx, token, ev.SHA)
	if err != nil {
		metrics.PRCIEvents.WithLabelValues("failed").Inc()
		return err
	}
	for _, pr := range open {
		changed, err := s.Apply(ctx, pr, State{Head: ev.SHA, CI: ci})
		if err != nil {
			metrics.PRCIEvents.WithLabelValues("failed").Inc()
			return err
		}
		metrics.PRCIEvents.WithLabelValues(result(changed)).Inc()
	}
	return nil
}

func result(changed bool) string {
	if changed {
		return "updated"
	}
	return "unchanged"
}

// Run compares the open PRs with the provider every Interval; the first pass a minute after the start.
func (s *Service) Run(ctx context.Context) error {
	if s.Interval <= 0 {
		return nil
	}
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if err := s.Pass(ctx); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "pr sync pass", "err", err)
		}
		t.Reset(s.Interval)
	}
}

// Pass compares one batch of open PRs, one worker at a time; a failure of one PR does not stop the others.
func (s *Service) Pass(ctx context.Context) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+lockKey+`)`).Scan(&locked); err != nil || !locked {
		return err
	}
	defer func() { _, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(`+lockKey+`)`) }()
	cd := cycledata.New(s.Pool)
	prs, err := cd.OpenPRs(ctx, batch)
	if err != nil {
		return err
	}
	tokens := map[string]string{}
	for _, pr := range prs {
		if ctx.Err() != nil {
			return nil
		}
		changed, err := s.one(ctx, pr, tokens)
		if err != nil {
			metrics.PRSync.WithLabelValues("failed").Inc()
			slog.WarnContext(ctx, "pr sync", "repo", pr.Repo, "number", pr.Number, "err", err)
		} else {
			metrics.PRSync.WithLabelValues(result(changed)).Inc()
		}
		// a failed PR goes to the end of the queue too: one broken repository does not hold the others
		if err := cd.MarkPRSynced(ctx, pr.ID); err != nil {
			return err
		}
	}
	return nil
}

// one compares a PR: its title and head always (one call), the checks only of a new head or of a
// status not final yet — the rate limit of the provider (arch §8).
func (s *Service) one(ctx context.Context, pr cycledata.PR, tokens map[string]string) (bool, error) {
	prov := s.Git.ForRepo(pr.Repo)
	token, ok := tokens[pr.Repo]
	if !ok {
		var err error
		if token, err = prov.BotToken(ctx); err != nil {
			return false, err
		}
		tokens[pr.Repo] = token
	}
	info, err := prov.GetPR(ctx, token, pr.Number)
	if err != nil {
		return false, err
	}
	if info.State != "open" {
		return false, nil // a merge or a close comes with the webhook of the PR and its machines
	}
	if info.HeadSHA == "" {
		return false, errors.New("the provider gave no head of the PR")
	}
	st := State{Title: info.Title, Head: info.HeadSHA}
	if info.HeadSHA != pr.HeadSHA || !final(pr.CIStatus) {
		if st.CI, err = prov.CommitCI(ctx, token, info.HeadSHA); err != nil {
			return false, err
		}
	}
	return s.Apply(ctx, pr, st)
}
