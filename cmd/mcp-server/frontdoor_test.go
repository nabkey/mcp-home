package main

// Attack tests for the two front doors as the entrypoint wires them: the real
// mux, the real bridge and RequireBearerToken, the real tailnet middleware
// (with WhoIs faked), the real MCP handler and audit middleware. Token
// cryptography itself is covered in internal/cfaccess; here the verifier is a
// stand-in that accepts exactly one token, so each test is about routing and
// header handling.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/config"
	"github.com/nabkey/mcp-home/internal/server"
	"github.com/nabkey/mcp-home/internal/tsauth"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

const goodToken = "good.jwt.token"

// fakeVerifier accepts goodToken only, as cfaccess would accept a valid
// Access JWT.
func fakeVerifier(_ context.Context, tok string, _ *http.Request) (*auth.TokenInfo, error) {
	if tok != goodToken {
		return nil, fmt.Errorf("%w: token rejected", auth.ErrInvalidToken)
	}
	return &auth.TokenInfo{
		UserID:     "chris@example.com",
		Expiration: time.Now().Add(time.Hour),
		Extra:      map[string]any{"front_door": "cloudflare"},
	}, nil
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// mcpHandler is the production server + HTTP handler with one tool that
// counts its runs, and the log the audit middleware writes to.
func mcpHandler(t *testing.T) (http.Handler, *atomic.Int32, *syncBuf) {
	t.Helper()
	logs := &syncBuf{}
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	srv := server.New(context.Background(), config.CLI{}, "test", logger)
	ran := new(atomic.Int32)
	mcp.AddTool(srv, &mcp.Tool{Name: "unlock_door"}, func(context.Context, *mcp.CallToolRequest, struct {
		Door string `json:"door"`
	}) (*mcp.CallToolResult, any, error) {
		ran.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "unlocked"}}}, nil, nil
	})
	return server.NewHTTPHandler(srv, logger), ran, logs
}

// toolCall builds a 2026-07-28 tools/call POST for unlock_door.
func toolCall(mutate func(*http.Request)) *http.Request {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "unlock_door", "arguments": map[string]any{"door": "front"},
			"_meta": map[string]any{
				mcp.MetaKeyProtocolVersion:    "2026-07-28",
				mcp.MetaKeyClientInfo:         map[string]any{"name": "test", "version": "1"},
				mcp.MetaKeyClientCapabilities: map[string]any{},
			},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "unlock_door")
	req.RemoteAddr = "127.0.0.1:50000" // what cloudflared's connections look like
	req.Host = "mcp.example.com"
	if mutate != nil {
		mutate(req)
	}
	return req
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func cfDoor(t *testing.T) (http.Handler, *atomic.Int32, *syncBuf) {
	h, ran, logs := mcpHandler(t)
	return cloudflareMux(h, fakeVerifier, "mcp.example.com", "https://myteam.cloudflareaccess.com"), ran, logs
}

func bearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

// The control: a valid token through either header runs the tool.
func TestCloudflareDoorAdmitsValidToken(t *testing.T) {
	for name, mutate := range map[string]func(*http.Request){
		"authorization": bearer(goodToken),
		"cf header":     func(r *http.Request) { r.Header.Set("Cf-Access-Jwt-Assertion", goodToken) },
	} {
		door, ran, _ := cfDoor(t)
		rec := serve(door, toolCall(mutate))
		if rec.Code != http.StatusOK || ran.Load() != 1 {
			t.Errorf("%s: status=%d ran=%d body=%s", name, rec.Code, ran.Load(), rec.Body.String())
		}
	}
}

// Everything an attacker reaching the localhost port (or the edge, with
// Access misconfigured) can send instead of a valid token.
func TestCloudflareDoorRejectsWithoutValidToken(t *testing.T) {
	cases := map[string]func(*http.Request){
		"no credentials":       nil,
		"forged cf header":     func(r *http.Request) { r.Header.Set("Cf-Access-Jwt-Assertion", "eyJhbGciOiJub25lIn0.e30.") },
		"forged bearer":        bearer("eyJhbGciOiJub25lIn0.e30."),
		"empty bearer":         func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
		"basic auth":           func(r *http.Request) { r.SetBasicAuth("chris@example.com", "x") },
		"token without scheme": func(r *http.Request) { r.Header.Set("Authorization", goodToken) },
		"two tokens":           func(r *http.Request) { r.Header.Set("Authorization", "Bearer x "+goodToken) },
		"bad bearer, good cf":  func(r *http.Request) { bearer("bad")(r); r.Header.Set("Cf-Access-Jwt-Assertion", goodToken) },
		"spoofed forwarding": func(r *http.Request) {
			r.Header.Set("X-Forwarded-For", "1.2.3.4")
			r.Header.Set("Cf-Connecting-Ip", "1.2.3.4")
		},
		"spoofed access identity":  func(r *http.Request) { r.Header.Set("Cf-Access-Authenticated-User-Email", "chris@example.com") },
		"localhost host header":    func(r *http.Request) { r.Host = "127.0.0.1" },
		"tailnet-looking identity": func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "chris@example.com") },
		"tailnet placeholder":      bearer("tailnet-whois"),
		"insecure placeholder":     bearer("insecure"),
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			door, ran, _ := cfDoor(t)
			rec := serve(door, toolCall(mutate))
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if ran.Load() != 0 {
				t.Error("tool ran")
			}
		})
	}
}

// The 401 must point the client at OAuth discovery, and the metadata must
// name Cloudflare Access as the authorization server.
func TestCloudflareDoorAdvertisesOAuth(t *testing.T) {
	door, _, _ := cfDoor(t)
	rec := serve(door, toolCall(nil))
	if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, `resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`) {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	for _, p := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		rec := serve(door, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://myteam.cloudflareaccess.com") {
			t.Errorf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

// No path or method variant reaches the MCP handler without the gate.
func TestCloudflareDoorPathVariants(t *testing.T) {
	paths := []string{"/mcp/", "/MCP", "/mcp/sse/", "//mcp", "/mcp/../mcp", "/./mcp", "/mcp%2F", "/health/../mcp",
		"/.well-known/oauth-protected-resource/../../mcp", "/mcp?x=1", "/mcp/sse", "/mcpx", "/"}
	for _, p := range paths {
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodOptions} {
			door, ran, _ := cfDoor(t)
			req := toolCall(nil)
			req.Method = method
			req.URL.Path, req.URL.RawPath, req.RequestURI = "", "", p
			u, err := req.URL.Parse(p)
			if err != nil {
				continue
			}
			req.URL = u
			rec := serve(door, req)
			if ran.Load() != 0 {
				t.Errorf("%s %s ran the tool without a token", method, p)
			}
			if rec.Code == http.StatusOK && !strings.HasPrefix(p, "/.well-known") {
				t.Errorf("%s %s = 200 without a token: %s", method, p, rec.Body.String())
			}
		}
	}
}

// /health is unauthenticated and must say nothing beyond "ok".
func TestHealthRevealsNothing(t *testing.T) {
	door, _, _ := cfDoor(t)
	rec := serve(door, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("health = %d %q", rec.Code, rec.Body.String())
	}
}

// The gate runs before the body is read: an unauthenticated multi-megabyte
// POST costs a 401, not a buffer. Authenticated, the SDK caps the body.
func TestOversizedBodies(t *testing.T) {
	big := strings.Repeat("a", int(mcp.DefaultMaxRequestBodyBytes)+1)
	var read atomic.Int64
	body := func() io.Reader {
		return &countingReader{r: strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"x":"` + big + `"}}`), n: &read}
	}

	door, _, _ := cfDoor(t)
	req := toolCall(nil)
	req.Body = io.NopCloser(body())
	if rec := serve(door, req); rec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: status %d, want 401", rec.Code)
	}
	if read.Load() != 0 {
		t.Errorf("unauthenticated request body was read (%d bytes)", read.Load())
	}

	req = toolCall(bearer(goodToken))
	req.Header.Set("Mcp-Method", "tools/list")
	req.Header.Del("Mcp-Name")
	req.Body = io.NopCloser(body())
	if rec := serve(door, req); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("authenticated: status %d, want 413", rec.Code)
	}
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// Mcp-Method/Mcp-Name must agree with the body: an intermediary routing or
// filtering on the header (a WAF rule allowing only tools/list, say) must
// not be fooled into passing a tools/call.
func TestHeaderBodyMismatchRejected(t *testing.T) {
	cases := map[string]func(*http.Request){
		"method":  func(r *http.Request) { r.Header.Set("Mcp-Method", "tools/list") },
		"name":    func(r *http.Request) { r.Header.Set("Mcp-Name", "get_home_states") },
		"missing": func(r *http.Request) { r.Header.Del("Mcp-Method") },
	}
	for name, mutate := range cases {
		door, ran, _ := cfDoor(t)
		rec := serve(door, toolCall(func(r *http.Request) { bearer(goodToken)(r); mutate(r) }))
		if ran.Load() != 0 || !strings.Contains(rec.Body.String(), "-32020") {
			t.Errorf("%s: ran=%d status=%d body=%s", name, ran.Load(), rec.Code, rec.Body.String())
		}
	}
}

// A browser on an allowed machine visiting a hostile page is the CSRF case.
// The page cannot send application/json cross-origin without a preflight,
// the preflight gets no CORS grant, and a "simple" text/plain POST is
// refused by content type, so the tool never runs.
func TestBrowserCrossOriginCannotCall(t *testing.T) {
	door, ran, _ := cfDoor(t)
	pre := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	pre.Header.Set("Origin", "https://evil.example")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	pre.Header.Set("Cf-Access-Jwt-Assertion", goodToken) // edge-injected for a logged-in browser
	rec := serve(door, pre)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("preflight granted CORS: %v", rec.Header())
	}

	simple := toolCall(func(r *http.Request) {
		r.Header.Set("Cf-Access-Jwt-Assertion", goodToken)
		r.Header.Set("Content-Type", "text/plain")
		r.Header.Set("Origin", "https://evil.example")
	})
	rec = serve(door, simple)
	if ran.Load() != 0 || rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("simple cross-origin POST: status=%d ran=%d", rec.Code, ran.Load())
	}
}

// --insecure drops the token check on the Cloudflare door only, and its
// calls are attributed to the insecure door rather than looking like
// authenticated ones.
func TestInsecureDoorIsLabelled(t *testing.T) {
	h, ran, logs := mcpHandler(t)
	door := cloudflareMux(h, nil, "mcp.example.com", "")
	rec := serve(door, toolCall(nil))
	if rec.Code != http.StatusOK || ran.Load() != 1 {
		t.Fatalf("insecure door: %d %s", rec.Code, rec.Body.String())
	}
	line := auditLine(t, logs)
	if line["front_door"] != "cloudflare-insecure" || line["user"] != "anonymous" {
		t.Errorf("audit = %v", line)
	}
	// No OAuth metadata is published when nothing would check the token.
	if rec := serve(door, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil)); rec.Code != http.StatusNotFound {
		t.Errorf("metadata served in insecure mode: %d", rec.Code)
	}
}

func auditLine(t *testing.T, logs *syncBuf) map[string]any {
	t.Helper()
	for _, l := range strings.Split(logs.String(), "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(l), &rec) == nil && rec["msg"] == "tool call" {
			return rec
		}
	}
	t.Fatalf("no audit line in:\n%s", logs.String())
	return nil
}

func whoIs(login string, tags ...string) tsauth.WhoIsFunc {
	return func(context.Context, string) (*apitype.WhoIsResponse, error) {
		return &apitype.WhoIsResponse{
			Node:        &tailcfg.Node{Name: "voice.tailnet.ts.net.", Tags: tags},
			UserProfile: &tailcfg.UserProfile{LoginName: login},
		}, nil
	}
}

func tsDoor(t *testing.T, w tsauth.WhoIsFunc) (http.Handler, *atomic.Int32, *syncBuf) {
	h, ran, logs := mcpHandler(t)
	gate := tsauth.NewMiddleware(w, slog.New(slog.DiscardHandler), []string{"chris@example.com"}, []string{"tag:voice-agent"})
	return tailnetMux(h, gate), ran, logs
}

// End to end on the tailnet: the call runs and the audit line names the
// WhoIs identity and the door, whatever bearer token the peer sent.
func TestTailnetDoorAttributesWhoIs(t *testing.T) {
	door, ran, logs := tsDoor(t, whoIs("tagged-devices", "tag:voice-agent"))
	rec := serve(door, toolCall(bearer(goodToken)))
	if rec.Code != http.StatusOK || ran.Load() != 1 {
		t.Fatalf("status=%d ran=%d %s", rec.Code, ran.Load(), rec.Body.String())
	}
	line := auditLine(t, logs)
	if line["user"] != "voice(tag:voice-agent)" || line["front_door"] != "tailnet" || !strings.Contains(fmt.Sprint(line["args"]), "front") {
		t.Errorf("audit line = %v", line)
	}
}

// End to end on the Cloudflare door, the same question.
func TestCloudflareDoorAttributesUser(t *testing.T) {
	door, _, logs := cfDoor(t)
	serve(door, toolCall(bearer(goodToken)))
	line := auditLine(t, logs)
	// The audit log claims to record the negotiated protocol; on the
	// stateless 2026-07-28 path that comes from params._meta, not a handshake.
	if line["protocol"] != "2026-07-28" {
		t.Errorf("protocol = %v, want 2026-07-28", line["protocol"])
	}
	if line["user"] != "chris@example.com" || line["front_door"] != "cloudflare" || line["outcome"] != "ok" {
		t.Errorf("audit line = %v", line)
	}
}

// A tailnet peer that is not allowlisted gets nothing, even carrying a
// token the Cloudflare door would accept: the doors do not share gates.
func TestTailnetDoorIgnoresCloudflareCredentials(t *testing.T) {
	door, ran, _ := tsDoor(t, whoIs("mallory@example.com"))
	rec := serve(door, toolCall(func(r *http.Request) {
		bearer(goodToken)(r)
		r.Header.Set("Cf-Access-Jwt-Assertion", goodToken)
	}))
	if rec.Code != http.StatusForbidden || ran.Load() != 0 {
		t.Errorf("status=%d ran=%d", rec.Code, ran.Load())
	}
	// And a tag that merely extends an allowed one is not that tag.
	door, ran, _ = tsDoor(t, whoIs("tagged-devices", "tag:voice-agent-dev"))
	if rec := serve(door, toolCall(nil)); rec.Code != http.StatusForbidden || ran.Load() != 0 {
		t.Errorf("prefix tag: status=%d ran=%d", rec.Code, ran.Load())
	}
}

// runDoors: one door failing to come up must stop what the other started
// before the error is returned, or the process exits with cloudflared or the
// tsnet node still running.
func TestRunDoorsStopsOthersOnFailedStart(t *testing.T) {
	var stopped atomic.Bool
	started := make(chan struct{})
	good := func(ctx context.Context, g *errgroupGroup) error {
		g.Go(func() error {
			close(started)
			<-ctx.Done()
			time.Sleep(20 * time.Millisecond) // a process taking a moment to exit
			stopped.Store(true)
			return nil
		})
		return nil
	}
	bad := func(ctx context.Context, _ *errgroupGroup) error {
		<-started
		return errors.New("tailscale: TS_STATE_DIR \"/x\" is not writable")
	}
	err := runDoors(context.Background(), slog.New(slog.DiscardHandler), []frontDoor{good, bad})
	if err == nil || !strings.Contains(err.Error(), "TS_STATE_DIR") || !strings.Contains(err.Error(), "refusing to start") {
		t.Errorf("err = %v, want the failing door's cause", err)
	}
	if !stopped.Load() {
		t.Error("runDoors returned before the started door had stopped")
	}
}

// A door interrupted because another failed reports a cancellation; the
// error returned must be the cause, not the interruption.
func TestRunDoorsReportsCauseNotInterruption(t *testing.T) {
	slow := func(ctx context.Context, _ *errgroupGroup) error {
		<-ctx.Done()
		return fmt.Errorf("tunnel setup interrupted: %w", ctx.Err())
	}
	bad := func(context.Context, *errgroupGroup) error {
		return errors.New("cloudflare access: CF_API_TOKEN lacks permission")
	}
	err := runDoors(context.Background(), slog.New(slog.DiscardHandler), []frontDoor{slow, bad})
	if err == nil || !strings.Contains(err.Error(), "lacks permission") {
		t.Errorf("err = %v", err)
	}
}

// SIGTERM is a clean stop.
func TestRunDoorsSignalIsCleanExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	door := func(ctx context.Context, g *errgroupGroup) error {
		g.Go(func() error { <-ctx.Done(); return nil })
		return nil
	}
	done := make(chan error, 1)
	go func() { done <- runDoors(ctx, slog.New(slog.DiscardHandler), []frontDoor{door}) }()
	cancel()
	if err := <-done; err != nil {
		t.Errorf("runDoors after signal = %v", err)
	}
}
