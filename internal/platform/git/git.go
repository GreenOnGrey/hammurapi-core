// Package git abstracts the instance's git provider (GitHub or GitLab).
// All repository access goes through the provider API; there is no local clone.
package git

//go:generate go tool mockgen -destination=mocks/provider.go -package=mocks . Provider,TokenSource

import (
	"context"
	"crypto/sha1" //nolint:gosec // git object ids are sha1 by definition
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Sentinel errors returned by providers.
var (
	ErrNotFound     = errors.New("git: not found")
	ErrUnauthorized = errors.New("git: token rejected")
	ErrConflict     = errors.New("git: ref changed concurrently")
)

// APIError is a provider refusal (branch protection, merge conflicts, rate limits…).
type APIError struct {
	Provider string
	Status   int
	Message  string
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: %d %s", e.Provider, e.Status, e.Message) }

// Reason returns a human-readable reason for API errors.
func Reason(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Message
	}
	return err.Error()
}

// Token is an OAuth user token.
type Token struct {
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
}

// User is the provider identity of the signed-in user.
type User struct {
	ID        string
	Username  string
	Name      string
	AvatarURL string
}

// FileChange is one file operation in a commit.
type FileChange struct {
	Path    string
	Content []byte
	Delete  bool
}

// File is file content at a ref.
type File struct {
	Content []byte
	BlobSHA string
}

// PR is a pull request (GitHub) or merge request (GitLab).
type PR struct {
	Number int
	URL    string
}

// SearchHit is a code search result.
type SearchHit struct {
	Path    string `json:"path"`
	Snippet string `json:"snippet"`
}

// PushCommit is a commit from a push webhook.
type PushCommit struct {
	SHA       string    `json:"sha"`
	Message   string    `json:"message"`
	Added     []string  `json:"added"`
	Modified  []string  `json:"modified"`
	Removed   []string  `json:"removed"`
	Timestamp time.Time `json:"timestamp"`
}

// PushEvent is a normalized push webhook (branch or tag).
type PushEvent struct {
	EventID string       `json:"eventId"`
	Repo    string       `json:"repo"`
	Branch  string       `json:"branch"`
	Tag     string       `json:"tag,omitempty"`
	After   string       `json:"after,omitempty"`
	Actor   string       `json:"actor"` // provider username of the pusher
	Commits []PushCommit `json:"commits"`
}

// PREvent is a normalized pull/merge request webhook.
type PREvent struct {
	EventID  string `json:"eventId"`
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	Action   string `json:"action"` // opened | updated | merged | closed | reopened
	Title    string `json:"title"`
	Branch   string `json:"branch"`
	Base     string `json:"base"`
	HeadSHA  string `json:"headSha"`
	MergeSHA string `json:"mergeSha,omitempty"`
	URL      string `json:"url"`
	Author   string `json:"author"`
	Actor    string `json:"actor"`
}

// ReviewEvent is a normalized review or review comment on a PR/MR.
type ReviewEvent struct {
	EventID string `json:"eventId"`
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	State   string `json:"state"` // approved | changes_requested | commented
	Body    string `json:"body"`
	Author  string `json:"author"`
}

// HookEvent is any webhook Hammurapi understands.
type HookEvent struct {
	Kind   string       `json:"kind"` // push | tag | pr | review
	Push   *PushEvent   `json:"push,omitempty"`
	PR     *PREvent     `json:"pr,omitempty"`
	Review *ReviewEvent `json:"review,omitempty"`
}

// PRInfo is the current state of a PR/MR.
type PRInfo struct {
	Number    int
	URL       string
	Title     string
	State     string // open | merged | closed
	Branch    string
	HeadSHA   string
	MergeSHA  string
	Mergeable *bool // nil: not known yet
}

// TreeEntry is a file of a repository tree (FTR.HMR.CMN-0005): its path, the git
// blob id, which changes exactly when the content does, and the size in bytes
// (-1 when the provider does not report it in the tree).
type TreeEntry struct {
	Path string
	SHA  string
	Size int64
}

// ChangedFile is a file changed by a PR/MR.
type ChangedFile struct {
	Path    string
	OldPath string
	Status  string // added | modified | removed | renamed
}

// Provider is the git provider API used by Hammurapi. Methods taking a token act
// on behalf of that user, so commits, PRs and issues are attributed to them.
type Provider interface {
	Name() string

	AuthCodeURL(state, redirectURL string) string
	Exchange(ctx context.Context, code, redirectURL string) (*Token, error)
	Refresh(ctx context.Context, refreshToken string) (*Token, error)
	CurrentUser(ctx context.Context, token string) (*User, error)

	BranchHead(ctx context.Context, token, branch string) (string, error)
	CreateBranch(ctx context.Context, token, branch, fromSHA string) error
	DeleteBranch(ctx context.Context, token, branch string) error
	GetFile(ctx context.Context, token, ref, path string) (*File, error)
	ListFiles(ctx context.Context, token, ref, dir string) ([]string, error)
	// Tree lists the files under dir at ref with their blob ids (FTR.HMR.CMN-0005).
	Tree(ctx context.Context, token, ref, dir string) ([]TreeEntry, error)
	// Blob returns the content of a blob by its id.
	Blob(ctx context.Context, token, sha string) ([]byte, error)
	Commit(ctx context.Context, token, branch, message string, changes []FileChange) (string, error)
	LatestCommit(ctx context.Context, token, ref, path string) (string, error)

	CreatePR(ctx context.Context, token, head, base, title, body string) (*PR, error)
	MergePR(ctx context.Context, token string, number int, message string) error
	ClosePR(ctx context.Context, token string, number int) error
	ApprovePR(ctx context.Context, token string, number int) error

	CreateIssue(ctx context.Context, token, title, body string) (string, error)
	SearchCode(ctx context.Context, token, query string) ([]SearchHit, error)
	CommitURL(sha string) string

	VerifyWebhook(h http.Header, body []byte, secret string) bool
	ParsePush(h http.Header, body []byte) (*PushEvent, bool, error)
	// ParseHook normalizes push, tag, PR/MR, review and comment webhooks; nil for others.
	ParseHook(h http.Header, body []byte) (*HookEvent, error)

	// ─── FTR.HMR.CMN-0002: service repositories, bot, CI/CD ───

	// Repo returns the repository this provider is bound to (owner/name).
	Repo() string
	// ForRepo returns the same provider bound to another repository of the instance.
	ForRepo(repo string) Provider
	// BotToken returns a short-lived bot token limited to the bound repository.
	BotToken(ctx context.Context) (string, error)
	// DefaultBranch returns the default branch of the bound repository.
	DefaultBranch(ctx context.Context, token string) (string, error)
	GetPR(ctx context.Context, token string, number int) (*PRInfo, error)
	UpdatePRBranch(ctx context.Context, token string, number int) error
	RequestReview(ctx context.Context, token string, number int, reviewers []string) error
	CommentPR(ctx context.Context, token string, number int, body string) error
	PRChangedFiles(ctx context.Context, token string, number int) ([]ChangedFile, error)
	CommitParents(ctx context.Context, token, sha string) ([]string, error)
	// IsAncestor reports whether ancestor is reachable from descendant.
	IsAncestor(ctx context.Context, token, ancestor, descendant string) (bool, error)
	// GroupMembers lists logins of a provider group/team (for catalog owners).
	GroupMembers(ctx context.Context, token, group string) ([]string, error)
	// RunPipeline starts a CI/CD pipeline (GitHub workflow_dispatch / GitLab pipeline API).
	RunPipeline(ctx context.Context, token, workflow, ref string, params map[string]string) (string, error)
}

// TokenSource returns a valid (refreshed if needed) provider access token of a user.
type TokenSource interface {
	Token(ctx context.Context, userID uuid.UUID) (string, error)
}

// BlobSHA computes the git blob id of content. It is identical on both providers,
// so it serves as the document version for optimistic concurrency (baseSha).
func BlobSHA(content []byte) string {
	h := sha1.New() //nolint:gosec
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// ─── Commit trailers ────────────────────────────────────────────────

// Trailers are the Hammurapi git trailers appended to commit messages.
type Trailers struct {
	Feature string
	Area    string
	Agent   bool
	Import  string
	Delete  string
	// FTR.HMR.CMN-0002
	Req       []string
	Initiator string
	Task      string
	Release   string
	Generated bool
}

// Message builds a commit message with trailers.
func (t Trailers) Message(subject string) string {
	var b strings.Builder
	b.WriteString(subject)
	b.WriteString("\n\n")
	if t.Feature != "" {
		fmt.Fprintf(&b, "Hammurapi-Feature: %s\n", t.Feature)
	}
	if t.Area != "" {
		fmt.Fprintf(&b, "Hammurapi-Area: %s\n", t.Area)
	}
	if t.Agent {
		b.WriteString("Hammurapi-Agent: true\n")
	}
	if t.Import != "" {
		fmt.Fprintf(&b, "Hammurapi-Import: %s\n", t.Import)
	}
	if t.Delete != "" {
		fmt.Fprintf(&b, "Hammurapi-Delete: %s\n", t.Delete)
	}
	if len(t.Req) > 0 {
		fmt.Fprintf(&b, "Hammurapi-Req: %s\n", strings.Join(t.Req, ", "))
	}
	if t.Initiator != "" {
		fmt.Fprintf(&b, "Hammurapi-Initiator: %s\n", t.Initiator)
	}
	if t.Task != "" {
		fmt.Fprintf(&b, "Hammurapi-Task: %s\n", t.Task)
	}
	if t.Release != "" {
		fmt.Fprintf(&b, "Hammurapi-Release: %s\n", t.Release)
	}
	if t.Generated {
		b.WriteString("Hammurapi-Generated: true\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ParseTrailers extracts Hammurapi trailers from a commit message.
func ParseTrailers(message string) Trailers {
	var t Trailers
	for _, line := range strings.Split(message, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "hammurapi-feature":
			t.Feature = v
		case "hammurapi-area":
			t.Area = v
		case "hammurapi-agent":
			t.Agent = strings.EqualFold(v, "true")
		case "hammurapi-import":
			t.Import = v
		case "hammurapi-delete":
			t.Delete = v
		case "hammurapi-req":
			for _, r := range strings.Split(v, ",") {
				if r = strings.TrimSpace(r); r != "" {
					t.Req = append(t.Req, r)
				}
			}
		case "hammurapi-initiator":
			t.Initiator = v
		case "hammurapi-task":
			t.Task = v
		case "hammurapi-release":
			t.Release = v
		case "hammurapi-generated":
			t.Generated = strings.EqualFold(v, "true")
		}
	}
	return t
}

// ─── Repository layout ──────────────────────────────────────────────

// ServiceBranch is the agent's branch in a service repository: hammurapi/<feature>/<service>.
func ServiceBranch(featureKey, service string) string {
	return "hammurapi/" + featureKey + "/" + service
}

// RevertBranch is the branch of a revert PR during a rollback.
func RevertBranch(releaseKey, service string) string {
	return "hammurapi/revert/" + releaseKey + "/" + service
}

// FeatureBranch returns the branch name of a feature.
func FeatureBranch(uniqueID string) string { return "feature/" + uniqueID }

// UniqueIDFromBranch extracts the feature id from feature/<id>.
func UniqueIDFromBranch(branch string) (string, bool) {
	id, ok := strings.CutPrefix(branch, "feature/")
	return id, ok && id != ""
}

// SpecDir is specs/<domain>/<system>/<uniqueId>/<area>.
func SpecDir(domainKey, systemKey, uniqueID, area string) string {
	return fmt.Sprintf("specs/%s/%s/%s/%s", domainKey, systemKey, uniqueID, area)
}

// DiscoveryDoc is the name of the Discovery document in the folder of materials.
const DiscoveryDoc = "discovery.md"

// DiscoveryDir is the folder of the materials of the research beside the areas
// of a feature: the Discovery document and the files people put there
// (FTR.HMR.DISC-0001 R30).
func DiscoveryDir(domainKey, systemKey, uniqueID string) string {
	return fmt.Sprintf("specs/%s/%s/%s/discovery", domainKey, systemKey, uniqueID)
}

// DiscoveryPath is the Discovery document copied into the feature folder on acceptance.
func DiscoveryPath(domainKey, systemKey, uniqueID string) string {
	return DiscoveryDir(domainKey, systemKey, uniqueID) + "/" + DiscoveryDoc
}

// LegacyDiscoveryPath is where the Discovery document lay before R30 of
// FTR.HMR.DISC-0001: read, never written.
func LegacyDiscoveryPath(domainKey, systemKey, uniqueID string) string {
	return fmt.Sprintf("specs/%s/%s/%s/%s", domainKey, systemKey, uniqueID, DiscoveryDoc)
}

// SpecPath is the spec.md path of a gate.
func SpecPath(domainKey, systemKey, uniqueID, area string) string {
	return SpecDir(domainKey, systemKey, uniqueID, area) + "/spec.md"
}

// RulePath is rules/<area>/template.md or fix-template.md.
func RulePath(area string, fix bool) string {
	if fix {
		return "rules/" + area + "/fix-template.md"
	}
	return "rules/" + area + "/template.md"
}

// SpecLocation is a file path parsed as specs/<domain>/<system>/<uniqueId>/<area>/<rest>.
type SpecLocation struct {
	Domain, System, UniqueID, Area, Rest string
}

// ParseSpecPath parses a repository path under specs/.
func ParseSpecPath(p string) (SpecLocation, bool) {
	parts := strings.SplitN(strings.TrimPrefix(p, "/"), "/", 6)
	if len(parts) < 6 || parts[0] != "specs" {
		return SpecLocation{}, false
	}
	return SpecLocation{Domain: parts[1], System: parts[2], UniqueID: parts[3], Area: parts[4], Rest: parts[5]}, true
}
