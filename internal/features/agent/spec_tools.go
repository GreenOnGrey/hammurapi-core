package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/GreenOnGrey/speclint/v2/contract"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/speccontract"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
)

// SpecRule tells the personal agent of a user that it does not write the
// documents of a specification (FTR.HMR.DEV-0001 R1, R17): the writer does.
const SpecRule = " The documents of a feature are written only by the writer agent of Hammurapi. Never write or rewrite them yourself, " +
	"not in the chat and not in a file. To change one, clear up with the user what to change, then call request_spec_revision."

// CodeRegenerationToolDescription is the description of request_code_regeneration (FTR.HMR.DEV-0004 R20).
const CodeRegenerationToolDescription = "Run the agent of code of one service of the feature open in the chat of Hammurapi again, when its last run is done. " +
	"The agent works over the current code of the service branch and gets the comments and the failed CI checks of its PR by itself; " +
	"comment adds what the user wants changed. The PR is updated, not replaced. " +
	"This is the only way to change the code of a feature: never write code yourself. Confirm the service and the comment with the user before calling."

// SpecRevisionToolDescription is the contract of the personal agent (R12, R17, R32).
const SpecRevisionToolDescription = "Give the writer agent a brief to change the specification of the feature open in the chat of Hammurapi. " +
	"This is the only way to change a specification: never write a document yourself, in the chat or in a file. " +
	"Before calling: read the open questions (read_spec_questions); answer what you can from what you know about the user and confirm each such answer with the user; " +
	"ask the rest a few at a time, each with your recommendation; show the whole brief and call after the user agrees. " +
	"Name in areas every gate to change, also an approved one: an approved gate returns to draft and is approved again by its experts. " +
	"Set allowTouches of an answer only if the user agrees that the other gates its question names may change. " +
	"Only what concerns this feature goes into the brief. Skills or instructions of your agent may refine how you ask, not the result."

// SpecBrief is the argument of request_spec_revision.
type SpecBrief struct {
	FeatureKey   string             `json:"featureKey"`
	Areas        []string           `json:"areas"`
	Answers      []cycledata.Answer `json:"answers"`
	SkipAreas    []string           `json:"skipAreas"`
	RestoreAreas []string           `json:"restoreAreas"`
	Context      string             `json:"context"`
	Instructions string             `json:"instructions"`
}

// CheckSubmission tells what is wrong with a call of submit_gate, "" — nothing.
// The scope comes from the server: the rule of R21 does not depend on the model.
func CheckSubmission(w *mcp.WriterScope, raw json.RawMessage) string {
	var in struct {
		Area      domain.Area               `json:"area"`
		Content   string                    `json:"content"`
		Unchanged bool                      `json:"unchanged"`
		Conflicts string                    `json:"conflicts"`
		Questions []cycledata.QuestionInput `json:"questions"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "invalid arguments"
	}
	if !w.Allows(in.Area) {
		return fmt.Sprintf("The area %s is not allowed in this run: ask a question about it instead", in.Area)
	}
	if len(in.Questions) > 0 && in.Area.Generated() {
		return "tech and qa do not ask questions: use conflicts"
	}
	if in.Area.ApproverKind() == domain.ExpertProduct {
		for _, q := range in.Questions { // R11: the rule does not depend on the model
			if domain.ExpertKind(q.Audience) == domain.ExpertTechnical {
				return fmt.Sprintf("%s does not ask technical experts: take an assumption in the text, arch decides it", in.Area)
			}
			if name := technicalName(q.QuestionText); name != "" {
				return fmt.Sprintf("Question %q names %s: a product expert answers without knowing the code or the data. Ask about behavior in plain words, or leave the decision to arch", q.Title, name)
			}
		}
	}
	if in.Conflicts != "" && !in.Area.Generated() {
		return "conflicts are for tech and qa: ask a question instead"
	}
	if in.Unchanged {
		if w.Mode != "impact" {
			return "unchanged is for a check of a gate only"
		}
		return ""
	}
	if strings.TrimSpace(in.Content) != "" {
		return ""
	}
	for _, q := range in.Questions {
		if q.SkipArea == string(in.Area) && (in.Area == domain.AreaDesign || in.Area == domain.AreaArch) {
			return ""
		}
	}
	return "content is required, unless the gate proposes to skip itself with a question with skipArea"
}

// technicalNameRe is a name of code or data: catalog_usage_daily, LAST_CHECK_AT.
var technicalNameRe = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*_[A-Za-z0-9_]*[A-Za-z0-9]\b`)

// technicalName finds such a name in a question. It only catches the obvious
// case; the boundary of a gate is mostly on the prompt.
func technicalName(q cycledata.QuestionText) string {
	return technicalNameRe.FindString(strings.Join(append([]string{q.Title, q.Body, q.Recommended, q.Why}, q.Options...), " "))
}

// Bounds of a submitted file (R8).
const (
	MaxSubmittedFile = 2 << 20
)

var (
	fileNameRe = regexp.MustCompile(`^[a-z0-9._-]{1,64}\.(html|svg)$`)
	// contractNameRe: a key of the catalog and the kind of the contract (FTR.HMR.SPEC-0004 R6)
	contractNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}\.(openapi\.(yaml|yml|json)|proto)$`)
	baseTagRe      = regexp.MustCompile(`(?i)<base[\s>]`)
	scriptSrcRe    = regexp.MustCompile(`(?i)<script[^>]*\ssrc\s*=`)
	linkTagRe      = regexp.MustCompile(`(?i)<link\b[^>]*>`)
	hrefRe         = regexp.MustCompile(`(?i)href\s*=\s*["']?https?://([^/"'\s>]+)`)
)

// CheckFile tells what is wrong with a call of submit_gate_file, "" — nothing.
func CheckFile(w *mcp.WriterScope, raw json.RawMessage) string {
	var in struct {
		Area    domain.Area `json:"area"`
		Path    string      `json:"path"`
		Content string      `json:"content"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return "invalid arguments"
	}
	if in.Area == domain.AreaTech { // FTR.HMR.SPEC-0004 R8: a contract of a service
		return checkContractFile(w, in.Path, in.Content)
	}
	switch {
	case in.Area != domain.AreaDesign || !w.Allows(in.Area):
		return "Files are submitted only for the design and tech gates of this run"
	case in.Path == "spec.md" || !fileNameRe.MatchString(in.Path):
		return "path: one file name, a-z 0-9 . _ -, .html or .svg"
	case len(in.Content) > MaxSubmittedFile:
		return fmt.Sprintf("the file is larger than %d bytes: submit a smaller one", MaxSubmittedFile)
	case baseTagRe.MatchString(in.Content), scriptSrcRe.MatchString(in.Content):
		return "the mockup must be self-contained: no <base>, no <script src>"
	}
	for _, l := range linkTagRe.FindAllString(in.Content, -1) {
		if m := hrefRe.FindStringSubmatch(l); m != nil && m[1] != "fonts.googleapis.com" && m[1] != "fonts.gstatic.com" {
			return "the mockup must be self-contained: only fonts may come from the network"
		}
	}
	return ""
}

// checkContractFile accepts a contract of a service for tech: <service>.openapi.yaml|yml|json or <service>.proto, in
// its format (FTR.HMR.SPEC-0004 R6, R8). The service of the name is checked when the gate is submitted.
func checkContractFile(w *mcp.WriterScope, name, content string) string {
	svc, _, ok := contract.ContractFile(name)
	switch {
	case !w.Allows(domain.AreaTech):
		return "Files are submitted only for the design and tech gates of this run"
	case !ok || !contractNameRe.MatchString(name):
		return "path: <service key>.openapi.yaml, .openapi.yml, .openapi.json or <service key>.proto; a-z 0-9 . _ -"
	case len(content) > MaxSubmittedFile:
		return fmt.Sprintf("the file is larger than %d bytes: submit a smaller one", MaxSubmittedFile)
	}
	for _, v := range speccontract.CheckFile("tech/"+name, []byte(content), []string{svc}) {
		return fmt.Sprintf("%s (line %d): %s", v.Check, v.Line, v.Message)
	}
	return ""
}
