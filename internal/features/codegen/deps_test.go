package codegen

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
)

// archiveOf is an archive of changes with the given paths and operations.
func archiveOf(t *testing.T, ops map[string]string) []byte {
	t.Helper()
	type change struct {
		Path string `json:"path"`
		Op   string `json:"op"`
	}
	var cs []change
	var hs []tar.Header
	bodies := map[string]string{}
	for p, op := range ops {
		cs = append(cs, change{p, op})
		if op != "deleted" {
			hs = append(hs, tar.Header{Name: p, Typeflag: tar.TypeReg, Mode: 0o644})
			bodies[p] = "x"
		}
	}
	m, _ := json.Marshal(map[string]any{"changes": cs})
	bodies["changes.json"] = string(m)
	hs = append(hs, tar.Header{Name: "changes.json", Typeflag: tar.TypeReg, Mode: 0o644})
	return tgz(t, hs, bodies)
}

func depsWarning(ws []string) string {
	for _, w := range ws {
		if strings.HasPrefix(w, warningDependencies) {
			return strings.TrimPrefix(w, warningDependencies)
		}
	}
	return ""
}

// FTR.HMR.DEV-0004 qa DPN-01…DPN-04: what counts as a change of the dependencies.
func TestDependencyChanges(t *testing.T) {
	cases := []struct {
		name string
		ops  map[string]string
		want string
	}{
		{"DPN-01", map[string]string{"go.mod": "modified", "third_party/klauspost-compress/s2/s2.go": "added", "vendor/x/y.go": "added", "a.go": "modified"},
			"go.mod, third_party/klauspost-compress/s2/s2.go, vendor/x/y.go"},
		{"DPN-02", map[string]string{"web/package-lock.json": "modified", "requirements-dev.txt": "modified", "uv.lock": "added"},
			"requirements-dev.txt, uv.lock, web/package-lock.json"},
		{"DPN-03", map[string]string{"third_party/kept.go": "modified", "internal/vendor.go": "added"}, ""},
		{"DPN-04", map[string]string{"go.mod": "modified", "go.sum": "modified", "package.json": "modified", "yarn.lock": "modified",
			"poetry.lock": "modified", "pyproject.toml": "modified", "pnpm-lock.yaml": "modified"},
			"go.mod, go.sum, package.json, pnpm-lock.yaml, poetry.lock and 2 more"},
	}
	for _, c := range cases {
		_, _, warnings, err := ReadChanges(bytes.NewReader(archiveOf(t, c.ops)))
		if err != nil {
			t.Fatal(err)
		}
		if got := depsWarning(warnings); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if c.want != "" && !strings.HasPrefix(warnings[0], warningDependencies) {
			t.Errorf("%s: the warning of the dependencies is not first: %v", c.name, warnings)
		}
	}
}

// DPN-05: changed dependencies need a person even when the check passed; the warning opens the PR.
func TestDependenciesNeedReview(t *testing.T) {
	r := Result{Check: &nabu.CheckResult{Status: CheckPassed}, Warnings: []string{warningProtected + "a_test.go", warningDependencies + "go.mod"}}
	if !r.needsReview() {
		t.Fatal("DPN-05 no review")
	}
	if w := checkWarnings(r); !strings.HasPrefix(w, "⚠️ "+warningDependencies+"go.mod") || !strings.Contains(w, warningProtected) {
		t.Fatalf("DPN-05 warning %q", w)
	}
	if (Result{Check: &nabu.CheckResult{Status: CheckPassed}}).needsReview() {
		t.Fatal("no dependencies, a passed check — no review")
	}
}

// DPN-07: the instruction of the agent on dependencies.
func TestPromptDependencies(t *testing.T) {
	p := Prompt(commitDescription())
	for _, want := range []string{"no `replace` to a local path", "`file:` or `link:`", "third_party/", "name it in the summary"} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt lacks %q", want)
		}
	}
}
