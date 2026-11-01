package codegen

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
)

func tgz(t *testing.T, entries []tar.Header, bodies map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		h := h
		body := bodies[h.Name]
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(body))
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	return b.Bytes()
}

func names(t *testing.T, b []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var out []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, h.Name)
	}
}

// FTR.HMR.CMN-0007 CT-02: the archive of the provider loses its top
// directory and .git; files already taken are not repeated (update_pr).
func TestCopyArchive(t *testing.T) {
	src := tgz(t, []tar.Header{
		{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader},
		{Name: "org-repo-abc123/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "org-repo-abc123/main.go", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "org-repo-abc123/.git/config", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "org-repo-abc123/cmd/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "org-repo-abc123/cmd/run.sh", Typeflag: tar.TypeReg, Mode: 0o4755},
		{Name: "org-repo-abc123/link", Typeflag: tar.TypeSymlink, Linkname: "main.go"},
		{Name: "org-repo-abc123/old.go", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"org-repo-abc123/main.go": "package main", "org-repo-abc123/cmd/run.sh": "#!/bin/sh", "org-repo-abc123/old.go": "x"})
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	seen := map[string]bool{"old.go": true}
	if err := copyArchive(bytes.NewReader(src), tw, seen); err != nil {
		t.Fatal(err)
	}
	_ = tw.Close()
	_ = gz.Close()
	if got := strings.Join(names(t, out.Bytes()), " "); got != "main.go cmd/ cmd/run.sh link" {
		t.Fatalf("entries: %s", got)
	}
}

// CT-07: more than MaxCopyFiles files stop the copy before Nabu.
func TestCopyLimit(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < MaxCopyFiles; i++ {
		seen[string(rune(i))] = true
	}
	tw := tar.NewWriter(io.Discard)
	err := put(tw, seen, &tar.Header{Name: "one-more", Typeflag: tar.TypeReg}, nil)
	var tl *tooLarge
	if !errors.As(err, &tl) || !strings.Contains(err.Error(), "5000") {
		t.Fatalf("%v", err)
	}
}

// CT-08, CT-11, CT-12: the archive of changes of Nabu — deletions, the plan,
// paths outside the copy and what the sandbox left out.
func TestReadChanges(t *testing.T) {
	manifest := `{"changes":[{"path":"a.go","op":"modified"},{"path":"b_test.go","op":"added"},{"path":"old.go","op":"deleted"},
		{"path":"HAMMURAPI_PLAN.md","op":"added"},{"path":"../x","op":"added"},{"path":".git/config","op":"modified"},{"path":"gone.go","op":"added"}],
		"skipped":[{"path":"big.bin","reason":"too_large"}]}`
	b := tgz(t, []tar.Header{
		{Name: "changes.json", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "a.go", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "b_test.go", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "HAMMURAPI_PLAN.md", Typeflag: tar.TypeReg, Mode: 0o644},
	}, map[string]string{"changes.json": manifest, "a.go": "package a // v2", "b_test.go": "package a", "HAMMURAPI_PLAN.md": "1. do it"})
	changes, plan, warnings, err := ReadChanges(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range changes {
		if c.Delete {
			got = append(got, "-"+c.Path)
		} else {
			got = append(got, c.Path+"="+string(c.Content))
		}
	}
	if strings.Join(got, " ") != "a.go=package a // v2 b_test.go=package a -old.go" {
		t.Fatalf("changes: %v", got)
	}
	if plan != "1. do it" {
		t.Fatalf("plan %q", plan)
	}
	if len(warnings) != 4 || !strings.Contains(strings.Join(warnings, "|"), "too_large") {
		t.Fatalf("warnings %v", warnings)
	}
}

// CT-13, CT-14: the classes of failed runs of Nabu.
func TestClassOf(t *testing.T) {
	for in, want := range map[string]string{"workspace_not_connected": ClassSandboxStart, "limit_exceeded": ClassLimit, "rate_limit": "rate_limit"} {
		if got := classOf(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
}

// CT-17: a repeat of code.start gives the run the same task token.
func TestTaskToken(t *testing.T) {
	id := uuid.New()
	a, ha := TaskToken([]byte("key"), id)
	b, hb := TaskToken([]byte("key"), id)
	if a != b || !bytes.Equal(ha, hb) || !strings.HasPrefix(a, "hmt_") || !bytes.Equal(HashToken(a), ha) {
		t.Fatalf("not stable: %s %s", a, b)
	}
	if c, _ := TaskToken([]byte("key"), uuid.New()); c == a {
		t.Fatal("same token for another task")
	}
	if c, _ := TaskToken([]byte("other"), id); c == a {
		t.Fatal("same token with another key")
	}
}

// R2: Nabu not answering or limiting is retried, a refusal is not.
func TestRetryable(t *testing.T) {
	if !retryable(fmt.Errorf("x: %w", nabu.ErrUnavailable)) || !retryable(&nabu.APIError{Status: 429, Code: "rate_limited"}) {
		t.Fatal("unavailable and 429 are retried")
	}
	if retryable(&nabu.APIError{Status: 422, Code: "no_workspace"}) || retryable(errors.New("x")) {
		t.Fatal("refusals are not retried")
	}
}
