package releases

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/deploy"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specchanges"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Provider effects of the release and rollback workflows.
const (
	EffectMerge     = "provider.merge"
	EffectMergeSpec = "provider.merge_spec"
	EffectCheckTag  = "provider.check_tag"
	EffectClosePRs  = "provider.close_prs"
)

// Effects executes provider calls (worker). Merges use the token of the
// expert who started the step (arch §8): the action of a person in the
// provider's audit, and branch protection applies.
type Effects struct {
	Q      postgres.Querier
	Store  specdata.Store
	Git    git.Provider
	Tokens git.TokenSource
	// Changes and DefaultBranch put the notes of the feature over the fresh
	// default branch before the specification is merged (FTR.HMR.DEV-0002 R8); nil — none.
	Changes       *specchanges.Service
	DefaultBranch string
	// UpdateWait and UpdatePoll bound the wait for an update of the branch (30 s, every 2 s by default).
	UpdateWait, UpdatePoll time.Duration
}

// permanent reports a provider refusal that retries will not fix.
func permanent(err error) bool {
	var ae *git.APIError
	if errors.As(err, &ae) {
		return ae.Status >= 400 && ae.Status < 500 && ae.Status != http.StatusTooManyRequests
	}
	return errors.Is(err, git.ErrNotFound) || errors.Is(err, git.ErrUnauthorized)
}

// MergePayload is the payload of provider.merge.
type MergePayload struct {
	PRID   uuid.UUID `json:"prId"`
	UserID uuid.UUID `json:"userId"`
}

// Merge merges a service or revert PR after checking it merges cleanly and CI
// is not red; otherwise the step asks for an update_pr task (R27).
func (e *Effects) Merge(ctx context.Context, _ workflows.RunRef, payload json.RawMessage) ([]workflows.NewEvent, error) {
	var in MergePayload
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	cd := cycledata.New(e.Q)
	pr, err := cd.PRByID(ctx, in.PRID)
	if err != nil {
		return nil, err
	}
	token, err := e.Tokens.Token(ctx, in.UserID)
	if err != nil {
		return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"prId": pr.ID.String(), "reason": "the expert's git token is not available: " + err.Error()}}}, nil
	}
	p := e.Git.ForRepo(pr.Repo)
	info, err := p.GetPR(ctx, token, pr.Number)
	if err != nil {
		if permanent(err) {
			return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"prId": pr.ID.String(), "reason": git.Reason(err)}}}, nil
		}
		return nil, err
	}
	switch info.State {
	case "merged":
		return []workflows.NewEvent{{Type: "merged", Payload: map[string]string{"prId": pr.ID.String(), "sha": info.MergeSHA}}}, nil
	case "closed":
		return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"prId": pr.ID.String(), "reason": fmt.Sprintf("PR #%d is closed", pr.Number)}}}, nil
	}
	if info.Mergeable != nil && !*info.Mergeable {
		return []workflows.NewEvent{{Type: "needs_update", Payload: map[string]string{"prId": pr.ID.String(), "reason": "conflict with the default branch"}}}, nil
	}
	if pr.CIStatus != nil && *pr.CIStatus == "failure" && pr.Kind != "revert" {
		return []workflows.NewEvent{{Type: "needs_update", Payload: map[string]string{"prId": pr.ID.String(), "reason": "CI is red on the current head"}}}, nil
	}
	if err := p.MergePR(ctx, token, pr.Number, fmt.Sprintf("Merge #%d by Hammurapi release", pr.Number)); err != nil {
		if errors.Is(err, git.ErrConflict) {
			return []workflows.NewEvent{{Type: "needs_update", Payload: map[string]string{"prId": pr.ID.String(), "reason": "conflict with the default branch"}}}, nil
		}
		if permanent(err) {
			// REL-05: branch protection, missing rights — blocked with the provider's reason.
			return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"prId": pr.ID.String(), "reason": git.Reason(err)}}}, nil
		}
		return nil, err
	}
	after, err := p.GetPR(ctx, token, pr.Number)
	if err != nil {
		return nil, err
	}
	if err := cd.MarkPRMerged(ctx, pr.ID, after.MergeSHA, &in.UserID); err != nil {
		return nil, err
	}
	return []workflows.NewEvent{{Type: "merged", Payload: map[string]string{"prId": pr.ID.String(), "sha": after.MergeSHA}}}, nil
}

// MergeSpec merges the specification PR on confirmation (R31).
func (e *Effects) MergeSpec(ctx context.Context, _ workflows.RunRef, payload json.RawMessage) ([]workflows.NewEvent, error) {
	var in struct {
		FeatureID uuid.UUID `json:"featureId"`
		UserID    uuid.UUID `json:"userId"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	f, err := e.Store.FeatureByID(ctx, in.FeatureID)
	if err != nil {
		return nil, err
	}
	token, err := e.Tokens.Token(ctx, in.UserID)
	if err != nil {
		return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"reason": "the expert's git token is not available: " + err.Error()}}}, nil
	}
	info, err := e.Git.GetPR(ctx, token, f.PRNumber)
	if err == nil && info.State == "merged" {
		return []workflows.NewEvent{{Type: "spec_merged", Payload: map[string]string{"sha": info.MergeSHA}}}, nil
	}
	if e.Changes != nil {
		if reason, err := e.alignNotes(ctx, token, f); err != nil {
			if permanent(err) {
				return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"reason": git.Reason(err)}}}, nil
			}
			return nil, err
		} else if reason != "" {
			return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"reason": reason}}}, nil
		}
	}
	if err := e.Git.MergePR(ctx, token, f.PRNumber, fmt.Sprintf("%s: specification of the confirmed release", f.UniqueID)); err != nil {
		if permanent(err) || errors.Is(err, git.ErrConflict) {
			return []workflows.NewEvent{{Type: "merge_failed", Payload: map[string]string{"reason": git.Reason(err)}}}, nil
		}
		return nil, err
	}
	return []workflows.NewEvent{{Type: "spec_merged", Payload: map[string]string{}}}, nil
}

// alignNotes brings the branch of the feature up to the default branch and
// puts its notes over it (FTR.HMR.DEV-0002 arch §3.4, tech §5.3). Files of other
// features in the branch come only from the notes: when the update conflicts,
// they go back to the default branch and the notes are put again. A conflict
// that stays is in the files of the feature: the reason of merge_failed.
func (e *Effects) alignNotes(ctx context.Context, token string, f *specdata.Feature) (string, error) {
	conflict, err := e.updateBranch(ctx, token, f)
	if err != nil {
		return "", err
	}
	if conflict {
		files, err := e.Git.PRChangedFiles(ctx, token, f.PRNumber)
		if err != nil {
			return "", err
		}
		var reset []git.FileChange
		for _, cf := range files {
			loc, ok := specindex.ParsePath(cf.Path)
			if !ok || loc.Folder == f.UniqueID {
				continue
			}
			main, err := e.Git.GetFile(ctx, token, e.DefaultBranch, cf.Path)
			switch {
			case errors.Is(err, git.ErrNotFound):
				reset = append(reset, git.FileChange{Path: cf.Path, Delete: true})
			case err != nil:
				return "", err
			default:
				reset = append(reset, git.FileChange{Path: cf.Path, Content: main.Content})
			}
		}
		if len(reset) == 0 {
			return "conflict with the default branch in the files of the feature", nil
		}
		if _, err := e.Git.Commit(ctx, token, f.Branch, specchanges.CommitMessage(f.UniqueID), reset); err != nil {
			return "", err
		}
		if conflict, err = e.updateBranch(ctx, token, f); err != nil {
			return "", err
		}
		if conflict {
			return "conflict with the default branch in the files of the feature", nil
		}
		metrics.SpecChangeNotes.WithLabelValues("conflict_reapplied").Inc()
	}
	product, err := e.Git.GetFile(ctx, token, f.Branch, git.SpecPath(f.DomainKey, f.SystemKey, f.UniqueID, "product"))
	if errors.Is(err, git.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// rows that turned wrong since the submission (a target left the index) get no note: the merge goes on
	changes, _, err := e.Changes.Align(ctx, token, f, string(product.Content))
	if err != nil || len(changes) == 0 {
		return "", err
	}
	if _, err = e.Git.Commit(ctx, token, f.Branch, specchanges.CommitMessage(f.UniqueID), changes); err != nil {
		return "", err
	}
	specchanges.Applied(changes)
	return "", nil
}

// updateBranch merges the default branch into the branch of the PR and waits
// for it: the providers update a branch asynchronously, and refuse a conflict
// and a branch already up to date alike. The branch is up to date when it
// contains the head of the default branch; a conflict is a PR that says it
// does not merge. Waiting longer than UpdateWait is an error the effect retries.
func (e *Effects) updateBranch(ctx context.Context, token string, f *specdata.Feature) (bool, error) {
	mainHead, err := e.Git.BranchHead(ctx, token, e.DefaultBranch)
	if err != nil {
		return false, err
	}
	updated := func() (bool, error) {
		head, err := e.Git.BranchHead(ctx, token, f.Branch)
		if err != nil {
			return false, err
		}
		return e.Git.IsAncestor(ctx, token, mainHead, head)
	}
	if ok, err := updated(); err != nil || ok {
		return false, err
	}
	if err := e.Git.UpdatePRBranch(ctx, token, f.PRNumber); err != nil {
		var ae *git.APIError // 422: a conflict, decided below by the PR
		if !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity {
			return false, err
		}
	}
	wait, step := e.UpdateWait, e.UpdatePoll
	if wait <= 0 {
		wait = 30 * time.Second
	}
	if step <= 0 {
		step = 2 * time.Second
	}
	for deadline := time.Now().Add(wait); ; {
		if ok, err := updated(); err != nil || ok {
			return false, err
		}
		info, err := e.Git.GetPR(ctx, token, f.PRNumber)
		if err != nil {
			return false, err
		}
		if info.Mergeable != nil && !*info.Mergeable {
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("the branch %s is not updated with the default branch yet", f.Branch)
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(step):
		}
	}
}

// CheckTag counts a tag containing the merge commit as the release of the
// service (R28): the deploy run gets signal "tag" unless another signal came first.
func (e *Effects) CheckTag(ctx context.Context, _ workflows.RunRef, payload json.RawMessage) ([]workflows.NewEvent, error) {
	var in struct {
		DeployRunID uuid.UUID `json:"deployRunId"`
		Repo        string    `json:"repo"`
		MergeSHA    string    `json:"mergeSha"`
		TagSHA      string    `json:"tagSha"`
		Tag         string    `json:"tag"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	p := e.Git.ForRepo(in.Repo)
	token, err := p.BotToken(ctx)
	if err != nil {
		return nil, err
	}
	ok := in.MergeSHA == in.TagSHA
	if !ok {
		if ok, err = p.IsAncestor(ctx, token, in.MergeSHA, in.TagSHA); err != nil {
			if permanent(err) {
				return nil, nil
			}
			return nil, err
		}
	}
	if !ok {
		return nil, nil
	}
	cd := cycledata.New(e.Q)
	tag := in.Tag
	changed, err := cd.UpdateDeployRun(ctx, in.DeployRunID, "success", "tag", &tag, nil, nil, nil)
	if err != nil || !changed {
		return nil, err
	}
	run, err := cd.DeployRunByID(ctx, in.DeployRunID)
	if err != nil {
		return nil, err
	}
	return nil, deploy.Deliver(ctx, e.Q, run)
}

// ClosePRs closes the specification PR without merging and the service PRs
// that were not merged (R32, RB-05, RB-07).
func (e *Effects) ClosePRs(ctx context.Context, _ workflows.RunRef, payload json.RawMessage) ([]workflows.NewEvent, error) {
	var in struct {
		FeatureID uuid.UUID `json:"featureId"`
		UserID    uuid.UUID `json:"userId"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	f, err := e.Store.FeatureByID(ctx, in.FeatureID)
	if err != nil {
		return nil, err
	}
	if token, err := e.Tokens.Token(ctx, in.UserID); err == nil {
		if err := e.Git.ClosePR(ctx, token, f.PRNumber); err != nil && !errors.Is(err, git.ErrNotFound) && !permanent(err) {
			return nil, err
		}
	} else if bot, err := e.Git.BotToken(ctx); err == nil {
		_ = e.Git.ClosePR(ctx, bot, f.PRNumber)
	}
	cd := cycledata.New(e.Q)
	prs, err := cd.FeaturePRs(ctx, f.ID, "service")
	if err != nil {
		return nil, err
	}
	for _, pr := range prs {
		if pr.State != "open" {
			continue
		}
		p := e.Git.ForRepo(pr.Repo)
		if bot, err := p.BotToken(ctx); err == nil {
			if err := p.ClosePR(ctx, bot, pr.Number); err != nil && !errors.Is(err, git.ErrNotFound) && !permanent(err) {
				return nil, err
			}
		}
		if err := cd.MarkPRClosed(ctx, pr.ID); err != nil {
			return nil, err
		}
	}
	return []workflows.NewEvent{{Type: "closed", Payload: map[string]any{}}}, nil
}
