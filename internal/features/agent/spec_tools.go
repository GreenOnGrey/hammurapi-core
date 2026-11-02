package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
)

// SpecRule tells the personal agent of a user that it does not write the
// documents of a specification (FTR.HMR.DEV-0001 R1, R17): the writer does.
const SpecRule = " The documents of a feature are written only by the writer agent of Hammurapi. Never write or rewrite them yourself, " +
	"not in the chat and not in a file. To change one, clear up with the user what to change, then call request_spec_revision."

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

// Bounds of a submitted file (R8).
const (
	MaxSubmittedFile = 2 << 20
)

var (
	fileNameRe  = regexp.MustCompile(`^[a-z0-9._-]{1,64}\.(html|svg)$`)
	baseTagRe   = regexp.MustCompile(`(?i)<base[\s>]`)
	scriptSrcRe = regexp.MustCompile(`(?i)<script[^>]*\ssrc\s*=`)
	linkTagRe   = regexp.MustCompile(`(?i)<link\b[^>]*>`)
	hrefRe      = regexp.MustCompile(`(?i)href\s*=\s*["']?https?://([^/"'\s>]+)`)
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
	switch {
	case in.Area != domain.AreaDesign || !w.Allows(in.Area):
		return "Files are submitted only for the design gate of this run"
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
