package imports

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
)

// Routes mounts /imports.
func (s *Service) Routes(r chi.Router) {
	r.Post("/imports", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return err
		}
		// a body above the limit is refused before the multipart reader streams it: the client used to send the whole
		// archive to learn that it does not fit
		if r.ContentLength > s.cfg.MaxBytes+1<<20 {
			return apperr.TooLarge("archive_too_large", "the archive is too large").With("maxBytes", s.cfg.MaxBytes)
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBytes+1<<20)
		file, hdr, err := r.FormFile("file")
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				return apperr.TooLarge("archive_too_large", "the archive is too large").With("maxBytes", s.cfg.MaxBytes)
			}
			return apperr.BadRequest("invalid_upload", "multipart field 'file' is required")
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, s.cfg.MaxBytes+1))
		if err != nil {
			return err
		}
		j, err := s.Upload(r.Context(), p, hdr.Filename, data)
		if err != nil {
			return err
		}
		httpx.JSON(w, http.StatusCreated, j)
		return nil
	}))
	job := func(fn func(w http.ResponseWriter, r *http.Request) (any, error)) http.HandlerFunc {
		return httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
			res, err := fn(w, r)
			if err != nil {
				return err
			}
			if res == nil {
				httpx.NoContent(w)
			} else {
				httpx.JSON(w, 200, res)
			}
			return nil
		})
	}
	r.Get("/imports/{id}", job(func(w http.ResponseWriter, r *http.Request) (any, error) {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return nil, err
		}
		id, err := httpx.ParamUUID(r, "id")
		if err != nil {
			return nil, err
		}
		return s.Get(r.Context(), p, id)
	}))
	r.Post("/imports/{id}/revalidate", job(func(w http.ResponseWriter, r *http.Request) (any, error) {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return nil, err
		}
		id, err := httpx.ParamUUID(r, "id")
		if err != nil {
			return nil, err
		}
		return s.Revalidate(r.Context(), p, id)
	}))
	r.Post("/imports/{id}/confirm", job(func(w http.ResponseWriter, r *http.Request) (any, error) {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return nil, err
		}
		id, err := httpx.ParamUUID(r, "id")
		if err != nil {
			return nil, err
		}
		return s.Confirm(r.Context(), p, id)
	}))
	r.Delete("/imports/{id}", job(func(w http.ResponseWriter, r *http.Request) (any, error) {
		p, err := httpx.MustPrincipal(r)
		if err != nil {
			return nil, err
		}
		id, err := httpx.ParamUUID(r, "id")
		if err != nil {
			return nil, err
		}
		return nil, s.Cancel(r.Context(), p, id)
	}))
}
