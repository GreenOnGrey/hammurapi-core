// Package featuretopics is the topic of a feature in the chat (FTR.HMR.DEV-0001
// R15, R16, R20): every user has his own topic of the personal agent in Nabu per
// feature, shared by all its gates. The topic is created by the first message
// only; until then the Specification tab shows a draft.
package featuretopics

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
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/nabuconn"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/topics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Service serves the topics of features.
type Service struct {
	Pool  *pgxpool.Pool
	Store specdata.Store
	// Nabu is nil without Nabu: the topics are unavailable.
	Nabu *nabu.Client
	// Email returns the email of a user ("" — none).
	Email func(ctx context.Context, userID uuid.UUID) string
}

// View is GET /features/{uniqueId}/topic.
type View struct {
	ConversationID *string `json:"conversationId"`
	Title          string  `json:"title"`
}

// SendInput is POST /features/{uniqueId}/topic/messages.
type SendInput struct {
	Text          string   `json:"text"`
	AttachmentIDs []string `json:"attachmentIds"`
	// Area is the open gate: the agent gets it as the context (R15).
	Area string `json:"area"`
}

// Sent is the answer of POST /features/{uniqueId}/topic/messages.
type Sent struct {
	ConversationID string          `json:"conversationId"`
	Message        json.RawMessage `json:"message"`
}

func (s *Service) prepare(ctx context.Context, p *domain.Principal, key string) (*specdata.Feature, string, error) {
	if s.Nabu == nil {
		return nil, "", apperr.Unavailable("nabu_unavailable", "the agent is temporarily unavailable")
	}
	f, err := s.Store.FeatureByUniqueID(ctx, key)
	if errors.Is(err, specdata.ErrNotFound) {
		return nil, "", apperr.NotFound("feature_not_found", "feature not found")
	}
	if err != nil {
		return nil, "", err
	}
	email := s.Email(ctx, p.UserID)
	if email == "" {
		return nil, "", apperr.Conflict("email_required", "the user has no email: the agent of Nabu is reached by email")
	}
	return f, email, nil
}

// ConversationOf returns the topic of the user for the feature ("" — none).
func ConversationOf(ctx context.Context, q postgres.Querier, userID, featureID uuid.UUID) (string, error) {
	return topics.Feature.ConversationOf(ctx, q, userID, featureID)
}

// Forget drops the link to a topic that is gone in Nabu.
func Forget(ctx context.Context, q postgres.Querier, conversationID string) {
	topics.Feature.Forget(ctx, q, conversationID)
}

// Topic returns the topic of the user; a topic archived or gone in Nabu is
// forgotten and the screen shows the draft again.
func (s *Service) Topic(ctx context.Context, p *domain.Principal, key string) (*View, error) {
	f, email, err := s.prepare(ctx, p, key)
	if err != nil {
		return nil, err
	}
	v := &View{Title: topics.Title(f.UniqueID, f.Title)}
	conv, err := ConversationOf(ctx, s.Pool, p.UserID, f.ID)
	if err != nil || conv == "" {
		return v, err
	}
	c, err := s.Nabu.Conversation(ctx, email, conv)
	switch {
	case nabu.IsNotFound(err) || err == nil && c.ArchivedAt != nil:
		Forget(ctx, s.Pool, conv)
		return v, nil
	case err != nil:
		return nil, nabuconn.MapErr(err)
	}
	v.ConversationID = &conv
	return v, nil
}

// Send writes a message of the user to the topic and creates the topic with
// the first one. A repeat after a failed send finds the link.
func (s *Service) Send(ctx context.Context, p *domain.Principal, key string, in SendInput) (*Sent, error) {
	f, email, err := s.prepare(ctx, p, key)
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
	msgContext := map[string]any{"type": "feature", "key": f.UniqueID, "featureId": f.ID}
	if a, err := domain.ParseArea(in.Area); err == nil {
		msgContext["area"] = a
	}
	for attempt := 0; ; attempt++ {
		conv, err := topics.Feature.Ensure(ctx, s.Pool, s.Nabu, p.UserID, email, f.ID, topics.Title(f.UniqueID, f.Title), metrics.FeatureTopicsCreated.Inc)
		if err != nil {
			return nil, err
		}
		var out struct {
			Message json.RawMessage `json:"message"`
		}
		err = s.Nabu.PostMessage(ctx, email, conv, map[string]any{"text": in.Text, "attachmentIds": in.AttachmentIDs, "context": msgContext}, &out)
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

// Archive archives every topic of a feature that left the specification phase
// or is deleted (R20) and forgets the links. It is best effort: a topic that
// failed to archive keeps its link and is logged.
func (s *Service) Archive(ctx context.Context, featureID uuid.UUID) {
	if s == nil || s.Nabu == nil {
		return
	}
	links, err := topics.Feature.All(ctx, s.Pool, featureID)
	if err != nil {
		slog.WarnContext(ctx, "topics of a feature", "feature_id", featureID, "err", err)
		return
	}
	for _, l := range links {
		err := topics.ErrNoEmail
		if email := s.Email(ctx, l.User); email != "" {
			err = s.Nabu.ArchiveTopic(ctx, email, l.Conv)
		}
		if err != nil && !nabu.IsNotFound(err) {
			slog.WarnContext(ctx, "archive the topic of a feature", "feature_id", featureID, "conversation_id", l.Conv, "err", err)
			continue
		}
		Forget(ctx, s.Pool, l.Conv)
	}
}

// Routes mounts the endpoints of the topic of a feature.
func (s *Service) Routes(r chi.Router) {
	r.Get("/features/{uniqueId}/topic", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		v, err := s.Topic(r.Context(), p, chi.URLParam(r, "uniqueId"))
		if err != nil {
			return err
		}
		httpx.JSON(w, 200, v)
		return nil
	}))
	r.Post("/features/{uniqueId}/topic/messages", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		var in SendInput
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
		out, err := s.Send(r.Context(), p, chi.URLParam(r, "uniqueId"), in)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusCreated, out)
		return nil
	}))
}
