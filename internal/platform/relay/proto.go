// Package relay is the workspace side of the relay of Nabu (FTR.NAB.CMN-0001
// tech §6, FTR.HMR.CMN-0006 tech §3.5): the runner opens a WebSocket to Nabu
// and serves the agent's tool calls with its workspace server, so it needs no
// inbound port. The frames are those of nabu-core/internal/relay; keep the two
// in step.
package relay

import (
	"context"
	"encoding/json"
	"time"

	"github.com/coder/websocket"
)

// Protocol is the version in hello.
const Protocol = 1

// MaxFrame bounds a frame; larger bodies travel in chunks (RLY-04).
const MaxFrame = 1 << 20

// chunkSize keeps a base64-encoded chunk with its envelope under MaxFrame.
const chunkSize = 512 << 10

// Frame types.
const (
	FrameHello  = "hello"
	FrameCall   = "call"
	FrameResult = "result"
	FrameAbort  = "abort"
	FramePing   = "ping"
	FramePong   = "pong"
)

// Frame is one message of the channel.
type Frame struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	// hello
	WorkspaceID string `json:"workspaceId,omitempty"`
	Kind        string `json:"kind,omitempty"` // sandbox | external
	Protocol    int    `json:"protocol,omitempty"`
	// call: Tool is the operation of the workspace server (fs/read, exec, …).
	Tool   string          `json:"tool,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	// result: Status is set in the first frame; Chunk carries body bytes
	// (base64 in JSON); Final ends the response.
	Seq    int    `json:"seq,omitempty"`
	Status int    `json:"status,omitempty"`
	Chunk  []byte `json:"chunk,omitempty"`
	Final  bool   `json:"final,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Ops are the operations a call may name.
var Ops = map[string]bool{
	"fs/read": true, "fs/access": true, "fs/stat": true, "fs/readdir": true, "fs/write": true,
	"fs/mkdir": true, "fs/glob": true, "grep": true, "exec": true, "abort": true,
}

// Streaming reports operations whose interruption is not retried: their
// effects cannot be repeated safely (tech §6: bash returns workspace_disconnected).
func Streaming(op string) bool { return op == "exec" }

func readFrame(ctx context.Context, ws *websocket.Conn, f *Frame) error {
	_, data, err := ws.Read(ctx)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, f)
}

func writeFrame(ctx context.Context, ws *websocket.Conn, f Frame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, b)
}
