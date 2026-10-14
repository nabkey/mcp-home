package server

// The auditor's question, asked of the logs alone: who unlocked the door at
// 3am, through which front door, with what arguments, and did it work? And
// the corollary: no credential the call carried ends up in the log.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newLogger(buf *bytes.Buffer, format string) *slog.Logger {
	if format == "json" {
		return slog.New(slog.NewJSONHandler(buf, nil))
	}
	return slog.New(slog.NewTextHandler(buf, nil))
}

func auditOnce(t *testing.T, format string, req mcp.Request, result mcp.Result, err error) string {
	t.Helper()
	var buf bytes.Buffer
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) { return result, err }
	_, _ = auditMiddleware(newLogger(&buf, format))(next)(context.Background(), "tools/call", req)
	return buf.String()
}

func toolReq(name, args string, ti *auth.TokenInfo) *mcp.ServerRequest[*mcp.CallToolParamsRaw] {
	return &mcp.ServerRequest[*mcp.CallToolParamsRaw]{
		Params: &mcp.CallToolParamsRaw{Name: name, Arguments: json.RawMessage(args)},
		Extra:  &mcp.RequestExtra{TokenInfo: ti},
	}
}

var (
	cfUser      = &auth.TokenInfo{UserID: "chris@example.com", Extra: map[string]any{"front_door": "cloudflare"}}
	tailnetUser = &auth.TokenInfo{UserID: "voice(tag:voice-agent)", Extra: map[string]any{"front_door": "tailnet"}}
)

// Every field the auditor needs is present, in both formats, as structured
// fields rather than buried in a message.
func TestAuditAnswersWhoWhatWhereOutcome(t *testing.T) {
	args := `{"name":"Front Door","domain":"lock"}`
	cases := []struct {
		ti     *auth.TokenInfo
		result mcp.Result
		err    error
		want   map[string]string
	}{
		{cfUser, &mcp.CallToolResult{}, nil, map[string]string{
			"user": "chris@example.com", "front_door": "cloudflare", "tool": "home_turn_off", "outcome": "ok"}},
		{tailnetUser, &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "Error: lock.unlock is denied by HASS_DENY_SERVICES"}}}, nil, map[string]string{
			"user": "voice(tag:voice-agent)", "front_door": "tailnet", "outcome": "tool_error", "error": "Error: lock.unlock is denied by HASS_DENY_SERVICES"}},
		{nil, nil, errors.New("invalid params"), map[string]string{
			"user": "anonymous", "front_door": "unknown", "outcome": "error", "error": "invalid params"}},
		{cfUser, &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{"confirm": &mcp.ElicitParams{}}}, nil, map[string]string{
			"outcome": "awaiting_input"}},
	}
	for _, c := range cases {
		out := auditOnce(t, "json", toolReq("home_turn_off", args, c.ti), c.result, c.err)
		var rec map[string]any
		if err := json.Unmarshal([]byte(out), &rec); err != nil {
			t.Fatalf("json log not one JSON object: %v\n%s", err, out)
		}
		for k, v := range c.want {
			if rec[k] != v {
				t.Errorf("json %s = %v, want %q (line %s)", k, rec[k], v, out)
			}
		}
		if !strings.Contains(rec["args"].(string), "Front Door") {
			t.Errorf("args missing the target: %v", rec["args"])
		}
		if _, ok := rec["time"]; !ok {
			t.Error("no timestamp")
		}

		text := auditOnce(t, "text", toolReq("home_turn_off", args, c.ti), c.result, c.err)
		for k, v := range c.want {
			if !strings.Contains(text, k+"=") || !strings.Contains(text, v) {
				t.Errorf("text log missing %s=%q: %s", k, v, text)
			}
		}
	}
}

// Credentials in arguments: by key, as a token in free text, and inside a
// YAML file being written. None may survive in either format.
func TestAuditRedactsSecrets(t *testing.T) {
	const jwt = "eyJhbGciOiJSUzI1NiJ9.eyJlbWFpbCI6ImFAYi5jIn0.c2lnbmF0dXJlc2lnbmF0dXJl"
	secrets := []string{"hunter2", "s3cr3t-api", "wifi-pass-123", "otaPW", jwt, "abcdef0123456789abcdef", "Zm9vYmFyYmF6cXV4"}
	args, _ := json.Marshal(map[string]any{
		"password": "hunter2",
		"nested":   map[string]any{"api_key": "s3cr3t-api", "entity_id": "lock.front_door"},
		"content": "ota_password: !secret pump_pw\nwifi:\n  ssid: home\n  password: wifi-pass-123\nota:\n  - platform: esphome\n    password: \"otaPW\"\n" +
			"api:\n  encryption:\n    key: Zm9vYmFyYmF6cXV4\n",
		"note":    "use Bearer abcdef0123456789abcdef or " + jwt,
		"url":     "http://x/api?access_token=abcdef0123456789abcdef&x=1",
		"list":    []any{map[string]any{"token": "hunter2"}},
		"retries": 3,
	})
	for _, format := range []string{"text", "json"} {
		out := auditOnce(t, format, toolReq("write_esphome_file", string(args), cfUser), &mcp.CallToolResult{}, nil)
		for _, s := range secrets {
			if strings.Contains(out, s) {
				t.Errorf("%s log leaks %q:\n%s", format, s, out)
			}
		}
		for _, keep := range []string{"lock.front_door", "REDACTED", "!secret pump_pw"} {
			if !strings.Contains(out, keep) {
				t.Errorf("%s log lost %q:\n%s", format, keep, out)
			}
		}
	}
}

// Tool error text is client-visible content written by tools, and can quote
// what failed — including a URL with a token in it.
func TestAuditRedactsErrorText(t *testing.T) {
	res := &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{
		Text: "Error: GET http://ha/api/x?token=hunter2hunter2 failed: Authorization: Bearer abcdefghijklmnopqrstuvwxyz",
	}}}
	out := auditOnce(t, "text", toolReq("get_home_states", `{}`, cfUser), res, nil)
	if strings.Contains(out, "hunter2hunter2") || strings.Contains(out, "abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("error detail leaks a credential: %s", out)
	}
}

// Attacker-controlled strings (tool name, arguments) must not be able to
// forge a second audit line in either format.
func TestAuditLineCannotBeForged(t *testing.T) {
	forged := "x\ntime=2026-01-01T03:00:00Z level=INFO msg=\"tool call\" tool=home_turn_off user=someone-else outcome=ok"
	args, _ := json.Marshal(map[string]string{"name": forged})
	for _, format := range []string{"text", "json"} {
		out := auditOnce(t, format, toolReq(forged, string(args), cfUser), &mcp.CallToolResult{}, nil)
		if n := strings.Count(strings.TrimRight(out, "\n"), "\n"); n != 0 {
			t.Errorf("%s: one call produced %d extra lines:\n%s", format, n, out)
		}
	}
}

// Arguments that are not JSON still get pattern redaction and the cap.
func TestAuditNonJSONArgs(t *testing.T) {
	got := auditArgs(json.RawMessage("password: hunter2 " + strings.Repeat("a", 2000)))
	if strings.Contains(got, "hunter2") || len(got) > maxAuditArgsLen+len("…(truncated)") {
		t.Errorf("auditArgs = %q", got)
	}
}

// A long value does not crowd the identifying ones out of the capped line.
func TestAuditLongValueDoesNotHideTarget(t *testing.T) {
	args, _ := json.Marshal(map[string]string{"content": strings.Repeat("x", 5000), "path": "pump.yaml"})
	if got := auditArgs(args); !strings.Contains(got, "pump.yaml") {
		t.Errorf("target lost behind a long value: %q", got)
	}
}

// truncate must not split a multi-byte character, which would put invalid
// UTF-8 in the log (and the JSON handler would mangle it).
func TestTruncateRuneSafe(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	got := truncate(s, 5)
	if !strings.HasPrefix(got, "éé") || strings.Contains(got, "\uFFFD") || strings.HasPrefix(got, "ééé") {
		t.Errorf("truncate = %q", got)
	}
}
