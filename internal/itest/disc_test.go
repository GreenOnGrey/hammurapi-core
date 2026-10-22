//go:build integration

package itest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/agent"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/discovery"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/issues"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/issuetopics"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/mcp"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
)

// ─── A fake of the Client API of Nabu: topics, messages, notices ────

type fakeTopic struct {
	id, email, title string
	archived         bool
	messages         []map[string]any
	notices          []nabu.Notice
}

type fakeNabu struct {
	mu       sync.Mutex
	srv      *httptest.Server
	topics   map[string]*fakeTopic
	created  int
	failMsg  int  // the next messages answer 500
	failGet  bool // reading a conversation answers 500
	failNote int  // the next notices are stored, then answer 500 (a lost answer)
}

func newFakeNabu(t *testing.T) *fakeNabu {
	f := &fakeNabu{topics: map[string]*fakeTopic{}}
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	notFound := func(w http.ResponseWriter) {
		write(w, 404, map[string]any{"error": map[string]string{"code": "not_found", "message": "conversation not found"}})
	}
	// topic finds a topic of the user of the delegation.
	topic := func(r *http.Request) *fakeTopic {
		tp := f.topics[r.PathValue("id")]
		if tp == nil || tp.email != r.Header.Get(nabu.DelegationHeader) {
			return nil
		}
		return tp
	}
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		write(w, 200, map[string]any{"access_token": "tok", "expires_in": 3600})
	})
	mux.HandleFunc("POST /client/v1/me/conversations", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Title string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		tp := &fakeTopic{id: uuid.NewString(), email: r.Header.Get(nabu.DelegationHeader), title: in.Title}
		f.topics[tp.id] = tp
		f.created++
		write(w, 201, map[string]any{"id": tp.id, "kind": "topic", "title": tp.title})
	})
	mux.HandleFunc("GET /client/v1/me/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failGet {
			write(w, 500, map[string]any{})
			return
		}
		tp := topic(r)
		if tp == nil {
			notFound(w)
			return
		}
		var archived any
		if tp.archived {
			archived = time.Now()
		}
		write(w, 200, map[string]any{"id": tp.id, "kind": "topic", "title": tp.title, "archivedAt": archived})
	})
	mux.HandleFunc("PATCH /client/v1/me/conversations/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Title string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		tp := topic(r)
		if tp == nil {
			notFound(w)
			return
		}
		tp.title = in.Title
		write(w, 200, map[string]any{"id": tp.id})
	})
	mux.HandleFunc("POST /client/v1/me/conversations/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failMsg > 0 {
			f.failMsg--
			write(w, 500, map[string]any{})
			return
		}
		tp := topic(r)
		if tp == nil {
			notFound(w)
			return
		}
		tp.messages = append(tp.messages, in)
		write(w, 202, map[string]any{"message": map[string]any{"id": uuid.NewString(), "role": "user", "text": in["text"]}})
	})
	mux.HandleFunc("POST /client/v1/me/conversations/{id}/notices", func(w http.ResponseWriter, r *http.Request) {
		var in nabu.Notice
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		defer f.mu.Unlock()
		tp := topic(r)
		if tp == nil {
			notFound(w)
			return
		}
		for _, n := range tp.notices {
			if n.IdempotencyKey == in.IdempotencyKey {
				write(w, 200, map[string]any{})
				return
			}
		}
		tp.notices = append(tp.notices, in)
		if f.failNote > 0 {
			f.failNote--
			write(w, 500, map[string]any{})
			return
		}
		write(w, 201, map[string]any{})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNabu) client() *nabu.Client { return nabu.New(f.srv.URL, "hammurapi", "secret") }

func (f *fakeNabu) topicsOf(email string) []*fakeTopic {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*fakeTopic
	for _, tp := range f.topics {
		if tp.email == email {
			out = append(out, tp)
		}
	}
	return out
}

func (f *fakeNabu) topic(id string) fakeTopic {
	f.mu.Lock()
	defer f.mu.Unlock()
	if tp := f.topics[id]; tp != nil {
		return *tp
	}
	return fakeTopic{}
}

// ─── The rig ─────────────────────────────────────────────────────────

type discRig struct {
	t              *testing.T
	nabu           *fakeNabu
	topics         *issuetopics.Service
	iss            *issues.Service
	disc           *discovery.Service
	engine         *workflows.Engine
	anna, boris    *domain.Principal
	carl           *domain.Principal // not an expert of the domain
	emails         map[uuid.UUID]string
	research       func(reqs int) ([]workflows.NewEvent, error)
	domainKey, alt string
}

func newDiscRig(t *testing.T) *discRig {
	ctx := context.Background()
	r := &discRig{t: t, nabu: newFakeNabu(t), emails: map[uuid.UUID]string{}}
	metricSource(t, "ch", "clickhouse")
	suffix := strings.ToUpper(uuid.NewString()[:4])
	r.domainKey, r.alt = "D"+suffix[:3], "E"+suffix[:3]
	user := func(name string, expert bool) *domain.Principal {
		var id uuid.UUID
		email := strings.ToLower(name) + "-" + suffix + "@company.ru"
		must(t, pool.QueryRow(ctx, `INSERT INTO users (provider_uid, username, display_name, language, agent_name, agent_tone, email)
			VALUES ($1,$1,$2,'ru','Кодекс','business',$3) RETURNING id`, name+suffix, name, email).Scan(&id))
		r.emails[id] = email
		var p *domain.Principal
		if expert {
			p = tu.User("expert:"+r.domainKey+":product", "expert:"+r.alt+":product")
		} else {
			p = tu.User()
		}
		p.UserID = id
		return p
	}
	for _, d := range []string{r.domainKey, r.alt} {
		must(t, pool.QueryRow(ctx, `WITH d AS (INSERT INTO domains (key, name) VALUES ($1,$1) RETURNING id)
			INSERT INTO systems (domain_id, key, name) SELECT id, 'SYS', 'System' FROM d RETURNING domain_id`, d).Scan(new(uuid.UUID)))
	}
	r.anna, r.boris, r.carl = user("Анна", true), user("Борис", true), user("Карл", false)
	email := func(_ context.Context, id uuid.UUID) string { return r.emails[id] }
	r.topics = &issuetopics.Service{Pool: pool, Nabu: r.nabu.client(), Email: email}
	store := specdata.NewPG(pool)
	r.iss = issues.NewService(store, nil, nil, nopEvents{})
	r.iss.KeyChanged = r.topics.Rename
	r.disc = discovery.NewService(pool, nopEvents{})

	r.engine = workflows.New(pool, nopEvents{}, workflows.Config{MaxAttempts: 2, Lease: time.Minute})
	r.engine.Register(discovery.Machine{})
	r.research = func(int) ([]workflows.NewEvent, error) {
		return r.ready("high", nil, "- Метрика по заказам"), nil
	}
	r.engine.Handle(discovery.Effect, workflows.EffectHandler{Do: func(ctx context.Context, run workflows.RunRef, _ json.RawMessage) ([]workflows.NewEvent, error) {
		reqs, err := discovery.RunRequests(ctx, pool, run.ID)
		if err != nil {
			return nil, err
		}
		return r.research(len(reqs))
	}})
	r.engine.Handle(discovery.EffectDrain, workflows.EffectHandler{Do: (&discovery.DrainEffects{Q: pool}).Do})
	r.engine.Handle(discovery.EffectNotice, workflows.EffectHandler{Do: (&discovery.NoticeEffects{Q: pool, Nabu: r.nabu.client(), Email: email}).Do})
	return r
}

// ready is the result of the researcher with an estimate.
func (r *discRig) ready(confidence string, basis []string, summary string) []workflows.NewEvent {
	if basis == nil {
		basis = []string{}
	}
	return []workflows.NewEvent{{Type: "discovery_ready", Payload: agent.DiscoveryInput{Content: "# Discovery", Value: "Больше заказов",
		Measure: &cycledata.Measure{Source: "ch", Query: "SELECT 1", Target: "+5%", Window: "28d"}, ChangeSummary: summary,
		Estimate: &cycledata.Estimate{Agent: []cycledata.EstimateAgent{{Stage: "codegen", USD: 26, Tokens: 1000}},
			People: []cycledata.EstimatePeople{{Stage: "spec", Expertise: "product", PersonHours: 3}}, Days: 9, WaitDays: 4,
			Confidence: confidence, Basis: cycledata.EstimateBasis{Features: basis}}}}}
}

func (r *discRig) run() { r.t.Helper(); drain(r.t, r.engine) }

func (r *discRig) issue(key string) *cycledata.Issue {
	r.t.Helper()
	is, _, err := cycledata.New(pool).IssueByKey(context.Background(), key)
	must(r.t, err)
	return is
}

func (r *discRig) grant(p *domain.Principal, conv, ch string) mcp.Grant {
	return mcp.Grant{UserID: p.UserID, Mode: mcp.ModeNabu, Conversation: conv, Channel: ch}
}

func toolMsg(err error) string {
	var te *mcp.ToolError
	if errors.As(err, &te) {
		return te.Msg
	}
	return fmt.Sprint(err)
}

// ─── FTR.HMR.DISC-0001: the topic of an issue ─────────────────────────

// TP-01, TP-02, TP-05…TP-09, TP-17, TP-18
func TestIssueTopics(t *testing.T) {
	ctx := context.Background()
	r := newDiscRig(t)
	is, err := r.iss.Create(ctx, r.anna, issues.CreateInput{Type: domain.IssueIdea, Domain: r.domainKey, Title: "Скидка за ранний возврат"})
	must(t, err)

	// TP-02: opening the issue creates nothing
	v, err := r.topics.Topic(ctx, r.anna, is.Key)
	must(t, err)
	if v.ConversationID != nil || v.Title != is.Key+" — Скидка за ранний возврат" || r.nabu.created != 0 {
		t.Fatalf("TP-02: %+v created %d", v, r.nabu.created)
	}
	// TP-05: two tabs send the first message at once — one topic
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = r.topics.Send(ctx, r.anna, is.Key, issuetopics.SendInput{Text: fmt.Sprintf("вопрос %d", i)})
		}()
	}
	wg.Wait()
	must(t, errors.Join(errs...))
	annas := r.nabu.topicsOf(r.emails[r.anna.UserID])
	if len(annas) != 1 || len(annas[0].messages) != 2 {
		t.Fatalf("TP-05: %d topics", len(annas))
	}
	ctxOf := annas[0].messages[0]["context"].(map[string]any)
	if ctxOf["type"] != "issue" || ctxOf["key"] != is.Key {
		t.Fatalf("the screen context %v", ctxOf)
	}
	// TP-01: another expert gets his own topic
	_, err = r.topics.Send(ctx, r.boris, is.Key, issuetopics.SendInput{Text: "мой вопрос"})
	must(t, err)
	if b := r.nabu.topicsOf(r.emails[r.boris.UserID]); len(b) != 1 || b[0].id == annas[0].id {
		t.Fatal("TP-01: Boris has no topic of his own")
	}
	// TP-06: Nabu fails the message after the topic exists; the repeat goes to the same topic
	r.nabu.failMsg = 1
	_, err = r.topics.Send(ctx, r.carl, is.Key, issuetopics.SendInput{Text: "раз"})
	tu.Code(t, err, 503, "nabu_unavailable")
	_, err = r.topics.Send(ctx, r.carl, is.Key, issuetopics.SendInput{Text: "раз"})
	must(t, err)
	if c := r.nabu.topicsOf(r.emails[r.carl.UserID]); len(c) != 1 || len(c[0].messages) != 1 {
		t.Fatalf("TP-06: %d topics", len(c))
	}
	// TP-18: Nabu is down while the issue opens — the link stays
	r.nabu.failGet = true
	_, err = r.topics.Topic(ctx, r.anna, is.Key)
	tu.Code(t, err, 503, "nabu_unavailable")
	r.nabu.failGet = false
	if conv, _ := issuetopics.ConversationOf(ctx, pool, r.anna.UserID, is.ID); conv != annas[0].id {
		t.Fatal("TP-18: the link is lost")
	}
	// TP-07: a topic archived in Nabu is a draft again; the next message makes a new one
	r.nabu.mu.Lock()
	r.nabu.topics[annas[0].id].archived = true
	r.nabu.mu.Unlock()
	if v, err = r.topics.Topic(ctx, r.anna, is.Key); err != nil || v.ConversationID != nil {
		t.Fatalf("TP-07: %+v %v", v, err)
	}
	sent, err := r.topics.Send(ctx, r.anna, is.Key, issuetopics.SendInput{Text: "снова"})
	must(t, err)
	if sent.ConversationID == annas[0].id {
		t.Fatal("TP-07: the archived topic was used")
	}
	// TP-08: the move gives every topic the new key; TP-09: a topic gone in Nabu is forgotten
	r.nabu.mu.Lock()
	delete(r.nabu.topics, r.nabu.topicsOf_unlocked(r.emails[r.boris.UserID]))
	r.nabu.mu.Unlock()
	newKey, err := r.iss.Move(ctx, r.anna, is.Key, r.alt)
	must(t, err)
	if tp := r.nabu.topic(sent.ConversationID); !strings.HasPrefix(tp.title, newKey+" — ") {
		t.Fatalf("TP-08: %q", tp.title)
	}
	if conv, _ := issuetopics.ConversationOf(ctx, pool, r.boris.UserID, is.ID); conv != "" {
		t.Fatal("TP-09: the link to a deleted topic stays")
	}
	// TP-17: without the agent the topic is unavailable
	domain.SetAgentDisabled(true)
	_, err = r.topics.Topic(ctx, r.anna, newKey)
	domain.SetAgentDisabled(false)
	tu.Code(t, err, 409, "agent_disabled")
}

// topicsOf_unlocked is the id of the first topic of the user; the caller holds the lock.
func (f *fakeNabu) topicsOf_unlocked(email string) string { //nolint:revive // a test helper
	for id, tp := range f.topics {
		if tp.email == email {
			return id
		}
	}
	return ""
}

// ─── Instructions to the researcher and notices ──────────────────────

// RQ-01, RQ-03…RQ-12, RQ-15…RQ-20, NT-01…NT-06
func TestDiscoveryRevisions(t *testing.T) {
	ctx := context.Background()
	r := newDiscRig(t)
	is, err := r.iss.Create(ctx, r.anna, issues.CreateInput{Type: domain.IssueIdea, Domain: r.domainKey, Title: "Подписка на авто"})
	must(t, err)
	r.run()
	if s := r.issue(is.Key).Status; s != domain.IssueVerification {
		t.Fatalf("the first Discovery: %s", s)
	}
	topicOf := func(p *domain.Principal) string {
		t.Helper()
		out, err := r.topics.Send(ctx, p, is.Key, issuetopics.SendInput{Text: "привет"})
		must(t, err)
		return out.ConversationID
	}
	annaConv, borisConv := topicOf(r.anna), topicOf(r.boris)
	carlConv := topicOf(r.carl)
	ch := discovery.Channel("hammurapi")
	ask := func(p *domain.Principal, g mcp.Grant, text string) (*discovery.Request, error) {
		return discovery.RequestRevision(ctx, pool, ch, p, g, is.Key, text)
	}
	// RQ-03, RQ-05: another channel or none
	for _, c := range []string{"telegram", ""} {
		if _, err := ask(r.anna, r.grant(r.anna, annaConv, c), "x"); !strings.Contains(toolMsg(err), "from the chat of the issue in Hammurapi") {
			t.Fatalf("RQ-03 %q: %v", c, err)
		}
	}
	// RQ-03a: the channel is the one of the service client of this installation (NABU_CLIENT_NAME)
	if _, err := discovery.RequestRevision(ctx, pool, discovery.Channel("hammurapi-stage"), r.anna, r.grant(r.anna, annaConv, ch), is.Key, "x"); !strings.Contains(toolMsg(err), "from the chat of the issue in Hammurapi") {
		t.Fatalf("RQ-03a: %v", err)
	}
	// RQ-04: another topic
	if _, err := ask(r.anna, r.grant(r.anna, borisConv, ch), "x"); !strings.Contains(toolMsg(err), "Open the chat of the issue") {
		t.Fatalf("RQ-04: %v", err)
	}
	// RQ-07: not an expert
	if _, err := ask(r.carl, r.grant(r.carl, carlConv, ch), "x"); !strings.Contains(toolMsg(err), "Only experts of domain") {
		t.Fatalf("RQ-07: %v", err)
	}
	// RQ-09: the length of the instruction
	for _, text := range []string{" ", strings.Repeat("я", discovery.MaxInstructions+1)} {
		if _, err := ask(r.anna, r.grant(r.anna, annaConv, ch), text); !strings.Contains(toolMsg(err), "instructions: 1–4000") {
			t.Fatalf("RQ-09: %v", err)
		}
	}

	// RQ-01, RQ-10, NT-01: the researcher answers the instruction, the author gets the notice
	req, err := ask(r.anna, r.grant(r.anna, annaConv, ch), "Пересчитать метрику по заказам")
	must(t, err)
	if req.State != discovery.RequestRunning {
		t.Fatalf("RQ-01: %s", req.State)
	}
	r.run()
	req, err = discovery.GetRequest(ctx, pool, req.ID)
	must(t, err)
	if req.State != discovery.RequestDone || req.Revision == nil || *req.Revision != 2 || r.issue(is.Key).Status != domain.IssueVerification {
		t.Fatalf("RQ-10: %+v", req)
	}
	notes := r.nabu.topic(annaConv).notices
	if len(notes) != 1 || !strings.HasPrefix(notes[0].Text, "**Анализ обновлён · ревизия 2**\n\n- Метрика по заказам") {
		t.Fatalf("NT-01: %+v", notes)
	}
	if p, _ := json.Marshal(notes[0].Payload); !strings.Contains(string(p), `"kind":"discovery.revision"`) {
		t.Fatalf("NT-01 payload: %s", p)
	}
	// RQ-19: the history names the instruction; a manual edit is a revision of a person
	revs, err := cycledata.New(pool).Revisions(ctx, is.ID)
	must(t, err)
	if len(revs[0].Requests) != 1 || !strings.HasPrefix(revs[0].Requests[0].Instructions, "Пересчитать") || !revs[0].IsAgent {
		t.Fatalf("history: %+v", revs[0])
	}

	// RQ-02a: read_issue tells the personal agent that it does not write Discovery itself
	var readIssue mcp.Tool
	for _, tl := range agent.Tools(agent.ToolDeps{Store: specdata.NewPG(pool)}) {
		if tl.Name == "read_issue" {
			readIssue = tl
		}
	}
	arg, _ := json.Marshal(map[string]string{"key": is.Key})
	out, err := readIssue.Handler(ctx, mcp.Grant{UserID: r.anna.UserID, Mode: mcp.ModeNabu}, arg)
	must(t, err)
	if !strings.Contains(out, agent.DiscoveryRule) || !strings.Contains(out, "request_discovery_revision") {
		t.Fatalf("RQ-02a: %s", out)
	}
	if out, _ := readIssue.Handler(ctx, mcp.Grant{Mode: mcp.ModeDiscovery, Subject: is.ID}, arg); strings.Contains(out, agent.DiscoveryRule) {
		t.Fatal("RQ-02a: the researcher gets the rule of the personal agent")
	}

	// RQ-11, NT-02, NT-03: instructions during a run wait and go together
	first, err := ask(r.anna, r.grant(r.anna, annaConv, ch), "Добавить юристов")
	must(t, err)
	_, err = r.engine.ProcessOnce(ctx) // the run takes the first instruction
	must(t, err)
	b1, err := ask(r.boris, r.grant(r.boris, borisConv, ch), "Проверить окно оценки")
	must(t, err)
	a2, err := ask(r.anna, r.grant(r.anna, annaConv, ch), "И ещё про праздники")
	must(t, err)
	if b1.State != discovery.RequestQueued || *b1.Position != 1 || *a2.Position != 2 {
		t.Fatalf("RQ-11: %+v %+v", b1, a2)
	}
	r.run()
	for _, id := range []uuid.UUID{first.ID, b1.ID, a2.ID} {
		q, _ := discovery.GetRequest(ctx, pool, id)
		if q.State != discovery.RequestDone {
			t.Fatalf("RQ-11: %+v", q)
		}
	}
	q1, _ := discovery.GetRequest(ctx, pool, first.ID)
	qb, _ := discovery.GetRequest(ctx, pool, b1.ID)
	qa, _ := discovery.GetRequest(ctx, pool, a2.ID)
	if *q1.Revision == *qb.Revision || *qb.Revision != *qa.Revision {
		t.Fatalf("RQ-11: one run for the queue %d %d %d", *q1.Revision, *qb.Revision, *qa.Revision)
	}
	annaNotes, borisNotes := r.nabu.topic(annaConv).notices, r.nabu.topic(borisConv).notices
	if len(annaNotes) != 3 || len(borisNotes) != 1 || !strings.Contains(borisNotes[0].Text, "Вместе с поручениями: Анна") ||
		!strings.Contains(annaNotes[2].Text, "Вместе с поручениями: Борис") {
		t.Fatalf("NT-02, NT-03: %d %d %+v", len(annaNotes), len(borisNotes), borisNotes)
	}

	// RQ-15, NT-04: the researcher fails — the issue is back in verification with the old revision
	failedBefore := effectsFailed(discovery.Effect)
	r.research = func(int) ([]workflows.NewEvent, error) {
		return nil, workflows.Permanent(errors.New("timeout of the agent"))
	}
	failedReq, err := ask(r.anna, r.grant(r.anna, annaConv, ch), "Ещё раз")
	must(t, err)
	r.run()
	failedReq, _ = discovery.GetRequest(ctx, pool, failedReq.ID)
	doc, _ := cycledata.New(pool).Discovery(ctx, is.ID)
	if failedReq.State != discovery.RequestFailed || r.issue(is.Key).Status != domain.IssueVerification || !discovery.Blocked(ctx, pool, is.ID) {
		t.Fatalf("RQ-15: %+v", failedReq)
	}
	if n := r.nabu.topic(annaConv).notices; !strings.Contains(n[len(n)-1].Text, fmt.Sprintf("Документ остался на ревизии %d", doc.Revision)) {
		t.Fatalf("NT-04: %q", n[len(n)-1].Text)
	}
	if got := effectsFailed(discovery.Effect); got != failedBefore+1 {
		t.Fatalf("a final failure of an effect is counted once: %v → %v", failedBefore, got)
	}
	card, err := r.iss.Get(ctx, r.anna, is.Key)
	must(t, err)
	if !card.Permissions.Discover {
		t.Fatal("R13: «Retry» is offered in verification after a failure")
	}
	// RQ-17: an instruction while blocked waits
	waiting, err := ask(r.boris, r.grant(r.boris, borisConv, ch), "Пока ждём")
	must(t, err)
	r.run()
	if q, _ := discovery.GetRequest(ctx, pool, waiting.ID); q.State != discovery.RequestQueued {
		t.Fatalf("RQ-17: %+v", q)
	}
	// RQ-16: «Retry» runs the failed instruction again with the waiting one
	r.research = func(n int) ([]workflows.NewEvent, error) {
		if n != 2 {
			return nil, workflows.Permanent(fmt.Errorf("the retry took %d instructions", n))
		}
		return r.ready("medium", nil, "- Повтор"), nil
	}
	must(t, r.iss.Rediscover(ctx, r.anna, is.Key))
	r.run()
	for _, id := range []uuid.UUID{failedReq.ID, waiting.ID} {
		if q, _ := discovery.GetRequest(ctx, pool, id); q.State != discovery.RequestDone {
			t.Fatalf("RQ-16: %+v", q)
		}
	}
	// NT-05: a notice whose answer was lost is written once; NT-06: a topic gone in Nabu is forgotten
	r.research = func(int) ([]workflows.NewEvent, error) { return r.ready("medium", nil, "- Ещё"), nil }
	r.nabu.failNote = 1
	before := len(r.nabu.topic(annaConv).notices)
	_, err = ask(r.anna, r.grant(r.anna, annaConv, ch), "Проверка повтора")
	must(t, err)
	r.run()
	time.Sleep(1100 * time.Millisecond) // the backoff of the repeat
	_, _ = pool.Exec(ctx, `UPDATE outbox SET next_attempt_at = now() WHERE sent_at IS NULL AND failed_at IS NULL`)
	r.run()
	if n := len(r.nabu.topic(annaConv).notices); n != before+1 {
		t.Fatalf("NT-05: %d notices", n-before)
	}
	r.nabu.mu.Lock()
	delete(r.nabu.topics, borisConv)
	r.nabu.mu.Unlock()
	_, err = ask(r.boris, r.grant(r.boris, borisConv, ch), "Тема удалена")
	must(t, err)
	r.run()
	if conv, _ := issuetopics.ConversationOf(ctx, pool, r.boris.UserID, is.ID); conv != "" {
		t.Fatal("NT-06: the link to a deleted topic stays")
	}
	var unsent int
	must(t, pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE effect = 'nabu.notice' AND sent_at IS NULL`).Scan(&unsent))
	if unsent != 0 {
		t.Fatalf("NT-06: %d notices not sent", unsent)
	}

	// RQ-18: a rejected issue fails its waiting instructions without notices
	_, err = r.engine.ProcessOnce(ctx)
	must(t, err)
	closing, err := ask(r.anna, r.grant(r.anna, annaConv, ch), "До отклонения")
	must(t, err)
	notesBefore := len(r.nabu.topic(annaConv).notices)
	must(t, r.iss.Reject(ctx, r.anna, is.Key, "не нужно"))
	r.run()
	if q, _ := discovery.GetRequest(ctx, pool, closing.ID); q.State != discovery.RequestFailed || q.Error == nil || *q.Error != "issue closed" {
		t.Fatalf("RQ-18: %+v", q)
	}
	if len(r.nabu.topic(annaConv).notices) != notesBefore {
		t.Fatal("RQ-18: a notice for a closed issue")
	}
	// RQ-08: a closed issue takes no instructions
	if _, err := ask(r.anna, r.grant(r.anna, annaConv, ch), "x"); !strings.Contains(toolMsg(err), "is not revised any more") {
		t.Fatalf("RQ-08: %v", err)
	}
}

// ─── The draft estimate ──────────────────────────────────────────────

// ES-03…ES-08, ES-13, ES-14, ES-17
func TestDiscoveryEstimate(t *testing.T) {
	ctx := context.Background()
	r := newDiscRig(t)
	cd := cycledata.New(pool)
	newIssue := func(title string) *cycledata.Issue {
		is, err := r.iss.Create(ctx, r.anna, issues.CreateInput{Type: domain.IssueIdea, Domain: r.domainKey, Title: title, Description: title})
		must(t, err)
		return is
	}
	// ES-03: high on two similar features is medium; ES-04: no basis is low
	r.research = func(int) ([]workflows.NewEvent, error) { return r.ready("high", nil, ""), nil }
	low := newIssue("Тарифы выходного дня")
	r.run()
	doc, err := cd.Discovery(ctx, low.ID)
	must(t, err)
	if doc.Estimate == nil || doc.Estimate.Confidence != "low" || !doc.Estimate.Basis.Judgement {
		t.Fatalf("ES-04: %+v", doc.Estimate)
	}

	// released features: two of the domain about tariffs, one of another domain
	var sys, other uuid.UUID
	must(t, pool.QueryRow(ctx, `SELECT s.id FROM systems s JOIN domains d ON d.id = s.domain_id WHERE d.key = $1`, r.domainKey).Scan(&sys))
	must(t, pool.QueryRow(ctx, `SELECT s.id FROM systems s JOIN domains d ON d.id = s.domain_id WHERE d.key = $1`, r.alt).Scan(&other))
	released := func(sysID uuid.UUID, n int, title string) string {
		key := fmt.Sprintf("FTR.X%s.SYS-%04d", strings.ToUpper(uuid.NewString()[:3]), n)
		var id uuid.UUID
		must(t, pool.QueryRow(ctx, `INSERT INTO features (unique_id, system_id, number, title, branch_name, pr_number, pr_url, phase, created_by, created_at)
			VALUES ($1,$2,$3,$4,'b',1,'u','released',$5, now() - interval '10 days') RETURNING id`, key, sysID, n, title, r.anna.UserID).Scan(&id))
		_, err := pool.Exec(ctx, `INSERT INTO agent_usage (context, feature_id, tokens_in, tokens_out, cost_usd) VALUES
			('gate', $1, 100, 50, 2.5), ('codegen', $1, 1000, 500, 20), ('check', $1, 10, 10, 1), ('chat', $1, 999, 999, 99)`, id)
		must(t, err)
		return key
	}
	f1, f2 := released(sys, 901, "Тарифы выходного дня для аренды"), released(sys, 902, "Скидка на тарифы выходного дня")
	foreign := released(other, 903, "Тарифы выходного дня в другом домене")
	_ = released(sys, 904, "Совсем про другое")
	// ES-06, ES-08: the similar features of the domain with the cost by stage
	list, err := discovery.SimilarFeaturesCost(ctx, pool, low.ID, 5)
	must(t, err)
	keys := map[string]discovery.SimilarCost{}
	for _, f := range list {
		keys[f.Key] = f
	}
	if _, ok := keys[foreign]; ok || len(list) != 2 {
		t.Fatalf("ES-08: %+v", list)
	}
	if c := keys[f1]; len(c.Agent) != 3 || c.Agent[0].Stage != "spec" || c.Agent[1].USD != 20 || c.Agent[1].Tokens != 1500 {
		t.Fatalf("ES-06: %+v", c)
	}
	text, err := discovery.SimilarFeaturesCostText(ctx, pool, low.ID, 5)
	must(t, err)
	if !strings.Contains(text, discovery.SimilarNote) {
		t.Fatal("ES-06: the note on person-hours")
	}
	// ES-07: nothing similar
	nothing := newIssue("Квантовая телепортация")
	if l, err := discovery.SimilarFeaturesCost(ctx, pool, nothing.ID, 5); err != nil || len(l) != 0 {
		t.Fatalf("ES-07: %+v %v", l, err)
	}

	// ES-03 with a basis of two features
	r.research = func(int) ([]workflows.NewEvent, error) { return r.ready("high", []string{f1, f2}, ""), nil }
	med := newIssue("Скидка на выходные")
	r.run()
	doc, _ = cd.Discovery(ctx, med.ID)
	if doc.Estimate.Confidence != "medium" {
		t.Fatalf("ES-03: %s", doc.Estimate.Confidence)
	}
	// ES-02: an unknown feature of the basis
	e := r.ready("medium", []string{"FTR.NONE.SYS-0001"}, "")[0].Payload.(agent.DiscoveryInput).Estimate
	var ee *cycledata.EstimateError
	if err := discovery.CheckEstimate(ctx, pool, e); !errors.As(err, &ee) || ee.Field != "basis.features" {
		t.Fatalf("ES-02: %v", err)
	}
	// ES-05, ES-17: an expert keeps his high confidence; the edit is a revision of a person
	manual := r.ready("high", []string{}, "")[0].Payload.(agent.DiscoveryInput)
	manual.Estimate.Days = 30
	must(t, r.disc.Edit(ctx, r.anna, med.Key, manual))
	v, err := r.disc.Get(ctx, med.Key)
	must(t, err)
	if v.Estimate.Confidence != "high" || v.EstimateTotals.Days != 30 || v.EstimateTotals.AgentUSD != 26 {
		t.Fatalf("ES-05: %+v", v.EstimateTotals)
	}
	bad := r.ready("high", nil, "")[0].Payload.(agent.DiscoveryInput)
	bad.Estimate.Days = 0
	var ae *apperr.Error
	if err := r.disc.Edit(ctx, r.anna, med.Key, bad); !errors.As(err, &ae) || ae.Code != "invalid_estimate" || ae.Details["field"] != "days" {
		t.Fatalf("ES-02 API: %v", err)
	}

	// MS-01…MS-03 (R23): the way to measure is optional, whole, and its source is a configured one
	for _, c := range []struct {
		m     *cycledata.Measure
		field string
	}{
		{&cycledata.Measure{Source: "Продуктовая БД Nabu (Postgres)", Query: "q", Target: "t", Window: "w"}, "measure.source"},
		{&cycledata.Measure{Source: "ch", Query: "", Target: "t", Window: "w"}, "measure.query"},
	} {
		in := r.ready("medium", nil, "")[0].Payload.(agent.DiscoveryInput)
		in.Measure = c.m
		var ae *apperr.Error
		if err := r.disc.Edit(ctx, r.anna, med.Key, in); !errors.As(err, &ae) || ae.Code != "invalid_measure" || ae.Details["field"] != c.field {
			t.Fatalf("MS-02, MS-03 %s: %v", c.field, err)
		}
	}
	noMeasure := r.ready("medium", nil, "")[0].Payload.(agent.DiscoveryInput)
	noMeasure.Measure = nil
	noMeasure.Estimate.Days = 30 // the estimate of the expert stays for ES-13
	must(t, r.disc.Edit(ctx, r.anna, med.Key, noMeasure))
	if v, err := r.disc.Get(ctx, med.Key); err != nil || !v.Complete || len(v.Missing) != 0 || v.Measure != nil {
		t.Fatalf("MS-01: %+v %v", v, err)
	}

	// ES-13: the sort by days puts issues without an estimate last both ways; ES-14: an unknown sort
	_, err = pool.Exec(ctx, `UPDATE discovery_docs SET estimate = NULL WHERE issue_id = $1`, nothing.ID)
	must(t, err)
	order := func(sort string) []string {
		l, err := r.iss.List(ctx, cycledata.IssueFilter{Domain: r.domainKey, Status: "all", Sort: sort, Page: httpx.Page{Limit: 50}})
		must(t, err)
		var out []string
		for _, is := range l {
			out = append(out, is.Key)
		}
		return out
	}
	if asc, desc := order("days"), order("-days"); asc[0] != low.Key || asc[len(asc)-1] != nothing.Key || desc[0] != med.Key || desc[len(desc)-1] != nothing.Key {
		t.Fatalf("ES-13: %v %v", asc, desc)
	}
	l, err := r.iss.List(ctx, cycledata.IssueFilter{Domain: r.domainKey, Status: "all", Page: httpx.Page{Limit: 50}})
	must(t, err)
	for _, is := range l {
		if is.Key == med.Key && (is.Estimate == nil || is.Estimate.PersonHours != 3 || is.Estimate.Days != 30) {
			t.Fatalf("R17: %+v", is.Estimate)
		}
	}
	if _, err := r.iss.List(ctx, cycledata.IssueFilter{Sort: "foo"}); !errors.Is(err, cycledata.ErrInvalidSort) {
		t.Fatalf("ES-14: %v", err)
	}
}

// effectsFailed reads hammurapi_workflow_effects_failed_total of the effect.
func effectsFailed(effect string) float64 {
	mfs, _ := metrics.Registry.Gather()
	for _, mf := range mfs {
		if mf.GetName() != "hammurapi_workflow_effects_failed_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "effect" && l.GetValue() == effect {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// metricSource configures a metric source: a way to measure names one of them
// (FTR.HMR.DISC-0001 R23).
func metricSource(t *testing.T, name, typ string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO metric_sources (name, type, endpoint) VALUES ($1, $2, 'http://metrics.invalid')
		ON CONFLICT (name) DO NOTHING`, name, typ)
	must(t, err)
}
