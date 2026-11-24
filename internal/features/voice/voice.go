// Package voice transcribes push-to-talk recordings. The transcript goes back
// to the user for review; nothing is sent to the agent here.
package voice

import (
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/whisper"
)

const maxAudioBytes = 25 << 20

// Routes mounts POST /chat/voice.
func Routes(r chi.Router, t whisper.Transcriber) {
	r.Post("/chat/voice", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		if _, err := httpx.MustPrincipal(r); err != nil {
			return err
		}
		// a recording above the limit is refused before the multipart reader streams it
		if r.ContentLength > maxAudioBytes {
			return apperr.TooLarge("audio_too_large", "the recording is too large").With("maxBytes", maxAudioBytes)
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxAudioBytes)
		file, hdr, err := r.FormFile("audio")
		if err != nil {
			return apperr.BadRequest("invalid_upload", "multipart field 'audio' is required")
		}
		defer file.Close()
		text, err := t.Transcribe(r.Context(), file, hdr.Filename)
		if err != nil {
			slog.ErrorContext(r.Context(), "transcription failed", "err", err)
			return apperr.Unavailable("transcription_failed", "speech recognition is unavailable")
		}
		if text == "" {
			return apperr.Unprocessable("speech_not_recognized", "no speech was recognized")
		}
		httpx.JSON(w, 200, map[string]string{"transcript": text})
		return nil
	}))
}
