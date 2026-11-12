// Package runlog is the log of a run of an agent for the page of a feature
// (FTR.HMR.DEV-0004 R8–R11, arch §4.3): the events of the run of Nabu turned
// into entries — the messages of the agent, its tools, the check, the end —
// and the steps of Hammurapi after the agent.
package runlog

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
)

// Kinds of an entry.
const (
	KindMessage = "message"
	KindTool    = "tool"
	KindCheck   = "check"
	KindStalled = "stalled"
	KindError   = "error"
	KindDone    = "done"
	KindStep    = "step"
	KindExpired = "expired"
)

// Entry is one entry of the log.
type Entry struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// At is when Hammurapi got the event of a live run; Nabu keeps no time of
	// its events, so the entries of a replayed run have none.
	At      string          `json:"at,omitempty"`
	Text    string          `json:"text,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Args    json.RawMessage `json:"args,omitempty"`
	Command string          `json:"command,omitempty"`
	IsError bool            `json:"isError,omitempty"`
	// Result is nil while the tool runs.
	Result *string `json:"result,omitempty"`
	// Data is the payload of Nabu as is: the check, the stall, the error, the end.
	Data json.RawMessage `json:"data,omitempty"`
	// The steps of Hammurapi after the agent.
	Step   string `json:"step,omitempty"`
	Status string `json:"status,omitempty"`
	Detail string `json:"detail,omitempty"`
	Retry  bool   `json:"retry,omitempty"`
}

// Out is an event of the stream to the browser: "entry" (new or replaced by
// its id) or "delta" (more text of an open message).
type Out struct {
	Event string
	ID    string // the id of the SSE event: the number of the event of Nabu
	Entry Entry
}

// taskTokenRe is a task token of Hammurapi (codegen.TaskToken): never in the log.
var taskTokenRe = regexp.MustCompile(`hmt_[0-9a-f]{16,}`)

func mask(s string) string { return taskTokenRe.ReplaceAllString(s, "***") }

// assembler turns the events of a run into entries: deltas of text glue into
// one message until another event or a flush; a call and the result of a tool
// share an entry.
type assembler struct {
	live    bool
	now     func() time.Time
	msg     *Entry // the open message
	msgSeq  int
	sent    int // the length of the text of msg already sent
	lastSeq int
}

func (a *assembler) at() string {
	if !a.live {
		return ""
	}
	return a.now().UTC().Format(time.RFC3339)
}

// flush sends the text of the open message gathered since the last flush.
func (a *assembler) flush() []Out {
	if a.msg == nil || len(a.msg.Text) == a.sent {
		return nil
	}
	id := strconv.Itoa(a.msgSeq)
	var o Out
	if a.sent == 0 {
		o = Out{Event: "entry", ID: id, Entry: *a.msg}
	} else {
		o = Out{Event: "delta", ID: id, Entry: Entry{ID: a.msg.ID, Text: a.msg.Text[a.sent:]}}
	}
	a.sent = len(a.msg.Text)
	return []Out{o}
}

// close ends the open message.
func (a *assembler) close() []Out {
	out := a.flush()
	a.msg, a.sent = nil, 0
	return out
}

// add takes one event of Nabu.
func (a *assembler) add(ev nabu.RunEvent) []Out {
	if ev.Seq > a.lastSeq {
		a.lastSeq = ev.Seq
	}
	id := strconv.Itoa(ev.Seq)
	switch ev.Type {
	case "text_delta":
		var d struct {
			Delta string `json:"delta"`
		}
		_ = json.Unmarshal(ev.Data, &d)
		if a.msg == nil {
			a.msg, a.msgSeq, a.sent = &Entry{ID: "m" + id, Kind: KindMessage, At: a.at()}, ev.Seq, 0
		}
		a.msg.Text += mask(d.Delta)
		a.msgSeq = ev.Seq
		return nil
	case "tool_call":
		var c struct {
			ID   string          `json:"id"`
			Name string          `json:"name"`
			Args json.RawMessage `json:"args"`
		}
		_ = json.Unmarshal(ev.Data, &c)
		e := Entry{ID: "t" + c.ID, Kind: KindTool, At: a.at(), Tool: c.Name, Args: json.RawMessage(mask(string(c.Args)))}
		if !json.Valid(e.Args) {
			e.Args = nil
		}
		var bash struct {
			Command string `json:"command"`
		}
		if c.Name == "bash" && json.Unmarshal(c.Args, &bash) == nil {
			e.Command = mask(bash.Command)
		}
		return append(a.close(), Out{Event: "entry", ID: id, Entry: e})
	case "tool_result":
		var r struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			IsError bool   `json:"isError"`
			Summary string `json:"summary"`
		}
		_ = json.Unmarshal(ev.Data, &r)
		res := mask(r.Summary)
		return append(a.close(), Out{Event: "entry", ID: id, Entry: Entry{ID: "t" + r.ID, Kind: KindTool, Tool: r.Name, IsError: r.IsError, Result: &res}})
	case "check_started", "check_finished":
		return append(a.close(), Out{Event: "entry", ID: id, Entry: Entry{ID: "c" + id, Kind: KindCheck, At: a.at(), Status: strings.TrimPrefix(ev.Type, "check_"),
			Data: json.RawMessage(mask(string(ev.Data)))}})
	case "stalled":
		return append(a.close(), Out{Event: "entry", ID: id, Entry: Entry{ID: "e" + id, Kind: KindStalled, At: a.at(), Data: ev.Data}})
	case "error":
		return append(a.close(), Out{Event: "entry", ID: id, Entry: Entry{ID: "e" + id, Kind: KindError, At: a.at(), Data: json.RawMessage(mask(string(ev.Data)))}})
	case "completed", "cancelled":
		return append(a.close(), Out{Event: "entry", ID: id, Entry: Entry{ID: "done", Kind: KindDone, At: a.at(), Data: ev.Data}})
	}
	return nil // started, usage: not entries
}
