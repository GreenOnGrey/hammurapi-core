package gategen

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/featuretopics"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// The result of a run goes to the topic of every author of its instructions as
// a notice of Nabu (FTR.HMR.DEV-0001 R9, R18, R19, tech §5.7).

// EffectNotice sends a notice of the writer to a topic.
const EffectNotice = "spec.notice"

// Kinds of notices (payload.kind).
const (
	NoticeRevision = "spec.revision"
	NoticeFailed   = "spec.failed"
	NoticeConflict = "spec.conflict"
)

// NoticePayload is the payload of spec.notice.
type NoticePayload struct {
	UserID         uuid.UUID `json:"userId"`
	ConversationID string    `json:"conversationId"`
	Kind           string    `json:"kind"`
	FeatureKey     string    `json:"featureKey"`
	Areas          []string  `json:"areas"`
	OtherAreas     []string  `json:"otherAreas"`
	RequestIDs     []string  `json:"requestIds"`
	CoAuthors      []string  `json:"coAuthors"`
	Error          string    `json:"error,omitempty"`
	Text           string    `json:"text"`
}

func authorName(r SpecRequest) string {
	if r.Author != nil {
		return *r.Author
	}
	return "—"
}

type noticeTarget struct {
	id   uuid.UUID
	name string
	conv string
	reqs []SpecRequest
}

// targets groups the instructions by author; an author without a topic is left out.
func targets(ctx context.Context, tx pgx.Tx, f *specdata.Feature, reqs []SpecRequest) ([]*noticeTarget, error) {
	var order []*noticeTarget
	by := map[uuid.UUID]*noticeTarget{}
	for _, r := range reqs {
		a := by[r.AuthorID]
		if a == nil {
			a = &noticeTarget{id: r.AuthorID, name: authorName(r)}
			by[r.AuthorID] = a
			order = append(order, a)
		}
		if r.ConversationID != nil {
			a.conv = r.ConversationID.String()
		}
		a.reqs = append(a.reqs, r)
	}
	out := order[:0]
	for _, a := range order {
		if a.conv == "" {
			conv, err := featuretopics.ConversationOf(ctx, tx, a.id, f.ID)
			if err != nil {
				return nil, err
			}
			if a.conv = conv; conv == "" {
				metrics.NabuNotices.WithLabelValues("gone").Inc()
				continue
			}
		}
		out = append(out, a)
	}
	return out, nil
}

func requestIDs(reqs []SpecRequest) []string {
	out := make([]string, len(reqs))
	for i, r := range reqs {
		out[i] = r.ID.String()
	}
	return out
}

func languageOf(ctx context.Context, tx pgx.Tx, id uuid.UUID) string {
	var lang string
	_ = tx.QueryRow(ctx, `SELECT language FROM users WHERE id = $1`, id).Scan(&lang)
	return lang
}

// revisionNotices tells the authors of the instructions what changed and the
// initiator of tech and qa what contradicts them; automatic runs without an
// instruction send no result: it is seen on the page (tech §5.7).
func revisionNotices(ctx context.Context, tx pgx.Tx, f *specdata.Feature, req Request, reqs []SpecRequest, g Generated, changed []domain.Area) ([]workflows.Effect, error) {
	var out []workflows.Effect
	ts, err := targets(ctx, tx, f, reqs)
	if err != nil {
		return nil, err
	}
	for _, a := range ts {
		// a gate above the current stage is named apart: it has to be approved again (R18)
		var mine, other []domain.Area
		stage := slices.Index(stageOrder, stageOf(f))
		for _, c := range changed {
			if i := slices.Index(stageOrder, string(c)); i >= 0 && i < stage {
				other = append(other, c)
			} else {
				mine = append(mine, c)
			}
		}
		var summary []string
		for _, c := range changed {
			if s := strings.TrimSpace(g.Results[c].ChangeSummary); s != "" {
				summary = append(summary, s)
			}
		}
		co := []string{}
		for _, o := range ts {
			if o.id != a.id {
				co = append(co, o.name)
			}
		}
		p := NoticePayload{UserID: a.id, ConversationID: a.conv, Kind: NoticeRevision, FeatureKey: f.UniqueID, Areas: areaStrings(mine), OtherAreas: areaStrings(other),
			RequestIDs: requestIDs(a.reqs), CoAuthors: co, Text: RevisionText(languageOf(ctx, tx, a.id), mine, other, strings.Join(summary, "\n"), co, f.ApprovalRequired)}
		out = append(out, workflows.Effect{Type: EffectNotice, Payload: p, Key: "notice-" + a.id.String()})
	}
	for _, area := range []domain.Area{domain.AreaTech, domain.AreaQA} {
		s, ok := g.Results[area]
		if !ok || strings.TrimSpace(s.Conflicts) == "" || req.Initiator == uuid.Nil {
			continue
		}
		conv, err := featuretopics.ConversationOf(ctx, tx, req.Initiator, f.ID)
		if err != nil {
			return nil, err
		}
		if conv == "" {
			metrics.NabuNotices.WithLabelValues("gone").Inc()
			continue
		}
		p := NoticePayload{UserID: req.Initiator, ConversationID: conv, Kind: NoticeConflict, FeatureKey: f.UniqueID, Areas: []string{string(area)},
			OtherAreas: []string{}, RequestIDs: []string{}, CoAuthors: []string{}, Text: ConflictText(languageOf(ctx, tx, req.Initiator), area, s.Conflicts)}
		out = append(out, workflows.Effect{Type: EffectNotice, Payload: p, Key: "conflict-" + string(area)})
	}
	return out, nil
}

func failureNotices(ctx context.Context, tx pgx.Tx, f *specdata.Feature, reqs []SpecRequest, reason string) ([]workflows.Effect, error) {
	ts, err := targets(ctx, tx, f, reqs)
	if err != nil {
		return nil, err
	}
	var out []workflows.Effect
	for _, a := range ts {
		p := NoticePayload{UserID: a.id, ConversationID: a.conv, Kind: NoticeFailed, FeatureKey: f.UniqueID, Areas: []string{}, OtherAreas: []string{},
			RequestIDs: requestIDs(a.reqs), CoAuthors: []string{}, Error: reason, Text: FailureText(languageOf(ctx, tx, a.id), reason)}
		out = append(out, workflows.Effect{Type: EffectNotice, Payload: p, Key: "notice-" + a.id.String()})
	}
	return out, nil
}

func areaStrings(l []domain.Area) []string {
	out := make([]string, 0, len(l))
	for _, a := range l {
		out = append(out, string(a))
	}
	return out
}

var areaNamesRu = map[domain.Area]string{domain.AreaProduct: "Продукт", domain.AreaDesign: "Дизайн", domain.AreaArch: "Архитектура",
	domain.AreaTech: "Техспецификация", domain.AreaQA: "Тестирование"}

var areaNamesEn = map[domain.Area]string{domain.AreaProduct: "Product", domain.AreaDesign: "Design", domain.AreaArch: "Architecture",
	domain.AreaTech: "Tech specification", domain.AreaQA: "Testing"}

func namesOf(l []domain.Area, names map[domain.Area]string) string {
	out := make([]string, 0, len(l))
	for _, a := range l {
		out = append(out, names[a])
	}
	return strings.Join(out, ", ")
}

// noticeTexts are the texts of the notices in a language.
type noticeTexts struct {
	names                                                     map[domain.Area]string
	updated, taken, together, alsoApprove, also, approveAgain string
	failed, noAgent, conflict                                 string
}

var ruTexts = noticeTexts{names: areaNamesRu, updated: "**Спецификация обновлена · %s**", taken: "**Поручение принято: документы пока не менялись**",
	together: "Вместе с поручениями: %s", alsoApprove: "Также изменён %s: нужно заново согласовать", also: "Также изменён %s", approveAgain: "%s нужно заново согласовать",
	failed: "**Не удалось изменить спецификацию**\n\n%s. Документы не изменились.", noAgent: "Служебный агент не назначен",
	conflict: "**В разделе %s есть противоречие**\n\n%s\n\nРешите его поручением."}

var enTexts = noticeTexts{names: areaNamesEn, updated: "**Specification updated · %s**", taken: "**Instruction taken: no document changed yet**",
	together: "Together with the instructions of: %s", alsoApprove: "Also changed %s: it has to be approved again", also: "Also changed %s", approveAgain: "%s has to be approved again",
	failed: "**The specification could not be changed**\n\n%s. The documents did not change.", noAgent: "No service agent is assigned",
	conflict: "**A contradiction in %s**\n\n%s\n\nSettle it with an instruction."}

// textsOf: Russian or English, the other languages of the interface get English (as in Discovery).
func textsOf(lang string) noticeTexts {
	if strings.HasPrefix(lang, "ru") {
		return ruTexts
	}
	return enTexts
}

// RevisionText is the text of a result in the language of its user. When only
// gates above the stage changed, they are the news; otherwise they are named apart (R18).
func RevisionText(lang string, areas, other []domain.Area, summary string, coAuthors []string, approval bool) string {
	x := textsOf(lang)
	onlyAbove := len(areas) == 0
	if onlyAbove {
		areas = other
	}
	var b strings.Builder
	if len(areas) > 0 {
		fmt.Fprintf(&b, x.updated, namesOf(areas, x.names))
	} else {
		b.WriteString(x.taken)
	}
	if s := strings.TrimSpace(summary); s != "" {
		b.WriteString("\n\n" + s)
	}
	for _, a := range other {
		line := x.also
		switch {
		case onlyAbove && !approval:
			continue
		case onlyAbove:
			line = x.approveAgain
		case approval:
			line = x.alsoApprove
		}
		b.WriteString("\n\n")
		fmt.Fprintf(&b, line, x.names[a])
	}
	if len(coAuthors) > 0 {
		b.WriteString("\n\n")
		fmt.Fprintf(&b, x.together, strings.Join(coAuthors, ", "))
	}
	return b.String()
}

// FailureText is the text of a failed run.
func FailureText(lang, reason string) string {
	x := textsOf(lang)
	if reason == ReasonNoAgent {
		reason = x.noAgent
	}
	return fmt.Sprintf(x.failed, strings.TrimSuffix(strings.TrimSpace(reason), "."))
}

// ConflictText is the text about a contradiction found by tech or qa (R9).
func ConflictText(lang string, area domain.Area, conflicts string) string {
	x := textsOf(lang)
	return fmt.Sprintf(x.conflict, x.names[area], strings.TrimSpace(conflicts))
}

// NoticeEffects executes spec.notice in the worker.
type NoticeEffects struct {
	Q postgres.Querier
	// Nabu is nil without Nabu: notices are skipped.
	Nabu *nabu.Client
	// Email returns the email of a user ("" — none).
	Email func(ctx context.Context, userID uuid.UUID) string
}

// Do sends the notice; a repeat after a timeout writes one message, a topic
// gone in Nabu is forgotten.
func (e *NoticeEffects) Do(ctx context.Context, run workflows.RunRef, payload json.RawMessage) ([]workflows.NewEvent, error) {
	var p NoticePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, workflows.Permanent(err)
	}
	if e.Nabu == nil {
		return nil, nil
	}
	email := e.Email(ctx, p.UserID)
	if email == "" {
		return nil, nil
	}
	data := map[string]any{"kind": p.Kind, "featureKey": p.FeatureKey, "areas": p.Areas, "otherAreas": p.OtherAreas, "requestIds": p.RequestIDs, "coAuthors": p.CoAuthors}
	if p.Error != "" {
		data["error"] = p.Error
	}
	key := "spec-notice:" + run.ID.String() + ":" + p.UserID.String() + ":" + p.Kind
	if len(p.Areas) > 0 {
		key += ":" + p.Areas[0]
	}
	err := e.Nabu.PostNotice(ctx, email, p.ConversationID, nabu.Notice{Text: p.Text, Payload: data, IdempotencyKey: key})
	switch {
	case nabu.IsNotFound(err):
		metrics.NabuNotices.WithLabelValues("gone").Inc()
		featuretopics.Forget(ctx, e.Q, p.ConversationID)
		return nil, nil
	case err != nil:
		metrics.NabuNotices.WithLabelValues("error").Inc()
		return nil, err
	}
	metrics.NabuNotices.WithLabelValues("sent").Inc()
	return nil, nil
}
