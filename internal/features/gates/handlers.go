package gates

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
)

// Handlers serves /api/v1/features/{uniqueId}/gates.
type Handlers struct{ svc *Service }

// NewHandlers creates handlers.
func NewHandlers(svc *Service) *Handlers { return &Handlers{svc: svc} }

// Routes mounts the routes.
func (h *Handlers) Routes(r chi.Router) {
	r.Route("/features/{uniqueId}/gates/{area}", func(r chi.Router) {
		r.Get("/", httpx.Handler(h.meta))
		r.Get("/document", httpx.Handler(h.getDoc))
		r.Put("/document", httpx.Handler(h.putDoc))
		r.Get("/diff", httpx.Handler(h.diff))
		r.Get("/history", httpx.Handler(h.history))
		r.Get("/files", httpx.Handler(h.files))       // FTR.HMR.DEV-0003 R1
		r.Get("/files/raw", httpx.Handler(h.fileRaw)) // R3, R5
	})
	r.Get("/features/{uniqueId}/spec/changes", httpx.Handler(h.changes))
	r.Get("/features/{uniqueId}/spec/links", httpx.Handler(h.links)) // FTR.HMR.DEV-0003 R6
}

// changes is GET /features/{uniqueId}/spec/changes?area=product|arch
// (FTR.HMR.DEV-0002 tech §2.1, FTR.HMR.DEV-0003 tech §2.5): product by default.
func (h *Handlers) changes(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	area := domain.AreaProduct
	switch a := r.URL.Query().Get("area"); a {
	case "", string(domain.AreaProduct):
	case string(domain.AreaArch):
		area = domain.AreaArch
	default:
		return apperr.BadRequest("invalid_area", "area must be product or arch")
	}
	items, err := h.svc.SpecChanges(r.Context(), p, chi.URLParam(r, "uniqueId"), area)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
	return nil
}

func params(r *http.Request) (*domain.Principal, string, domain.Area, error) {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return nil, "", "", err
	}
	a, err := httpx.ParamArea(r)
	return p, chi.URLParam(r, "uniqueId"), a, err
}

func (h *Handlers) meta(w http.ResponseWriter, r *http.Request) error {
	_, uid, a, err := params(r)
	if err != nil {
		return err
	}
	m, err := h.svc.Meta(r.Context(), uid, a)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, m)
	return nil
}

func (h *Handlers) getDoc(w http.ResponseWriter, r *http.Request) error {
	p, uid, a, err := params(r)
	if err != nil {
		return err
	}
	d, err := h.svc.GetDocument(r.Context(), p, uid, a)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, d)
	return nil
}

func (h *Handlers) putDoc(w http.ResponseWriter, r *http.Request) error {
	p, uid, a, err := params(r)
	if err != nil {
		return err
	}
	var in SaveInput
	if err := httpx.Decode(r, &in); err != nil {
		return err
	}
	res, err := h.svc.SaveDocument(r.Context(), p, uid, a, in)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, res)
	return nil
}

func (h *Handlers) diff(w http.ResponseWriter, r *http.Request) error {
	p, uid, a, err := params(r)
	if err != nil {
		return err
	}
	d, err := h.svc.Diff(r.Context(), p, uid, a)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, d)
	return nil
}

func (h *Handlers) history(w http.ResponseWriter, r *http.Request) error {
	_, uid, a, err := params(r)
	if err != nil {
		return err
	}
	page, err := httpx.ParsePage(r)
	if err != nil {
		return err
	}
	l, err := h.svc.History(r.Context(), uid, a, page)
	if err != nil {
		return err
	}
	httpx.JSON(w, 200, l)
	return nil
}
