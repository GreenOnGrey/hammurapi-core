package approvals

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Handlers serves approval endpoints.
type Handlers struct{ svc *Service }

// NewHandlers creates handlers.
func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

// Routes mounts the routes.
func (h *Handlers) Routes(r chi.Router) {
	r.Get("/approvals", httpx.Handler(h.list))
	r.Post("/features/{uniqueId}/gates/{area}/submit", httpx.Handler(h.submit))
	r.Post("/features/{uniqueId}/gates/{area}/approve", httpx.Handler(h.approve))
}

func (h *Handlers) list(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	l, err := h.svc.List(r.Context(), p, page)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, l)
	return nil
}

func (h *Handlers) submit(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	a, err := httpx.ParamArea(r)
	if err != nil {
		return err
	}
	var in struct {
		Force bool `json:"force"`
		// ForceQuestions submits a gate with open questions (FTR.HMR.DEV-0001 R14).
		ForceQuestions bool `json:"forceQuestions"`
	}
	if r.ContentLength > 0 {
		if err := httpx.Decode(r, &in); err != nil {
			return err
		}
	}
	g, err := h.svc.Submit(r.Context(), p, chi.URLParam(r, "uniqueId"), a, in.Force, in.ForceQuestions)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, specdata.ToGateDTO(*g))
	return nil
}

func (h *Handlers) approve(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	a, err := httpx.ParamArea(r)
	if err != nil {
		return err
	}
	g, err := h.svc.Approve(r.Context(), p, chi.URLParam(r, "uniqueId"), a)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, specdata.ToGateDTO(*g))
	return nil
}
