package codegen

// A revert of a rollback (R32): done by the worker without an agent.

import (
	"context"
	"errors"
	"fmt"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
)

// revert restores the files a merged PR changed to their state before the
// merge, on a revert branch, and opens a revert PR from the bot.
func revert(ctx context.Context, p git.Provider, token, base string, d *Description) Result {
	res := Result{Status: "failed", Requirements: []string{}, TestCases: []string{}}
	if d.Input.MergeSHA == "" || d.Input.PRNumber == 0 {
		return withErr(res, ClassGit, errors.New("revert task without the merged PR"))
	}
	parents, err := p.CommitParents(ctx, token, d.Input.MergeSHA)
	if err != nil || len(parents) == 0 {
		return withErr(res, ClassGit, fmt.Errorf("parents of %s: %v", d.Input.MergeSHA, err))
	}
	before := parents[0]
	files, err := p.PRChangedFiles(ctx, token, d.Input.PRNumber)
	if err != nil {
		return withErr(res, ClassGit, err)
	}
	var changes []git.FileChange
	restore := func(path string) error {
		f, err := p.GetFile(ctx, token, before, path)
		if errors.Is(err, git.ErrNotFound) {
			changes = append(changes, git.FileChange{Path: path, Delete: true})
			return nil
		}
		if err != nil {
			return err
		}
		changes = append(changes, git.FileChange{Path: path, Content: f.Content})
		return nil
	}
	for _, f := range files {
		switch f.Status {
		case "added":
			changes = append(changes, git.FileChange{Path: f.Path, Delete: true})
		case "renamed":
			changes = append(changes, git.FileChange{Path: f.Path, Delete: true})
			if err := restore(f.OldPath); err != nil {
				return withErr(res, ClassGit, err)
			}
		default:
			if err := restore(f.Path); err != nil {
				return withErr(res, ClassGit, err)
			}
		}
	}
	if len(changes) == 0 {
		return withErr(res, ClassGit, errors.New("the merged PR changed no files"))
	}
	head, err := p.BranchHead(ctx, token, base)
	if err != nil {
		return withErr(res, ClassGit, err)
	}
	if _, err := p.BranchHead(ctx, token, d.Branch); err != nil {
		if err := p.CreateBranch(ctx, token, d.Branch, head); err != nil {
			return withErr(res, ClassGit, err)
		}
	}
	msg := git.Trailers{Feature: d.Feature, Agent: true, Initiator: d.Initiator, Task: d.ID.String(), Release: d.Release}.
		Message(fmt.Sprintf("Revert %s in %s (rollback of %s)", d.Feature, d.Service, d.Release))
	sha, err := p.Commit(ctx, token, d.Branch, msg, changes)
	if err != nil {
		return withErr(res, ClassGit, err)
	}
	pr, err := p.CreatePR(ctx, token, d.Branch, base, fmt.Sprintf("Revert %s %s (%s)", d.Feature, d.Service, d.Release),
		fmt.Sprintf("Rollback of release **%s**: reverts PR #%d (merge %s).\n\nReason: %s\n\nPrepared by the Hammurapi agent; merged by the release in Hammurapi.",
			d.Release, d.Input.PRNumber, d.Input.MergeSHA, d.Input.Reason))
	if err != nil {
		return withErr(res, ClassGit, err)
	}
	res.Status, res.PRNumber, res.PRURL, res.Branch, res.HeadSHA = "succeeded", pr.Number, pr.URL, d.Branch, sha
	res.Summary = fmt.Sprintf("Revert of PR #%d", d.Input.PRNumber)
	return res
}
