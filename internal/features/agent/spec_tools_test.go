package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
)

func raw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RT-01, RT-05, WR-20: what a run may submit is decided by the server.
func TestCheckSubmission(t *testing.T) {
	w := &mcp.WriterScope{Mode: "write", Areas: []domain.Area{domain.AreaArch, domain.AreaDesign}}
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"its gate", map[string]any{"area": "arch", "content": "# A"}, ""},
		{"another gate", map[string]any{"area": "product", "content": "# P"}, "not allowed in this run"},
		{"no scope words", map[string]any{"area": "arch"}, "content is required"},
		{"skip", map[string]any{"area": "design", "questions": []map[string]any{{"title": "Нужен ли дизайн?", "recommended": "x", "skipArea": "design"}}}, ""},
		{"check out of a check", map[string]any{"area": "arch", "unchanged": true}, "for a check"},
		{"conflicts of arch", map[string]any{"area": "arch", "content": "# A", "conflicts": "x"}, "for tech and qa"},
	}
	for _, c := range cases {
		got := CheckSubmission(w, raw(t, c.in))
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q", c.name, got)
		}
	}
	gen := &mcp.WriterScope{Mode: "generate", Areas: []domain.Area{domain.AreaTech}}
	if got := CheckSubmission(gen, raw(t, map[string]any{"area": "tech", "content": "x", "questions": []map[string]any{{"title": "t"}}})); !strings.Contains(got, "do not ask") {
		t.Errorf("tech asked: %q", got)
	}
	if got := CheckSubmission(nil, raw(t, map[string]any{"area": "tech", "content": "x"})); got == "" {
		t.Error("no scope accepted")
	}
	check := &mcp.WriterScope{Mode: "impact", Areas: []domain.Area{domain.AreaArch}}
	if got := CheckSubmission(check, raw(t, map[string]any{"area": "arch", "unchanged": true})); got != "" {
		t.Errorf("a check: %q", got)
	}
}

// WR-18: the mockup is self-contained and small enough.
func TestCheckFile(t *testing.T) {
	w := &mcp.WriterScope{Mode: "write", Areas: []domain.Area{domain.AreaDesign}}
	ok := `<!doctype html><link rel="stylesheet" href="https://fonts.googleapis.com/css2"><style>a{}</style>`
	if got := CheckFile(w, raw(t, map[string]any{"area": "design", "path": "mockups.html", "content": ok})); got != "" {
		t.Errorf("fine file: %q", got)
	}
	bad := []map[string]any{
		{"area": "product", "path": "mockups.html", "content": ok},
		{"area": "design", "path": "../x.html", "content": ok},
		{"area": "design", "path": "spec.md", "content": ok},
		{"area": "design", "path": "m.html", "content": `<script src="https://x.io/a.js"></script>`},
		{"area": "design", "path": "m.html", "content": `<base href="/">`},
		{"area": "design", "path": "m.html", "content": `<link rel="stylesheet" href="https://x.io/a.css">`},
		{"area": "design", "path": "m.html", "content": strings.Repeat("x", MaxSubmittedFile+1)},
	}
	for i, in := range bad {
		if CheckFile(w, raw(t, in)) == "" {
			t.Errorf("case %d accepted", i)
		}
	}
}

// RQ-05: the old way to change a specification is gone; R32: no names of skills.
func TestSpecTools(t *testing.T) {
	names := map[string]bool{}
	for _, tool := range Tools(ToolDeps{}) {
		names[tool.Name] = true
	}
	if names["edit_spec"] || names["regenerate_gate"] || !names["request_spec_revision"] || !names["read_spec_questions"] || !names["submit_gate_file"] {
		t.Fatalf("tools: %v", names)
	}
	for _, want := range []string{"read_spec_questions", "never write a document yourself", "confirm each such answer", "a few at a time", "show the whole brief", "Skills or instructions"} {
		if !strings.Contains(SpecRevisionToolDescription, want) {
			t.Errorf("no %q", want)
		}
	}
}
