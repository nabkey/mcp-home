package server

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolListTTL is how long a client may reuse a cached tools/list response.
// The tool set is decided once at startup from the configured integrations and
// never changes while the process runs, so caching it is always safe; the TTL
// is bounded only so that a restart with different integrations is picked up
// without the client having to reconnect.
const toolListTTL = 5 * time.Minute

// cacheHintMiddleware attaches a freshness hint (SEP-2549, added to the
// protocol in go-sdk v1.7.0) to tools/list responses. Clients that understand
// it stop re-listing tools on every request, which matters here because this
// server registers ~40 tools whose schemas are static for the process lifetime.
//
// The hint is advisory: the SDK still emits tools/list_changed notifications,
// and clients that ignore ttlMs behave exactly as before.
func cacheHintMiddleware() mcp.Middleware {
	ttlMs := int(toolListTTL.Milliseconds())
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)
			if err != nil || method != "tools/list" {
				return result, err
			}
			// Pages are cached per cursor, so hinting each page is correct.
			if r, ok := result.(*mcp.ListToolsResult); ok {
				r.TTLMs = ttlMs
			}
			return result, nil
		}
	}
}
