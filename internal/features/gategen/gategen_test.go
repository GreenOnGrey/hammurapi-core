package gategen

import (
	"strings"
	"testing"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/discovery"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// WR-08, WR-13: the next stage without the skipped gates.
func TestNextStage(t *testing.T) {
	cases := []struct {
		stage   string
		skipped []string
		want    string
	}{
		{"product", nil, "design"},
		{"product", []string{"design"}, "arch"},
		{"product", []string{"design", "arch"}, "generated"},
		{"design", nil, "arch"},
		{"arch", nil, "generated"},
	}
	for _, c := range cases {
		if got := NextStage(c.stage, c.skipped); got != c.want {
			t.Errorf("NextStage(%s, %v) = %s, want %s", c.stage, c.skipped, got, c.want)
		}
	}
}

func writerFeature(stage string, approval bool) *specdata.Feature {
	return &specdata.Feature{UniqueID: "FTR.FMS.CAR-0005", Title: "Скидка", DomainKey: "FMS", SystemKey: "CAR", SpecFlow: specdata.FlowWriter, SpecStage: &stage, ApprovalRequired: approval}
}

func gate(a domain.Area, s domain.GateStatus) specdata.Gate {
	return specdata.Gate{Area: a, Status: s, Generated: a.Generated()}
}

// WR-09: tech and qa do not start before the stage reaches them.
func TestGenerationAllowed(t *testing.T) {
	approved := []specdata.Gate{gate(domain.AreaProduct, domain.GateApproved)}
	if generationAllowed(writerFeature("design", true), approved) {
		t.Fatal("started after product alone")
	}
	if !generationAllowed(writerFeature("generated", true), approved) {
		t.Fatal("not started at the stage")
	}
	draft := []specdata.Gate{gate(domain.AreaProduct, domain.GateDraft)}
	if generationAllowed(writerFeature("generated", true), draft) {
		t.Fatal("started with a gate to approve")
	}
	if !generationAllowed(writerFeature("generated", false), draft) {
		t.Fatal("a domain without approval does not wait")
	}
	legacy := &specdata.Feature{SpecFlow: specdata.FlowLegacy, ApprovalRequired: true}
	if !generationAllowed(legacy, approved) {
		t.Fatal("MG-03: a feature without stages keeps the old condition")
	}
}

// RT-07, RT-08, RT-09, RT-11, MG-03: what follows a finished run.
func TestFollowing(t *testing.T) {
	all := []specdata.Gate{gate(domain.AreaProduct, domain.GateApproved), gate(domain.AreaDesign, domain.GateApproved), gate(domain.AreaArch, domain.GateApproved),
		gate(domain.AreaTech, domain.GateApproved), gate(domain.AreaQA, domain.GateApproved)}
	// RT-08, RT-11: a check starts tech and qa, not another check.
	next := following(writerFeature("generated", true), Request{Mode: ModeImpact}, nil, all)
	if next == nil || next.Mode != ModeGenerate {
		t.Fatalf("after a check: %+v", next)
	}
	// RT-07: a gate changed by the check waits for its approval.
	all[2].Status = domain.GateDraft
	if next := following(writerFeature("generated", true), Request{Mode: ModeImpact}, []domain.Area{domain.AreaArch}, all); next != nil {
		t.Fatalf("started with a draft: %+v", next)
	}
	// RT-09: without approval a changed gate above the stage starts the check at once.
	next = following(writerFeature("arch", false), Request{Mode: ModeWrite}, []domain.Area{domain.AreaProduct}, all)
	if next == nil || next.Mode != ModeImpact || len(next.Areas) != 2 {
		t.Fatalf("no check: %+v", next)
	}
	// with approval it waits for the approval
	if next := following(writerFeature("arch", true), Request{Mode: ModeWrite}, []domain.Area{domain.AreaProduct}, all); next != nil {
		t.Fatalf("started before the approval: %+v", next)
	}
	// MG-03: nothing follows in a feature without stages
	if next := following(&specdata.Feature{SpecFlow: specdata.FlowLegacy}, Request{Mode: ModeImpact}, nil, all); next != nil {
		t.Fatalf("legacy: %+v", next)
	}
}

// MG-02: a run of the old format is a generation; the list of waiting requests is read from both forms.
func TestOldContext(t *testing.T) {
	if (Request{}).mode() != ModeGenerate {
		t.Fatal("old run")
	}
	old := pendingOf(map[string]any{"pending": map[string]any{"areas": []any{"qa"}}})
	if len(old) != 1 || old[0].Areas[0] != domain.AreaQA {
		t.Fatalf("old pending %+v", old)
	}
	l := addPending(nil, Request{Mode: ModeImpact, Areas: []domain.Area{domain.AreaArch}})
	l = addPending(l, Request{Mode: ModeImpact, Areas: []domain.Area{domain.AreaDesign}})
	l = addPending(l, Request{Mode: ModeGenerate})
	if len(l) != 2 || len(l[0].Areas) != 2 {
		t.Fatalf("pending %+v", l)
	}
}

// WR-05, WR-07, QS-03, R33: what the writer reads and the rules it gets.
func TestPrompt(t *testing.T) {
	f := writerFeature("product", true)
	value := "Освобождаем машины"
	d := PromptData{
		Docs:      map[domain.Area]string{domain.AreaProduct: "# Черновик"},
		Templates: map[domain.Area]string{domain.AreaProduct: "# Шаблон"},
		Discovery: []IssueDiscovery{{Key: "ISS.FMS-0007", Doc: &cycledata.Discovery{Content: "Анализ", Value: &value,
			Questions: []cycledata.Question{{ID: "Q9", QuestionText: cycledata.QuestionText{Title: "Открытый", Recommended: "Да"}}}},
			Requests: []discovery.Request{{Answers: []cycledata.Answer{{QuestionID: "Q2", Question: "Источник", Answer: "VictoriaMetrics"}}}}}},
		Requests: []SpecRequest{{Areas: []string{"product"}, Instructions: "добавь аудит"}},
	}
	p := Prompt(f, Request{Mode: ModeWrite, Areas: []domain.Area{domain.AreaProduct}}, d)
	for _, want := range []string{"[hammurapi:task=write_spec", "areas=product", "Answer of — to Q2 (Источник): VictoriaMetrics", "Open question Q9", "Освобождаем машины",
		"Do not ask about them again", "добавь аудит", "# Шаблон", "never wait for an answer", "**R<n>.**", "FTR.FMS.CAR-0002-R3", "Mockups are not indexed", "touches"} {
		if !strings.Contains(p, want) {
			t.Errorf("no %q in the prompt", want)
		}
	}
	if strings.Contains(strings.ToLower(p), "grill") {
		t.Error("the prompt names a skill")
	}
	if g := Prompt(f, Request{Mode: ModeGenerate, Areas: []domain.Area{domain.AreaTech, domain.AreaQA}}, PromptData{}); !strings.Contains(g, "[hammurapi:task=generate_gates") ||
		!strings.Contains(g, "You ask no questions") || strings.Contains(g, "=== Discovery") {
		t.Errorf("generation prompt: %s", g)
	}
	if g := Prompt(f, Request{Mode: ModeImpact, Areas: []domain.Area{domain.AreaArch}}, PromptData{}); !strings.Contains(g, "unchanged: true") {
		t.Errorf("check prompt: %s", g)
	}
}

// WR-13, RT-04: the answer «skip» decides the gate; touches open a gate only with the permission of the expert.
func TestApplyAnswer(t *testing.T) {
	var nr newRequest
	applyAnswer(&nr, domain.AreaDesign, cycledata.Question{QuestionText: cycledata.QuestionText{SkipArea: "design"}}, OptionSkip, false)
	if len(nr.Skip) != 1 || len(nr.Areas) != 0 {
		t.Fatalf("%+v", nr)
	}
	nr = newRequest{}
	applyAnswer(&nr, domain.AreaDesign, cycledata.Question{QuestionText: cycledata.QuestionText{SkipArea: "design"}}, OptionDo, false)
	if len(nr.Skip) != 0 || nr.Areas[0] != "design" {
		t.Fatalf("%+v", nr)
	}
	touching := cycledata.Question{QuestionText: cycledata.QuestionText{Touches: []string{"product"}}}
	nr = newRequest{}
	if opened := applyAnswer(&nr, domain.AreaArch, touching, "нет, не менять", false); opened != nil || strings.Join(nr.Areas, ",") != "arch" {
		t.Fatalf("opened without a permission: %+v", nr)
	}
	nr = newRequest{}
	if opened := applyAnswer(&nr, domain.AreaArch, touching, "да", true); len(opened) != 1 || strings.Join(nr.Areas, ",") != "arch,product" {
		t.Fatalf("%+v", nr)
	}
}

// QS-06, QS-07: who answers.
func TestAudience(t *testing.T) {
	if audienceOf(domain.AreaArch, cycledata.Question{}) != domain.ExpertTechnical {
		t.Fatal("the experts of the gate by default")
	}
	q := cycledata.Question{QuestionText: cycledata.QuestionText{Audience: "product"}}
	if audienceOf(domain.AreaArch, q) != domain.ExpertProduct {
		t.Fatal("the audience of the question")
	}
}

// WR-15, RT-14: what a brief may name.
func TestCheckAreas(t *testing.T) {
	f := writerFeature("arch", true)
	f.SkippedAreas = []string{"design"}
	all := []specdata.Gate{gate(domain.AreaProduct, domain.GateApproved)}
	if msg := checkAreasOf(f, all, []string{"tech"}, nil, nil); !strings.Contains(msg, "nothing to change yet") {
		t.Fatal(msg)
	}
	if msg := checkAreasOf(f, all, nil, []string{"product"}, nil); msg != "Only design and arch can be skipped" {
		t.Fatal(msg)
	}
	if msg := checkAreasOf(f, all, nil, nil, []string{"arch"}); !strings.Contains(msg, "not skipped") {
		t.Fatal(msg)
	}
	if msg := checkAreasOf(f, all, []string{"bad"}, nil, nil); !strings.Contains(msg, "areas:") {
		t.Fatal(msg)
	}
	if msg := checkAreasOf(f, all, []string{"product"}, nil, []string{"design"}); msg != "" {
		t.Fatal(msg)
	}
	early := writerFeature("product", true)
	if msg := checkAreasOf(early, all, []string{"arch"}, nil, nil); !strings.Contains(msg, "has not started yet") {
		t.Fatal(msg)
	}
	legacy := &specdata.Feature{SpecFlow: specdata.FlowLegacy}
	if msg := checkAreasOf(legacy, all, []string{"design"}, nil, nil); msg != "" {
		t.Fatal(msg)
	}
}

// RQ-07: a gate above the stage is named apart.
func TestRevisionText(t *testing.T) {
	txt := RevisionText("ru", []domain.Area{domain.AreaArch}, []domain.Area{domain.AreaProduct}, "- добавлен аудит", []string{"Борис"}, true)
	for _, want := range []string{"Спецификация обновлена · Архитектура", "добавлен аудит", "Также изменён Продукт: нужно заново согласовать", "Вместе с поручениями: Борис"} {
		if !strings.Contains(txt, want) {
			t.Errorf("no %q in %q", want, txt)
		}
	}
	txt = RevisionText("ru", nil, []domain.Area{domain.AreaProduct}, "", nil, true)
	if !strings.Contains(txt, "Спецификация обновлена · Продукт") || !strings.Contains(txt, "Продукт нужно заново согласовать") {
		t.Errorf("only above the stage: %q", txt)
	}
	if !strings.Contains(RevisionText("en", []domain.Area{domain.AreaTech}, nil, "", nil, false), "Specification updated · Tech specification") {
		t.Error("english")
	}
	if !strings.Contains(FailureText("ru", "нечитаемый результат."), "нечитаемый результат. Документы не изменились") {
		t.Error("failure")
	}
	if !strings.Contains(ConflictText("ru", domain.AreaTech, "нет формата"), "Техспецификация") {
		t.Error("conflict")
	}
}

func checkAreasOf(f *specdata.Feature, all []specdata.Gate, areas, skip, restore []string) string {
	return checkAreas(f, all, agent.SpecBrief{Areas: areas, SkipAreas: skip, RestoreAreas: restore})
}

// QS-15: a gate a run had to submit is not silently left out; one only allowed to change may be.
func TestMissingAreas(t *testing.T) {
	req := Request{Mode: ModeWrite, Areas: []domain.Area{domain.AreaArch, domain.AreaProduct}}
	got := map[domain.Area]Submission{domain.AreaProduct: {}}
	if m := MissingAreas(req, nil, got); len(m) != 1 || m[0] != domain.AreaArch {
		t.Fatalf("%v", m)
	}
	reqs := []SpecRequest{{Answers: []cycledata.Answer{{Touches: []string{"product"}}}}}
	if m := MissingAreas(req, reqs, map[domain.Area]Submission{domain.AreaArch: {}}); len(m) != 0 {
		t.Fatalf("optional: %v", m)
	}
}

// WR-22, RQ-11: the reason of a run without an agent is told in the language of the user.
func TestFailureTextNoAgent(t *testing.T) {
	if !strings.Contains(FailureText("ru", ReasonNoAgent), "Служебный агент не назначен") {
		t.Fatal(FailureText("ru", ReasonNoAgent))
	}
}
