package server

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func callThroughAudit(t *testing.T, req mcp.Request, result mcp.Result) string {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		return result, nil
	}
	if _, err := auditMiddleware(logger)(next)(context.Background(), "tools/call", req); err != nil {
		t.Fatalf("middleware: %v", err)
	}
	return buf.String()
}

func TestAuditLogsToolCallWithUser(t *testing.T) {
	req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Params: &mcp.CallToolParamsRaw{
			Name:      "call_home_service",
			Arguments: json.RawMessage(`{"domain":"light","service":"turn_on"}`),
		},
		Extra: &mcp.RequestExtra{
			TokenInfo: &auth.TokenInfo{UserID: "user@example.com"},
		},
	}

	out := callThroughAudit(t, req, &mcp.CallToolResult{})
	for _, want := range []string{"tool=call_home_service", "user=user@example.com", "light", "outcome=ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
}

func TestAuditLogsAnonymousAndToolError(t *testing.T) {
	req := &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Params: &mcp.CallToolParamsRaw{Name: "execute_script"},
	}

	out := callThroughAudit(t, req, &mcp.CallToolResult{IsError: true})
	for _, want := range []string{"user=anonymous", "outcome=tool_error"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
}

func TestAuditIgnoresOtherMethods(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		return nil, nil
	}
	req := &mcp.ServerRequest[*mcp.ListToolsParams]{Params: &mcp.ListToolsParams{}}
	if _, err := auditMiddleware(logger)(next)(context.Background(), "tools/list", req); err != nil {
		t.Fatalf("middleware: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("tools/list should not be audited, got %q", buf.String())
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("truncate(short) = %q", got)
	}
	long := strings.Repeat("a", 600)
	got := truncate(long, 512)
	if len(got) >= 600 || !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("truncate did not shorten: len=%d", len(got))
	}
}
