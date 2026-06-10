package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxAuditArgsLen bounds how much of a tool call's arguments end up in the
// audit log line.
const maxAuditArgsLen = 512

// auditMiddleware logs every tool call with the authenticated user (the
// Cloudflare Access email carried in the bearer token), the tool name, its
// arguments, the outcome, and the duration. This is the audit trail for what
// the assistant did in the home and on whose behalf.
func auditMiddleware(logger *slog.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}

			user := "anonymous"
			if extra := req.GetExtra(); extra != nil && extra.TokenInfo != nil && extra.TokenInfo.UserID != "" {
				user = extra.TokenInfo.UserID
			}

			tool, args := "unknown", ""
			if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
				tool = p.Name
				args = truncate(string(p.Arguments), maxAuditArgsLen)
			}

			start := time.Now()
			result, err := next(ctx, method, req)

			outcome := "ok"
			switch {
			case err != nil:
				outcome = "error: " + err.Error()
			default:
				if r, ok := result.(*mcp.CallToolResult); ok && r.IsError {
					outcome = "tool_error"
				}
			}

			logger.Info("tool call",
				"tool", tool,
				"user", user,
				"args", args,
				"outcome", outcome,
				"duration", time.Since(start).Round(time.Millisecond),
			)
			return result, err
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
