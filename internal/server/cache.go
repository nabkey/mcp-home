package server

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolListTTL is how long a client may reuse a tools/list response.
//
// The list is fixed for the lifetime of the process: the hand-written tools
// are registered at startup and the generated ones are built then too, from a
// single pass over the live Home Assistant instance. So the only thing that
// can change it is a restart. The TTL is what bounds how long a client keeps
// calling tools that a redeploy has since removed, which is why this is
// minutes rather than hours despite the list being immutable in-process.
const toolListTTL = 5 * time.Minute

// cacheHintMiddleware attaches a freshness hint to tools/list responses
// (SEP-2549). Without it TTLMs is 0, which tells clients the response is
// immediately stale, so every conversation re-fetches a list that here runs to
// dozens of tools each carrying a full JSON Schema.
//
// The scope is deliberately "private" rather than the SDK's "public" default.
// The list is not sensitive in the credential sense, but it is a description
// of this specific home — script names, areas, floors, device names — and
// "public" would let any intermediary cache and serve it. Only the requesting
// user's client has a reason to hold it.
func cacheHintMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			result, err := next(ctx, method, req)
			if err != nil || method != "tools/list" {
				return result, err
			}
			if r, ok := result.(*mcp.ListToolsResult); ok {
				// A paginated page is only meaningful next to its cursor, so
				// hint only the complete list.
				if r.NextCursor == "" {
					r.TTLMs = int(toolListTTL / time.Millisecond)
					r.CacheScope = "private"
				}
			}
			return result, err
		}
	}
}
