package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gates"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// DiscoveryInput is the structured Discovery document written by the agent.
type DiscoveryInput struct {
	Content        string              `json:"content"`
	Value          string              `json:"value"`
	Measure        *cycledata.Measure  `json:"measure"`
	Similar        []cycledata.Similar `json:"similar"`
	Systems        []string            `json:"systems"`
	Services       []string            `json:"services"`
	ProblemFeature string              `json:"problemFeature"`
}

// ToolDeps are the dependencies of the MCP tools.
type ToolDeps struct {
	Store         specdata.Store
	Git           git.Provider
	Tokens        git.TokenSource
	Gates         *gates.Service
	Principal     PrincipalLoader
	DefaultBranch string
	// EditDiscovery saves an agent edit of a Discovery document from the chat
	// and returns the issue to verification (R5).
	EditDiscovery func(ctx context.Context, p *domain.Principal, issueKey string, in DiscoveryInput) error
	// TestMetric dry-runs a metric query in a configured source.
	TestMetric func(ctx context.Context, source, query string) (float64, error)
	// CreateIssue creates an issue on behalf of the user of a Nabu call
	// (FTR.HMR.CMN-0006 R7) and returns its key.
	CreateIssue func(ctx context.Context, p *domain.Principal, in IssueInput) (string, error)
}

// IssueInput is the argument of create_issue.
type IssueInput struct {
	Type        string `json:"type"`
	Domain      string `json:"domain"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

var (
	readers   = []string{mcp.ModeGeneral, mcp.ModeSpec, mcp.ModeDiscovery, mcp.ModeGenerate, mcp.ModeCheck, mcp.ModeTask, mcp.ModeNabu}
	research  = []string{mcp.ModeGeneral, mcp.ModeSpec, mcp.ModeDiscovery, mcp.ModeGenerate, mcp.ModeCheck, mcp.ModeNabu}
	codeRead  = []string{mcp.ModeDiscovery, mcp.ModeGenerate, mcp.ModeCheck}
	specModes = []string{mcp.ModeSpec}
)

func schema(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var areaProp = map[string]any{"type": "string", "enum": []string{"product", "design", "arch", "tech", "qa"}}

var discoverySchema = schema(map[string]any{
	"content": map[string]any{"type": "string", "description": "Full Discovery markdown: value, how to measure, similar issues and features, affected systems and services, risks, rough size; for a Problem — cause hypothesis and the feature with the error."},
	"value":   map[string]any{"type": "string", "description": "What value the issue has (one paragraph)."},
	"measure": map[string]any{"type": "object", "properties": map[string]any{
		"source": map[string]any{"type": "string", "description": "Name of a configured metric source"},
		"query":  map[string]any{"type": "string"},
		"target": map[string]any{"type": "string"},
		"window": map[string]any{"type": "string", "description": "Evaluation window, e.g. 14d"},
	}},
	"similar": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
		"key": map[string]any{"type": "string"}, "title": map[string]any{"type": "string"}, "kind": map[string]any{"type": "string", "enum": []string{"issue", "feature"}},
	}}},
	"systems":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "DOMAIN/SYSTEM keys"},
	"services":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	"problemFeature": map[string]any{"type": "string", "description": "For a Problem: key of the feature with the error, if it exists in Hammurapi"},
}, "content", "value", "measure")

func toolErr(err error) error {
	if err == nil {
		return nil
	}
	var te *mcp.ToolError
	if errors.As(err, &te) {
		return err
	}
	return &mcp.ToolError{Msg: err.Error()}
}

// Tools builds Hammurapi's MCP tools. The mode of a grant decides which are
// offered: the chat, Discovery, gate generation, the code check or a runner
// task. There is deliberately no delete or merge tool: irreversible actions
// are performed only by people.
func Tools(d ToolDeps) []mcp.Tool {
	cd := func() *cycledata.DB { return cycledata.New(d.Store.Q()) }
	return []mcp.Tool{
		{
			Name:        "list_issues",
			Description: "List issues (Ideas and Problems) with their status. Optional filters: query (title or key substring), domain key, status (open | accepted | resolved | closed | all).",
			Modes:       research,
			InputSchema: schema(map[string]any{
				"query": map[string]any{"type": "string"}, "domain": map[string]any{"type": "string"},
				"status": map[string]any{"type": "string", "enum": []string{"open", "accepted", "resolved", "closed", "all"}},
			}),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Query, Domain, Status string }
				_ = json.Unmarshal(raw, &a)
				if a.Status == "" {
					a.Status = "all"
				}
				items, err := cd().ListIssues(ctx, cycledata.IssueFilter{Domain: a.Domain, Status: a.Status, Query: a.Query, Page: httpx.Page{Limit: 100}})
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, is := range items {
					fmt.Fprintf(&b, "%s [%s, %s] %s\n", is.Key, is.Type, is.Status, is.Title)
				}
				if b.Len() == 0 {
					return "No issues found.", nil
				}
				return b.String(), nil
			},
		},
		{
			Name:        "read_issue",
			Description: "Read an issue with its description and the current Discovery document.",
			Modes:       research,
			InputSchema: schema(map[string]any{"key": map[string]any{"type": "string"}}, "key"),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Key string }
				_ = json.Unmarshal(raw, &a)
				is, _, err := cd().IssueByKey(ctx, a.Key)
				if errors.Is(err, cycledata.ErrNotFound) {
					return "", &mcp.ToolError{Msg: "issue not found"}
				}
				if err != nil {
					return "", err
				}
				var b strings.Builder
				fmt.Fprintf(&b, "%s — %s\ntype: %s, status: %s, domain: %s, source: %s\n\n%s\n", is.Key, is.Title, is.Type, is.Status, is.Domain, is.Source, is.Description)
				if is.RolledBackRelease != nil {
					fmt.Fprintf(&b, "\nReturned after rollback of release %s.\n", *is.RolledBackRelease)
				}
				doc, err := cd().Discovery(ctx, is.ID)
				if err != nil {
					return "", err
				}
				if doc != nil {
					fmt.Fprintf(&b, "\n--- Discovery (revision %d) ---\n%s\n", doc.Revision, doc.Content)
					if doc.Measure != nil {
						fmt.Fprintf(&b, "measure: source=%s query=%q target=%s window=%s\n", doc.Measure.Source, doc.Measure.Query, doc.Measure.Target, doc.Measure.Window)
					}
				}
				return b.String(), nil
			},
		},
		{
			Name: "list_features",
			Description: "List features (solutions) with their phase and gate statuses. Optional filters: query (title or key substring), " +
				"status (active | released | rolled_back | all), domain key.",
			Modes: research,
			InputSchema: schema(map[string]any{
				"query":  map[string]any{"type": "string"},
				"status": map[string]any{"type": "string", "enum": []string{"active", "released", "rolled_back", "all"}},
				"domain": map[string]any{"type": "string"},
			}),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Query, Status, Domain string }
				_ = json.Unmarshal(raw, &a)
				if a.Status == "" {
					a.Status = "all"
				}
				if a.Domain == "" {
					a.Domain = "all"
				}
				items, err := d.Store.ListFeatures(ctx, specdata.ListFilter{UserID: g.UserID, Domain: a.Domain, Status: a.Status, Query: a.Query, Page: httpx.Page{Limit: 100}})
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, f := range items {
					fmt.Fprintf(&b, "%s — %s [%s]", f.UniqueID, f.Title, f.Phase)
					if f.ParentUniqueID != nil {
						fmt.Fprintf(&b, " fix of %s", *f.ParentUniqueID)
					}
					if len(f.Issues) > 0 {
						fmt.Fprintf(&b, " issues: %s", strings.Join(f.Issues, ", "))
					}
					for _, gt := range f.Gates {
						fmt.Fprintf(&b, " %s:%s", gt.Area, gt.Status)
					}
					b.WriteString("\n")
				}
				if b.Len() == 0 {
					return "No features found.", nil
				}
				return b.String(), nil
			},
		},
		{
			Name:        "search_specs",
			Description: "Full-text search in specification documents merged to the default branch, plus feature titles. Use it to find where something is already described.",
			Modes:       research,
			InputSchema: schema(map[string]any{"query": map[string]any{"type": "string"}}, "query"),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Query string }
				if err := json.Unmarshal(raw, &a); err != nil || strings.TrimSpace(a.Query) == "" {
					return "", &mcp.ToolError{Msg: "query is required"}
				}
				var b strings.Builder
				items, err := d.Store.ListFeatures(ctx, specdata.ListFilter{UserID: g.UserID, Domain: "all", Status: "all", Query: a.Query, Page: httpx.Page{Limit: 20}})
				if err != nil {
					return "", err
				}
				for _, f := range items {
					fmt.Fprintf(&b, "feature %s — %s\n", f.UniqueID, f.Title)
				}
				token, err := d.Tokens.Token(ctx, g.UserID)
				if err != nil {
					return "", err
				}
				hits, err := d.Git.SearchCode(ctx, token, a.Query)
				if err == nil {
					for _, h := range hits {
						fmt.Fprintf(&b, "%s: %s\n", h.Path, strings.ReplaceAll(truncate(h.Snippet, 300), "\n", " "))
					}
				} else {
					fmt.Fprintf(&b, "(document search unavailable: %s)\n", git.Reason(err))
				}
				if b.Len() == 0 {
					return "Nothing found.", nil
				}
				return b.String(), nil
			},
		},
		{
			Name:        "read_spec",
			Description: "Read the current markdown of a feature's gate document.",
			Modes:       readers,
			InputSchema: schema(map[string]any{"uniqueId": map[string]any{"type": "string"}, "area": areaProp}, "uniqueId", "area"),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct {
					UniqueID string `json:"uniqueId"`
					Area     string `json:"area"`
				}
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", &mcp.ToolError{Msg: "invalid arguments"}
				}
				if g.Mode == mcp.ModeTask && a.UniqueID != g.Feature {
					return "", &mcp.ToolError{Msg: "a task reads only the specification of its feature " + g.Feature}
				}
				area, err := domain.ParseArea(a.Area)
				if err != nil {
					return "", &mcp.ToolError{Msg: err.Error()}
				}
				p, err := d.Principal(ctx, g.UserID)
				if err != nil {
					return "", err
				}
				doc, err := d.Gates.GetDocument(ctx, p, a.UniqueID, area)
				if err != nil {
					return "", toolErr(err)
				}
				return fmt.Sprintf("sha: %s\n\n%s", doc.SHA, doc.Content), nil
			},
		},
		{
			Name:        "read_rules",
			Description: "Read the rules template of an area: kind=template for regular features, kind=fix for fix features.",
			Modes:       readers,
			InputSchema: schema(map[string]any{"area": areaProp, "kind": map[string]any{"type": "string", "enum": []string{"template", "fix"}}}, "area"),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Area, Kind string }
				_ = json.Unmarshal(raw, &a)
				area, err := domain.ParseArea(a.Area)
				if err != nil {
					return "", &mcp.ToolError{Msg: err.Error()}
				}
				token, err := d.Tokens.Token(ctx, g.UserID)
				if err != nil {
					return "", err
				}
				return features.Template(ctx, d.Git, token, d.DefaultBranch, area, a.Kind == "fix"), nil
			},
		},
		{
			Name:        "list_services",
			Description: "List services of the catalog (Backstage components or the admin list) with their system, repository and autonomy level.",
			Modes:       readers,
			InputSchema: schema(map[string]any{"system": map[string]any{"type": "string", "description": "DOMAIN/SYSTEM"}, "query": map[string]any{"type": "string"}}),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ System, Query string }
				_ = json.Unmarshal(raw, &a)
				svcs, err := cd().ListServices(ctx, cycledata.ServiceFilter{System: a.System, Query: a.Query})
				if err != nil {
					return "", err
				}
				var b strings.Builder
				for _, s := range svcs {
					sys := "-"
					if s.System != nil {
						sys = *s.System
					}
					fmt.Fprintf(&b, "%s (system %s, repo %s, autonomy %s)\n", s.Key, sys, s.Repo, s.Autonomy)
				}
				if b.Len() == 0 {
					return "No services in the catalog.", nil
				}
				return b.String(), nil
			},
		},
		{
			Name:        "read_service_file",
			Description: "Read a file or list a directory in a service repository (default branch), to understand the code. path='' lists the root.",
			Modes:       codeRead,
			InputSchema: schema(map[string]any{"service": map[string]any{"type": "string"}, "path": map[string]any{"type": "string"}}, "service"),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Service, Path string }
				_ = json.Unmarshal(raw, &a)
				svc, err := cd().ServiceByKey(ctx, a.Service)
				if errors.Is(err, cycledata.ErrNotFound) {
					return "", &mcp.ToolError{Msg: "unknown service"}
				}
				if err != nil {
					return "", err
				}
				rp := d.Git.ForRepo(svc.Repo)
				token, err := rp.BotToken(ctx)
				if err != nil {
					return "", &mcp.ToolError{Msg: "no bot access to " + svc.Repo + ": " + err.Error()}
				}
				ref, err := rp.DefaultBranch(ctx, token)
				if err != nil {
					return "", &mcp.ToolError{Msg: git.Reason(err)}
				}
				p := strings.Trim(a.Path, "/")
				if f, err := rp.GetFile(ctx, token, ref, p); err == nil && p != "" {
					return truncate(string(f.Content), 60000), nil
				}
				files, err := rp.ListFiles(ctx, token, ref, p)
				if err != nil {
					return "", &mcp.ToolError{Msg: git.Reason(err)}
				}
				if len(files) > 500 {
					files = files[:500]
				}
				return strings.Join(files, "\n"), nil
			},
		},
		{
			Name: "create_issue",
			Description: "Create an issue (an idea or a problem) in a domain on behalf of the user. The user becomes its author; " +
				"Discovery starts automatically. Returns the issue key.",
			Modes: []string{mcp.ModeNabu},
			InputSchema: schema(map[string]any{
				"type":        map[string]any{"type": "string", "enum": []string{"idea", "problem"}},
				"domain":      map[string]any{"type": "string", "description": "domain key, e.g. FMS"},
				"title":       map[string]any{"type": "string", "description": "up to 200 characters"},
				"description": map[string]any{"type": "string", "description": "markdown"},
			}, "type", "domain", "title"),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a IssueInput
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", &mcp.ToolError{Msg: "invalid arguments"}
				}
				if d.CreateIssue == nil {
					return "", &mcp.ToolError{Msg: "issues cannot be created here"}
				}
				p, err := d.Principal(ctx, g.UserID)
				if err != nil {
					return "", err
				}
				key, err := d.CreateIssue(ctx, p, a)
				if err != nil {
					return "", toolErr(err)
				}
				return "Created issue " + key + ".", nil
			},
		},
		{
			Name:        "test_metric_query",
			Description: "Dry-run a success-metric query in a configured read-only metric source and return the current value or the source error.",
			Modes:       []string{mcp.ModeSpec, mcp.ModeDiscovery},
			InputSchema: schema(map[string]any{"source": map[string]any{"type": "string"}, "query": map[string]any{"type": "string"}}, "source", "query"),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Source, Query string }
				_ = json.Unmarshal(raw, &a)
				if d.TestMetric == nil {
					return "", &mcp.ToolError{Msg: "metric sources are not available"}
				}
				v, err := d.TestMetric(ctx, a.Source, a.Query)
				if err != nil {
					return "", toolErr(err)
				}
				return fmt.Sprintf("value: %g", v), nil
			},
		},
		{
			Name: "edit_spec",
			Description: "Replace the full markdown of a gate document of the current feature: product, design or arch (tech and qa are generated — use regenerate_gate). " +
				"Allowed only in the chat of a feature, for domain experts, and only for gates that are not approved. Supported markdown: CommonMark + GFM " +
				"(tables, task lists, strikethrough), no raw HTML or footnotes; keep front matter of fix documents. Requirements: **R<n>.** with acceptance criteria.",
			Modes:       specModes,
			InputSchema: schema(map[string]any{"area": areaProp, "content": map[string]any{"type": "string"}}, "area", "content"),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Area, Content string }
				if err := json.Unmarshal(raw, &a); err != nil {
					return "", &mcp.ToolError{Msg: "invalid arguments"}
				}
				area, err := domain.ParseArea(a.Area)
				if err != nil {
					return "", &mcp.ToolError{Msg: err.Error()}
				}
				if err := CheckEdit(ctx, d.Store, g, area); err != nil {
					return "", err
				}
				p, err := d.Principal(ctx, g.UserID)
				if err != nil {
					return "", err
				}
				res, err := d.Gates.SaveDocument(ctx, p, g.Feature, area, gates.SaveInput{Content: a.Content}, true)
				if err != nil {
					return "", toolErr(err)
				}
				if res.Commit == nil {
					return "No changes: the document already has this content.", nil
				}
				return fmt.Sprintf("Saved %s/%s as commit %s. The gate returns to draft.", g.Feature, area, *res.Commit), nil
			},
		},
		{
			Name:        "regenerate_gate",
			Description: "Ask for regeneration of the tech or qa gate of the current feature, taking the user's comment into account.",
			Modes:       specModes,
			InputSchema: schema(map[string]any{"area": map[string]any{"type": "string", "enum": []string{"tech", "qa"}}, "comment": map[string]any{"type": "string"}}, "area", "comment"),
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				var a struct{ Area, Comment string }
				_ = json.Unmarshal(raw, &a)
				if g.ContextType != "feature" || !g.Expert {
					return "", &mcp.ToolError{Msg: "regeneration is available in the chat of a feature, for domain experts"}
				}
				p, err := d.Principal(ctx, g.UserID)
				if err != nil {
					return "", err
				}
				if err := d.Gates.Regenerate(ctx, p, g.Feature, domain.Area(a.Area), a.Comment); err != nil {
					return "", toolErr(err)
				}
				return fmt.Sprintf("Regeneration of %s started; the gate will return to draft when it is ready.", a.Area), nil
			},
		},
		{
			Name:        "edit_discovery",
			Description: "Replace the Discovery document of the current issue. The issue returns to verification.",
			Modes:       specModes,
			InputSchema: discoverySchema,
			Handler: func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
				if !g.CanEditDiscovery() || d.EditDiscovery == nil {
					return "", &mcp.ToolError{Msg: "Discovery is edited in the chat of an issue, by experts of its domain"}
				}
				var in DiscoveryInput
				if err := json.Unmarshal(raw, &in); err != nil {
					return "", &mcp.ToolError{Msg: "invalid arguments"}
				}
				p, err := d.Principal(ctx, g.UserID)
				if err != nil {
					return "", err
				}
				if err := d.EditDiscovery(ctx, p, g.ContextKey, in); err != nil {
					return "", toolErr(err)
				}
				return "Discovery updated; the issue is back in verification.", nil
			},
		},
		{
			Name:        "save_discovery",
			Description: "Save the result of the Discovery. Required fields: content, value and measure (source, query, target, window).",
			Modes:       []string{mcp.ModeDiscovery},
			InputSchema: discoverySchema,
			Handler:     sinkHandler("discovery"),
		},
		{
			Name:        "submit_gate",
			Description: "Submit the generated markdown of the tech or qa gate. tech must contain a table of changes by service with the requirements of each service; qa a table of test cases (ID, level U/I/E, requirements).",
			Modes:       []string{mcp.ModeGenerate},
			InputSchema: schema(map[string]any{"area": map[string]any{"type": "string", "enum": []string{"tech", "qa"}}, "content": map[string]any{"type": "string"}}, "area", "content"),
			Handler:     sinkHandler("gate"),
		},
		{
			Name:        "report_discrepancy",
			Description: "Report a mismatch between the code of a PR and a requirement of the specification.",
			Modes:       []string{mcp.ModeCheck},
			InputSchema: schema(map[string]any{"requirement": map[string]any{"type": "string"}, "service": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}}, "requirement", "description"),
			Handler:     sinkHandler("discrepancy"),
		},
		{
			Name:        "report_progress",
			Description: "Report progress of the task in one short sentence (shown to the user).",
			Modes:       []string{mcp.ModeTask},
			InputSchema: schema(map[string]any{"message": map[string]any{"type": "string"}}, "message"),
			Handler:     sinkHandler("progress"),
		},
	}
}

func sinkHandler(kind string) func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
	return func(ctx context.Context, g mcp.Grant, raw json.RawMessage) (string, error) {
		if g.Sink == nil {
			return "", &mcp.ToolError{Msg: "not available in this context"}
		}
		if err := g.Sink(kind, raw); err != nil {
			return "", toolErr(err)
		}
		return "Saved.", nil
	}
}

// CheckEdit enforces the edit_spec rules on the grant: the chat of a feature,
// an expert of its domain, a human (not generated) area, the specification
// phase, and a gate that exists and is not approved.
func CheckEdit(ctx context.Context, store specdata.Store, g mcp.Grant, area domain.Area) error {
	if g.Mode != mcp.ModeSpec || g.ContextType != "feature" {
		return &mcp.ToolError{Msg: "editing is only possible in the chat of a feature; ask the user to open it"}
	}
	if area.Generated() {
		return &mcp.ToolError{Msg: "tech and qa are generated; call regenerate_gate with the user's comment instead"}
	}
	if !g.CanEditArea(area) {
		return &mcp.ToolError{Msg: "the user is not an expert of this feature's domain"}
	}
	f, err := store.FeatureByUniqueID(ctx, g.Feature)
	if errors.Is(err, specdata.ErrNotFound) {
		return &mcp.ToolError{Msg: "feature not found"}
	}
	if err != nil {
		return err
	}
	if f.Phase != domain.PhaseSpec {
		return &mcp.ToolError{Msg: "the specification is read-only in phase " + string(f.Phase)}
	}
	gt, err := store.ActiveGate(ctx, f.ID, area)
	if errors.Is(err, specdata.ErrNotFound) {
		return &mcp.ToolError{Msg: fmt.Sprintf("the feature has no %s gate; the user can add it in the interface", area)}
	}
	if err != nil {
		return err
	}
	if gt.Status == domain.GateApproved {
		return &mcp.ToolError{Msg: "the gate is approved; the agent does not edit approved gates"}
	}
	return nil
}
