package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v4/option"
)

// fakeAPI is a stateful stand-in for the Cloudflare endpoints Setup calls.
type fakeAPI struct {
	mu        sync.Mutex
	tunnels   map[string]string // id -> name (live tunnels)
	nextID    int
	dnsTarget string
	dnsID     string
	tokenGets int
	calls     int
	// fail queues HTTP statuses returned (in order) before serving normally.
	fail []int
}

func newFakeAPI(t *testing.T) (*fakeAPI, string) {
	t.Helper()
	f := &fakeAPI{tunnels: map[string]string{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeAPI) deleteAll() {
	f.mu.Lock()
	f.tunnels = map[string]string{}
	f.mu.Unlock()
}

func envelope(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success": true, "errors": []any{}, "messages": []any{}, "result": result,
		"result_info": map[string]any{"page": 1, "per_page": 50, "count": 1, "total_count": 1, "total_pages": 1},
	})
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.fail) > 0 {
		status := f.fail[0]
		f.fail = f.fail[1:]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": 1, "message": "injected"}}, "messages": []any{}, "result": nil})
		return
	}
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/cfd_tunnel"):
		name := r.URL.Query().Get("name")
		var out []map[string]any
		for id, n := range f.tunnels {
			if n == name {
				out = append(out, map[string]any{"id": id, "name": n})
			}
		}
		envelope(w, out)
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/cfd_tunnel"):
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.nextID++
		id := fmt.Sprintf("tun-%d", f.nextID)
		f.tunnels[id] = body.Name
		envelope(w, map[string]any{"id": id, "name": body.Name})
	case r.Method == http.MethodPut && strings.HasSuffix(p, "/configurations"):
		envelope(w, map[string]any{})
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/token"):
		f.tokenGets++
		id := strings.Split(strings.TrimSuffix(p, "/token"), "/cfd_tunnel/")[1]
		envelope(w, "token-for-"+id)
	case r.Method == http.MethodGet && strings.HasSuffix(p, "/dns_records"):
		if f.dnsID == "" {
			envelope(w, []any{})
			return
		}
		envelope(w, []map[string]any{{"id": f.dnsID, "name": "mcp.example.com", "type": "CNAME", "content": f.dnsTarget, "proxied": true}})
	case r.Method == http.MethodPost && strings.HasSuffix(p, "/dns_records"),
		r.Method == http.MethodPut && strings.Contains(p, "/dns_records/"):
		var body struct {
			Content string `json:"content"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.dnsID, f.dnsTarget = "rec-1", body.Content
		envelope(w, map[string]any{"id": f.dnsID})
	default:
		http.Error(w, "unexpected "+r.Method+" "+p, http.StatusNotImplemented)
	}
}

// fakeCloudflared writes a shell script standing in for cloudflared. It
// records each start (pid, token, grace period, argv) and a SIGTERM, and
// exits with status 1 while the "crashes" counter is positive.
func fakeCloudflared(t *testing.T) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	bin = filepath.Join(dir, "cloudflared")
	script := `#!/bin/sh
d="` + dir + `"
echo "start $$ token=$TUNNEL_TOKEN grace=$TUNNEL_GRACE_PERIOD args=$*" >> "$d/log"
n=$(cat "$d/crashes" 2>/dev/null || echo 0)
if [ "$n" -gt 0 ]; then echo $((n-1)) > "$d/crashes"; echo "connection failed" >&2; exit 1; fi
trap 'echo "term $$" >> "$d/log"; exit 0' TERM
echo "Registered tunnel connection"
while true; do sleep 0.05; done
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func readLog(dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, "log"))
	return string(b)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fastPacing(t *testing.T) {
	t.Helper()
	oMin, oMax, oStable, oAttempts, oBackoff := restartMin, restartMax, stableRun, setupAttempts, setupBackoff
	restartMin, restartMax, stableRun, setupAttempts, setupBackoff = 10*time.Millisecond, 50*time.Millisecond, time.Hour, 3, 10*time.Millisecond
	t.Cleanup(func() {
		restartMin, restartMax, stableRun, setupAttempts, setupBackoff = oMin, oMax, oStable, oAttempts, oBackoff
	})
}

func testConfig(base, bin string, logs *syncBuffer) Config {
	return Config{
		APIToken: "api", AccountID: "acct", ZoneID: "zone", Hostname: "mcp.example.com",
		TunnelName: "mcp-server", LocalAddr: "http://127.0.0.1:1234",
		Logger:     slog.New(slog.NewTextHandler(logs, nil)),
		APIOptions: []option.RequestOption{option.WithBaseURL(base + "/"), option.WithMaxRetries(0)},
		Binary:     bin,
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func pids(log, prefix string) []int {
	var out []int
	for _, line := range strings.Split(log, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == prefix {
			n, _ := strconv.Atoi(f[1])
			out = append(out, n)
		}
	}
	return out
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestSetupCreatesTunnelAndDNS(t *testing.T) {
	api, base := newFakeAPI(t)
	var logs syncBuffer
	tun, err := Setup(context.Background(), testConfig(base, "", &logs))
	if err != nil {
		t.Fatal(err)
	}
	if tun.ID != "tun-1" || tun.token != "token-for-tun-1" || api.dnsTarget != "tun-1.cfargotunnel.com" {
		t.Errorf("tunnel=%+v dns=%q", tun, api.dnsTarget)
	}
	// Second boot reuses the tunnel instead of creating another.
	again, err := Setup(context.Background(), testConfig(base, "", &logs))
	if err != nil || again.ID != "tun-1" {
		t.Errorf("reuse: %v %+v", err, again)
	}
}

// A Cloudflare API blip at boot is retried instead of crash-looping the
// whole server.
func TestSetupRetriesTransientFailures(t *testing.T) {
	fastPacing(t)
	api, base := newFakeAPI(t)
	api.fail = []int{http.StatusServiceUnavailable, http.StatusTooManyRequests}
	var logs syncBuffer
	if _, err := Setup(context.Background(), testConfig(base, "", &logs)); err != nil {
		t.Fatalf("Setup after transient failures: %v", err)
	}
	if !strings.Contains(logs.String(), "retrying") {
		t.Error("retries not logged")
	}
}

// A bad token is not retried, and the error says what to change.
func TestSetupPermanentFailureIsActionable(t *testing.T) {
	fastPacing(t)
	cases := map[int]string{
		http.StatusForbidden:    "Cloudflare Tunnel Edit",
		http.StatusUnauthorized: "CF_API_TOKEN",
		http.StatusNotFound:     "CF_ACCOUNT_ID",
	}
	for status, want := range cases {
		api, base := newFakeAPI(t)
		api.fail = []int{status, status, status}
		var logs syncBuffer
		_, err := Setup(context.Background(), testConfig(base, "", &logs))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("HTTP %d: err = %v, want it to mention %q", status, err, want)
		}
		if api.calls != 1 {
			t.Errorf("HTTP %d: %d API calls, want 1 (no retry of a permanent error)", status, api.calls)
		}
	}
}

// SIGTERM at shutdown must reach cloudflared as SIGTERM (so it drains
// in-flight requests), and the token must travel in the environment, not
// argv.
func TestRunStopsCloudflaredGracefully(t *testing.T) {
	bin, dir := fakeCloudflared(t)
	var logs syncBuffer
	tun := &Tunnel{ID: "tun-1", token: "sekrit-token", config: testConfig("http://unused", bin, &logs)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tun.Run(ctx) }()
	waitFor(t, "cloudflared start", func() bool { return strings.Contains(readLog(dir), "start") })
	waitFor(t, "cloudflared output logged", func() bool { return strings.Contains(logs.String(), "Registered tunnel connection") })
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run after cancel = %v, want nil", err)
	}
	log := readLog(dir)
	if !strings.Contains(log, "term ") {
		t.Errorf("cloudflared was not sent SIGTERM (killed instead?): %s", log)
	}
	if !strings.Contains(log, "token=sekrit-token") || !strings.Contains(log, "grace="+GracePeriod.String()) {
		t.Errorf("token/grace not passed via env: %s", log)
	}
	if strings.Contains(log, "args=tunnel --no-autoupdate run") == false || strings.Contains(strings.SplitN(log, "args=", 2)[1], "sekrit") {
		t.Errorf("unexpected argv (token must not be in it): %s", log)
	}
	for _, pid := range pids(log, "start") {
		if alive(pid) {
			t.Errorf("cloudflared pid %d still running after Run returned", pid)
		}
	}
}

// cloudflared crashing must not take the server down: it is restarted after
// a fresh Setup, and after shutdown no cloudflared process is left behind.
func TestSuperviseRestartsAfterCrash(t *testing.T) {
	fastPacing(t)
	api, base := newFakeAPI(t)
	bin, dir := fakeCloudflared(t)
	if err := os.WriteFile(filepath.Join(dir, "crashes"), []byte("2"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs syncBuffer
	cfg := testConfig(base, bin, &logs)
	tun, err := Setup(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tun.Supervise(ctx) }()

	waitFor(t, "third start", func() bool { return len(pids(readLog(dir), "start")) >= 3 })
	select {
	case err := <-done:
		t.Fatalf("Supervise returned while cloudflared was crashing: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Supervise after cancel = %v", err)
	}
	api.mu.Lock()
	gets := api.tokenGets
	api.mu.Unlock()
	if gets < 3 {
		t.Errorf("token fetched %d times, want a fresh Setup before each restart", gets)
	}
	if !strings.Contains(logs.String(), "cloudflared exited unexpectedly") {
		t.Errorf("crash not logged as an error: %s", logs.String())
	}
	for _, pid := range pids(readLog(dir), "start") {
		if alive(pid) {
			t.Errorf("leaked cloudflared pid %d", pid)
		}
	}
}

// The tunnel deleted out from under us (in the dashboard, or by another
// deployment): cloudflared exits, re-setup creates a new tunnel, points DNS
// at it, and the restart uses its token.
func TestSuperviseRecreatesDeletedTunnel(t *testing.T) {
	fastPacing(t)
	api, base := newFakeAPI(t)
	bin, dir := fakeCloudflared(t)
	var logs syncBuffer
	tun, err := Setup(context.Background(), testConfig(base, bin, &logs))
	if err != nil {
		t.Fatal(err)
	}
	api.deleteAll()
	if err := os.WriteFile(filepath.Join(dir, "crashes"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- tun.Supervise(ctx) }()

	waitFor(t, "start with the new tunnel's token", func() bool { return strings.Contains(readLog(dir), "token=token-for-tun-2") })
	api.mu.Lock()
	target := api.dnsTarget
	api.mu.Unlock()
	if target != "tun-2.cfargotunnel.com" {
		t.Errorf("DNS target = %q, want the recreated tunnel", target)
	}
	cancel()
	<-done
}

// A binary that cannot be executed is not something restarting fixes; the
// supervisor says so instead of spinning.
func TestSuperviseMissingBinaryIsFatal(t *testing.T) {
	fastPacing(t)
	var logs syncBuffer
	tun := &Tunnel{ID: "x", token: "t", config: testConfig("http://unused", filepath.Join(t.TempDir(), "nope"), &logs)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := tun.Supervise(ctx)
	var be *binaryError
	if !errors.As(err, &be) {
		t.Errorf("Supervise = %v, want a binaryError", err)
	}
}
