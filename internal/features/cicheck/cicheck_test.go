package cicheck

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
)

// FTR.HMR.SPEC-0002 arch A8: the include of the file of the check in .gitlab-ci.yml keeps the rest of the text.
func TestAddInclude(t *testing.T) {
	const path = ".gitlab/speclint.gitlab-ci.yml"
	for _, tc := range []struct {
		name, in, want string
		ok             bool
	}{
		{"no file", "", "include:\n  - local: " + path + "\n", true},
		{"no include", "stages: [test]\n# a comment\n", "include:\n  - local: " + path + "\n\nstages: [test]\n# a comment\n", true},
		{"a block list", "include:\n    - project: a/b\n      file: x.yml\nstages: [test]\n",
			"include:\n    - local: " + path + "\n    - project: a/b\n      file: x.yml\nstages: [test]\n", true},
		{"already there", "include:\n  - local: " + path + "\n", "include:\n  - local: " + path + "\n", true},
		{"a string", "include: 'other.yml'\n", "", false},
		{"a flow list", "include: [a.yml]\n", "", false},
		{"a mapping", "include:\n  local: a.yml\n", "", false},
	} {
		got, ok := addInclude([]byte(tc.in), path)
		if ok != tc.ok || (ok && string(got) != tc.want) {
			t.Errorf("%s: %v %q", tc.name, ok, got)
		}
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(2, time.Minute)
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }
	id := uuid.New().String()
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow(id); !ok {
			t.Fatal("within the limit")
		}
	}
	if ok, wait := l.allow(id); ok || wait != time.Minute {
		t.Fatalf("over the limit: %v %v", ok, wait)
	}
	if ok, _ := l.allow(uuid.New().String()); !ok {
		t.Fatal("another token has its own window")
	}
	now = now.Add(time.Minute)
	if ok, _ := l.allow(id); !ok {
		t.Fatal("a new window")
	}
}

func TestBearer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for h, want := range map[string]string{"Bearer hmr_ci_abc": "hmr_ci_abc", "bearer hmr_ci_abc ": "hmr_ci_abc", "Bearer a.b.c": "a.b.c",
		"Bearer hmr_svc_abc": "", "Bearer other": "", "Basic hmr_ci_abc": "", "": ""} {
		r.Header.Set("Authorization", h)
		if got := bearer(r); got != want {
			t.Errorf("%q: %q", h, got)
		}
	}
	tok, err := newToken()
	if err != nil || !strings.HasPrefix(tok, tokenPrefix) || len(tok) != len(tokenPrefix)+52 {
		t.Fatalf("token %q %v", tok, err)
	}
}

// FTR.HMR.SPEC-0002 arch §3.7: a GitHub App without Workflows: write.
func TestProviderError(t *testing.T) {
	var ae *apperr.Error
	err := providerError("github", &git.APIError{Provider: "github", Status: 403,
		Message: "refusing to allow a GitHub App to create or update workflow `.github/workflows/speclint.yml` without `workflows` permission"})
	if !errors.As(err, &ae) || ae.Code != "provider_forbidden" || ae.Status != 403 || ae.Details["missing"] != "workflows" {
		t.Fatalf("got %v", err)
	}
	// the rights of the user: no permission of the app is named
	if err := providerError("github", &git.APIError{Provider: "github", Status: 403, Message: "Resource not accessible by integration"}); !errors.As(err, &ae) || ae.Details["missing"] != nil {
		t.Fatalf("got %v", err)
	}
	if err := providerError("gitlab", &git.APIError{Provider: "gitlab", Status: 422, Message: "x"}); !errors.As(err, &ae) || ae.Code != "provider_refused" {
		t.Fatalf("got %v", err)
	}
}
