package esphome

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestListDevices(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/devices" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"configured":[{"name":"pool-pump","configuration":"pump.yaml","online":true}],"importable":[]}`))
	})

	devices, err := c.ListDevices(context.Background())
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devices) != 1 || devices[0]["name"] != "pool-pump" {
		t.Errorf("unexpected devices: %+v", devices)
	}
}

func TestSecretKeys(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/secret_keys" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`["wifi_ssid","wifi_password"]`))
	})

	keys, err := c.SecretKeys(context.Background())
	if err != nil {
		t.Fatalf("SecretKeys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "wifi_ssid" {
		t.Errorf("unexpected keys: %v", keys)
	}
}

func TestReadFile(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/edit" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("configuration"); got != "pump.yaml" {
			t.Errorf("configuration = %q", got)
		}
		_, _ = w.Write([]byte("esphome:\n  name: pool-pump\n"))
	})

	content, err := c.ReadFile(context.Background(), "pump.yaml")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(content, "pool-pump") {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestWriteFile(t *testing.T) {
	var gotBody, gotConfig string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/edit" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		gotConfig = r.URL.Query().Get("configuration")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	})

	if err := c.WriteFile(context.Background(), "pentair.h", "// header\n"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if gotConfig != "pentair.h" {
		t.Errorf("configuration = %q", gotConfig)
	}
	if gotBody != "// header\n" {
		t.Errorf("body = %q", gotBody)
	}
}

func TestWriteFileError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	if err := c.WriteFile(context.Background(), "pump.yaml", "x"); err == nil {
		t.Fatal("expected error on 500 response")
	}
}

func TestDownloadBinary(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/download.bin" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("file"); got != "firmware-factory.bin" {
			t.Errorf("file = %q", got)
		}
		_, _ = w.Write([]byte{0x00, 0x01, 0x02, 0x03})
	})

	data, err := c.DownloadBinary(context.Background(), "pump.yaml", "firmware-factory.bin")
	if err != nil {
		t.Fatalf("DownloadBinary: %v", err)
	}
	if len(data) != 4 {
		t.Errorf("len = %d, want 4", len(data))
	}
}

func TestLoginFlow(t *testing.T) {
	var loginHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			loginHits++
			if err := r.ParseForm(); err != nil {
				t.Errorf("ParseForm: %v", err)
			}
			if r.Form.Get("password") != "sekret" {
				t.Errorf("password = %q", r.Form.Get("password"))
			}
			http.SetCookie(w, &http.Cookie{Name: "authenticated", Value: "yes"})
			w.WriteHeader(http.StatusOK)
		case "/devices":
			if _, err := r.Cookie("authenticated"); err != nil {
				t.Errorf("devices request missing auth cookie")
			}
			_, _ = w.Write([]byte(`{"configured":[]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL, "sekret")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// Two calls: login should happen exactly once (sync.Once).
	if _, err := c.ListDevices(context.Background()); err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if _, err := c.ListDevices(context.Background()); err != nil {
		t.Fatalf("ListDevices (2): %v", err)
	}
	if loginHits != 1 {
		t.Errorf("login hits = %d, want 1", loginHits)
	}
}

// streamServer upgrades to a WebSocket and replays the dashboard command
// protocol: a series of line events followed by an exit event.
func streamServer(t *testing.T, lines []string, code int) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		// Read the spawn message the client sends.
		var spawn map[string]any
		if err := conn.ReadJSON(&spawn); err != nil {
			t.Errorf("read spawn: %v", err)
			return
		}
		if spawn["type"] != "spawn" {
			t.Errorf("spawn type = %v", spawn["type"])
		}
		for _, ln := range lines {
			_ = conn.WriteJSON(map[string]any{"event": "line", "data": ln})
		}
		_ = conn.WriteJSON(map[string]any{"event": "exit", "code": code})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRunCommandSuccess(t *testing.T) {
	srv := streamServer(t, []string{"Compiling...\n", "Done\n"}, 0)
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	res, err := c.Compile(context.Background(), "pump.yaml")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Output, "Compiling...") || !strings.Contains(res.Output, "Done") {
		t.Errorf("unexpected output: %q", res.Output)
	}
	if res.TimedOut {
		t.Error("unexpected TimedOut")
	}
}

func TestRunCommandFailure(t *testing.T) {
	srv := streamServer(t, []string{"ERROR: bad config\n"}, 1)
	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	res, err := c.Validate(context.Background(), "pump.yaml")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 1 {
		t.Errorf("exit code = %v, want 1", res.ExitCode)
	}
}

func TestLogsTimeout(t *testing.T) {
	// Server upgrades, reads spawn, then never sends an exit (like a live log
	// stream). The bounded context should stop the call.
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		var spawn map[string]any
		_ = conn.ReadJSON(&spawn)
		_ = conn.WriteJSON(map[string]any{"event": "line", "data": "[I] booted\n"})
		// Hold the connection open without exiting.
		time.Sleep(2 * time.Second)
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	res, err := c.Logs(ctx, "pump.yaml", "OTA")
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if !res.TimedOut {
		t.Error("expected TimedOut")
	}
	if !strings.Contains(res.Output, "booted") {
		t.Errorf("expected captured output, got %q", res.Output)
	}
}
