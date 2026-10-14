package hass

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
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

// maxSystemLogEntries caps the system log entries returned, newest first.
const maxSystemLogEntries = 30

// systemLog fetches system_log/list and shapes it: newest first, local
// times, messages joined, tracebacks cut to their last line.
func (t *Tools) systemLog(ctx context.Context) ([]map[string]any, error) {
	ws := t.client.NewWebsocketClient()
	if err := ws.Dial(ctx); err != nil {
		return nil, err
	}
	defer func() { _ = ws.Close() }()
	entries, err := ws.SystemLog()
	if err != nil {
		return nil, err
	}
	home := t.client.HomeContext(ctx)
	sort.SliceStable(entries, func(i, j int) bool {
		ti, _ := entries[i]["timestamp"].(float64)
		tj, _ := entries[j]["timestamp"].(float64)
		return ti > tj
	})
	if len(entries) > maxSystemLogEntries {
		entries = entries[:maxSystemLogEntries]
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		rec := pick(e, "level", "name", "count")
		if ts, ok := e["timestamp"].(float64); ok {
			rec["last"] = home.localTime(time.Unix(int64(ts), 0))
		}
		var msgs []string
		if list, ok := e["message"].([]any); ok {
			for _, m := range list {
				msgs = append(msgs, fmt.Sprint(m))
			}
		}
		msg := strings.Join(msgs, " | ")
		if len(msg) > 500 {
			msg = msg[:500] + "..."
		}
		rec["message"] = msg
		if src, ok := e["source"].([]any); ok && len(src) == 2 {
			rec["source"] = fmt.Sprintf("%v:%v", src[0], src[1])
		}
		if exc := strings.TrimSpace(str(e, "exception")); exc != "" {
			lines := strings.Split(exc, "\n")
			rec["exception"] = strings.TrimSpace(lines[len(lines)-1])
		}
		out = append(out, rec)
	}
	return out, nil
}

// slimRepairs keeps the fields of each open repair issue that say what is
// wrong, dropping ignored issues and nulls.
func slimRepairs(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	issues, ok := m["issues"].([]any)
	if !ok {
		return v
	}
	out := make([]map[string]any, 0, len(issues))
	for _, it := range issues {
		is, ok := it.(map[string]any)
		if !ok || is["ignored"] == true {
			continue
		}
		out = append(out, pick(is, "domain", "issue_domain", "issue_id", "severity", "translation_key", "translation_placeholders", "is_fixable", "learn_more_url", "created", "breaks_in_ha_version"))
	}
	return out
}

// --- get_diagnostics ---

type getDiagnosticsArgs struct {
	Kind string `json:"kind,omitempty" jsonschema:"Which diagnostics to fetch: error_log, system_health, repairs, notifications, or all (default). 'all' tolerates individual section failures and reports them per section."`
}

func (t *Tools) registerGetDiagnostics(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_diagnostics",
		Description: "Get Home Assistant health and error diagnostics: the recent error log, system health info, open repair issues, and active persistent notifications. Use kind=all (default) for a full picture when troubleshooting a misbehaving Home Assistant.",
		Annotations: mcputil.ReadOnly(),
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

		// error_log is REST, falling back to the WebSocket system log; the
		// rest are WebSocket.
		if kind == "error_log" || kind == "all" {
			log, err := t.client.GetErrorLog(ctx)
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				// No log file (Home Assistant OS default since 2024.11):
				// the in-memory system log has the same errors, deduplicated.
				entries, serr := t.systemLog(ctx)
				if serr == nil {
					out["error_log"] = entries
					err = nil
					log = ""
				} else {
					err = fmt.Errorf("no log file, and system_log/list failed: %w", serr)
				}
			}
			if err != nil {
				if kind == "error_log" {
					return mcputil.Errorf("%v", err), nil, nil
				}
				out["error_log_error"] = err.Error()
			} else if log != "" {
				tail, truncated := tailString(log, maxErrorLogChars)
				out["error_log"] = tail
				if truncated {
					out["error_log_truncated"] = true
				}
			}
		}

		if kind == "system_health" || kind == "repairs" || kind == "notifications" || kind == "all" {
			wsClient := t.client.NewWebsocketClient()
			if err := wsClient.Dial(ctx); err != nil {
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
				// system_health/info answers null on current Home Assistant
				// (it became a subscription); say nothing rather than null.
				if v != nil {
					out[name] = v
				}
				return nil
			}

			if kind == "system_health" || kind == "all" {
				if res := section("system_health", wsClient.SystemHealthInfo); res != nil {
					return res, nil, nil
				}
			}
			if kind == "repairs" || kind == "all" {
				if res := section("repairs", func() (any, error) {
					v, err := wsClient.RepairsListIssues()
					return slimRepairs(v), err
				}); res != nil {
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
