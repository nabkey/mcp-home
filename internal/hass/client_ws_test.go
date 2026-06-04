package hass

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
)

// wsRecorder collects the command frames a test WebSocket server receives,
// guarded by a mutex so assertions in the test goroutine don't race with the
// server goroutine.
type wsRecorder struct {
	mu   sync.Mutex
	cmds []map[string]any
}

func (r *wsRecorder) record(cmd map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, cmd)
}

// last returns the most recently received command, or nil if none.
func (r *wsRecorder) last() map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.cmds) == 0 {
		return nil
	}
	return r.cmds[len(r.cmds)-1]
}

// all returns a copy of every command received so far.
func (r *wsRecorder) all() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.cmds...)
}

// newTestWSClient starts an httptest server that performs the Home Assistant
// WebSocket auth handshake, then calls respond for each command frame. The
// returned response map has "id" and "type":"result" filled in automatically
// and "success" defaulted to true. A response with "success":false and an
// "error" object simulates a command failure. The returned client is already
// dialed and closed via t.Cleanup; the recorder captures every command.
func newTestWSClient(t *testing.T, respond func(cmd map[string]any) map[string]any) (*WebsocketClient, *wsRecorder) {
	t.Helper()
	rec := &wsRecorder{}
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()

		_ = conn.WriteJSON(map[string]any{"type": "auth_required"})
		var auth map[string]any
		if err := conn.ReadJSON(&auth); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "auth_ok"})

		for {
			var cmd map[string]any
			if err := conn.ReadJSON(&cmd); err != nil {
				return
			}
			rec.record(cmd)
			resp := respond(cmd)
			if resp == nil {
				resp = map[string]any{}
			}
			resp["id"] = cmd["id"]
			resp["type"] = "result"
			if _, ok := resp["success"]; !ok {
				resp["success"] = true
			}
			_ = conn.WriteJSON(resp)
		}
	}))
	t.Cleanup(srv.Close)

	c := &WebsocketClient{baseURL: srv.URL, token: "test-token"}
	if err := c.Dial(); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, rec
}
