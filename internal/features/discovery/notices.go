package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/issuetopics"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// The result of a run goes to the topic of every author of its instructions
// as a notice of Nabu (FTR.HMR.DISC-0001 R10, R13, tech §5.3).

// EffectNotice sends a notice to the topic of an author.
const EffectNotice = "nabu.notice"

// Kinds of notices (payload.kind).
const (
	NoticeRevision = "discovery.revision"
	NoticeFailed   = "discovery.failed"
)

// NoticePayload is the payload of nabu.notice and, without the routing, the
// payload of the notice the web draws as the card of the result.
type NoticePayload struct {
	UserID         uuid.UUID `json:"userId"`
	ConversationID string    `json:"conversationId"`
	Kind           string    `json:"kind"`
	IssueKey       string    `json:"issueKey"`
	Revision       int       `json:"revision"`
	RequestIDs     []string  `json:"requestIds"`
	CoAuthors      []string  `json:"coAuthors"`
	Error          string    `json:"error,omitempty"`
	Text           string    `json:"text"`
}

// notices builds one effect per author of the instructions of the run: two
// instructions of one author make one notice (NT-03), the other authors are
// named (NT-02). detail is the summary of the changes or the reason of the failure.
func notices(ctx context.Context, tx pgx.Tx, runID uuid.UUID, is *cycledata.Issue, reqs []Request, kind string, rev int, detail string) ([]workflows.Effect, error) {
	type authorOf struct {
		id   uuid.UUID
		name string
		conv string
		ids  []string
	}
	var order []*authorOf
	by := map[uuid.UUID]*authorOf{}
	for _, r := range reqs {
		a := by[r.AuthorID]
		if a == nil {
			a = &authorOf{id: r.AuthorID, name: author(r)}
			by[r.AuthorID] = a
			order = append(order, a)
		}
		if r.ConversationID != nil {
			a.conv = r.ConversationID.String() // the latest topic of the author
		}
		a.ids = append(a.ids, r.ID.String())
	}
	var out []workflows.Effect
	for _, a := range order {
		if a.conv == "" { // answers given on the page of the issue (tech §5.3): the topic of the author, if any
			conv, err := issuetopics.ConversationOf(ctx, tx, a.id, is.ID)
			if err != nil {
				return nil, err
			}
			if a.conv = conv; conv == "" {
				metrics.NabuNotices.WithLabelValues("gone").Inc()
				continue
			}
		}
		co := []string{}
		for _, o := range order {
			if o.id != a.id {
				co = append(co, o.name)
			}
		}
		var lang string
		_ = tx.QueryRow(ctx, `SELECT language FROM users WHERE id = $1`, a.id).Scan(&lang)
		p := NoticePayload{UserID: a.id, ConversationID: a.conv, Kind: kind, IssueKey: is.Key, Revision: rev, RequestIDs: a.ids, CoAuthors: co,
			Text: NoticeText(lang, kind, rev, detail, co)}
		if kind == NoticeFailed {
			p.Error = detail
		}
		out = append(out, workflows.Effect{Type: EffectNotice, Payload: p, Key: "notice-" + a.id.String()})
	}
	_ = runID
	return out, nil
}

// NoticeText is the text of a notice in the language of its user (NT-09):
// Russian or English; the other languages of the interface get English.
func NoticeText(lang, kind string, rev int, detail string, coAuthors []string) string {
	ru := strings.HasPrefix(lang, "ru")
	var b strings.Builder
	switch kind {
	case NoticeRevision:
		if ru {
			fmt.Fprintf(&b, "**Анализ обновлён · ревизия %d**", rev)
		} else {
			fmt.Fprintf(&b, "**Analysis updated · revision %d**", rev)
		}
		if s := strings.TrimSpace(detail); s != "" {
			b.WriteString("\n\n" + s)
		}
		if len(coAuthors) > 0 {
			if ru {
				b.WriteString("\n\nВместе с поручениями: " + strings.Join(coAuthors, ", "))
			} else {
				b.WriteString("\n\nTogether with the instructions of: " + strings.Join(coAuthors, ", "))
			}
		}
	default:
		reason := strings.TrimSuffix(strings.TrimSpace(detail), ".")
		switch {
		case ru && rev > 0:
			fmt.Fprintf(&b, "**Исследование не удалось**\n\n%s. Документ остался на ревизии %d.", reason, rev)
		case ru:
			fmt.Fprintf(&b, "**Исследование не удалось**\n\n%s. Документа ещё нет.", reason)
		case rev > 0:
			fmt.Fprintf(&b, "**The research failed**\n\n%s. The document stays at revision %d.", reason, rev)
		default:
			fmt.Fprintf(&b, "**The research failed**\n\n%s. There is no document yet.", reason)
		}
	}
	return b.String()
}

// NoticeEffects executes nabu.notice in the worker.
type NoticeEffects struct {
	Q postgres.Querier
	// Nabu is nil without Nabu: notices are skipped.
	Nabu *nabu.Client
	// Email returns the email of a user ("" — none).
	Email func(ctx context.Context, userID uuid.UUID) string
}

// Do sends the notice; a repeat after a timeout writes one message (NT-05),
// a topic gone in Nabu is forgotten (NT-06).
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
	data := map[string]any{"kind": p.Kind, "issueKey": p.IssueKey, "revision": p.Revision, "requestIds": p.RequestIDs, "coAuthors": p.CoAuthors}
	if p.Error != "" {
		data["error"] = p.Error
	}
	err := e.Nabu.PostNotice(ctx, email, p.ConversationID, nabu.Notice{Text: p.Text, Payload: data,
		IdempotencyKey: "discovery-notice:" + run.ID.String() + ":" + p.UserID.String()})
	switch {
	case nabu.IsNotFound(err):
		metrics.NabuNotices.WithLabelValues("gone").Inc()
		issuetopics.Forget(ctx, e.Q, p.ConversationID)
		return nil, nil
	case err != nil:
		metrics.NabuNotices.WithLabelValues("error").Inc() // retried; a final failure is hammurapi_workflow_effects_failed_total
		return nil, err
	}
	metrics.NabuNotices.WithLabelValues("sent").Inc()
	return nil, nil
}
