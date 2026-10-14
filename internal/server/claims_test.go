package server

// Tests that try to falsify what CLAUDE.md and SECURITY.md claim about the
// transport, rather than restate it.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
)

// Claim: the private cacheScope keeps the tool list from being shared.
// cacheScope is advice to MCP-aware clients; HTTP caches obey
// Cache-Control, and the SDK alone sends "no-cache", which permits a shared
// cache to store the response. The HTTP layer must say private, no-store.
func TestResponsesNotStorableByHTTPCaches(t *testing.T) {
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"tools/list": postToolsList(t, testHandler(t), nil),
		"tools/call": postToolCall(t, testHandler(t), nil),
	} {
		cc := rec.Header().Get("Cache-Control")
		if !strings.Contains(cc, "no-store") || !strings.Contains(cc, "private") {
			t.Errorf("%s: Cache-Control = %q, want private, no-store", name, cc)
		}
	}
}

// Claim: stateless mode makes a redeploy invisible to a connected client.
// Simulate it: two independent server instances (old and new container).
// A client that started on the old one keeps working on the new one without
// any handshake, on both the current and the legacy protocol.
func TestRedeployIsInvisibleToClients(t *testing.T) {
	oldC, newC := testHandler(t), testHandler(t)

	// Current protocol: every request is self-contained.
	if rec := postToolsList(t, oldC, nil); rec.Code != http.StatusOK {
		t.Fatalf("old instance: %d", rec.Code)
	}
	if rec := postToolCall(t, newC, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Errorf("new instance refused a client from the old one: %d %s", rec.Code, rec.Body.String())
	}

	// Legacy protocol: initialize on the old instance, call on the new one.
	const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"legacy","version":"1"}}}`
	if rec := rawPost(oldC, initBody, nil); rec.Code != http.StatusOK || rec.Header().Get("Mcp-Session-Id") != "" {
		t.Fatalf("legacy initialize: %d session=%q", rec.Code, rec.Header().Get("Mcp-Session-Id"))
	}
	const call = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ping","arguments":{}}}`
	rec := rawPost(newC, call, map[string]string{"Mcp-Protocol-Version": "2025-06-18"})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "pong") {
		t.Errorf("legacy client after redeploy: %d %s", rec.Code, rec.Body.String())
	}
}

func rawPost(h http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A client that sends the confirmation "yes" on its very first call, never
// having shown the user a prompt, does not get the destructive action. The
// server asks instead.
func TestPreAnsweredConfirmationOverHTTP(t *testing.T) {
	srv := newServer("test", nil)
	var ran atomic.Int32
	mcp.AddTool(srv, &mcp.Tool{Name: "wipe"}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		if ok, res := mcputil.Confirm(req, "Wipe everything"); !ok {
			return res, nil, nil
		}
		ran.Add(1)
		return mcputil.TextResult("wiped"), nil, nil
	})
	h := NewHTTPHandler(srv, nil)

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "wipe", "arguments": map[string]any{},
			"inputResponses": map[string]any{"confirm": map[string]any{"action": "accept", "content": map[string]any{"confirm": true}}},
			"_meta": map[string]any{
				mcp.MetaKeyProtocolVersion:    "2026-07-28",
				mcp.MetaKeyClientInfo:         map[string]any{"name": "t", "version": "1"},
				mcp.MetaKeyClientCapabilities: map[string]any{"elicitation": map[string]any{"form": map[string]any{}}},
			},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "wipe")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if ran.Load() != 0 {
		t.Fatalf("destructive tool ran on an unsolicited yes: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "inputRequests") {
		t.Errorf("expected the server to ask, got: %s", rec.Body.String())
	}
}
