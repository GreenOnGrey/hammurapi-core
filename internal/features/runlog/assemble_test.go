package runlog

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/nabu"
)

func ev(seq int, typ, data string) nabu.RunEvent {
	return nabu.RunEvent{Seq: seq, Type: typ, Data: json.RawMessage(data)}
}

// FTR.HMR.DEV-0004 qa LOG-01, LOG-02, MSK-03: deltas glue into one message, a call and its result share an entry,
// a task token never reaches the log.
func TestAssembler(t *testing.T) {
	a := &assembler{now: func() time.Time { return time.Unix(0, 0) }}
	var out []Out
	for i := 1; i <= 50; i++ {
		out = append(out, a.add(ev(i, "text_delta", `{"delta":"x"}`))...)
	}
	if len(out) != 0 {
		t.Fatalf("deltas went out before the message ended: %+v", out)
	}
	out = a.add(ev(51, "tool_call", `{"id":"c1","name":"bash","args":{"command":"curl -H 'X: hmt_0123456789abcdef0123' https://x"}}`))
	if len(out) != 2 || out[0].Entry.Kind != KindMessage || out[0].Entry.Text != strings.Repeat("x", 50) || out[0].Entry.ID != "m1" {
		t.Fatalf("LOG-01 message %+v", out)
	}
	call := out[1].Entry
	if call.ID != "tc1" || call.Result != nil || !strings.Contains(call.Command, "curl") || strings.Contains(call.Command, "hmt_") ||
		strings.Contains(string(call.Args), "hmt_") || !json.Valid(call.Args) {
		t.Fatalf("LOG-02, MSK-03 call %+v", call)
	}
	out = a.add(ev(52, "tool_result", `{"id":"c1","name":"bash","isError":true,"summary":"exit 127 hmt_0123456789abcdef0123"}`))
	if len(out) != 1 || out[0].Entry.ID != "tc1" || !out[0].Entry.IsError || *out[0].Entry.Result != "exit 127 ***" {
		t.Fatalf("LOG-02 result %+v", out)
	}
	if out := a.add(ev(53, "usage", `{"tokensIn":1}`)); out != nil {
		t.Fatalf("usage is not an entry: %+v", out)
	}
	out = a.add(ev(54, "completed", `{"status":"failed"}`))
	if len(out) != 1 || out[0].Entry.Kind != KindDone || out[0].ID != "54" || a.lastSeq != 54 {
		t.Fatalf("done %+v", out)
	}
}

// LOG-01: a live message goes out in pieces at every flush: the entry, then deltas.
func TestAssemblerFlush(t *testing.T) {
	a := &assembler{live: true, now: func() time.Time { return time.Unix(0, 0) }}
	a.add(ev(1, "text_delta", `{"delta":"Читаю "}`))
	out := a.flush()
	if len(out) != 1 || out[0].Event != "entry" || out[0].Entry.Text != "Читаю " || out[0].Entry.At == "" {
		t.Fatalf("first flush %+v", out)
	}
	if out := a.flush(); out != nil {
		t.Fatalf("nothing new %+v", out)
	}
	a.add(ev(2, "text_delta", `{"delta":"код"}`))
	out = a.flush()
	if len(out) != 1 || out[0].Event != "delta" || out[0].Entry.ID != "m1" || out[0].Entry.Text != "код" || out[0].ID != "2" {
		t.Fatalf("delta %+v", out)
	}
}
