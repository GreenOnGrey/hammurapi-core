package nabu

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func answer(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}
}

// R2: every 5xx of Nabu is ErrUnavailable, even with an error code; 4xx keep their code.
func TestDecode(t *testing.T) {
	if err := Decode(answer(500, `{"error":{"code":"internal","message":"boom"}}`), nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("5xx with a code: %v", err)
	}
	var ae *APIError
	if err := Decode(answer(503, ``), nil); !errors.Is(err, ErrUnavailable) || errors.As(err, &ae) {
		t.Fatalf("5xx: %v", err)
	}
	if err := Decode(answer(422, `{"error":{"code":"no_workspace","message":"x"}}`), nil); !errors.As(err, &ae) || ae.Code != "no_workspace" {
		t.Fatalf("4xx: %v", err)
	}
}
