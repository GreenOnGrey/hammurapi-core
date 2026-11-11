package codegen

// The changes of a run of Nabu (FTR.HMR.CMN-0007 R9, tech §7.4): the archive of
// changes turned into a commit of the bot and a PR.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
)

// changes reads the archive of changes of a run: the files to commit, the
// plan and what was left out.
func (c *Code) changes(ctx context.Context, runID string) ([]git.FileChange, string, []string, error) {
	rc, err := c.Nabu.Changes(ctx, runID)
	var ae *nabu.APIError
	if errors.As(err, &ae) && ae.Code == "no_changes" {
		return nil, "", nil, nil
	}
	if err != nil {
		return nil, "", nil, err
	}
	defer rc.Close()
	return ReadChanges(rc)
}

// ReadChanges turns the archive of changes of Nabu (changes.json and the
// files, tech §2.3) into changes for the provider; paths outside the copy and
// .git are dropped with a warning (tech §7.4).
func ReadChanges(r io.Reader) ([]git.FileChange, string, []string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, "", nil, err
	}
	tr := tar.NewReader(gz)
	var manifest struct {
		Changes []struct {
			Path string `json:"path"`
			Op   string `json:"op"`
		} `json:"changes"`
		Skipped []struct {
			Path   string `json:"path"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 64<<20))
		if err != nil {
			return nil, "", nil, err
		}
		if h.Name == "changes.json" {
			if err := json.Unmarshal(b, &manifest); err != nil {
				return nil, "", nil, fmt.Errorf("changes.json: %w", err)
			}
			continue
		}
		files[h.Name] = b
	}
	var out []git.FileChange
	var warnings, protected []string
	plan := ""
	for _, ch := range manifest.Changes {
		p := path.Clean(ch.Path)
		if p != ch.Path || strings.HasPrefix(p, "/") || p == ".." || strings.HasPrefix(p, "../") || p == ".git" || strings.HasPrefix(p, ".git/") {
			warnings = append(warnings, "dropped a change outside the working copy: "+ch.Path)
			continue
		}
		if p == PlanFile {
			plan = string(files[p])
			continue
		}
		if ch.Op != "added" && protectedPath(p) { // FTR.HMR.CMN-0008 R17
			protected = append(protected, p)
		}
		switch ch.Op {
		case "deleted":
			out = append(out, git.FileChange{Path: p, Delete: true})
		case "added", "modified":
			b, ok := files[p]
			if !ok {
				warnings = append(warnings, "a change without its file: "+p)
				continue
			}
			out = append(out, git.FileChange{Path: p, Content: b})
		}
	}
	for _, s := range manifest.Skipped {
		warnings = append(warnings, fmt.Sprintf("left out by the sandbox (%s): %s", s.Reason, s.Path))
	}
	if len(protected) > 0 {
		sort.Strings(protected)
		warnings = append(warnings, warningProtected+strings.Join(protected, ", "))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, plan, warnings, nil
}

// commit is what the runner did after the agent: the plan, empty changes,
// requirements and test cases, the branch, the commit with trailers, the PR.
func commit(ctx context.Context, p git.Provider, token string, d *Description, res Result, changes []git.FileChange, text string) Result {
	if d.Autonomy == "plan" && d.Type == TaskImplement {
		if res.Plan == "" {
			res.Plan = text
		}
		res.Status = "succeeded" // a person prepares the code and the PR
		return res
	}
	if len(changes) == 0 {
		if d.Type == TaskReview && d.Input.PRNumber > 0 {
			// The agent answered in the discussion instead of changing code.
			if err := p.CommentPR(ctx, token, d.Input.PRNumber, "Hammurapi agent: "+strings.TrimSpace(text)); err != nil {
				return withErr(res, ClassGit, err)
			}
			res.Status, res.PRNumber = "succeeded", d.Input.PRNumber
			return res
		}
		return withErr(res, ClassNoChanges, errors.New("the agent made no changes"))
	}
	base, err := p.DefaultBranch(ctx, token)
	if err != nil {
		return withErr(res, ClassGit, err)
	}
	res.Requirements = mentionedReqs(text, d)
	res.TestCases = testCasesIn(changes, d)
	created := false
	if h, err := p.BranchHead(ctx, token, d.Branch); err != nil || h == "" {
		head, err := p.BranchHead(ctx, token, base)
		if err != nil {
			return withErr(res, ClassGit, err)
		}
		if err := p.CreateBranch(ctx, token, d.Branch, head); err != nil {
			return withErr(res, ClassGit, fmt.Errorf("create branch: %w", err))
		}
		created = true
	}
	msg := git.Trailers{Feature: d.Feature, Req: res.Requirements, Agent: true, Initiator: d.Initiator, Task: d.ID.String(), Release: d.Release}.
		Message(commitSubject(d))
	sha, err := p.Commit(ctx, token, d.Branch, msg, changes)
	// FTR.HMR.DEV-0004 R3: a branch the provider has just created may not be
	// readable yet; a 404 then is a lag of the provider, not a missing branch.
	for i := 0; created && errors.Is(err, git.ErrNotFound) && i < len(newBranchRetries); i++ {
		select {
		case <-ctx.Done():
			return withErr(res, ClassGit, ctx.Err())
		case <-time.After(newBranchRetries[i]):
		}
		sha, err = p.Commit(ctx, token, d.Branch, msg, changes)
	}
	if err != nil {
		return withErr(res, ClassGit, fmt.Errorf("commit to %s: %w", d.Branch, err))
	}
	res.Branch, res.HeadSHA = d.Branch, sha
	number := d.Input.PRNumber
	if number == 0 {
		title := fmt.Sprintf("%s %s: %s", d.Feature, d.Service, d.FeatureTitle)
		pr, err := p.CreatePR(ctx, token, d.Branch, base, title, prBody(d, res))
		if err != nil {
			return withErr(res, ClassGit, fmt.Errorf("create PR: %w", err))
		}
		// FTR.HMR.DEV-0004 R2: the row of the PR shows its title, not the answer of the agent.
		number, res.PRURL, res.PRTitle = pr.Number, pr.URL, title
		if d.Autonomy == "pr" && len(d.Reviewers) > 0 {
			if err := p.RequestReview(ctx, token, number, d.Reviewers); err != nil {
				slog.Warn("request review failed", "err", err)
			}
		}
	} else {
		if pr, err := p.GetPR(ctx, token, number); err == nil {
			res.PRURL = pr.URL
		}
		if d.Type == TaskReview {
			_ = p.CommentPR(ctx, token, number, "Hammurapi agent addressed the review: "+strings.TrimSpace(text))
		}
		// FTR.HMR.CMN-0008 R15: the description of an open PR stays as it is.
		if w := checkWarnings(res); w != "" {
			if err := p.CommentPR(ctx, token, number, w); err != nil {
				slog.Warn("comment the check on a PR failed", "err", err)
			}
		}
	}
	res.PRNumber = number
	res.Status = "succeeded"
	return res
}

// newBranchRetries are the pauses before the commit is retried after a 404 on
// a branch just created; a variable for the tests.
var newBranchRetries = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

func withErr(res Result, class string, err error) Result {
	res.Status, res.ErrorClass, res.Error = "failed", class, err.Error()
	return res
}
