package codegen

// The changes of a run of Nabu (FTR.HMR.CMN-0007 R9, tech §7.4): the archive of
// changes turned into a commit of the bot and a PR.

import (
	"archive/tar"
	"bytes"
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

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
)

// changes reads the archive of changes of a run: the files to commit, the
// plan and what was left out.
func (c *Code) changes(ctx context.Context, t *cycledata.Task, runID string) ([]git.FileChange, string, []string, error) {
	rc, err := c.Nabu.Changes(ctx, runID)
	var ae *nabu.APIError
	if errors.As(err, &ae) && ae.Code == "no_changes" {
		return nil, "", nil, nil
	}
	if err != nil {
		return nil, "", nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxArchive))
	if err != nil {
		return nil, "", nil, err
	}
	c.keepArchive(ctx, t, b)
	return ReadChanges(bytes.NewReader(b))
}

// maxArchive bounds the archive of changes of a run read into memory.
const maxArchive = 512 << 20

// keepArchive keeps the archive of changes for «Повторить коммит» (FTR.HMR.DEV-0004 R13, arch A10):
// before the commit, so a failed commit can be repeated; an error of the store only loses the repeat.
func (c *Code) keepArchive(ctx context.Context, t *cycledata.Task, b []byte) {
	if c.Archives == nil {
		return
	}
	key := "codegen/changes/" + t.ID.String() + ".tar.gz"
	if err := c.Archives.Put(ctx, key, bytes.NewReader(b), int64(len(b)), "application/gzip"); err != nil {
		slog.WarnContext(ctx, "keep the changes of a run for a retry of the commit", "task", t.ID, "err", err)
		return
	}
	keep := c.ChangesKeep
	if keep <= 0 {
		keep = 14 * 24 * time.Hour
	}
	if _, err := c.Pool.Exec(ctx, `UPDATE agent_tasks SET changes_key = $2, changes_until = now() + $3::interval WHERE id = $1`,
		t.ID, key, fmt.Sprintf("%d seconds", int64(keep.Seconds()))); err != nil {
		slog.WarnContext(ctx, "keep the changes of a run for a retry of the commit", "task", t.ID, "err", err)
	}
	metrics.CodeChangesBytes.Add(float64(len(b)))
}

// archived reads the kept archive of changes of a task.
func (c *Code) archived(ctx context.Context, t *cycledata.Task) ([]git.FileChange, string, []string, error) {
	if c.Archives == nil || t.ChangesKey == nil {
		return nil, "", nil, errors.New("the changes of the agent are no longer kept")
	}
	rc, err := c.Archives.Get(ctx, *t.ChangesKey)
	if err != nil {
		return nil, "", nil, fmt.Errorf("read the kept changes: %w", err)
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
	var warnings, protected, deps []string
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
		if dependencyPath(p, ch.Op) { // FTR.HMR.DEV-0004 R16
			deps = append(deps, p)
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
	if len(deps) > 0 {
		sort.Strings(deps)
		warnings = append([]string{warningDependencies + listPaths(deps)}, warnings...)
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
		// The agent answered in the discussion instead of changing code; a regeneration may find the code right (FTR.HMR.DEV-0004 R18).
		if (d.Type == TaskReview || d.Input.Regenerate) && d.Input.PRNumber > 0 {
			if err := p.CommentPR(ctx, token, d.Input.PRNumber, "Hammurapi agent: "+strings.TrimSpace(text)); err != nil {
				res.step(StepComment, StepFailed, err.Error())
				return withErr(res, ClassGit, err)
			}
			res.step(StepComment, StepOK, fmt.Sprintf("#%d", d.Input.PRNumber))
			res.Status, res.PRNumber = "succeeded", d.Input.PRNumber
			return res
		}
		if d.Input.Regenerate { // no PR to answer in: the summary of the agent is the result
			res.Status = "succeeded"
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
			res.step(StepBranch, StepFailed, err.Error())
			return withErr(res, ClassGit, err)
		}
		if err := p.CreateBranch(ctx, token, d.Branch, head); err != nil {
			err = fmt.Errorf("create branch: %w", err)
			res.step(StepBranch, StepFailed, err.Error())
			return withErr(res, ClassGit, err)
		}
		created = true
		res.BranchHead = head
		res.step(StepBranch, StepOK, "created "+d.Branch)
	} else {
		res.BranchHead = h
		res.step(StepBranch, StepOK, d.Branch)
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
		err = fmt.Errorf("commit to %s: %w", d.Branch, err)
		res.step(StepCommit, StepFailed, err.Error())
		return withErr(res, ClassGit, err)
	}
	res.Branch, res.HeadSHA, res.BranchHead = d.Branch, sha, sha
	res.step(StepCommit, StepOK, fmt.Sprintf("%s · %d files", shortSHA(sha), len(changes)))
	number := d.Input.PRNumber
	if number == 0 {
		title := fmt.Sprintf("%s %s: %s", d.Feature, d.Service, d.FeatureTitle)
		pr, err := p.CreatePR(ctx, token, d.Branch, base, title, prBody(d, res))
		if err != nil {
			err = fmt.Errorf("create PR: %w", err)
			res.step(StepPR, StepFailed, err.Error())
			return withErr(res, ClassGit, err)
		}
		// FTR.HMR.DEV-0004 R2: the row of the PR shows its title, not the answer of the agent.
		number, res.PRURL, res.PRTitle = pr.Number, pr.URL, title
		res.step(StepPR, StepOK, fmt.Sprintf("created #%d", number))
		if d.Autonomy == "pr" && len(d.Reviewers) > 0 {
			if err := p.RequestReview(ctx, token, number, d.Reviewers); err != nil {
				slog.Warn("request review failed", "err", err)
			}
		}
	} else {
		if pr, err := p.GetPR(ctx, token, number); err == nil {
			res.PRURL = pr.URL
		}
		res.step(StepPR, StepOK, fmt.Sprintf("updated #%d", number))
		if d.Type == TaskReview {
			_ = p.CommentPR(ctx, token, number, "Hammurapi agent addressed the review: "+strings.TrimSpace(text))
		}
		if d.Input.Regenerate {
			_ = p.CommentPR(ctx, token, number, "Hammurapi agent regenerated the code: "+strings.TrimSpace(text))
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

// Steps of Hammurapi after the agent (FTR.HMR.DEV-0004 R8, arch §4.3): the log of the task shows them after the run.
const (
	StepBranch  = "branch"
	StepCommit  = "commit"
	StepPR      = "pr"
	StepComment = "comment"
	StepOK      = "ok"
	StepFailed  = "failed"
)

// Step is one step of Hammurapi after the agent.
type Step struct {
	Step   string    `json:"step"`
	Status string    `json:"status"`
	Detail string    `json:"detail,omitempty"`
	At     time.Time `json:"at"`
	// Retry: the step of «Повторить коммит» (R12), without a new run of the agent.
	Retry bool `json:"retry,omitempty"`
}

func (r *Result) step(name, status, detail string) {
	r.Steps = append(r.Steps, Step{Step: name, Status: status, Detail: detail, At: time.Now().UTC()})
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// newBranchRetries are the pauses before the commit is retried after a 404 on
// a branch just created; a variable for the tests.
var newBranchRetries = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

func withErr(res Result, class string, err error) Result {
	res.Status, res.ErrorClass, res.Error = "failed", class, err.Error()
	return res
}
