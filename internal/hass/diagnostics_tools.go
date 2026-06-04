package hass

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nabkey/mcp-home/internal/mcputil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxErrorLogChars bounds the error log returned to the model: only the most
// recent characters are kept, since the newest entries are the most relevant.
const maxErrorLogChars = 16000

// tailString returns the last max bytes of s and whether it was truncated,
// advancing to the next UTF-8 rune boundary so the result never starts with a
// partial rune.
func tailString(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	tail := s[len(s)-max:]
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return tail, true
}

// --- get_diagnostics ---

type getDiagnosticsArgs struct {
	Kind string `json:"kind,omitempty" jsonschema:"Which diagnostics to fetch: error_log, system_health, repairs, notifications, or all (default). 'all' tolerates individual section failures and reports them per section."`
}

func (t *Tools) registerGetDiagnostics(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_diagnostics",
		Description: "Get Home Assistant health and error diagnostics: the recent error log, system health info, open repair issues, and active persistent notifications. Use kind=all (default) for a full picture when troubleshooting a misbehaving Home Assistant.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getDiagnosticsArgs) (*mcp.CallToolResult, any, error) {
		kind := strings.ToLower(strings.TrimSpace(args.Kind))
		if kind == "" {
			kind = "all"
		}

		switch kind {
		case "error_log", "system_health", "repairs", "notifications", "all":
		default:
			return mcputil.TextResult(fmt.Sprintf("Unknown kind: %s (use error_log, system_health, repairs, notifications, all)", args.Kind)), nil, nil
		}

		out := map[string]any{}

		// error_log is REST; the rest are WebSocket.
		if kind == "error_log" || kind == "all" {
			log, err := t.client.GetErrorLog(ctx)
			if err != nil {
				if kind == "error_log" {
					return mcputil.Errorf("%v", err), nil, nil
				}
				out["error_log_error"] = err.Error()
			} else {
				tail, truncated := tailString(log, maxErrorLogChars)
				out["error_log"] = tail
				if truncated {
					out["error_log_truncated"] = true
				}
			}
		}

		if kind == "system_health" || kind == "repairs" || kind == "notifications" || kind == "all" {
			wsClient := t.client.NewWebsocketClient()
			if err := wsClient.Dial(); err != nil {
				// For a single WS section, surface the failure. For kind=all,
				// record it and still return whatever (e.g. the error_log) was
				// already gathered — the WS layer is often the thing failing.
				if kind != "all" {
					return mcputil.Errorf("connecting: %v", err), nil, nil
				}
				out["websocket_error"] = err.Error()
				return mcputil.JSONResult(out)
			}
			defer func() { _ = wsClient.Close() }()

			// section runs one WebSocket fetch, returning the whole tool result
			// on error for single-kind requests or recording a per-section error
			// for kind=all.
			section := func(name string, fetch func() (any, error)) *mcp.CallToolResult {
				v, err := fetch()
				if err != nil {
					if kind == "all" {
						out[name+"_error"] = err.Error()
						return nil
					}
					return mcputil.Errorf("%v", err)
				}
				out[name] = v
				return nil
			}

			if kind == "system_health" || kind == "all" {
				if res := section("system_health", wsClient.SystemHealthInfo); res != nil {
					return res, nil, nil
				}
			}
			if kind == "repairs" || kind == "all" {
				if res := section("repairs", wsClient.RepairsListIssues); res != nil {
					return res, nil, nil
				}
			}
			if kind == "notifications" || kind == "all" {
				if res := section("notifications", wsClient.PersistentNotifications); res != nil {
					return res, nil, nil
				}
			}
		}

		return mcputil.JSONResult(out)
	})
}
