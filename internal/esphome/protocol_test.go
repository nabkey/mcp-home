package esphome

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Adversarial fake dashboard: each test gets raw control of the server side
// of the socket so it can misbehave in ways the real dashboard might (drop
// mid-stream, stall, interleave, send junk) and check the client fails
// cleanly, promptly, and without leaking goroutines.

// rawDashboard serves /ws, handing each upgraded connection to handle.
func rawDashboard(t *testing.T, handle func(conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	var wg sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Add before the upgrade: until the connection is hijacked,
		// srv.Close waits for this handler, so Add cannot race Wait.
		wg.Add(1)
		defer wg.Done()
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		handle(conn)
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		wg.Wait()
	})
	return srv
}

func hello(conn *websocket.Conn) {
	_ = conn.WriteJSON(map[string]any{"server_version": "test", "esphome_version": "2026.9.1", "requires_auth": false})
}

// readCmd reads the client's next command; a read error leaves it zero,
// which the handlers tolerate (the client is gone anyway).
func readCmd(conn *websocket.Conn) command {
	var c command
	_ = conn.ReadJSON(&c)
	return c
}

// shrink temporarily lowers a protocol limit for one test.
func shrink[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// noLeaks fails the test if goroutines started during it are still running
// after cleanup. Registered first so it runs last (cleanups are LIFO).
func noLeaks(t *testing.T) {
	t.Helper()
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for {
			n := runtime.NumGoroutine()
			if n <= before {
				return
			}
			if time.Now().After(deadline) {
				buf := make([]byte, 1<<16)
				buf = buf[:runtime.Stack(buf, true)]
				t.Errorf("goroutine leak: %d before, %d after\n%s", before, n, buf)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

// within fails the test if fn takes longer than d.
func within(t *testing.T, d time.Duration, fn func()) {
	t.Helper()
	start := time.Now()
	fn()
	if el := time.Since(start); el > d {
		t.Fatalf("took %s, want under %s", el, d)
	}
}

func TestStreamSocketDropMidStream(t *testing.T) {
	noLeaks(t)
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		c := readCmd(conn)
		_ = conn.WriteJSON(map[string]any{"message_id": c.MessageID, "event": "output", "data": "INFO Reading configuration..."})
		// Drop without a terminal result (dashboard restart).
	})
	res, err := newClient(t, srv.URL).Validate(context.Background(), "pump.yaml")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.Final || res.Succeeded() {
		t.Fatal("a dropped stream must not read as finished, let alone successful")
	}
	if res.Incomplete == "" || !strings.Contains(res.Output(), "Reading configuration") {
		t.Errorf("want partial output and a reason; got %+v", res)
	}
	view := validateView("pump.yaml", res, nil)
	if view["valid"] != nil {
		t.Errorf("valid = %v, want null for an unfinished run", view["valid"])
	}
}

func TestStreamBareResultIsNotSuccess(t *testing.T) {
	// Upstream sends only the generic ResultMessage{result:null} when a
	// stream is cancelled server-side. The old client read that as exit 0.
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		c := readCmd(conn)
		_ = conn.WriteJSON(map[string]any{"message_id": c.MessageID, "event": "output", "data": "INFO starting"})
		_ = conn.WriteJSON(map[string]any{"message_id": c.MessageID, "result": nil})
		_, _, _ = conn.ReadMessage()
	})
	res, err := newClient(t, srv.URL).Validate(context.Background(), "pump.yaml")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.Succeeded() || res.Final || res.Incomplete == "" {
		t.Fatalf("bare result read as %+v", res)
	}
}

func TestInterleavedMessageIDsAndUnknownEvents(t *testing.T) {
	noLeaks(t)
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		c := readCmd(conn)
		id := c.MessageID
		// Unrelated pushes, another id's frames, an unknown event type for
		// our id, an out-of-order "result" for someone else, then ours.
		_ = conn.WriteJSON(map[string]any{"event": "device_updated", "data": map[string]any{"x": 1}})
		_ = conn.WriteJSON(map[string]any{"message_id": "999", "event": "output", "data": "NOT OURS"})
		_ = conn.WriteJSON(map[string]any{"message_id": id, "event": "snapshot", "data": map[string]any{"job": 1}})
		_ = conn.WriteJSON(map[string]any{"message_id": id, "event": "job_progress", "data": 42})
		_ = conn.WriteJSON(map[string]any{"message_id": id, "event": "output", "data": "line one"})
		_ = conn.WriteJSON(map[string]any{"message_id": "999", "event": "result", "data": map[string]any{"success": false, "code": 9}})
		_ = conn.WriteJSON(map[string]any{"message_id": id, "event": "output", "data": map[string]any{"structured": true}})
		_ = conn.WriteJSON(map[string]any{"message_id": id, "event": "output", "data": "line two"})
		_ = conn.WriteJSON(map[string]any{"message_id": id, "event": "result", "data": map[string]any{"success": true, "code": 0}})
		_, _, _ = conn.ReadMessage()
	})
	res, err := newClient(t, srv.URL).Validate(context.Background(), "pump.yaml")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !res.Succeeded() {
		t.Fatalf("want success, got %+v", res)
	}
	out := res.Output()
	if strings.Contains(out, "NOT OURS") {
		t.Error("another message_id's output leaked into ours")
	}
	if !strings.Contains(out, "line one\n") || !strings.Contains(out, "line two") || !strings.Contains(out, "structured") {
		t.Errorf("output = %q", out)
	}
}

func TestUnaryCommandIgnoresStrayFrames(t *testing.T) {
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		c := readCmd(conn)
		_ = conn.WriteJSON(map[string]any{"message_id": "7", "result": []string{"wrong"}})
		_ = conn.WriteJSON(map[string]any{"message_id": c.MessageID, "event": "output", "data": "noise"})
		_ = conn.WriteJSON(map[string]any{"message_id": c.MessageID, "result": []string{"wifi_ssid"}})
		_, _, _ = conn.ReadMessage()
	})
	keys, err := newClient(t, srv.URL).SecretKeys(context.Background())
	if err != nil || len(keys) != 1 || keys[0] != "wifi_ssid" {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
}

func TestEmptyIDErrorFailsFast(t *testing.T) {
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		readCmd(conn)
		_ = conn.WriteJSON(map[string]any{"message_id": "", "error_code": "invalid_message", "details": "Invalid command format"})
		_, _, _ = conn.ReadMessage()
	})
	within(t, 2*time.Second, func() {
		_, err := newClient(t, srv.URL).ListDevices(context.Background())
		if errorCode(err) != "invalid_message" {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestStalledCommandTimesOut(t *testing.T) {
	noLeaks(t)
	shrink(t, &commandTimeout, 300*time.Millisecond)
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		readCmd(conn)
		_, _, _ = conn.ReadMessage() // never answer
	})
	within(t, 3*time.Second, func() {
		_, err := newClient(t, srv.URL).GetJob(context.Background(), "abc")
		var te *transportError
		if !errors.As(err, &te) || te.kind != kindStalled {
			t.Fatalf("err = %v, want stalled", err)
		}
		if !strings.Contains(explainErr(err), "Retry") {
			t.Errorf("explanation lacks a next action: %s", explainErr(err))
		}
	})
}

func TestStalledCommandIgnoresKeepalivePushes(t *testing.T) {
	// A dashboard that keeps pushing unrelated frames but never answers must
	// still hit the command deadline, not reset it forever.
	shrink(t, &commandTimeout, 400*time.Millisecond)
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		readCmd(conn)
		for i := 0; i < 100; i++ {
			if conn.WriteJSON(map[string]any{"event": "ping"}) != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	within(t, 3*time.Second, func() {
		if _, err := newClient(t, srv.URL).ListDevices(context.Background()); err == nil {
			t.Fatal("want a timeout")
		}
	})
}

func TestNoServerInfo(t *testing.T) {
	shrink(t, &helloTimeout, 200*time.Millisecond)
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		_, _, _ = conn.ReadMessage() // upgrade, then silence
	})
	within(t, 3*time.Second, func() {
		_, err := newClient(t, srv.URL).ListDevices(context.Background())
		var te *transportError
		if !errors.As(err, &te) || te.kind != kindStalled {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestWrongFirstMessage(t *testing.T) {
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		_ = conn.WriteJSON(map[string]any{"hello": "world"})
		_, _, _ = conn.ReadMessage()
	})
	_, err := newClient(t, srv.URL).ListDevices(context.Background())
	var te *transportError
	if !errors.As(err, &te) || te.kind != kindProtocol {
		t.Fatalf("err = %v", err)
	}
}

func TestMalformedFrame(t *testing.T) {
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		readCmd(conn)
		_ = conn.WriteMessage(websocket.TextMessage, []byte("{not json"))
		_, _, _ = conn.ReadMessage()
	})
	_, err := newClient(t, srv.URL).ListDevices(context.Background())
	var te *transportError
	if !errors.As(err, &te) || te.kind != kindProtocol {
		t.Fatalf("err = %v", err)
	}
}

func TestOversizeFrameRejected(t *testing.T) {
	noLeaks(t)
	shrink(t, &maxMessageBytes, 64*1024)
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		c := readCmd(conn)
		_ = conn.WriteJSON(map[string]any{"message_id": c.MessageID, "result": strings.Repeat("x", 256*1024)})
		_, _, _ = conn.ReadMessage()
	})
	_, err := newClient(t, srv.URL).ReadConfig(context.Background(), "pump.yaml")
	var te *transportError
	if !errors.As(err, &te) || te.kind != kindProtocol {
		t.Fatalf("err = %v, want protocol error", err)
	}
}

func TestGiantLogStaysBounded(t *testing.T) {
	noLeaks(t)
	const n = 20000
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		c := readCmd(conn)
		huge := strings.Repeat("A", 200*1024)
		for i := 0; i < n; i++ {
			line := fmt.Sprintf("compiling file %d", i)
			if i%5000 == 0 {
				line = huge // a pathological single line
			}
			if i == 10 {
				line = "src/main.cpp:12:3: error: 'foo' was not declared in this scope\n"
			}
			if conn.WriteJSON(map[string]any{"message_id": c.MessageID, "event": "output", "data": line + "\n"}) != nil {
				return
			}
		}
		_ = conn.WriteJSON(map[string]any{"message_id": c.MessageID, "event": "output", "data": "*** [.pioenvs/x/src/main.cpp.o] Error 1\n"})
		_ = conn.WriteJSON(map[string]any{"message_id": c.MessageID, "event": "result", "data": map[string]any{"status": "failed", "exit_code": 1}})
		_, _, _ = conn.ReadMessage()
	})
	res, err := newClient(t, srv.URL).JobLog(context.Background(), "job")
	if err != nil {
		t.Fatalf("JobLog: %v", err)
	}
	if len(res.Lines) > maxLines {
		t.Errorf("retained %d lines, cap is %d", len(res.Lines), maxLines)
	}
	if res.Dropped == 0 {
		t.Error("expected earlier lines to be dropped")
	}
	for _, l := range res.Lines {
		if len(l) > maxLineBytes+64 {
			t.Fatalf("line of %d bytes retained", len(l))
		}
	}
	// The tail (where a build's failure summary is) must survive, and the
	// early compiler error must still be reported even though its line was
	// dropped from the tail.
	if last := res.Lines[len(res.Lines)-1]; !strings.Contains(last, "Error 1") {
		t.Errorf("last line = %q", last)
	}
	if len(res.Errors) == 0 || !strings.Contains(res.Errors[0], "was not declared") {
		t.Errorf("errors = %q", res.Errors)
	}
	if res.Status != "failed" || res.ExitCode == nil || *res.ExitCode != 1 {
		t.Errorf("terminal = %q %v", res.Status, res.ExitCode)
	}
}

func TestContextCancelStopsStreamPromptly(t *testing.T) {
	noLeaks(t)
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		c := readCmd(conn)
		for {
			if conn.WriteJSON(map[string]any{"message_id": c.MessageID, "event": "output", "data": "[D][sensor]: tick"}) != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	within(t, 2*time.Second, func() {
		res, err := newClient(t, srv.URL).Logs(ctx, "pump.yaml", LogOptions{})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if res == nil || len(res.Lines) == 0 {
			t.Error("partial output should be returned with the cancellation")
		}
	})
}

func TestDeadlineEndsLogCaptureCleanly(t *testing.T) {
	noLeaks(t)
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		c := readCmd(conn)
		_ = conn.WriteJSON(map[string]any{"message_id": c.MessageID, "event": "output", "data": "[I][app]: up"})
		_, _, _ = conn.ReadMessage() // silent device
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	within(t, 2*time.Second, func() {
		res, err := newClient(t, srv.URL).Logs(ctx, "pump.yaml", LogOptions{})
		if err != nil || !res.TimedOut {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
}

func TestCancelWhileWaitingForHello(t *testing.T) {
	noLeaks(t)
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		_, _, _ = conn.ReadMessage()
	})
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	within(t, 2*time.Second, func() {
		_, err := newClient(t, srv.URL).ListDevices(ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestStreamStallAbandoned(t *testing.T) {
	shrink(t, &validateIdle, 200*time.Millisecond)
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		c := readCmd(conn)
		_ = conn.WriteJSON(map[string]any{"message_id": c.MessageID, "event": "output", "data": "INFO Reading"})
		_, _, _ = conn.ReadMessage()
	})
	within(t, 3*time.Second, func() {
		res, err := newClient(t, srv.URL).Validate(context.Background(), "pump.yaml")
		if err != nil || res.Final || !strings.Contains(res.Incomplete, "sent nothing") {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
}

func TestDashboardStreamErrorKeepsPartialOutput(t *testing.T) {
	// Upstream answers a validate that went silent for 60 s with an
	// `unavailable` error after the output it did produce.
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		hello(conn)
		c := readCmd(conn)
		_ = conn.WriteJSON(map[string]any{"message_id": c.MessageID, "event": "output", "data": "INFO Reading configuration"})
		_ = conn.WriteJSON(map[string]any{"message_id": c.MessageID, "error_code": "unavailable", "details": "No output for 60s; the run was stopped"})
		_, _, _ = conn.ReadMessage()
	})
	res, err := newClient(t, srv.URL).Validate(context.Background(), "pump.yaml")
	if errorCode(err) != "unavailable" || res == nil || len(res.Lines) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	view := validateView("pump.yaml", res, err)
	if view["valid"] != nil || !strings.Contains(fmt.Sprint(view["incomplete"]), "Retry in a few seconds") {
		t.Errorf("view = %v", view)
	}
}

func TestDashboardDown(t *testing.T) {
	noLeaks(t)
	// Grab a free port and close it: nothing listens there.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	_, err = newClient(t, "http://"+addr).GetJob(context.Background(), "abc")
	var te *transportError
	if !errors.As(err, &te) || te.kind != kindUnreachable {
		t.Fatalf("err = %v", err)
	}
	if msg := explainErr(err); !strings.Contains(msg, "restart") || !strings.Contains(msg, "same job_id") {
		t.Errorf("explanation = %s", msg)
	}
}

func TestUpgradeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := newClient(t, srv.URL).ListDevices(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("err = %v", err)
	}
}

func TestAuthRejected(t *testing.T) {
	srv := rawDashboard(t, func(conn *websocket.Conn) {
		_ = conn.WriteJSON(map[string]any{"server_version": "t", "requires_auth": true})
		c := readCmd(conn)
		_ = conn.WriteJSON(map[string]any{"message_id": c.MessageID, "error_code": "not_authenticated", "details": "Invalid credentials"})
		_, _, _ = conn.ReadMessage()
	})
	c, _ := NewClient(srv.URL, "wrong")
	_, err := c.ListDevices(context.Background())
	var te *transportError
	if !errors.As(err, &te) || te.kind != kindAuth || !strings.Contains(explainErr(err), "ESPHOME_PASSWORD") {
		t.Fatalf("err = %v", err)
	}
}

func TestGetJobNullIsNotFound(t *testing.T) {
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		sc.result(c.MessageID, nil)
	})
	_, err := newClient(t, srv.URL).GetJob(context.Background(), "gone")
	if !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("err = %v, want ErrJobNotFound", err)
	}
}

func TestLogsRedactAndSplitLines(t *testing.T) {
	// Lines exactly as the live dashboard sends them: terminators stripped,
	// literal "\033" escapes, concealed SSID/BSSID.
	srv := wsDashboard(t, false, func(sc *srvConn, c command) {
		sc.output(c.MessageID, `\033[32mINFO Successful handshake with doorbell @ 192.168.1.48 in 0.456s\033[0m`)
		sc.output(c.MessageID, `[22:26:34.879]\033[0;35m[C][wifi:1279]:   SSID: \033[8m'HomeNet'\033[28m\033[0m`)
		sc.output(c.MessageID, `[22:26:34.879]\033[0;35m[C][wifi:1279]:   BSSID: \033[8mE0:63:DA:31:E4:2E\033[28m\033[0m`)
		sc.output(c.MessageID, "\x1b[8mraw-escape-secret\x1b[28m and \\033[8mcut-off-secret")
		sc.streamResult(c.MessageID, true, 0)
	})
	res, err := newClient(t, srv.URL).Logs(context.Background(), "doorbell.yaml", LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := res.Output()
	for _, secret := range []string{"HomeNet", "E0:63:DA", "raw-escape-secret", "cut-off-secret", `\033`, "\x1b"} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaks %q:\n%s", secret, out)
		}
	}
	if len(res.Lines) != 4 || !strings.HasPrefix(res.Lines[0], "INFO Successful handshake") {
		t.Errorf("lines = %q", res.Lines)
	}
}
