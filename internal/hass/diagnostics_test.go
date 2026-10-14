package hass

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGetErrorLog(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.Method != "GET" || r.URL.Path != "/api/error_log" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte("2026-06-04 ERROR something broke\n"))
	})

	log, err := c.GetErrorLog(context.Background())
	if err != nil {
		t.Fatalf("GetErrorLog: %v", err)
	}
	if !strings.Contains(log, "something broke") {
		t.Errorf("log = %q, want it to contain the error line", log)
	}
}

func TestDiagnosticsWSCommands(t *testing.T) {
	c, rec := newTestWSClient(t, func(cmd map[string]any) map[string]any {
		switch cmd["type"] {
		case "system_health/info":
			return map[string]any{"result": map[string]any{"homeassistant": map[string]any{"version": "2026.6.0"}}}
		case "repairs/list_issues":
			return map[string]any{"result": map[string]any{"issues": []any{}}}
		case "persistent_notification/get":
			return map[string]any{"result": []any{}}
		}
		return map[string]any{"success": false, "error": map[string]any{"message": "unexpected"}}
	})

	if _, err := c.SystemHealthInfo(); err != nil {
		t.Fatalf("SystemHealthInfo: %v", err)
	}
	if _, err := c.RepairsListIssues(); err != nil {
		t.Fatalf("RepairsListIssues: %v", err)
	}
	if _, err := c.PersistentNotifications(); err != nil {
		t.Fatalf("PersistentNotifications: %v", err)
	}

	types := []string{}
	for _, cmd := range rec.all() {
		types = append(types, cmd["type"].(string))
	}
	want := []string{"system_health/info", "repairs/list_issues", "persistent_notification/get"}
	for i := range want {
		if i >= len(types) || types[i] != want[i] {
			t.Fatalf("commands = %v, want %v", types, want)
		}
	}
}

// On Home Assistant OS the log file is off by default and /api/error_log
// 404s; the tool must fall back to the in-memory system log rather than
// report a dead end.
func TestDiagnosticsFallsBackToSystemLog(t *testing.T) {
	f := &replayFixture{now: time.Now(), config: map[string]any{"time_zone": "UTC"}}
	srv := f.server(t)
	// Wrap the replay server to answer system_log/list with one entry and
	// repairs with an ignored and an open issue.
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.Handle("/", srv.Config.Handler)
	mux.HandleFunc("/api/websocket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.WriteJSON(map[string]any{"type": "auth_required"})
		var auth map[string]any
		_ = conn.ReadJSON(&auth)
		_ = conn.WriteJSON(map[string]any{"type": "auth_ok"})
		for {
			var cmd map[string]any
			if conn.ReadJSON(&cmd) != nil {
				return
			}
			resp := map[string]any{"id": cmd["id"], "type": "result", "success": true}
			switch cmd["type"] {
			case "system_log/list":
				resp["result"] = []any{map[string]any{
					"name": "homeassistant.components.zha", "level": "ERROR", "count": 12.0,
					"message":   []any{"Failed to read attribute"},
					"source":    []any{"components/zha/core.py", 88.0},
					"timestamp": 1791950000.0,
					"exception": "Traceback (most recent call last):\n  File x\nTimeoutError: no response",
				}}
			case "repairs/list_issues":
				resp["result"] = map[string]any{"issues": []any{
					map[string]any{"domain": "esphome", "issue_id": "a", "ignored": true, "severity": "warning"},
					map[string]any{"domain": "bambu_lab", "issue_id": "b", "ignored": false, "severity": "error", "dismissed_version": nil, "translation_key": "access_code_rejected"},
				}}
			case "system_health/info":
				resp["result"] = nil
			default:
				resp["result"] = []any{}
			}
			_ = conn.WriteJSON(resp)
		}
	})
	wrapped := httptest.NewServer(mux)
	t.Cleanup(wrapped.Close)

	tools, err := NewTools(wrapped.URL, "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	tools.Register(server)
	st, ct := mcp.NewInMemoryTransports()
	ss, _ := server.Connect(context.Background(), st, nil)
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_diagnostics", Arguments: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	for _, want := range []string{`"level":"ERROR"`, `"count":12`, `"exception":"TimeoutError: no response"`, `"source":"components/zha/core.py:88"`, `"issue_id":"b"`} {
		if !strings.Contains(text, want) {
			t.Errorf("result lacks %s:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"error_log_error", `"issue_id":"a"`, "dismissed_version", "system_health", "Traceback"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("result contains %s:\n%s", unwanted, text)
		}
	}
}

func TestTailString(t *testing.T) {
	if got, truncated := tailString("short", 16000); got != "short" || truncated {
		t.Errorf("tailString(short) = %q, %v; want short, false", got, truncated)
	}
	long := strings.Repeat("x", 20) + "TAIL"
	got, truncated := tailString(long, 4)
	if got != "TAIL" || !truncated {
		t.Errorf("tailString(long, 4) = %q, %v; want TAIL, true", got, truncated)
	}

	// A cut landing mid-rune must advance to the next rune boundary so the
	// result is valid UTF-8 (é is two bytes here).
	multibyte := strings.Repeat("x", 10) + "é!"
	got, truncated = tailString(multibyte, 2)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if !utf8.ValidString(got) {
		t.Errorf("tailString result %q is not valid UTF-8", got)
	}
}
