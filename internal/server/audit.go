package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
)

const (
	// maxAuditArgsLen bounds how much of a tool call's arguments end up in
	// the audit log line.
	maxAuditArgsLen = 512
	// maxAuditStringLen bounds any single string argument before the whole
	// is capped, so one large value (a YAML file being written) does not
	// crowd the identifying ones (entity, service, file name) out of the line.
	maxAuditStringLen = 160
	// maxAuditErrorLen bounds the tool error text recorded on failure.
	maxAuditErrorLen = 300
	// redacted replaces a value the audit log must not carry.
	redacted = "[REDACTED]"
)

// auditMiddleware logs every tool call with: the authenticated user, the
// front door it came through, the tool name, its arguments (with secrets
// redacted), the outcome (and the tool's error text on failure), whether a
// confirmation was answered, the negotiated protocol, and the duration.
//
// The user is whatever the front door recorded as auth.TokenInfo.UserID: the
// Cloudflare Access email (or service:<client id>) from the JWT, or the
// WhoIs identity (login@node or node(tag:...)) on the tailnet listener. The
// front door is TokenInfo.Extra["front_door"]. Together that is the audit
// trail for what the assistant did in the home, on whose behalf, and how.
func auditMiddleware(logger *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}

			user, door := "anonymous", "unknown"
			if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil {
				if extra.TokenInfo.UserID != "" {
					user = extra.TokenInfo.UserID
				}
				if d, ok := extra.TokenInfo.Extra[frontDoorKey].(string); ok && d != "" {
					door = d
				}
			}

			tool, args, confirmation := "unknown", "", ""
			if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
				tool = p.Name
				args = auditArgs(p.Arguments)
				confirmation = mcputil.ConfirmationAnswer(p)
			}

			// What the client negotiated decides whether Confirm can ask it
			// anything; log it so the deployed server shows which clients
			// actually get confirmation prompts.
			ss, _ := req.GetSession().(*mcp.ServerSession)
			protocol := ""
			if ss != nil {
				if p := ss.InitializeParams(); p != nil {
					protocol = p.ProtocolVersion
				}
			}

			start := time.Now()
			result, err := next(ctx, method, req)

			outcome, detail := "ok", ""
			switch {
			case err != nil:
				outcome = "error"
				detail = err.Error()
			default:
				if r, ok := result.(*mcp.CallToolResult); ok {
					switch {
					case r.IsError:
						outcome = "tool_error"
						detail = firstText(r)
					case r.InputRequests != nil:
						outcome = "awaiting_input"
					}
				}
			}

			attrs := []any{
				"tool", tool,
				"user", user,
				"front_door", door,
				"args", args,
				"outcome", outcome,
			}
			if detail != "" {
				attrs = append(attrs, "error", truncate(redactText(detail), maxAuditErrorLen))
			}
			if confirmation != "" {
				attrs = append(attrs, "confirmation", confirmation)
			}
			attrs = append(attrs,
				"protocol", protocol,
				"elicitation", mcputil.CanElicit(ss),
				"duration", time.Since(start).Round(time.Millisecond),
			)
			logger.Info("tool call", attrs...)
			return result, err
		}
	}
}

// frontDoorKey is the TokenInfo.Extra key both front doors set
// (cfaccess.FrontDoorKey, tsauth.FrontDoorKey).
const frontDoorKey = "front_door"

func firstText(r *mcp.CallToolResult) string {
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			return t.Text
		}
	}
	return ""
}

// auditArgs renders tool arguments for the log: secrets redacted by key and
// by pattern, long strings shortened, and the whole capped. Arguments that
// are not valid JSON are logged only as pattern-redacted text.
func auditArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return truncate(redactText(string(raw)), maxAuditArgsLen)
	}
	b, err := json.Marshal(redactValue(v))
	if err != nil {
		return ""
	}
	return truncate(string(b), maxAuditArgsLen)
}

// sensitiveKey matches argument names whose values are credentials.
var sensitiveKey = regexp.MustCompile(`(?i)(pass(word|wd|phrase)?|secret|token|api[_-]?key|authorization|credential|private[_-]?key|psk|cookie|encryption[_-]?key|^key$)`)

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if _, isString := val.(string); isString && sensitiveKey.MatchString(k) {
				t[k] = redacted
				continue
			}
			t[k] = redactValue(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = redactValue(t[i])
		}
		return t
	case string:
		return truncate(redactText(t), maxAuditStringLen)
	default:
		return v
	}
}

var (
	// jwtLike matches a compact JWS: what a Cloudflare Access token, or a
	// Home Assistant long-lived token, looks like.
	jwtLike = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]*`)
	// bearer matches an Authorization-style credential in free text.
	bearer = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	// yamlSecret matches "password: value" style lines in config text (an
	// ESPHome YAML being written). "!secret name" references are kept: they
	// name a secret without revealing it.
	yamlSecret = regexp.MustCompile(`(?im)^(\s*-?\s*[\w.-]*(?:pass(?:word|wd|phrase)?|secret|token|api[_-]?key|psk|key)\s*:[ \t]*)([^\n]+)$`)
	// querySecret matches token=value in URLs and query strings.
	querySecret = regexp.MustCompile(`(?i)\b((?:access_)?token|password|secret|api[_-]?key|apikey)=([^&\s"']+)`)
)

// redactText scrubs credentials out of free text by pattern.
func redactText(s string) string {
	s = jwtLike.ReplaceAllString(s, redacted)
	s = bearer.ReplaceAllString(s, "$1 "+redacted)
	s = querySecret.ReplaceAllString(s, "$1="+redacted)
	s = yamlSecret.ReplaceAllStringFunc(s, func(line string) string {
		m := yamlSecret.FindStringSubmatch(line)
		value := strings.TrimSpace(m[2])
		if strings.HasPrefix(value, "!secret") || value == "" || value == "|" || value == ">" {
			return line
		}
		return m[1] + redacted
	})
	return s
}

// truncate shortens s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…(truncated)"
}
