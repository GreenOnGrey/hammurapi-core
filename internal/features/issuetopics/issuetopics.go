// Package issuetopics is the topic of an issue in the chat (FTR.HMR.DISC-0001
// R1–R5, tech §3.1): every user has his own topic of the personal agent in
// Nabu per issue. The topic is created by the first message only; until then
// the screen of the issue shows a draft. Hammurapi keeps the link "user +
// issue → topic", so it survives the move of the issue to another domain.
package issuetopics

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/nabuconn"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/topics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// MaxTitle bounds the title of a topic in Nabu.
const MaxTitle = topics.MaxTitle

// Title is the title of the topic of an issue: "<key> — <title>", at most
// MaxTitle characters with an ellipsis (TP-10).
func Title(key, title string) string { return topics.Title(key, title) }

// Service serves the topics of issues.
type Service struct {
	Pool *pgxpool.Pool
	// Nabu is nil without Nabu: the topics are unavailable (R22).
	Nabu *nabu.Client
	// Email returns the email of a user ("" — none).
	Email func(ctx context.Context, userID uuid.UUID) string
}

// View is GET /issues/{key}/topic.
type View struct {
	ConversationID *string `json:"conversationId"`
	Title          string  `json:"title"`
}

// SendInput is POST /issues/{key}/topic/messages.
type SendInput struct {
	Text          string   `json:"text"`
	AttachmentIDs []string `json:"attachmentIds"`
}

// Sent is the answer of POST /issues/{key}/topic/messages.
type Sent struct {
	ConversationID string          `json:"conversationId"`
	Message        json.RawMessage `json:"message"`
}

func (s *Service) prepare(ctx context.Context, p *domain.Principal, key string) (*cycledata.Issue, string, error) {
	if s.Nabu == nil {
		return nil, "", apperr.Unavailable("nabu_unavailable", "the agent is temporarily unavailable")
	}
	is, _, err := cycledata.New(s.Pool).IssueByKey(ctx, key)
	if errors.Is(err, cycledata.ErrNotFound) {
		return nil, "", apperr.NotFound("issue_not_found", "issue not found")
	}
	if err != nil {
		return nil, "", err
	}
	email := s.Email(ctx, p.UserID)
	if email == "" {
		return nil, "", apperr.Conflict("email_required", "the user has no email: the agent of Nabu is reached by email")
	}
	return is, email, nil
}

// ConversationOf returns the topic of the user for the issue ("" — none).
func ConversationOf(ctx context.Context, q postgres.Querier, userID, issueID uuid.UUID) (string, error) {
	return topics.Issue.ConversationOf(ctx, q, userID, issueID)
}

// Forget drops the link to a topic that is gone in Nabu.
func Forget(ctx context.Context, q postgres.Querier, conversationID string) {
	topics.Issue.Forget(ctx, q, conversationID)
}

// Topic returns the topic of the user for the issue; a topic archived or gone
// in Nabu is forgotten and the screen shows the draft again (R3, TP-07).
func (s *Service) Topic(ctx context.Context, p *domain.Principal, key string) (*View, error) {
	is, email, err := s.prepare(ctx, p, key)
	if err != nil {
		return nil, err
	}
	v := &View{Title: Title(is.Key, is.Title)}
	conv, err := ConversationOf(ctx, s.Pool, p.UserID, is.ID)
	if err != nil || conv == "" {
		return v, err
	}
	c, err := s.Nabu.Conversation(ctx, email, conv)
	switch {
	case nabu.IsNotFound(err) || err == nil && c.ArchivedAt != nil:
		Forget(ctx, s.Pool, conv)
		return v, nil
	case err != nil:
		return nil, nabuconn.MapErr(err) // TP-18: the link stays
	}
	v.ConversationID = &conv
	return v, nil
}

// Send writes a message of the user to the topic of the issue and creates the
// topic with the first one (R2, tech §3.1). The pair "user + issue" is locked
// while the topic is created, so two tabs never make two topics (TP-05); the
// message itself goes to Nabu after the commit, and a repeat finds the link (TP-06).
func (s *Service) Send(ctx context.Context, p *domain.Principal, key string, in SendInput) (*Sent, error) {
	is, email, err := s.prepare(ctx, p, key)
	if err != nil {
		return nil, err
	}
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" && len(in.AttachmentIDs) == 0 {
		return nil, apperr.Unprocessable("empty_message", "the message is empty").With("field", "text")
	}
	if in.AttachmentIDs == nil {
		in.AttachmentIDs = []string{}
	}
	for attempt := 0; ; attempt++ {
		conv, err := s.ensure(ctx, p.UserID, email, is)
		if err != nil {
			return nil, err
		}
		body := map[string]any{"text": in.Text, "attachmentIds": in.AttachmentIDs,
			"context": map[string]any{"type": "issue", "key": is.Key, "issueId": is.ID}}
		var out struct {
			Message json.RawMessage `json:"message"`
		}
		err = s.Nabu.PostMessage(ctx, email, conv, body, &out)
		if nabu.IsNotFound(err) && attempt == 0 {
			Forget(ctx, s.Pool, conv) // the topic was deleted in Nabu: a new one, once
			continue
		}
		if err != nil {
			return nil, nabuconn.MapErr(err)
		}
		return &Sent{ConversationID: conv, Message: out.Message}, nil
	}
}

func (s *Service) ensure(ctx context.Context, userID uuid.UUID, email string, is *cycledata.Issue) (string, error) {
	return topics.Issue.Ensure(ctx, s.Pool, s.Nabu, userID, email, is.ID, Title(is.Key, is.Title), metrics.IssueTopicsCreated.Inc)
}

// Rename gives every topic of the issue its current key and title after a
// rename or a move (R3, TP-08, TP-09). It is best effort: a topic that is
// gone is forgotten, other failures are logged and the next rename fixes them.
func (s *Service) Rename(ctx context.Context, issueID uuid.UUID) {
	if s == nil || s.Nabu == nil {
		return
	}
	is, err := cycledata.New(s.Pool).IssueByID(ctx, issueID)
	if err != nil {
		return
	}
	links, err := topics.Issue.All(ctx, s.Pool, issueID)
	if err != nil {
		slog.WarnContext(ctx, "topics of an issue", "issue", is.Key, "err", err)
		return
	}
	title := Title(is.Key, is.Title)
	for _, l := range links {
		email := s.Email(ctx, l.User)
		if email == "" {
			continue
		}
		err := s.Nabu.RenameTopic(ctx, email, l.Conv, title)
		switch {
		case nabu.IsNotFound(err):
			Forget(ctx, s.Pool, l.Conv)
		case err != nil:
			slog.WarnContext(ctx, "rename the topic of an issue", "issue", is.Key, "conversation_id", l.Conv, "err", err)
		}
	}
}

// Archive archives every topic of an issue whose research has ended — it is
// accepted, rejected or merged (R25, TP-19) — and forgets the links: a new
// message on the page of the issue starts a new topic. It is best effort: a
// topic that failed to archive keeps its link and is logged.
func (s *Service) Archive(ctx context.Context, issueID uuid.UUID) {
	if s == nil || s.Nabu == nil {
		return
	}
	is, err := cycledata.New(s.Pool).IssueByID(ctx, issueID)
	if err != nil {
		return
	}
	links, err := topics.Issue.All(ctx, s.Pool, issueID)
	if err != nil {
		slog.WarnContext(ctx, "topics of an issue", "issue", is.Key, "err", err)
		return
	}
	for _, l := range links {
		err := topics.ErrNoEmail
		if email := s.Email(ctx, l.User); email != "" {
			err = s.Nabu.ArchiveTopic(ctx, email, l.Conv)
		}
		if err != nil && !nabu.IsNotFound(err) {
			metrics.IssueTopicsArchived.WithLabelValues("error").Inc()
			slog.WarnContext(ctx, "archive the topic of an issue", "issue", is.Key, "conversation_id", l.Conv, "err", err)
			continue
		}
		metrics.IssueTopicsArchived.WithLabelValues("archived").Inc()
		Forget(ctx, s.Pool, l.Conv)
	}
}

// Routes mounts the endpoints of the topic of an issue.
func (s *Service) Routes(r chi.Router) {
	r.Get("/issues/{key}/topic", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		v, err := s.Topic(r.Context(), p, chi.URLParam(r, "key"))
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, v)
		return nil
	}))
	r.Post("/issues/{key}/topic/messages", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in SendInput
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		out, err := s.Send(r.Context(), p, chi.URLParam(r, "key"), in)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusCreated, out)
		return nil
	}))
}
