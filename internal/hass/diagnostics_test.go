package hass

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestGetErrorLog(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
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
