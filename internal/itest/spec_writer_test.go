//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/discovery"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/featuretopics"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/gategen"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/issues"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
)

// FTR.HMR.DEV-0001: the writer of specifications on a real database; the agent
// session is scripted, the machine, the instructions and the notices are real.

const specChannel = "client:hammurapi"

type specRig struct {
	*discRig
	store   *specdata.PG
	topics2 *featuretopics.Service
	svc     *gategen.Service
	gen     gategen.Generator
	// writer scripts the agent session: it gets the request of the run.
	writer func(gategen.Request) (gategen.Generated, error)
	runs   []gategen.Request
	tech   *domain.Principal
	seq    int
	// nabuDown: the session answers as if Nabu did not take the run.
	nabuDown bool
}

func newSpecRig(t *testing.T) *specRig {
	r := &specRig{discRig: newDiscRig(t), store: specdata.NewPG(pool), gen: gategen.Generator{Q: pool}}
	email := func(_ context.Context, id uuid.UUID) string { return r.emails[id] }
	r.topics2 = &featuretopics.Service{Pool: pool, Store: r.store, Nabu: r.nabu.client(), Email: email}
	r.svc = &gategen.Service{Pool: pool, Store: r.store, Events: nopEvents{}}
	r.tech = tu.User("expert:" + r.domainKey + ":technical")
	r.tech.UserID = r.boris.UserID
	r.writer = func(req gategen.Request) (gategen.Generated, error) {
		g := gategen.Generated{Commits: map[domain.Area]string{}, Results: map[domain.Area]gategen.Submission{}}
		for _, a := range req.Areas {
			r.seq++
			g.Commits[a] = "commit-" + uuid.NewString()[:8]
			g.Results[a] = gategen.Submission{Area: a, Content: "# " + string(a), ChangeSummary: "- изменён " + string(a)}
		}
		g.Removed = req.Remove
		return g, nil
	}
	r.engine.Register(gategen.Machine{})
	r.engine.Handle(gategen.Effect, workflows.EffectHandler{Do: func(_ context.Context, _ workflows.RunRef, raw json.RawMessage) ([]workflows.NewEvent, error) {
		var in struct {
			Request gategen.Request `json:"request"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		if r.nabuDown {
			return []workflows.NewEvent{{Type: "nabu_unavailable"}}, nil
		}
		r.runs = append(r.runs, in.Request)
		g, err := r.writer(in.Request)
		if err != nil {
			return nil, workflows.Permanent(err)
		}
		return []workflows.NewEvent{{Type: "gates_generated", Payload: g}}, nil
	}})
	r.engine.Handle(gategen.EffectTrace, workflows.EffectHandler{Do: func(context.Context, workflows.RunRef, json.RawMessage) ([]workflows.NewEvent, error) {
		return nil, nil
	}})
	r.engine.Handle(gategen.EffectDrain, workflows.EffectHandler{Do: (&gategen.DrainEffects{Q: pool}).Do})
	r.engine.Handle(gategen.EffectNotice, workflows.EffectHandler{Do: (&gategen.NoticeEffects{Q: pool, Nabu: r.nabu.client(), Email: email}).Do})
	return r
}

// feature creates a feature on the stage product without a gate of the writer.
func (r *specRig) feature(approval bool) *specdata.Feature {
	r.t.Helper()
	ctx := context.Background()
	var sysID uuid.UUID
	must(r.t, pool.QueryRow(ctx, `SELECT s.id FROM systems s JOIN domains d ON d.id = s.domain_id WHERE d.key = $1 AND s.key = 'SYS'`, r.domainKey).Scan(&sysID))
	_, err := pool.Exec(ctx, `UPDATE domains SET approval_required = $2 WHERE key = $1`, r.domainKey, approval)
	must(r.t, err)
	r.t.Cleanup(func() { // the other tests count the domains that require approval
		_, _ = pool.Exec(context.Background(), `UPDATE domains SET approval_required = true WHERE key = $1`, r.domainKey)
	})
	r.seq++
	stage := "product"
	key := "FTR." + r.domainKey + ".SYS-" + strings.Repeat("0", 3) + string(rune('0'+r.seq%10))
	f := &specdata.Feature{UniqueID: key, SystemID: sysID, Number: r.seq, Title: "Скидка", Branch: "b" + uuid.NewString()[:8], PRNumber: 1, PRURL: "http://pr",
		CreatedBy: r.anna.UserID, SpecFlow: specdata.FlowWriter, SpecStage: &stage}
	must(r.t, r.store.InsertFeature(ctx, f))
	must(r.t, r.store.InsertGate(ctx, &specdata.Gate{FeatureID: f.ID, Area: domain.AreaProduct, Status: domain.GateDraft, HeadCommit: "template", CreatedBy: r.anna.UserID}))
	return r.reload(f)
}

func (r *specRig) reload(f *specdata.Feature) *specdata.Feature {
	r.t.Helper()
	out, err := r.store.FeatureByID(context.Background(), f.ID)
	must(r.t, err)
	return out
}

func (r *specRig) gate(f *specdata.Feature, a domain.Area) *specdata.Gate {
	r.t.Helper()
	g, err := r.store.ActiveGate(context.Background(), f.ID, a)
	if errors.Is(err, specdata.ErrNotFound) {
		return nil
	}
	must(r.t, err)
	return g
}

// approve does what approvals.Approve does with the database.
func (r *specRig) approve(f *specdata.Feature, a domain.Area) {
	r.t.Helper()
	ctx := context.Background()
	g := r.gate(f, a)
	reapproved := g.ApprovedCommit != nil && *g.ApprovedCommit != g.HeadCommit
	head := g.HeadCommit
	g.Status, g.ApprovedCommit = domain.GateApproved, &head
	must(r.t, r.store.SaveGate(ctx, g))
	all, err := r.store.ActiveGates(ctx, f.ID)
	must(r.t, err)
	f = r.reload(f)
	must(r.t, r.gen.AfterApproval(ctx, f, a, reapproved, all, r.anna.UserID))
}

// topic creates the topic of a user with a first message.
func (r *specRig) topic(p *domain.Principal, f *specdata.Feature) string {
	r.t.Helper()
	sent, err := r.topics2.Send(context.Background(), p, f.UniqueID, featuretopics.SendInput{Text: "привет", Area: "product"})
	must(r.t, err)
	return sent.ConversationID
}

func (r *specRig) brief(p *domain.Principal, f *specdata.Feature, conv string, b agent.SpecBrief) (*gategen.SpecRequest, error) {
	b.FeatureKey = f.UniqueID
	return gategen.RequestRevision(context.Background(), pool, r.store, specChannel, p, r.grant(p, conv, specChannel), b)
}

func (r *specRig) notices(conv string) []nabu.Notice {
	r.nabu.mu.Lock()
	defer r.nabu.mu.Unlock()
	if tp := r.nabu.topics[conv]; tp != nil {
		return append([]nabu.Notice{}, tp.notices...)
	}
	return nil
}

func (r *specRig) lastEvents(f *specdata.Feature, a domain.Area) []string {
	r.t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT e.event_type::text FROM gate_events e JOIN gates g ON g.id = e.gate_id
		WHERE g.feature_id = $1 AND g.area = $2 ORDER BY e.created_at, e.id`, f.ID, a)
	must(r.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		must(r.t, rows.Scan(&s))
		out = append(out, s)
	}
	return out
}

// WR-01, WR-03, WR-04, WR-08, WR-10, WR-11: the stages from product to tech and qa.
func TestSpecWriterStages(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	r.writer = func(req gategen.Request) (gategen.Generated, error) {
		g := gategen.Generated{Commits: map[domain.Area]string{}, Results: map[domain.Area]gategen.Submission{}}
		for _, a := range req.Areas {
			g.Commits[a] = "c-" + string(a)
			s := gategen.Submission{Area: a, Content: "# doc"}
			if a == domain.AreaProduct {
				s.Questions = []cycledata.QuestionInput{{QuestionText: cycledata.QuestionText{Title: "Порог", Recommended: "2 часа", Why: "пики"}}}
			}
			g.Results[a] = s
		}
		return g, nil
	}
	must(t, r.gen.Write(ctx, f, []domain.Area{domain.AreaProduct}, r.anna.UserID))
	r.run()
	g := r.gate(f, domain.AreaProduct)
	if g.HeadCommit != "c-product" || g.Status != domain.GateDraft || g.Generated {
		t.Fatalf("product: %+v", g)
	}
	qs, err := gategen.LoadQuestions(ctx, pool, f.ID, domain.AreaProduct)
	must(t, err)
	if len(qs.Questions) != 1 || qs.Questions[0].ID != "Q1" || qs.Round != 1 || qs.RoundsLeft() != 4 {
		t.Fatalf("questions: %+v", qs)
	}
	// WR-09: nothing else starts by itself
	if r.gate(f, domain.AreaDesign) != nil || len(r.runs) != 1 {
		t.Fatalf("runs: %+v", r.runs)
	}
	r.approve(f, domain.AreaProduct)
	r.run()
	if s := r.reload(f).SpecStage; *s != "design" || r.gate(f, domain.AreaDesign) == nil {
		t.Fatalf("stage %s", *s)
	}
	if r.gate(f, domain.AreaTech) != nil {
		t.Fatal("WR-09: tech started after product")
	}
	r.approve(f, domain.AreaDesign)
	r.run()
	r.approve(f, domain.AreaArch)
	r.run()
	if s := r.reload(f).SpecStage; *s != specdata.StageGenerated {
		t.Fatalf("stage %s", *s)
	}
	for _, a := range []domain.Area{domain.AreaTech, domain.AreaQA} {
		if g := r.gate(f, a); g == nil || !g.Generated || g.Status != domain.GateDraft {
			t.Fatalf("%s: %+v", a, g)
		}
	}
}

// QS-05, QS-06, QS-07, QS-08: answers on the page.
func TestSpecWriterAnswers(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	must(t, r.gen.Write(ctx, f, []domain.Area{domain.AreaProduct}, r.anna.UserID))
	first := true
	r.writer = func(req gategen.Request) (gategen.Generated, error) {
		s := gategen.Submission{Area: domain.AreaProduct, Content: "# doc"}
		if first {
			first = false
			s.Questions = []cycledata.QuestionInput{
				{QuestionText: cycledata.QuestionText{Title: "Один", Recommended: "да", Options: []string{"нет"}}},
				{QuestionText: cycledata.QuestionText{Title: "Два", Recommended: "да"}},
				{QuestionText: cycledata.QuestionText{Title: "Три", Recommended: "да", Audience: "technical"}},
			}
		} else {
			s.Questions = []cycledata.QuestionInput{{ID: "Q2", QuestionText: cycledata.QuestionText{Title: "Два", Recommended: "да"}}, {ID: "Q3", QuestionText: cycledata.QuestionText{Title: "Три", Recommended: "да"}}}
		}
		return gategen.Generated{Commits: map[domain.Area]string{domain.AreaProduct: "c" + uuid.NewString()[:6]}, Results: map[domain.Area]gategen.Submission{domain.AreaProduct: s}}, nil
	}
	r.run()
	// a question for other experts
	_, err := r.svc.AnswerQuestions(ctx, r.anna, f.UniqueID, gategen.AnswersInput{Area: "product", Answers: []gategen.AnswerInput{{QuestionID: "Q3", Choice: "recommended"}}})
	tu.Code(t, err, 403, "question_other_expert")
	_, err = r.svc.AnswerQuestions(ctx, r.anna, f.UniqueID, gategen.AnswersInput{Area: "product", Answers: []gategen.AnswerInput{{QuestionID: "Q9", Choice: "recommended"}}})
	tu.Code(t, err, 422, "unknown_question")
	opt := 5
	_, err = r.svc.AnswerQuestions(ctx, r.anna, f.UniqueID, gategen.AnswersInput{Area: "product", Answers: []gategen.AnswerInput{{QuestionID: "Q1", Choice: "option", Option: &opt}}})
	tu.Code(t, err, 422, "invalid_answer")
	req, err := r.svc.AnswerQuestions(ctx, r.anna, f.UniqueID, gategen.AnswersInput{Area: "product", Answers: []gategen.AnswerInput{{QuestionID: "Q1", Choice: "recommended"}}})
	must(t, err)
	r.run()
	got, err := gategen.GetRequest(ctx, pool, req.ID)
	must(t, err)
	if got.State != gategen.RequestDone || got.Answers[0].Answer != "да" || got.Answers[0].Area != "product" || len(got.Commits) != 1 {
		t.Fatalf("%+v", got)
	}
	qs, err := gategen.LoadQuestions(ctx, pool, f.ID, domain.AreaProduct)
	must(t, err)
	// QS-17: a technical question kept on product closes, even without its audience in the call
	if len(qs.Questions) != 1 || qs.Questions[0].ID != "Q2" {
		t.Fatalf("%+v", qs)
	}
	counts, err := r.gen.OpenCounts(ctx, f.ID)
	must(t, err)
	if counts[domain.AreaProduct] != 1 {
		t.Fatalf("%v", counts)
	}
}

// WR-12, WR-13, WR-14: a gate is skipped by the answer of an expert and comes back by an instruction.
func TestSpecWriterSkip(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	must(t, r.gen.Write(ctx, f, []domain.Area{domain.AreaProduct}, r.anna.UserID))
	r.run()
	script := r.writer
	r.writer = func(req gategen.Request) (gategen.Generated, error) {
		if len(req.Areas) == 1 && req.Areas[0] == domain.AreaDesign {
			return gategen.Generated{Commits: map[domain.Area]string{}, Results: map[domain.Area]gategen.Submission{domain.AreaDesign: {Area: domain.AreaDesign,
				Questions: []cycledata.QuestionInput{{QuestionText: cycledata.QuestionText{Title: "Нужен ли дизайн?", Recommended: "что угодно", SkipArea: "design"}}}}}}, nil
		}
		return script(req)
	}
	r.approve(f, domain.AreaProduct)
	r.run()
	if r.gate(f, domain.AreaDesign) != nil {
		t.Fatal("a gate without a document")
	}
	qs, err := gategen.LoadQuestions(ctx, pool, f.ID, domain.AreaDesign)
	must(t, err)
	if len(qs.Questions) != 1 || qs.Questions[0].Recommended != gategen.OptionSkip || len(qs.Questions[0].Options) != 2 {
		t.Fatalf("the server sets the options: %+v", qs.Questions)
	}
	_, err = r.svc.AnswerQuestions(ctx, r.anna, f.UniqueID, gategen.AnswersInput{Area: "design", Answers: []gategen.AnswerInput{{QuestionID: "Q1", Choice: "recommended"}}})
	must(t, err)
	r.run()
	f = r.reload(f)
	if *f.SpecStage != "arch" || len(f.SkippedAreas) != 1 || r.gate(f, domain.AreaArch) == nil || r.gate(f, domain.AreaDesign) != nil {
		t.Fatalf("after the skip: %+v", f)
	}
	r.writer = script
	r.approve(f, domain.AreaArch)
	r.run()
	conv := r.topic(r.anna, f)
	_, err = r.brief(r.anna, f, conv, agent.SpecBrief{RestoreAreas: []string{"design"}, Instructions: "появился экран настроек"})
	must(t, err)
	r.run()
	f = r.reload(f)
	if len(f.SkippedAreas) != 0 || r.gate(f, domain.AreaDesign) == nil {
		t.Fatalf("not restored: %+v", f)
	}
}

// RQ-01…RQ-04, RQ-06, RT-02, RT-06, RT-07, RT-08, RT-11, RT-12, RT-13: instructions and the way back.
func TestSpecWriterWayBack(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	must(t, r.gen.Write(ctx, f, []domain.Area{domain.AreaProduct}, r.anna.UserID))
	r.run()
	r.approve(f, domain.AreaProduct)
	r.run()
	r.approve(f, domain.AreaDesign)
	r.run()
	conv := r.topic(r.anna, f)
	// refusals
	_, err := gategen.RequestRevision(ctx, pool, r.store, specChannel, r.anna, r.grant(r.anna, conv, "client:telegram"), agent.SpecBrief{FeatureKey: f.UniqueID, Areas: []string{"product"}})
	if !strings.Contains(toolMsg(err), "from the chat of the feature") {
		t.Fatal(toolMsg(err))
	}
	_, err = r.brief(r.anna, f, "другая", agent.SpecBrief{Areas: []string{"product"}})
	if !strings.Contains(toolMsg(err), "Open the Specification tab") {
		t.Fatal(toolMsg(err))
	}
	carlConv := r.topic(r.carl, f)
	_, err = r.brief(r.carl, f, carlConv, agent.SpecBrief{Areas: []string{"product"}})
	if !strings.Contains(toolMsg(err), "Only experts of domain") {
		t.Fatal(toolMsg(err))
	}
	_, err = r.brief(r.anna, f, conv, agent.SpecBrief{Areas: []string{"tech"}})
	if !strings.Contains(toolMsg(err), "nothing to change yet") {
		t.Fatal(toolMsg(err))
	}
	// the arch stage: a change of the approved product
	before := r.gate(f, domain.AreaProduct).HeadCommit
	req, err := r.brief(r.anna, f, conv, agent.SpecBrief{Areas: []string{"product"}, Instructions: "добавь аудит"})
	must(t, err)
	r.run()
	got, err := gategen.GetRequest(ctx, pool, req.ID)
	must(t, err)
	product := r.gate(f, domain.AreaProduct)
	if got.State != gategen.RequestDone || product.Status != domain.GateDraft || product.HeadCommit == before {
		t.Fatalf("%+v %+v", got, product)
	}
	f = r.reload(f)
	if *f.SpecStage != "arch" {
		t.Fatalf("RT-06: stage %s", *f.SpecStage)
	}
	notes := r.notices(conv)
	if len(notes) != 1 || !strings.Contains(notes[0].Text, "Спецификация обновлена · Продукт") || !strings.Contains(notes[0].Text, "заново согласовать") {
		t.Fatalf("notices %+v", notes)
	}
	// RT-07, RT-08: approved again: design has nothing to change, arch changes; no second check
	r.runs = nil
	script := r.writer
	r.writer = func(q gategen.Request) (gategen.Generated, error) {
		if q.Mode != gategen.ModeImpact {
			return script(q)
		}
		return gategen.Generated{Commits: map[domain.Area]string{domain.AreaArch: "arch-2"}, Results: map[domain.Area]gategen.Submission{
			domain.AreaDesign: {Area: domain.AreaDesign, Unchanged: true}, domain.AreaArch: {Area: domain.AreaArch, Content: "# arch 2"}}}, nil
	}
	designBefore := r.gate(f, domain.AreaDesign)
	r.approve(f, domain.AreaProduct)
	r.run()
	if len(r.runs) != 1 || r.runs[0].Mode != gategen.ModeImpact || len(r.runs[0].Areas) != 2 {
		t.Fatalf("runs %+v", r.runs)
	}
	design := r.gate(f, domain.AreaDesign)
	if design.Status != domain.GateApproved || design.HeadCommit != designBefore.HeadCommit {
		t.Fatalf("design %+v", design)
	}
	ev := r.lastEvents(f, domain.AreaDesign)
	if ev[len(ev)-1] != "checked" {
		t.Fatalf("events %v", ev)
	}
	if a := r.gate(f, domain.AreaArch); a.HeadCommit != "arch-2" || a.Status != domain.GateDraft {
		t.Fatalf("arch %+v", a)
	}
	r.writer = script
	// on to tech and qa
	r.approve(f, domain.AreaArch)
	r.run()
	if r.gate(f, domain.AreaTech) == nil {
		t.Fatal("no tech")
	}
	// RT-12: an instruction about qa changes qa alone
	r.approve(f, domain.AreaTech)
	r.approve(f, domain.AreaQA)
	techHead := r.gate(f, domain.AreaTech).HeadCommit
	r.runs = nil
	_, err = r.brief(r.anna, f, conv, agent.SpecBrief{Areas: []string{"qa"}, Instructions: "добавь кейс на отмену"})
	must(t, err)
	r.run()
	if len(r.runs) != 1 || r.runs[0].Mode != gategen.ModeGenerate || len(r.runs[0].Areas) != 1 || r.gate(f, domain.AreaTech).HeadCommit != techHead {
		t.Fatalf("runs %+v", r.runs)
	}
	if q := r.gate(f, domain.AreaQA); q.Status != domain.GateDraft {
		t.Fatalf("qa %+v", q)
	}
	// RT-13: tech with product: product now, tech later; the instruction stays in the history
	r.approve(f, domain.AreaQA)
	r.runs = nil
	_, err = r.brief(r.anna, f, conv, agent.SpecBrief{Areas: []string{"product", "tech"}, Instructions: "ещё правка"})
	must(t, err)
	r.run()
	if len(r.runs) != 1 || r.runs[0].Mode != gategen.ModeWrite || len(r.runs[0].Areas) != 1 || r.runs[0].Areas[0] != domain.AreaProduct {
		run, _ := workflows.LatestRun(ctx, pool, gategen.Kind, f.ID)
		t.Fatalf("runs %+v run %+v", r.runs, run)
	}
	hist, err := gategen.History(ctx, pool, f.ID)
	must(t, err)
	if hist[len(hist)-1].Instructions != "ещё правка" {
		t.Fatalf("%+v", hist)
	}
}

// RQ-06: instructions given during a run go with one next run to both authors.
func TestSpecWriterQueue(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	must(t, r.gen.Write(ctx, f, []domain.Area{domain.AreaProduct}, r.anna.UserID))
	annaConv, borisConv := r.topic(r.anna, f), r.topic(r.boris, f)
	// both before the engine runs: the first run takes them both or the next one does
	must2 := func(p *domain.Principal, conv string) {
		_, err := r.brief(p, f, conv, agent.SpecBrief{Areas: []string{"product"}, Instructions: "правка"})
		must(t, err)
	}
	must2(r.anna, annaConv)
	must2(r.boris, borisConv)
	r.run()
	if len(r.runs) != 1 {
		t.Fatalf("runs %+v", r.runs)
	}
	if n := r.notices(annaConv); len(n) != 1 || !strings.Contains(n[0].Text, "Вместе с поручениями: Борис") {
		t.Fatalf("%+v", n)
	}
	if n := r.notices(borisConv); len(n) != 1 {
		t.Fatalf("%+v", n)
	}
}

// RQ-09: a failure keeps the documents and «Retry» returns the instructions.
func TestSpecWriterFailure(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	must(t, r.gen.Write(ctx, f, []domain.Area{domain.AreaProduct}, r.anna.UserID))
	r.run()
	conv := r.topic(r.anna, f)
	before := r.gate(f, domain.AreaProduct).HeadCommit
	script := r.writer
	r.writer = func(gategen.Request) (gategen.Generated, error) {
		return gategen.Generated{}, errors.New("нечитаемый результат")
	}
	req, err := r.brief(r.anna, f, conv, agent.SpecBrief{Areas: []string{"product"}, Instructions: "правка"})
	must(t, err)
	r.run()
	run, err := workflows.LatestRun(ctx, pool, gategen.Kind, f.ID)
	must(t, err)
	got, err := gategen.GetRequest(ctx, pool, req.ID)
	must(t, err)
	if run.State != workflows.StateBlocked || got.State != gategen.RequestFailed || r.gate(f, domain.AreaProduct).HeadCommit != before {
		t.Fatalf("%s %+v", run.State, got)
	}
	if n := r.notices(conv); len(n) != 1 || !strings.Contains(n[0].Text, "Не удалось изменить спецификацию") {
		t.Fatalf("%+v", n)
	}
	// the run approved meanwhile waits in the blocked run
	r.writer = script
	must(t, r.svc.Retry(ctx, r.anna, f.UniqueID))
	r.run()
	got, err = gategen.GetRequest(ctx, pool, req.ID)
	must(t, err)
	if got.State != gategen.RequestDone || r.gate(f, domain.AreaProduct).HeadCommit == before {
		t.Fatalf("after retry %+v", got)
	}
	tu.Code(t, r.svc.Retry(ctx, r.anna, f.UniqueID), 409, "not_blocked")
}

// ED-03: a run waits for the end of a manual edit.
func TestSpecWriterWaitsForEdit(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	_, ok, err := r.store.AcquireLock(ctx, f.ID, r.anna.UserID)
	must(t, err)
	if !ok {
		t.Fatal("no lock")
	}
	must(t, r.gen.Write(ctx, f, []domain.Area{domain.AreaProduct}, r.anna.UserID))
	r.run()
	if len(r.runs) != 0 {
		t.Fatalf("started under the lock: %+v", r.runs)
	}
	active, err := r.gen.Active(ctx, f.ID)
	must(t, err)
	if active == nil || !active.WaitingLock || active.State != "queued" {
		t.Fatalf("%+v", active)
	}
	must(t, r.store.ReleaseLock(ctx, f.ID, r.anna.UserID))
	_, err = pool.Exec(ctx, `UPDATE workflow_runs SET next_run_at = now() WHERE subject_id = $1`, f.ID)
	must(t, err)
	r.run()
	if len(r.runs) != 1 {
		t.Fatalf("runs %+v", r.runs)
	}
	if active, _ := r.gen.Active(ctx, f.ID); active != nil {
		t.Fatalf("%+v", active)
	}
}

// WR-16, RT-09: a domain without approval moves by the button and checks at once.
func TestSpecWriterNoApproval(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(false)
	must(t, r.gen.Write(ctx, f, []domain.Area{domain.AreaProduct}, r.anna.UserID))
	if _, err := r.svc.Advance(ctx, r.anna, f.UniqueID); err == nil {
		t.Fatal("moved during a run")
	} else {
		tu.Code(t, err, 409, "spec_run_active")
	}
	r.run()
	stage, err := r.svc.Advance(ctx, r.anna, f.UniqueID)
	must(t, err)
	if stage != "design" {
		t.Fatal(stage)
	}
	r.run()
	stage, err = r.svc.Advance(ctx, r.anna, f.UniqueID)
	must(t, err)
	if stage != "arch" {
		t.Fatal(stage)
	}
	r.run()
	conv := r.topic(r.anna, f)
	r.runs = nil
	_, err = r.brief(r.anna, f, conv, agent.SpecBrief{Areas: []string{"product"}, Instructions: "правка"})
	must(t, err)
	r.run()
	if len(r.runs) != 2 || r.runs[1].Mode != gategen.ModeImpact {
		t.Fatalf("no check: %+v", r.runs)
	}
	stage, err = r.svc.Advance(ctx, r.anna, f.UniqueID)
	must(t, err)
	if stage != specdata.StageGenerated {
		t.Fatal(stage)
	}
	r.run()
	if r.gate(f, domain.AreaTech) == nil || r.gate(f, domain.AreaQA) == nil {
		t.Fatal("no tech and qa")
	}
	_, err = r.svc.Advance(ctx, r.anna, f.UniqueID)
	tu.Code(t, err, 409, "no_next_stage")
}

// MG-03, MG-04: a feature in the specification on the day of the release.
func TestSpecWriterLegacy(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	_, err := pool.Exec(ctx, `UPDATE features SET spec_flow = 'legacy', spec_stage = NULL WHERE id = $1`, f.ID)
	must(t, err)
	f = r.reload(f)
	r.approve(f, domain.AreaProduct)
	r.run()
	if len(r.runs) != 1 || r.runs[0].Mode != gategen.ModeGenerate {
		t.Fatalf("old condition: %+v", r.runs)
	}
	conv := r.topic(r.anna, f)
	_, err = r.brief(r.anna, f, conv, agent.SpecBrief{Areas: []string{"design"}, Instructions: "нужен дизайн"})
	must(t, err)
	r.run()
	if r.gate(f, domain.AreaDesign) == nil {
		t.Fatal("no design")
	}
}

// MG-06, MG-07: «Next stage» of an old feature goes through the gates it has not reached.
func TestSpecWriterLegacyAdvance(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(false)
	must(t, r.gen.Write(ctx, f, []domain.Area{domain.AreaProduct}, r.anna.UserID))
	r.run()
	_, err := pool.Exec(ctx, `UPDATE features SET spec_flow = 'legacy', spec_stage = NULL WHERE id = $1`, f.ID)
	must(t, err)
	runs := len(r.runs)
	stage, err := r.svc.Advance(ctx, r.anna, f.UniqueID)
	must(t, err)
	r.run()
	if stage != "design" || len(r.runs) != runs+1 || r.runs[runs].Mode != gategen.ModeWrite || r.runs[runs].Areas[0] != domain.AreaDesign {
		t.Fatalf("stage %q runs %+v", stage, r.runs[runs:])
	}
	if got := r.reload(f); got.SpecFlow != specdata.FlowWriter || got.SpecStage == nil || *got.SpecStage != "design" {
		t.Fatalf("%+v", got)
	}

	// with arch written only tech and qa are left, as before
	_, err = pool.Exec(ctx, `UPDATE features SET spec_flow = 'legacy', spec_stage = NULL WHERE id = $1`, f.ID)
	must(t, err)
	seed := r.reload(f)
	must(t, r.gen.Write(ctx, seed, []domain.Area{domain.AreaArch}, r.anna.UserID))
	r.run()
	runs = len(r.runs)
	stage, err = r.svc.Advance(ctx, r.anna, f.UniqueID)
	must(t, err)
	r.run()
	if stage != specdata.StageGenerated || r.runs[runs].Mode != gategen.ModeGenerate {
		t.Fatalf("stage %q runs %+v", stage, r.runs[runs:])
	}
}

// TP-01, TP-02, TP-05, TP-06, TP-09, TP-10: the topic of a feature.
func TestFeatureTopics(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	v, err := r.topics2.Topic(ctx, r.anna, f.UniqueID)
	must(t, err)
	if v.ConversationID != nil || !strings.HasPrefix(v.Title, f.UniqueID+" — ") {
		t.Fatalf("draft %+v", v)
	}
	a := r.topic(r.anna, f)
	b := r.topic(r.boris, f)
	if a == b || r.topic(r.anna, f) != a {
		t.Fatal("topics of users")
	}
	r.nabu.mu.Lock()
	msg := r.nabu.topics[a].messages[0]
	r.nabu.mu.Unlock()
	if c := msg["context"].(map[string]any); c["type"] != "feature" || c["key"] != f.UniqueID || c["area"] != "product" {
		t.Fatalf("context %+v", msg)
	}
	r.topics2.Archive(ctx, f.ID)
	if v, _ := r.topics2.Topic(ctx, r.anna, f.UniqueID); v.ConversationID != nil {
		t.Fatal("not archived")
	}
	r.nabu.mu.Lock()
	archived := r.nabu.topics[a].archived && r.nabu.topics[b].archived
	r.nabu.mu.Unlock()
	if !archived {
		t.Fatal("archived in Nabu")
	}
	if n := r.topic(r.anna, f); n == a {
		t.Fatal("a new topic after the archive")
	}
}

// WR-05, WR-06: the Discovery of the accepted and the merged issue reaches the writer.
func TestSpecWriterDiscoveryInput(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	for _, title := range []string{"Принятая", "Объединённая"} {
		is, err := r.iss.Create(ctx, r.anna, issues.CreateInput{Type: domain.IssueIdea, Domain: r.domainKey, Title: title, Description: title})
		must(t, err)
		_, err = pool.Exec(ctx, `INSERT INTO feature_issues (feature_id, issue_id) VALUES ($1,$2)`, f.ID, is.ID)
		must(t, err)
		_, err = discovery.Save(ctx, pool, is, agent.DiscoveryInput{Content: "Анализ: " + title, Value: "ценность"}, true, nil)
		must(t, err)
	}
	got, err := gategen.IssueDiscoveries(ctx, pool, f.ID)
	must(t, err)
	p := gategen.Prompt(f, gategen.Request{Mode: gategen.ModeWrite, Areas: []domain.Area{domain.AreaProduct}}, gategen.PromptData{Discovery: got})
	for _, want := range []string{"Анализ: Принятая", "Анализ: Объединённая", "Value: ценность"} {
		if !strings.Contains(p, want) {
			t.Errorf("no %q in the prompt", want)
		}
	}
}

// RT-15: a gate brought back above an approved one checks it after its approval.
func TestSpecWriterRestoredGateChecksBelow(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	_, err := pool.Exec(ctx, `UPDATE features SET spec_stage = 'arch', skipped_areas = '{design}' WHERE id = $1`, f.ID)
	must(t, err)
	must(t, r.store.InsertGate(ctx, &specdata.Gate{FeatureID: f.ID, Area: domain.AreaArch, Status: domain.GateApproved, HeadCommit: "a1", CreatedBy: r.anna.UserID}))
	conv := r.topic(r.anna, f)
	_, err = r.brief(r.anna, f, conv, agent.SpecBrief{Areas: []string{"design"}, Instructions: "нужен экран"})
	must(t, err)
	r.run()
	if f = r.reload(f); len(f.SkippedAreas) != 0 || r.gate(f, domain.AreaDesign) == nil {
		t.Fatalf("not brought back: %+v", f)
	}
	r.runs = nil
	r.approve(f, domain.AreaDesign)
	r.run()
	if len(r.runs) != 1 || r.runs[0].Mode != gategen.ModeImpact || r.runs[0].Areas[0] != domain.AreaArch {
		t.Fatalf("runs %+v", r.runs)
	}
}

// ED-07: the end of a manual edit starts the waiting run at once.
func TestSpecWriterWakesAfterEdit(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	_, _, err := r.store.AcquireLock(ctx, f.ID, r.anna.UserID)
	must(t, err)
	must(t, r.gen.Write(ctx, f, []domain.Area{domain.AreaProduct}, r.anna.UserID))
	r.run()
	must(t, r.store.ReleaseLock(ctx, f.ID, r.anna.UserID))
	r.gen.Wake(ctx, f.ID)
	r.run()
	if len(r.runs) != 1 {
		t.Fatalf("runs %+v", r.runs)
	}
}

// RQ-10, RQ-12: without Nabu a run waits and keeps its instructions; a blocked run tells the new ones.
func TestSpecWriterWaitsForNabu(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	must(t, r.gen.Write(ctx, f, []domain.Area{domain.AreaProduct}, r.anna.UserID))
	r.run()
	conv := r.topic(r.anna, f)
	req, err := r.brief(r.anna, f, conv, agent.SpecBrief{Areas: []string{"product"}, Instructions: "правка"})
	must(t, err)
	r.nabuDown = true
	r.run()
	waiting, err := workflows.LatestRun(ctx, pool, gategen.Kind, f.ID)
	must(t, err)
	got, err := gategen.GetRequest(ctx, pool, req.ID)
	must(t, err)
	if waiting.State != "queued" || waiting.NextRunAt == nil || got.State != gategen.RequestRunning {
		t.Fatalf("%s %+v", waiting.State, got)
	}
	_, err = pool.Exec(ctx, `UPDATE workflow_runs SET next_run_at = now() WHERE id = $1`, waiting.ID)
	must(t, err)
	r.nabuDown = false
	r.run()
	if got, _ = gategen.GetRequest(ctx, pool, req.ID); got.State != gategen.RequestDone {
		t.Fatalf("%+v", got)
	}
}

// WR-25: «Next stage» skips neither a gate without a document nor a stopped run.
func TestSpecWriterAdvanceSkipsNothing(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(false)
	must(t, r.gen.Write(ctx, f, []domain.Area{domain.AreaProduct}, r.anna.UserID))
	r.run()
	script := r.writer
	r.writer = func(gategen.Request) (gategen.Generated, error) { return gategen.Generated{}, errors.New("сбой") }
	_, err := r.svc.Advance(ctx, r.anna, f.UniqueID)
	must(t, err)
	r.run()
	_, err = r.svc.Advance(ctx, r.anna, f.UniqueID)
	tu.Code(t, err, 409, "writer_blocked")
	r.writer = func(gategen.Request) (gategen.Generated, error) {
		return gategen.Generated{Commits: map[domain.Area]string{}, Results: map[domain.Area]gategen.Submission{domain.AreaDesign: {Area: domain.AreaDesign,
			Questions: []cycledata.QuestionInput{{QuestionText: cycledata.QuestionText{Title: "Нужен ли дизайн?", Recommended: "x", SkipArea: "design"}}}}}}, nil
	}
	must(t, r.svc.Retry(ctx, r.anna, f.UniqueID))
	r.run()
	_, err = r.svc.Advance(ctx, r.anna, f.UniqueID)
	tu.Code(t, err, 409, "gate_not_written")
	r.writer = script
}

// RQ-12: an instruction given to a stopped writer says it waits for «Retry».
func TestSpecWriterBlockedInstruction(t *testing.T) {
	r := newSpecRig(t)
	ctx := context.Background()
	f := r.feature(true)
	must(t, r.gen.Write(ctx, f, []domain.Area{domain.AreaProduct}, r.anna.UserID))
	r.writer = func(gategen.Request) (gategen.Generated, error) { return gategen.Generated{}, errors.New("сбой") }
	r.run()
	conv := r.topic(r.anna, f)
	req, err := r.brief(r.anna, f, conv, agent.SpecBrief{Areas: []string{"product"}, Instructions: "правка"})
	must(t, err)
	if !req.Blocked || req.State != gategen.RequestQueued {
		t.Fatalf("%+v", req)
	}
}
