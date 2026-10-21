package specindex

import (
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
)

// Routes mounts the navigator under /api/v1/spec: any signed-in user (R16).
func (s *Service) Routes(r chi.Router) {
	r.Route("/spec", func(r chi.Router) {
		r.Get("/tree", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			tag, err := s.IndexTag(r.Context())
			if err != nil {
				return err
			}
			etag := `"` + tag + `"`
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return nil
			}
			t, err := s.Tree(r.Context(), "", "")
			if err != nil {
				return err
			}
			w.Header().Set("ETag", etag)
			httpx.JSON(w, http.StatusOK, t)
			return nil
		}))
		r.Get("/documents/{key}/{area}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			d, err := s.Document(r.Context(), chi.URLParam(r, "key"), chi.URLParam(r, "area"))
			if err != nil {
				return err
			}
			httpx.JSON(w, http.StatusOK, d)
			return nil
		}))
		r.Get("/files/{key}/{area}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			files, err := s.Files(r.Context(), chi.URLParam(r, "key"), chi.URLParam(r, "area"))
			if err != nil {
				return err
			}
			httpx.JSON(w, http.StatusOK, map[string]any{"files": files})
			return nil
		}))
		r.Get("/files/{key}/{area}/raw", httpx.Handler(s.raw))
		r.Get("/search", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			q := r.URL.Query()
			limit, _ := strconv.Atoi(q.Get("limit"))
			offset, _ := strconv.Atoi(q.Get("cursor"))
			res, err := s.Search(r.Context(), SearchQuery{Q: q.Get("q"), Domain: q.Get("domain"), System: q.Get("system"), Area: q.Get("area"),
				Offset: max(offset, 0), Limit: limit})
			if err != nil {
				return err
			}
			httpx.JSON(w, http.StatusOK, res)
			return nil
		}))
	})
}

// raw serves a file of an area (FTR.HMR.CMN-0005 arch §8): images inline with
// nosniff and a restrictive CSP; HTML in view mode as a page sandboxed by CSP
// into an opaque origin (FTR.HMR.SPEC-0001 arch §6), in preview mode as JSON for
// an iframe srcdoc (kept for one release), otherwise — like every other type —
// as a download.
func (s *Service) raw(w http.ResponseWriter, r *http.Request) error {
	mode := r.URL.Query().Get("mode")
	preview := mode == "preview" || mode == "view"
	f, data, err := s.RawFile(r.Context(), chi.URLParam(r, "key"), chi.URLParam(r, "area"), r.URL.Query().Get("name"))
	if err != nil {
		return err
	}
	if preview && int64(len(data)) > s.cfg.PreviewMaxBytes {
		return apperr.TooLarge("spec_file_too_large", "the file is too large to open; download it").With("maxBytes", s.cfg.PreviewMaxBytes)
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=300")
	switch {
	case strings.HasPrefix(f.MimeType, "image/") && previewable(f.MimeType):
		w.Header().Set("Content-Type", f.MimeType)
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		_, _ = w.Write(data)
	case f.MimeType == "text/html" && mode == "view":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": f.Name}))
		w.Header().Set("Content-Security-Policy", "sandbox allow-scripts")
		w.Header().Set("Referrer-Policy", "no-referrer")
		_, _ = w.Write(data)
	case f.MimeType == "text/html" && preview:
		httpx.JSON(w, http.StatusOK, map[string]string{"html": string(data)})
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
		_, _ = w.Write(data)
	}
	return nil
}

// AdminRoutes mounts /admin/api/v1/spec-scan: global administrators, except
// the list of missing domains and systems for any administrator (CAT-06).
func (s *Service) AdminRoutes(r chi.Router) {
	r.Route("/spec-scan", func(r chi.Router) {
		r.Get("/settings", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			p, err := httpx.MustPrincipal(r)
			if err != nil {
				return err
			}
			st, err := s.Settings(r.Context(), p)
			if err != nil {
				return err
			}
			httpx.JSON(w, http.StatusOK, st)
			return nil
		}))
		r.Put("/settings", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			p, err := httpx.MustPrincipal(r)
			if err != nil {
				return err
			}
			var in Settings
			if err := httpx.Decode(r, &in); err != nil {
				return err
			}
			st, err := s.PutSettings(r.Context(), p, in)
			if err != nil {
				return err
			}
			httpx.JSON(w, http.StatusOK, st)
			return nil
		}))
		r.Post("/runs", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			p, err := httpx.MustPrincipal(r)
			if err != nil {
				return err
			}
			id, err := s.CheckNow(r.Context(), p)
			if err != nil {
				return err
			}
			httpx.JSON(w, http.StatusAccepted, map[string]string{"runId": id.String()})
			return nil
		}))
		r.Get("/runs", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			p, err := httpx.MustPrincipal(r)
			if err != nil {
				return err
			}
			page, err := httpx.ParsePage(r)
			if err != nil {
				return err
			}
			l, err := s.Runs(r.Context(), p, page)
			if err != nil {
				return err
			}
			httpx.JSON(w, http.StatusOK, l)
			return nil
		}))
		r.Get("/issues", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			p, err := httpx.MustPrincipal(r)
			if err != nil {
				return err
			}
			status := r.URL.Query().Get("status")
			if status != "all" {
				status = "open"
			}
			items, err := s.Issues(r.Context(), p, status, r.URL.Query().Get("kind"))
			if err != nil {
				return err
			}
			httpx.JSON(w, http.StatusOK, map[string]any{"items": items})
			return nil
		}))
		r.Get("/missing-catalog", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			p, err := httpx.MustPrincipal(r)
			if err != nil {
				return err
			}
			m, err := s.Missing(r.Context(), p)
			if err != nil {
				return err
			}
			httpx.JSON(w, http.StatusOK, m)
			return nil
		}))
	})
}
