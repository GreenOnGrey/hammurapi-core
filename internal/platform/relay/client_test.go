package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// NB-12: the runner connects to the relay, says hello as an external
// workspace and serves a call with its workspace server; pings are answered.
func TestClientServesCalls(t *testing.T) {
	got := make(chan Frame, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ws-token" {
			w.WriteHeader(401)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		ctx := r.Context()
		var hello Frame
		if readFrame(ctx, ws, &hello) != nil {
			return
		}
		got <- hello
		_ = writeFrame(ctx, ws, Frame{Type: FramePing})
		_ = writeFrame(ctx, ws, Frame{Type: FrameCall, ID: "c1", Tool: "fs/read", Chunk: []byte(`{"path":"a.txt"}`), Final: true})
		for i := 0; i < 2; i++ {
			var f Frame
			if readFrame(ctx, ws, &f) != nil {
				return
			}
			got <- f
		}
		<-ctx.Done()
	}))
	defer srv.Close()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct{ Path string }
		_ = json.NewDecoder(r.Body).Decode(&p)
		if r.URL.Path != "/v1/fs/read" || r.Header.Get("Authorization") != "Bearer local" || p.Path != "a.txt" {
			w.WriteHeader(400)
			return
		}
		_, _ = w.Write([]byte(`{"content":"hello"}`))
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := &Client{URL: "ws" + strings.TrimPrefix(srv.URL, "http"), Token: "ws-token", WorkspaceID: "run-1", Kind: "external",
		Handler: handler, HandlerToken: "local"}
	go func() { _ = c.Run(ctx) }()
	hello := <-got
	if hello.Type != FrameHello || hello.Kind != "external" || hello.WorkspaceID != "run-1" || hello.Protocol != Protocol {
		t.Fatalf("hello %+v", hello)
	}
	var pong, result bool
	for i := 0; i < 2; i++ {
		select {
		case f := <-got:
			switch f.Type {
			case FramePong:
				pong = true
			case FrameResult:
				result = f.ID == "c1" && f.Status == 200 && f.Final && string(f.Chunk) == `{"content":"hello"}`
			}
		case <-ctx.Done():
			t.Fatal("timeout")
		}
	}
	if !pong || !result {
		t.Fatalf("pong %v result %v", pong, result)
	}
}
