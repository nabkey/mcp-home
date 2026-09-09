package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// testHandler builds the production handler around a server with one tool.
func testHandler(t *testing.T) http.Handler {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "mcp-home", Version: "test"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ping",
		Description: "test tool",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
	})
	srv.AddReceivingMiddleware(cacheHintMiddleware())
	return NewHTTPHandler(srv, nil)
}

// postToolsList sends a tools/list request at 2026-07-28. That revision has no
// handshake: the peer info the old initialize call carried now rides in
// params._meta on every request, and the method is mirrored into the Mcp-Method
// header so intermediaries can route without reading the body (SEP-2243).
func postToolsList(t *testing.T, h http.Handler, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	const method = "tools/list"
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params": map[string]any{
			"_meta": map[string]any{
				mcp.MetaKeyProtocolVersion:    "2026-07-28",
				mcp.MetaKeyClientInfo:         map[string]any{"name": "test", "version": "1"},
				mcp.MetaKeyClientCapabilities: map[string]any{},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if mutate != nil {
		mutate(req)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The point of running stateless is protocol 2026-07-28, which the SDK only
// serves when sessions are off. A single POST with no prior handshake must
// return the tool list.
func TestStatelessServesCurrentProtocolWithoutHandshake(t *testing.T) {
	rec := postToolsList(t, testHandler(t), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ping"`) {
		t.Errorf("tools/list did not list the tool: %s", rec.Body.String())
	}
}

// No session is issued, so nothing ties a client to one container. This is the
// property that makes a redeploy invisible to a connected client.
func TestStatelessIssuesNoSessionID(t *testing.T) {
	rec := postToolsList(t, testHandler(t), nil)

	if got := rec.Header().Get("Mcp-Session-Id"); got != "" {
		t.Errorf("Mcp-Session-Id = %q, want empty in stateless mode", got)
	}
}

// cloudflared forwards to 127.0.0.1 carrying the public Host header, which the
// default DNS-rebinding check would reject. Confirms that stays disabled.
func TestExternalHostHeaderAccepted(t *testing.T) {
	rec := postToolsList(t, testHandler(t), func(r *http.Request) {
		r.Host = "mcp.example.com"
		r.RemoteAddr = "127.0.0.1:54321"
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("external Host header rejected: %d %s", rec.Code, rec.Body.String())
	}
}

// Older clients keep working: they negotiate down rather than being refused.
// Guards against a stateless server breaking the Claude.ai connector if it has
// not moved to the new revision yet.
func TestLegacyClientNegotiatesDown(t *testing.T) {
	const body = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
		`"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"legacy","version":"1"}}}`

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "protocolVersion") {
		t.Fatalf("initialize did not return a negotiated version: %s", out)
	}
	if strings.Contains(out, "2026-07-28") {
		t.Errorf("legacy initialize should cap below 2026-07-28: %s", out)
	}
}

// Stateless mode drops the standalone GET stream. Asserted so the 405 is a
// recorded consequence rather than a surprise if a client ever tries to open
// one against /mcp/sse.
func TestStatelessRejectsStandaloneGET(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/mcp/sse", nil)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
}

// A body/header mismatch is a routing hazard for intermediaries, so the SDK
// rejects it with -32020 rather than trusting either side.
func TestHeaderBodyMethodMismatchRejected(t *testing.T) {
	rec := postToolsList(t, testHandler(t), func(r *http.Request) {
		r.Header.Set("Mcp-Method", "tools/call")
	})

	if rec.Code == http.StatusOK {
		t.Fatalf("mismatched Mcp-Method accepted: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "-32020") {
		t.Errorf("want HeaderMismatch (-32020), got: %s", rec.Body.String())
	}
}

// Reading the whole body is unbounded work driven by a remote peer; the SDK
// applies DefaultMaxRequestBodyBytes when the option is left at zero.
func TestOversizedBodyRejected(t *testing.T) {
	huge := strings.Repeat("a", int(mcp.DefaultMaxRequestBodyBytes)+1)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"x":"` + huge + `"}}`

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	testHandler(t).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
}

// The SDK applies its own cacheable defaults inside the list handler, so this
// checks the hint survives to the wire rather than being overwritten.
func TestCacheHintReachesTheWire(t *testing.T) {
	rec := postToolsList(t, testHandler(t), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"ttlMs":300000`) {
		t.Errorf("response missing the 5m TTL hint: %s", body)
	}
	if !strings.Contains(body, `"cacheScope":"private"`) {
		t.Errorf("response missing private cache scope: %s", body)
	}
}

// postToolCall sends a tools/call at 2026-07-28. SEP-2243 requires the tool
// name in an Mcp-Name header alongside the Mcp-Method one.
func postToolCall(t *testing.T, h http.Handler, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "ping",
			"arguments": map[string]any{},
			"_meta": map[string]any{
				mcp.MetaKeyProtocolVersion:    "2026-07-28",
				mcp.MetaKeyClientInfo:         map[string]any{"name": "test", "version": "1"},
				mcp.MetaKeyClientCapabilities: map[string]any{},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "ping")
	if mutate != nil {
		mutate(req)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Tool calls are essentially all this server does, so the happy path on the
// new protocol is worth pinning down.
func TestToolCallOnCurrentProtocol(t *testing.T) {
	rec := postToolCall(t, testHandler(t), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "pong") {
		t.Errorf("tool did not run: %s", rec.Body.String())
	}
}

// On 2026-07-28 the tool name must be mirrored into Mcp-Name, not just carried
// in the body. Recorded because every tool call this server serves depends on
// the client getting it right.
func TestToolCallRequiresMcpNameHeader(t *testing.T) {
	rec := postToolCall(t, testHandler(t), func(r *http.Request) {
		r.Header.Del("Mcp-Name")
	})

	if rec.Code == http.StatusOK {
		t.Fatalf("tools/call accepted without Mcp-Name: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "-32020") {
		t.Errorf("want HeaderMismatch (-32020), got: %s", rec.Body.String())
	}
}
