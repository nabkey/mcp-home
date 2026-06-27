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

// --- async firmware jobs (compile/upload/get_job) ---

func TestCompileEnqueue(t *testing.T) {
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		if c.Command != "firmware/compile" || c.Args["configuration"] != "pump.yaml" {
			t.Errorf("unexpected: %s %v", c.Command, c.Args)
		}
		sc.result(c.MessageID, map[string]any{
			"job_id": "job-1", "configuration": "pump.yaml",
			"job_type": "compile", "status": "queued",
		})
	})
	job, err := newClient(t, srv.URL).Compile(context.Background(), "pump.yaml")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if job.JobID != "job-1" || job.Status != "queued" {
		t.Errorf("unexpected job: %+v", job)
	}
	if job.Terminal() {
		t.Error("queued job should not be terminal")
	}
}

func TestUploadEnqueueSendsPort(t *testing.T) {
	var gotPort any
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		if c.Command != "firmware/upload" {
			t.Errorf("command = %s", c.Command)
		}
		gotPort = c.Args["port"]
		sc.result(c.MessageID, map[string]any{
			"job_id": "job-2", "job_type": "upload", "status": "queued",
		})
	})
	job, err := newClient(t, srv.URL).Upload(context.Background(), "pump.yaml", "/dev/ttyUSB0")
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if gotPort != "/dev/ttyUSB0" {
		t.Errorf("port = %v", gotPort)
	}
	if job.JobID != "job-2" {
		t.Errorf("job_id = %q", job.JobID)
	}
}

func TestUploadDefaultsOTA(t *testing.T) {
	var gotPort any
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		gotPort = c.Args["port"]
		sc.result(c.MessageID, map[string]any{"job_id": "j", "status": "queued"})
	})
	if _, err := newClient(t, srv.URL).Upload(context.Background(), "pump.yaml", ""); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if gotPort != "OTA" {
		t.Errorf("default port = %v, want OTA", gotPort)
	}
}

func TestGetJobTerminal(t *testing.T) {
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		if c.Command != "firmware/get_job" || c.Args["job_id"] != "job-1" {
			t.Errorf("unexpected: %s %v", c.Command, c.Args)
		}
		sc.result(c.MessageID, map[string]any{
			"job_id": "job-1", "job_type": "compile", "status": "completed",
			"exit_code": 0, "output": []string{"Compiling...\n", "Done\n"},
		})
	})
	job, err := newClient(t, srv.URL).GetJob(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !job.Terminal() || !job.Succeeded() {
		t.Errorf("expected terminal+success, got %+v", job)
	}
	if len(job.Output) != 2 {
		t.Errorf("output lines = %d", len(job.Output))
	}
}

func TestGetJobFailed(t *testing.T) {
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		sc.result(c.MessageID, map[string]any{
			"job_id": "job-3", "job_type": "compile", "status": "failed",
			"exit_code": 1, "error": "build error",
		})
	})
	job, err := newClient(t, srv.URL).GetJob(context.Background(), "job-3")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !job.Terminal() {
		t.Error("failed job should be terminal")
	}
	if job.Succeeded() {
		t.Error("failed job should not be a success")
	}
	if job.Error != "build error" {
		t.Errorf("error = %q", job.Error)
	}
}
