package nabuconn

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
)

// DelegationHeader carries the email of the user Nabu acts for (tech §3.6).
const DelegationHeader = "Nabu-On-Behalf-Of"

// MCPHandler serves the MCP of Hammurapi to the personal agents of Nabu
// (R7, tech §3.6): the JWT of Nabu is checked by its JWKS, the call runs with
// the rights of the user named by Nabu-On-Behalf-Of; an unknown user is
// created without roles. Without Nabu every call is 503 agent_disabled.
type MCPHandler struct {
	Verifier *nabu.Verifier // nil without NABU_URL
	Server   *mcp.Server
	// EnsureUser finds or creates the user by email.
	EnsureUser func(ctx context.Context, email string) (uuid.UUID, error)
}

func (h *MCPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Verifier == nil {
		httpx.Error(w, r, apperr.Unavailable("agent_disabled", "the agent is not connected: Hammurapi works without the agent"))
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, err := h.Verifier.Verify(r.Context(), tok)
	if err != nil {
		httpx.Error(w, r, apperr.Unauthorized("invalid_token", err.Error()))
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.Header.Get(DelegationHeader)))
	if email == "" {
		// Personal calls are always on behalf of a user (NB-07).
		httpx.Error(w, r, apperr.Forbidden("delegation_required", DelegationHeader+" is required"))
		return
	}
	if claims.Email != "" && !strings.EqualFold(claims.Email, email) {
		httpx.Error(w, r, apperr.Forbidden("delegation_mismatch", "the token was issued for another user"))
		return
	}
	uid, err := h.EnsureUser(r.Context(), email)
	if err != nil {
		var ae *apperr.Error
		if !errors.As(err, &ae) {
			slog.ErrorContext(r.Context(), "nabu delegation", "err", err)
		}
		httpx.Error(w, r, err)
		return
	}
	h.Server.Serve(w, r, mcp.Grant{UserID: uid, Mode: mcp.ModeNabu})
}
