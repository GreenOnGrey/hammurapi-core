// Package cycledata is the shared data access of the closed development cycle
// (FTR.HMR.CMN-0002): issues and Discovery, services, traceability, pull requests,
// agent tasks, CI results, validation, releases, deploys, flags, settings and
// activity. The state machines and several slices use the same rows, so the
// repository takes any Querier — the pool or a workflow transaction.
package cycledata

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// DB wraps a Querier.
type DB struct{ q postgres.Querier }

// New creates a DB over a pool or transaction.
func New(q postgres.Querier) *DB { return &DB{q: q} }

// Q exposes the underlying querier.
func (d *DB) Q() postgres.Querier { return d.q }

func nf(err error) error {
	if postgres.IsNoRows(err) {
		return ErrNotFound
	}
	return err
}

// Issue is an issue (Idea or Problem).
type Issue struct {
	ID                  uuid.UUID          `json:"-"`
	Key                 string             `json:"key"`
	DomainID            uuid.UUID          `json:"-"`
	Domain              string             `json:"domain"`
	Number              int                `json:"-"`
	Type                domain.IssueType   `json:"type"`
	Title               string             `json:"title"`
	Description         string             `json:"description"`
	Source              string             `json:"source"`
	SourceRef           json.RawMessage    `json:"sourceRef"`
	Status              domain.IssueStatus `json:"status"`
	AuthorID            *uuid.UUID         `json:"-"`
	Author              *string            `json:"author"`
	MergedInto          *string            `json:"mergedInto"`
	RejectReason        *string            `json:"rejectReason"`
	RolledBackReleaseID *uuid.UUID         `json:"-"`
	RolledBackRelease   *string            `json:"rolledBackRelease"`
	CreatedAt           time.Time          `json:"createdAt"`
	UpdatedAt           time.Time          `json:"updatedAt"`
	// Estimate is the summary of the draft estimate in the Research list
	// (FTR.HMR.DISC-0001 R17); set by ListIssues only.
	Estimate *EstimateTotals `json:"estimate,omitempty"`
}

// Measure is "how we will measure" from Discovery.
type Measure struct {
	Source string `json:"source"`
	Query  string `json:"query"`
	Target string `json:"target"`
	Window string `json:"window"`
}

// Complete reports whether every field needed for acceptance is present (R6).
func (m *Measure) Complete() bool {
	return m != nil && m.Source != "" && m.Query != "" && m.Target != "" && m.Window != ""
}

// Similar is a similar issue or feature found by Discovery.
type Similar struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Kind  string `json:"kind"` // issue | feature
}

// Discovery is the Discovery document of an issue.
type Discovery struct {
	IssueID          uuid.UUID  `json:"-"`
	Content          string     `json:"content"`
	Value            *string    `json:"value"`
	Measure          *Measure   `json:"measure"`
	MeasureCheckedAt *time.Time `json:"measureCheckedAt"`
	Similar          []Similar  `json:"similar"`
	Systems          []string   `json:"systems"`
	Services         []string   `json:"services"`
	ProblemFeature   *string    `json:"problemFeature"`
	// Estimate is the draft estimate of the work (FTR.HMR.DISC-0001 R15).
	Estimate *Estimate `json:"estimate"`
	// ChangeSummary is what the researcher changed for the instructions of
	// the revision (FTR.HMR.DISC-0001 tech §4.3).
	ChangeSummary *string   `json:"changeSummary"`
	Revision      int       `json:"revision"`
	UpdatedAt     time.Time `json:"updatedAt"`
	// RequestIDs are the instructions the revision answers (written only).
	RequestIDs []uuid.UUID `json:"-"`
}

// Revision is a stored version of a Discovery document.
type Revision struct {
	Revision      int       `json:"revision"`
	Content       string    `json:"content"`
	IsAgent       bool      `json:"isAgent"`
	Actor         *string   `json:"actor"`
	Estimate      *Estimate `json:"estimate"`
	ChangeSummary *string   `json:"changeSummary"`
	Requests      []RevReq  `json:"requests"`
	CreatedAt     time.Time `json:"createdAt"`
}

// RevReq is an instruction a revision answers, as the history shows it
// (FTR.HMR.DISC-0001 design §3.5): the author and the first characters.
type RevReq struct {
	ID           uuid.UUID `json:"id"`
	Author       *string   `json:"author"`
	Instructions string    `json:"instructions"`
}

// Service is a product service (Backstage Component).
type Service struct {
	ID                  uuid.UUID       `json:"-"`
	Key                 string          `json:"key"`
	Name                string          `json:"name"`
	SystemID            *uuid.UUID      `json:"-"`
	System              *string         `json:"system"` // DOMAIN/SYSTEM
	Domain              *string         `json:"domain"`
	Repo                string          `json:"repo"`
	OwnerRef            string          `json:"ownerRef"`
	Owners              []string        `json:"owners"`
	Autonomy            domain.Autonomy `json:"autonomy"`
	DeployOverride      json.RawMessage `json:"deployOverride"`
	OverrideFromCatalog bool            `json:"overrideFromCatalog"`
	Source              string          `json:"source"`
	CatalogRef          *string         `json:"catalogRef"`
	DeletedInCatalog    bool            `json:"deletedInCatalog"`
}

// Requirement is a requirement of the product specification (R15).
type Requirement struct {
	ID       string   `json:"id"` // R3
	Text     string   `json:"text"`
	Services []string `json:"services"`
}

// TestCase is a test case of the qa specification.
type TestCase struct {
	ID     string   `json:"id"` // QA-03
	Level  string   `json:"level"`
	ReqIDs []string `json:"requirements"`
	Title  string   `json:"title"`
}

// PR is a pull/merge request tracked by Hammurapi.
type PR struct {
	ID          uuid.UUID  `json:"id"`
	Repo        string     `json:"repo"`
	Number      int        `json:"number"`
	URL         string     `json:"url"`
	Title       string     `json:"title"`
	Branch      string     `json:"branch"`
	Kind        string     `json:"kind"` // spec | service | revert
	FeatureID   uuid.UUID  `json:"-"`
	ServiceID   *uuid.UUID `json:"-"`
	Service     *string    `json:"service"`
	ByAgent     bool       `json:"byAgent"`
	State       string     `json:"state"`  // open | merged | closed
	Review      string     `json:"review"` // none | required | changes_requested | approved | not_required
	HeadSHA     string     `json:"headSha"`
	CIStatus    *string    `json:"ciStatus"`
	MergeSHA    *string    `json:"mergeSha"`
	MergedBy    *uuid.UUID `json:"-"`
	MergedAt    *time.Time `json:"mergedAt"`
	RevertsPRID *uuid.UUID `json:"-"`
	CreatedAt   time.Time  `json:"createdAt"`
	ReqIDs      []string   `json:"requirements"`
}

// Task is an agent task executed by the runner.
type Task struct {
	ID          uuid.UUID       `json:"id"`
	Type        string          `json:"type"`   // implement | address_review | update_pr | revert | ci_setup
	Status      string          `json:"status"` // queued | running | succeeded | failed | cancelled
	FeatureID   *uuid.UUID      `json:"-"`
	ReleaseID   *uuid.UUID      `json:"-"`
	ServiceID   uuid.UUID       `json:"-"`
	Service     string          `json:"service"`
	RunID       uuid.UUID       `json:"-"`
	InitiatorID *uuid.UUID      `json:"-"`
	TokenHash   []byte          `json:"-"`
	ExecutorRef *string         `json:"-"`
	Input       json.RawMessage `json:"input"`
	Result      json.RawMessage `json:"result"`
	Progress    *string         `json:"progress"`
	Error       *string         `json:"error"`
	TokensIn    int64           `json:"tokensIn"`
	TokensOut   int64           `json:"tokensOut"`
	CreatedAt   time.Time       `json:"createdAt"`
	StartedAt   *time.Time      `json:"startedAt"`
	FinishedAt  *time.Time      `json:"finishedAt"`
}

// Release is a release (RLS).
type Release struct {
	ID             uuid.UUID       `json:"-"`
	Key            string          `json:"key"`
	SystemID       uuid.UUID       `json:"-"`
	Number         int             `json:"-"`
	FeatureID      uuid.UUID       `json:"-"`
	Feature        string          `json:"feature"`
	FeatureTitle   string          `json:"featureTitle"`
	Domain         string          `json:"domain"`
	Status         string          `json:"status"`
	Plan           ReleasePlan     `json:"plan"`
	MergeStartedBy *uuid.UUID      `json:"-"`
	MetricResult   *string         `json:"metricResult"`
	ConfirmedBy    *string         `json:"confirmedBy"`
	ConfirmedAt    *time.Time      `json:"confirmedAt"`
	RollbackReason *string         `json:"rollbackReason"`
	RolledBackBy   *string         `json:"rolledBackBy"`
	RolledBackAt   *time.Time      `json:"rolledBackAt"`
	BlockedReason  *string         `json:"blockedReason"`
	CreatedAt      time.Time       `json:"createdAt"`
	Extra          json.RawMessage `json:"-"`
}

// ReleasePlan is the merge and deploy order.
type ReleasePlan struct {
	Order []string `json:"order"`
}

// DeployRun is a deploy of one service to one environment.
type DeployRun struct {
	ID          uuid.UUID  `json:"id"`
	Environment string     `json:"environment"`
	ServiceID   uuid.UUID  `json:"-"`
	Service     string     `json:"service"`
	FeatureID   *uuid.UUID `json:"-"`
	ReleaseID   *uuid.UUID `json:"-"`
	Ref         string     `json:"ref"`
	Status      string     `json:"status"`
	Signal      *string    `json:"signal"`
	Version     *string    `json:"version"`
	RunURL      *string    `json:"runUrl"`
	Error       *string    `json:"error"`
	MarkedBy    *string    `json:"markedBy"`
	IsRollback  bool       `json:"isRollback"`
	CreatedAt   time.Time  `json:"createdAt"`
	FinishedAt  *time.Time `json:"finishedAt"`
}

// Activity is a history entry of an issue, feature or release.
type Activity struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	Actor     *string         `json:"actor"`
	IsAgent   bool            `json:"isAgent"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"createdAt"`
}

// Signature is a validation signature.
type Signature struct {
	Side     string    `json:"side"`
	User     string    `json:"user"`
	UserID   uuid.UUID `json:"-"`
	Comment  *string   `json:"comment"`
	SignedAt time.Time `json:"signedAt"`
}

// Discrepancy is a code/spec mismatch found by the agent.
type Discrepancy struct {
	ID          uuid.UUID  `json:"id"`
	ReqID       string     `json:"requirement"`
	Service     *string    `json:"service"`
	PRURL       *string    `json:"prUrl"`
	Description string     `json:"description"`
	FoundAt     time.Time  `json:"foundAt"`
	ResolvedAt  *time.Time `json:"resolvedAt"`
}
