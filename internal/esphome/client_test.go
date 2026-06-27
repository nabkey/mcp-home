package esphome

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// --- /ws dashboard fake ---

type srvConn struct {
	t    *testing.T
	conn *websocket.Conn
}

func (sc *srvConn) sendInfo(requiresAuth bool) {
	_ = sc.conn.WriteJSON(map[string]any{
		"server_version":  "test",
		"esphome_version": "2026.6.2",
		"port":            6052,
		"ha_addon":        true,
		"requires_auth":   requiresAuth,
	})
}
func (sc *srvConn) result(id string, result any) {
	_ = sc.conn.WriteJSON(map[string]any{"message_id": id, "result": result})
}
func (sc *srvConn) errorMsg(id, code, details string) {
	_ = sc.conn.WriteJSON(map[string]any{"message_id": id, "error_code": code, "details": details})
}
func (sc *srvConn) output(id, line string) {
	_ = sc.conn.WriteJSON(map[string]any{"message_id": id, "event": "output", "data": line})
}
func (sc *srvConn) streamResult(id string, success bool, code int) {
	_ = sc.conn.WriteJSON(map[string]any{"message_id": id, "event": "result",
		"data": map[string]any{"success": success, "code": code}})
}

type command struct {
	Command   string         `json:"command"`
	MessageID string         `json:"message_id"`
	Args      map[string]any `json:"args"`
}

// wsDashboard starts an httptest server serving /ws (and /api/firmware/download)
// driven by dispatch, which is called once per received command.
func wsDashboard(t *testing.T, requiresAuth bool, dispatch func(sc *srvConn, c command)) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		sc := &srvConn{t: t, conn: conn}
		sc.sendInfo(requiresAuth)
		for {
			var c command
			if err := conn.ReadJSON(&c); err != nil {
				return
			}
			if c.Command == "auth/login" {
				sc.result(c.MessageID, map[string]any{"token": "tok", "expires_at": 1})
				continue
			}
			dispatch(sc, c)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T, url string) *Client {
	t.Helper()
	c, err := NewClient(url, "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestListDevices(t *testing.T) {
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		if c.Command != "devices/list" {
			t.Errorf("command = %s", c.Command)
		}
		sc.result(c.MessageID, map[string]any{
			"configured": []map[string]any{{"name": "pool-pump", "configuration": "pump.yaml"}},
			"importable": []any{},
		})
	})
	devices, err := newClient(t, srv.URL).ListDevices(context.Background())
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if len(devices) != 1 || devices[0]["name"] != "pool-pump" {
		t.Errorf("unexpected devices: %+v", devices)
	}
}

func TestSecretKeys(t *testing.T) {
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		if c.Command != "config/get_secrets" {
			t.Errorf("command = %s", c.Command)
		}
		sc.result(c.MessageID, []string{"wifi_ssid", "wifi_password"})
	})
	keys, err := newClient(t, srv.URL).SecretKeys(context.Background())
	if err != nil {
		t.Fatalf("SecretKeys: %v", err)
	}
	if len(keys) != 2 || keys[0] != "wifi_ssid" {
		t.Errorf("unexpected keys: %v", keys)
	}
}

func TestReadConfig(t *testing.T) {
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		if c.Command != "devices/get_config" || c.Args["configuration"] != "pump.yaml" {
			t.Errorf("unexpected: %s %v", c.Command, c.Args)
		}
		sc.result(c.MessageID, "esphome:\n  name: pool-pump\n")
	})
	content, err := newClient(t, srv.URL).ReadConfig(context.Background(), "pump.yaml")
	if err != nil {
		t.Fatalf("ReadConfig: %v", err)
	}
	if !strings.Contains(content, "pool-pump") {
		t.Errorf("unexpected content: %q", content)
	}
}

func TestWriteConfigUpdate(t *testing.T) {
	var gotContent string
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		if c.Command != "devices/update_config" {
			t.Errorf("expected update_config, got %s", c.Command)
		}
		gotContent, _ = c.Args["content"].(string)
		sc.result(c.MessageID, nil)
	})
	created, err := newClient(t, srv.URL).WriteConfig(context.Background(), "pump.yaml", "x: 1\n")
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if created {
		t.Error("expected created=false for existing device")
	}
	if gotContent != "x: 1\n" {
		t.Errorf("content = %q", gotContent)
	}
}

func TestWriteConfigCreateFallback(t *testing.T) {
	var sawCreate bool
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		switch c.Command {
		case "devices/update_config":
			sc.errorMsg(c.MessageID, "not_found", "no such device")
		case "devices/create":
			sawCreate = true
			if c.Args["name"] != "pump" {
				t.Errorf("create name = %v, want pump", c.Args["name"])
			}
			if c.Args["overwrite"] != true {
				t.Errorf("create overwrite = %v", c.Args["overwrite"])
			}
			sc.result(c.MessageID, map[string]any{"configuration": "pump.yaml"})
		default:
			t.Errorf("unexpected command %s", c.Command)
		}
	})
	created, err := newClient(t, srv.URL).WriteConfig(context.Background(), "pump.yaml", "x: 1\n")
	if err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if !created || !sawCreate {
		t.Errorf("expected create fallback (created=%v sawCreate=%v)", created, sawCreate)
	}
}

func TestValidateStreaming(t *testing.T) {
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		if c.Command != "devices/validate" {
			t.Errorf("command = %s", c.Command)
		}
		sc.output(c.MessageID, "Checking...\n")
		sc.output(c.MessageID, "Configuration is valid!\n")
		sc.streamResult(c.MessageID, true, 0)
	})
	res, err := newClient(t, srv.URL).Validate(context.Background(), "pump.yaml")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Output, "valid") {
		t.Errorf("output = %q", res.Output)
	}
}

func TestValidateError(t *testing.T) {
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		sc.errorMsg(c.MessageID, "invalid_args", "bad configuration name")
	})
	_, err := newClient(t, srv.URL).Validate(context.Background(), "pump.yaml")
	if err == nil || !strings.Contains(err.Error(), "invalid_args") {
		t.Fatalf("expected invalid_args error, got %v", err)
	}
}

func TestLogsTimeout(t *testing.T) {
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		if c.Command != "devices/logs" {
			t.Errorf("command = %s", c.Command)
		}
		sc.output(c.MessageID, "[I] booted\n")
		time.Sleep(2 * time.Second) // never sends terminal result
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := newClient(t, srv.URL).Logs(ctx, "pump.yaml", "OTA")
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

func TestDownloadBinary(t *testing.T) {
	mux := http.NewServeMux()
	up := websocket.Upgrader{}
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		sc := &srvConn{t: t, conn: conn}
		sc.sendInfo(false)
		for {
			var c command
			if err := conn.ReadJSON(&c); err != nil {
				return
			}
			switch c.Command {
			case "firmware/get_binaries":
				sc.result(c.MessageID, []map[string]any{
					{"title": "Factory", "file": "pool-pump.factory.bin", "type": "factory"},
					{"title": "OTA", "file": "pool-pump.ota.bin", "type": "ota"},
				})
			case "firmware/download_token":
				if c.Args["file"] != "pool-pump.factory.bin" {
					t.Errorf("token file = %v", c.Args["file"])
				}
				sc.result(c.MessageID, map[string]any{"token": "dltok"})
			default:
				t.Errorf("unexpected command %s", c.Command)
			}
		}
	})
	mux.HandleFunc("/api/firmware/download", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "dltok" {
			http.Error(w, "bad token", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte{0xE9, 0x01, 0x02, 0x03})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	data, file, err := newClient(t, srv.URL).DownloadBinary(context.Background(), "pump.yaml", true)
	if err != nil {
		t.Fatalf("DownloadBinary: %v", err)
	}
	if file != "pool-pump.factory.bin" {
		t.Errorf("file = %q", file)
	}
	if len(data) != 4 || data[0] != 0xE9 {
		t.Errorf("unexpected data: %v", data)
	}
}

func TestAuthRequired(t *testing.T) {
	var authed bool
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		sc := &srvConn{t: t, conn: conn}
		sc.sendInfo(true)
		for {
			var c command
			if err := conn.ReadJSON(&c); err != nil {
				return
			}
			switch c.Command {
			case "auth/login":
				if c.Args["password"] != "sekret" {
					t.Errorf("password = %v", c.Args["password"])
				}
				authed = true
				sc.result(c.MessageID, map[string]any{"token": "tok"})
			case "devices/list":
				if !authed {
					t.Error("devices/list before auth")
				}
				sc.result(c.MessageID, map[string]any{"configured": []any{}, "importable": []any{}})
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL, "sekret")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.ListDevices(context.Background()); err != nil {
		t.Fatalf("ListDevices: %v", err)
	}
	if !authed {
		t.Error("expected auth/login to run")
	}
}

func TestAuthRequiredNoPassword(t *testing.T) {
	srv := wsDashboard(t, true, func(sc *srvConn, c command) {})
	c := newClient(t, srv.URL) // empty password
	if _, err := c.ListDevices(context.Background()); err == nil || !strings.Contains(err.Error(), "requires authentication") {
		t.Fatalf("expected auth-required error, got %v", err)
	}
}

// --- legacy spawn (compile/upload) ---

func spawnServer(t *testing.T, path string, lines []string, code int) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		var spawn map[string]any
		if err := conn.ReadJSON(&spawn); err != nil {
			return
		}
		if spawn["type"] != "spawn" {
			t.Errorf("spawn type = %v", spawn["type"])
		}
		for _, ln := range lines {
			_ = conn.WriteJSON(map[string]any{"event": "line", "data": ln})
		}
		_ = conn.WriteJSON(map[string]any{"event": "exit", "code": code})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestCompileSpawn(t *testing.T) {
	srv := spawnServer(t, "/compile", []string{"Compiling...\n", "Done\n"}, 0)
	res, err := newClient(t, srv.URL).Compile(context.Background(), "pump.yaml")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 0 {
		t.Errorf("exit code = %v, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Output, "Done") {
		t.Errorf("output = %q", res.Output)
	}
}

func TestUploadSpawnFailure(t *testing.T) {
	srv := spawnServer(t, "/upload", []string{"ERROR: offline\n"}, 1)
	res, err := newClient(t, srv.URL).Upload(context.Background(), "pump.yaml", "OTA")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if res.ExitCode == nil || *res.ExitCode != 1 {
		t.Errorf("exit code = %v, want 1", res.ExitCode)
	}
}

// Ensure the spawn upload sends the port in its spawn payload.
func TestUploadSendsPort(t *testing.T) {
	up := websocket.Upgrader{}
	mux := http.NewServeMux()
	var gotPort any
	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		var spawn map[string]any
		_ = conn.ReadJSON(&spawn)
		gotPort = spawn["port"]
		_ = conn.WriteJSON(map[string]any{"event": "exit", "code": 0})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	if _, err := newClient(t, srv.URL).Upload(context.Background(), "pump.yaml", "/dev/ttyUSB0"); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if gotPort != "/dev/ttyUSB0" {
		t.Errorf("port = %v", gotPort)
	}
}
