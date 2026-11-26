package tui

import (
	"context"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/kennethfan/jiaoag/internal/protocol"
)

// Transport 是 TUI 与服务端之间的帧通道。它有两条实现：
//   - wsTransport：连 `jiaoag serve`（Web/远程，或本地已有服务端）；
//   - localTransport：TUI 自己持有运行时，直接在进程内调契约层。
//
// 上层（Model、sendCmd、waitForEnvelope）只认这个接口，因此两种模式共用同一套
// 渲染与状态机，区别只在帧从哪来。
type Transport interface {
	Send(protocol.Envelope) error
	Read() (protocol.Envelope, error)
	Close() error
}

// wsTransport 把 WebSocket 连接包成 Transport。
type wsTransport struct {
	conn *websocket.Conn
}

func newWSTransport(conn *websocket.Conn) *wsTransport { return &wsTransport{conn: conn} }

func (t *wsTransport) Send(env protocol.Envelope) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return wsjson.Write(ctx, t.conn, env)
}

func (t *wsTransport) Read() (protocol.Envelope, error) {
	var env protocol.Envelope
	err := wsjson.Read(context.Background(), t.conn, &env)
	return env, err
}

func (t *wsTransport) Close() error {
	return t.conn.Close(websocket.StatusNormalClosure, "bye")
}
