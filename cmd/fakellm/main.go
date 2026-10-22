// Command fakellm is a scripted OpenAI-compatible LLM endpoint for the demo
// stack and smoke runs (FTR.HMR.CMN-0004): Hammurapi's real agent (Pi in the agent
// operator) talks to it like to DeepSeek. Prompts with the header
// "[hammurapi:task=<name> …]" get scripted tool calls — Analysis
// (save_discovery), tech/qa generation (submit_gate), code generation (Pi's
// write tool, executed in the runner's working copy) and checks; anything else
// is echoed. Model ids e401, e402, e429, e503 imitate provider errors.
//
// It is not an LLM; never use it in production.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent/fakellm"
)

var (
	headerRe   = regexp.MustCompile(`\[hammurapi:task=([a-z_]+)([^\]]*)\]`)
	kvRe       = regexp.MustCompile(`(\w+)=(\S+)`)
	reqRe      = regexp.MustCompile(`\*\*R(\d+)\.\*\*`)
	reqLineRe  = regexp.MustCompile(`(?m)^- (R\d+): `)
	tcLineRe   = regexp.MustCompile(`(?m)^- ([A-Z]+-\d+) \[`)
	serviceRe  = regexp.MustCompile(`(?m)^- ([a-z0-9._-]+) \(system ([A-Z0-9]+/[A-Z0-9]+|-), repo`)
	featureRe  = regexp.MustCompile(`feature (FTR\.([A-Z0-9]+)\.([A-Z0-9]+)-\d+)`)
	issueTitle = regexp.MustCompile(`issue (ISS\.[A-Z0-9]+-\d+) "([^"]*)"`)
)

const mcp = "mcp__hammurapi__"

func main() {
	addr := os.Getenv("FAKELLM_ADDR")
	if addr == "" {
		addr = ":8099"
	}
	srv := &fakellm.Server{Key: os.Getenv("FAKELLM_KEY"), Script: script}
	log.Printf("fakellm listening on %s (not an LLM: development only)", addr)
	s := &http.Server{Addr: addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(s.ListenAndServe())
}

// script answers the first turn of a task with all its tool calls and the
// second (after the tool results) with the summary.
func script(r fakellm.Request) *fakellm.Reply {
	text := r.FirstUser()
	m := headerRe.FindStringSubmatch(text)
	if m == nil {
		return nil // chat: echo
	}
	task, kv := m[1], map[string]string{}
	for _, x := range kvRe.FindAllStringSubmatch(m[2], -1) {
		kv[x[1]] = x[2]
	}
	if r.ToolResults() > 0 {
		return &fakellm.Reply{Text: summary(task, text, kv)}
	}
	switch task {
	case "discovery":
		title := "the issue"
		if t := issueTitle.FindStringSubmatch(text); t != nil {
			title = t[2]
		}
		source := os.Getenv("FAKELLM_METRIC_SOURCE")
		if source == "" {
			source = "demo"
		}
		content := fmt.Sprintf("## Value\n\n%s makes the product more useful for the users of the domain.\n\n"+
			"## How we measure\n\n| Source | Query | Target | Window |\n| --- | --- | --- | --- |\n| %s | `sum(hammurapi_demo_value)` | +5%% | 14d |\n\n"+
			"## Similar issues and features\n\nNone found.\n\n## Affected systems and services\n\nSee the catalog.\n\n## Risks\n\nLow.\n", title, source)
		if kv["type"] == "problem" {
			content += "\n## Cause hypothesis\n\nA recent change broke the flow.\n"
		}
		args := map[string]any{
			"content": content, "value": title + " increases the value metric",
			"measure": map[string]string{"source": source, "query": "sum(hammurapi_demo_value)", "target": "+5%", "window": "14d"},
			// FTR.HMR.DISC-0001 R15: the draft estimate — agents do the work, people take part
			"estimate": map[string]any{
				"agent": []map[string]any{{"stage": "spec", "usd": 9, "tokens": 1100000}, {"stage": "codegen", "usd": 26, "tokens": 3400000},
					{"stage": "checks", "usd": 5, "tokens": 600000}},
				"people": []map[string]any{{"stage": "spec", "expertise": "product", "personHours": 3}, {"stage": "spec", "expertise": "tech", "personHours": 2},
					{"stage": "approval", "expertise": "product", "personHours": 1}, {"stage": "validation", "expertise": "tech", "personHours": 3},
					{"stage": "release", "expertise": "product", "personHours": 1}},
				"days": 9, "waitDays": 4, "confidence": "low", "basis": map[string]any{"features": []string{}, "judgement": true}}}
		if strings.Contains(text, "New instructions of experts") {
			args["changeSummary"] = "- Followed the instructions of the experts"
		}
		return &fakellm.Reply{ToolCalls: []fakellm.ToolCall{{Name: mcp + "save_discovery", Args: args}}}
	case "generate_gates":
		var calls []fakellm.ToolCall
		reqs, services := requirements(text), servicesOf(text)
		for _, area := range strings.Split(kv["areas"], ",") {
			var b strings.Builder
			switch area {
			case "tech":
				b.WriteString("# Technical specification\n\n## Changes by service\n\n| Service | Changes | Requirements |\n| --- | --- | --- |\n")
				for _, s := range services {
					fmt.Fprintf(&b, "| %s | Implement the feature in %s | %s |\n", s, s, strings.Join(reqs, ", "))
				}
				b.WriteString("\n## Rollout order\n\n")
				for i, s := range services {
					fmt.Fprintf(&b, "%d. %s\n", i+1, s)
				}
			case "qa":
				b.WriteString("# Test specification\n\n## Test cases\n\n| ID | Requirements | Scenario | Expected result | Level |\n| --- | --- | --- | --- | --- |\n")
				for i, r := range reqs {
					fmt.Fprintf(&b, "| QA-%02d | %s | Check %s | Works as specified | U |\n", i+1, r, r)
				}
			default:
				continue
			}
			calls = append(calls, fakellm.ToolCall{Name: mcp + "submit_gate", Args: map[string]string{"area": area, "content": b.String()}})
		}
		return &fakellm.Reply{ToolCalls: calls}
	case "implement", "address_review", "update_pr":
		feature := kv["feature"]
		slug := strings.ToLower(strings.NewReplacer(".", "_", "-", "_").Replace(feature))
		calls := []fakellm.ToolCall{{Name: mcp + "report_progress", Args: map[string]string{"message": "reading the specification"}}}
		if kv["autonomy"] == "plan" && task == "implement" {
			plan := fmt.Sprintf("# Plan for %s\n\n1. Add the handler.\n2. Add tests for the test cases.\n", feature)
			return &fakellm.Reply{ToolCalls: append(calls, fakellm.ToolCall{Name: "write", Args: map[string]string{"path": "HAMMURAPI_PLAN.md", "content": plan}})}
		}
		var reqs, tcs []string
		for _, x := range reqLineRe.FindAllStringSubmatch(text, -1) {
			reqs = append(reqs, x[1])
		}
		for _, x := range tcLineRe.FindAllStringSubmatch(text, -1) {
			tcs = append(tcs, x[1])
		}
		code := fmt.Sprintf("package main\n\n// %s implements %s (%s).\nfunc %s() string { return %q }\n", "feature"+slug, feature, strings.Join(reqs, ", "), "feature"+slug, feature)
		if task != "implement" {
			code += fmt.Sprintf("\n// %s: revision after %s.\n", slug, task)
		}
		var tests strings.Builder
		tests.WriteString("package main\n\nimport \"testing\"\n")
		for _, tc := range tcs {
			fmt.Fprintf(&tests, "\nfunc Test%s_%s(t *testing.T) {\n\tif feature%s() == \"\" {\n\t\tt.Fatal(\"empty\")\n\t}\n}\n", strings.ReplaceAll(tc, "-", ""), slug, slug)
		}
		return &fakellm.Reply{ToolCalls: append(calls,
			fakellm.ToolCall{Name: "write", Args: map[string]string{"path": "feature_" + slug + ".go", "content": code}},
			fakellm.ToolCall{Name: "write", Args: map[string]string{"path": "feature_" + slug + "_test.go", "content": tests.String()}})}
	case "check":
		return &fakellm.Reply{Text: "The code matches the specification."}
	}
	return &fakellm.Reply{Text: "unknown task " + task}
}

func summary(task, text string, kv map[string]string) string {
	switch task {
	case "discovery":
		return "Analysis saved."
	case "generate_gates":
		return "Generated " + kv["areas"] + "."
	case "implement", "address_review", "update_pr":
		if kv["autonomy"] == "plan" && task == "implement" {
			return "Plan written."
		}
		var reqs []string
		for _, x := range reqLineRe.FindAllStringSubmatch(text, -1) {
			reqs = append(reqs, x[1])
		}
		return "Done. Implemented: " + strings.Join(reqs, ", ")
	}
	return "Done."
}

func requirements(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range reqRe.FindAllStringSubmatch(text, -1) {
		if id := "R" + m[1]; !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return len(out[i]) < len(out[j]) || (len(out[i]) == len(out[j]) && out[i] < out[j])
	})
	if len(out) == 0 {
		out = []string{"R1"}
	}
	return out
}

func servicesOf(text string) []string {
	system := ""
	if m := featureRe.FindStringSubmatch(text); m != nil {
		system = m[2] + "/" + m[3]
	}
	var services []string
	for _, m := range serviceRe.FindAllStringSubmatch(text, -1) {
		if m[2] == system {
			services = append(services, m[1])
		}
	}
	if len(services) == 0 {
		for _, m := range serviceRe.FindAllStringSubmatch(text, -1) {
			services = append(services, m[1])
			if len(services) == 2 {
				break
			}
		}
	}
	return services
}
