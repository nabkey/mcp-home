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

// setCacheable is the ServerOptions.SetCacheable policy: it attaches a
// freshness hint (SEP-2549) to tools/list and server/discover responses.
// Without it TTLMs is 0, which tells clients the response is immediately
// stale, so every conversation re-fetches a list that here runs to dozens of
// tools each carrying a full JSON Schema.
//
// The scope is deliberately "private" rather than the SDK's "public" default.
// The list is not sensitive in the credential sense, but it is a description
// of this specific home — script names, areas, floors, device names — and
// "public" would let any intermediary cache and serve it. Only the requesting
// user's client has a reason to hold it. server/discover carries the same
// instructions text and capabilities, so it gets the same treatment.
//
// The hook sees the request but not the result, so it cannot tell a complete
// list from the first page of a paginated walk. It hints only a request for
// the first page and relies on ServerOptions.PageSize being unset, which makes
// tools/list a single page; TestToolListIsSinglePage pins that. A request
// with a cursor is left unhinted, since a page is only meaningful next to its
// cursor.
//
// Runs with the server's lock held: it must not call back into the server.
func setCacheable(_ context.Context, req mcp.Request, c *mcp.Cacheable) {
	if req == nil {
		return
	}
	switch p := req.GetParams().(type) {
	case *mcp.ListToolsParams:
		if p != nil && p.Cursor != "" {
			return
		}
	case *mcp.DiscoverParams:
	default:
		return
	}
	c.TTLMs = int(toolListTTL / time.Millisecond)
	c.CacheScope = "private"
}
