// Package domain holds the core vocabulary shared by all feature slices:
// areas, expert kinds, statuses, entity keys and the authenticated principal.
package domain

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"

	"github.com/google/uuid"
)

// Area is a specification area; it is also a gate.
type Area string

const (
	AreaProduct Area = "product"
	AreaDesign  Area = "design"
	AreaArch    Area = "arch"
	AreaTech    Area = "tech"
	AreaQA      Area = "qa"
)

// Areas lists all areas in the fixed approval order.
var Areas = []Area{AreaProduct, AreaDesign, AreaArch, AreaTech, AreaQA}

// Index returns the position of the area in approval order, or -1.
func (a Area) Index() int {
	for i, x := range Areas {
		if x == a {
			return i
		}
	}
	return -1
}

// Valid reports whether a is a known area.
func (a Area) Valid() bool { return a.Index() >= 0 }

// Generated reports whether the agent writes the gate (tech and qa, FTR.HMR.CMN-0002 R12–R14).
// Without the agent people write every gate (FTR.HMR.CMN-0006 R9).
func (a Area) Generated() bool { return (a == AreaTech || a == AreaQA) && !AgentDisabled() }

// ApproverKind is the expert kind that approves the area: product and design
// by product experts, arch, tech and qa by technical experts.
func (a Area) ApproverKind() ExpertKind {
	if a == AreaProduct || a == AreaDesign {
		return ExpertProduct
	}
	return ExpertTechnical
}

// ParseArea validates an area string.
func ParseArea(s string) (Area, error) {
	a := Area(s)
	if !a.Valid() {
		return "", fmt.Errorf("unknown area %q", s)
	}
	return a, nil
}

// SortAreas orders areas by approval order.
func SortAreas(as []Area) {
	sort.Slice(as, func(i, j int) bool { return as[i].Index() < as[j].Index() })
}

// ExpertKind is the kind of a domain expert.
type ExpertKind string

const (
	ExpertProduct   ExpertKind = "product"
	ExpertTechnical ExpertKind = "technical"
)

// Valid reports whether k is known.
func (k ExpertKind) Valid() bool { return k == ExpertProduct || k == ExpertTechnical }

// FeaturePhase is the lifecycle phase of a feature (solution).
type FeaturePhase string

const (
	PhaseSpec       FeaturePhase = "spec"
	PhaseCodegen    FeaturePhase = "codegen"
	PhaseValidation FeaturePhase = "validation"
	PhaseInRelease  FeaturePhase = "in_release"
	PhaseReleased   FeaturePhase = "released"
	PhaseRolledBack FeaturePhase = "rolled_back"
	PhaseDeleted    FeaturePhase = "deleted"
	// PhaseIndexed: found in the default branch of the repository and indexed
	// as implemented, without gates (FTR.HMR.CMN-0005 R4).
	PhaseIndexed FeaturePhase = "indexed"
)

// Active reports whether the feature is still in development (before a release exists).
func (p FeaturePhase) Active() bool {
	return p == PhaseSpec || p == PhaseCodegen || p == PhaseValidation
}

// GateStatus is the status of a gate.
type GateStatus string

const (
	GateDraft    GateStatus = "draft"
	GateInReview GateStatus = "in_review"
	GateApproved GateStatus = "approved"
)

// GateEventType is a gate history event.
type GateEventType string

const (
	EventCreated   GateEventType = "created"
	EventEdited    GateEventType = "edited"
	EventSubmitted GateEventType = "submitted"
	EventApproved  GateEventType = "approved"
	EventReset     GateEventType = "reset"
	EventDeleted   GateEventType = "deleted"
	EventGenerated GateEventType = "generated"
)

// IssueType is Idea or Problem.
type IssueType string

const (
	IssueIdea    IssueType = "idea"
	IssueProblem IssueType = "problem"
)

// IssueStatus is the status of an issue.
type IssueStatus string

const (
	IssueNew          IssueStatus = "new"
	IssueDiscovery    IssueStatus = "discovery"
	IssueVerification IssueStatus = "verification"
	IssueAccepted     IssueStatus = "accepted"
	IssueResolved     IssueStatus = "resolved"
	IssueRejected     IssueStatus = "rejected"
	IssueMerged       IssueStatus = "merged"
)

// Autonomy is the agent autonomy level of a service.
type Autonomy string

const (
	AutonomyPlan       Autonomy = "plan"
	AutonomyPR         Autonomy = "pr"
	AutonomyAutonomous Autonomy = "autonomous"
)

// Valid reports whether the level is known.
func (a Autonomy) Valid() bool {
	return a == AutonomyPlan || a == AutonomyPR || a == AutonomyAutonomous
}

// AgentTone is the communication tone of the user's agent.
type AgentTone string

const (
	ToneBusiness AgentTone = "business"
	ToneFriendly AgentTone = "friendly"
	ToneConcise  AgentTone = "concise"
	ToneMentor   AgentTone = "mentor"
)

// Tones lists all agent tones.
var Tones = []AgentTone{ToneBusiness, ToneFriendly, ToneConcise, ToneMentor}

// Valid reports whether t is a known tone.
func (t AgentTone) Valid() bool {
	for _, x := range Tones {
		if x == t {
			return true
		}
	}
	return false
}

// Languages supported by the interface; the first is the fallback default.
var Languages = []string{"en", "ru", "de", "es", "zh-CN"}

// ValidLanguage reports whether lang is supported.
func ValidLanguage(lang string) bool {
	for _, l := range Languages {
		if l == lang {
			return true
		}
	}
	return false
}

var keyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

// ValidKey reports whether s is a valid domain or system key.
func ValidKey(s string) bool { return keyRe.MatchString(s) }

// ─── Entity keys ────────────────────────────────────────────────────

var (
	issueKeyRe   = regexp.MustCompile(`^ISS\.([A-Z][A-Z0-9]{1,9})-(\d{4,})$`)
	featureKeyRe = regexp.MustCompile(`^FTR\.([A-Z][A-Z0-9]{1,9})\.([A-Z][A-Z0-9]{1,9})-(\d{4,})$`)
	releaseKeyRe = regexp.MustCompile(`^RLS\.([A-Z][A-Z0-9]{1,9})\.([A-Z][A-Z0-9]{1,9})-(\d{4,})$`)
	// FeatureKeyInText finds a feature key inside branch names, PR titles and commit messages.
	FeatureKeyInText = regexp.MustCompile(`FTR\.[A-Z][A-Z0-9]{1,9}\.[A-Z][A-Z0-9]{1,9}-\d{4,}`)
)

// IssueKey builds ISS.FMS-0042.
func IssueKey(domainKey string, n int) string { return fmt.Sprintf("ISS.%s-%04d", domainKey, n) }

// FeatureKey builds FTR.FMS.CAR-0007.
func FeatureKey(domainKey, systemKey string, n int) string {
	return fmt.Sprintf("FTR.%s.%s-%04d", domainKey, systemKey, n)
}

// ReleaseKey builds RLS.FMS.CAR-0003.
func ReleaseKey(domainKey, systemKey string, n int) string {
	return fmt.Sprintf("RLS.%s.%s-%04d", domainKey, systemKey, n)
}

// ParseIssueKey splits ISS.FMS-0042.
func ParseIssueKey(s string) (domainKey string, n int, ok bool) {
	m := issueKeyRe.FindStringSubmatch(s)
	if m == nil {
		return "", 0, false
	}
	n, _ = strconv.Atoi(m[2])
	return m[1], n, true
}

// ParseFeatureKey splits FTR.FMS.CAR-0007.
func ParseFeatureKey(s string) (domainKey, systemKey string, n int, ok bool) {
	m := featureKeyRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", 0, false
	}
	n, _ = strconv.Atoi(m[3])
	return m[1], m[2], n, true
}

// ParseReleaseKey splits RLS.FMS.CAR-0003.
func ParseReleaseKey(s string) (domainKey, systemKey string, n int, ok bool) {
	m := releaseKeyRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", 0, false
	}
	n, _ = strconv.Atoi(m[3])
	return m[1], m[2], n, true
}

// ─── Principal ──────────────────────────────────────────────────────

// Principal is the authenticated user with their roles, loaded per request.
//
// Roles (FTR.HMR.CMN-0002 §5): product and technical experts per domain, owners
// of services, area administrators, global administrator. Everybody reads
// everything and can create issues.
type Principal struct {
	UserID        uuid.UUID
	Username      string
	DisplayName   string
	GlobalAdmin   bool
	AreaAdmins    map[Area]bool
	Experts       map[string]map[ExpertKind]bool // domain key → kinds
	OwnedServices map[string]bool                // service keys
}

// NewPrincipal builds a principal without roles.
func NewPrincipal(id uuid.UUID, username, displayName string, globalAdmin bool) *Principal {
	return &Principal{UserID: id, Username: username, DisplayName: displayName, GlobalAdmin: globalAdmin,
		AreaAdmins: map[Area]bool{}, Experts: map[string]map[ExpertKind]bool{}, OwnedServices: map[string]bool{}}
}

// GrantExpert makes the principal an expert of a kind in a domain.
func (p *Principal) GrantExpert(domainKey string, k ExpertKind) {
	if p.Experts == nil {
		p.Experts = map[string]map[ExpertKind]bool{}
	}
	if p.Experts[domainKey] == nil {
		p.Experts[domainKey] = map[ExpertKind]bool{}
	}
	p.Experts[domainKey][k] = true
}

// GrantAreaAdmin makes the principal an administrator of an area.
func (p *Principal) GrantAreaAdmin(a Area) {
	if p.AreaAdmins == nil {
		p.AreaAdmins = map[Area]bool{}
	}
	p.AreaAdmins[a] = true
}

// GrantOwner marks the principal as owner of a service.
func (p *Principal) GrantOwner(serviceKey string) {
	if p.OwnedServices == nil {
		p.OwnedServices = map[string]bool{}
	}
	p.OwnedServices[serviceKey] = true
}

// IsExpertOf reports whether the principal is any kind of expert in the domain.
func (p *Principal) IsExpertOf(domainKey string) bool {
	return p != nil && len(p.Experts[domainKey]) > 0
}

// HasExpert reports whether the principal is an expert of kind k in the domain.
func (p *Principal) HasExpert(domainKey string, k ExpertKind) bool {
	return p != nil && p.Experts[domainKey][k]
}

// IsAnyExpert reports whether the principal is an expert anywhere.
func (p *Principal) IsAnyExpert() bool {
	if p == nil {
		return false
	}
	for _, ks := range p.Experts {
		if len(ks) > 0 {
			return true
		}
	}
	return false
}

// ExpertDomains lists domains where the principal is an expert (any kind), sorted.
func (p *Principal) ExpertDomains() []string {
	var out []string
	for d, ks := range p.Experts {
		if len(ks) > 0 {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

// CanApprove reports whether the principal approves the gate of an area in a domain.
func (p *Principal) CanApprove(domainKey string, a Area) bool {
	return p.HasExpert(domainKey, a.ApproverKind())
}

// IsAreaAdmin reports whether the principal administers an area.
func (p *Principal) IsAreaAdmin(a Area) bool { return p != nil && p.AreaAdmins[a] }

// AdminAreas lists administered areas in approval order.
func (p *Principal) AdminAreas() []Area {
	var out []Area
	for _, a := range Areas {
		if p.IsAreaAdmin(a) {
			out = append(out, a)
		}
	}
	return out
}

// IsAnyAdmin reports whether the principal is a global admin or an admin of any area.
func (p *Principal) IsAnyAdmin() bool { return p != nil && (p.GlobalAdmin || len(p.AdminAreas()) > 0) }

// Owns reports whether the principal owns a service.
func (p *Principal) Owns(serviceKey string) bool { return p != nil && p.OwnedServices[serviceKey] }
