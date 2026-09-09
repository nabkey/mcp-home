package server

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// listRequest builds a tools/list request asking for the page after cursor.
func listRequest(cursor string) mcp.Request {
	return &mcp.ServerRequest[*mcp.ListToolsParams]{
		Params: &mcp.ListToolsParams{Cursor: cursor},
	}
}

func throughCacheHint(t *testing.T, method string, req mcp.Request, result mcp.Result) mcp.Result {
	t.Helper()
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return result, nil
	}
	got, err := cacheHintMiddleware()(next)(context.Background(), method, req)
	if err != nil {
		t.Fatalf("middleware: %v", err)
	}
	return got
}

func TestCacheHintOnToolList(t *testing.T) {
	got := throughCacheHint(t, "tools/list", listRequest(""), &mcp.ListToolsResult{
		Tools: []*mcp.Tool{{Name: "ping"}},
	})

	res, ok := got.(*mcp.ListToolsResult)
	if !ok {
		t.Fatalf("result type = %T", got)
	}
	if res.TTLMs != int(toolListTTL.Milliseconds()) {
		t.Errorf("TTLMs = %d, want %d", res.TTLMs, toolListTTL.Milliseconds())
	}
	// "public" would let any intermediary cache a description of this home.
	if res.CacheScope != "private" {
		t.Errorf("CacheScope = %q, want private", res.CacheScope)
	}
}

// A page is only meaningful next to its cursor, so a partial list gets no hint.
func TestNoCacheHintOnPaginatedPage(t *testing.T) {
	got := throughCacheHint(t, "tools/list", listRequest(""), &mcp.ListToolsResult{
		Tools:      []*mcp.Tool{{Name: "ping"}},
		NextCursor: "more",
	})

	if res := got.(*mcp.ListToolsResult); res.TTLMs != 0 {
		t.Errorf("TTLMs = %d, want 0 for a paginated page", res.TTLMs)
	}
}

// The last page of a paginated walk also has an empty NextCursor, so the
// response alone cannot tell it apart from a complete list. The request cursor
// can: a client that asked for a later page did not receive the whole list.
func TestNoCacheHintOnFinalPageOfWalk(t *testing.T) {
	got := throughCacheHint(t, "tools/list", listRequest("page-2"), &mcp.ListToolsResult{
		Tools: []*mcp.Tool{{Name: "ping"}},
	})

	if res := got.(*mcp.ListToolsResult); res.TTLMs != 0 {
		t.Errorf("TTLMs = %d, want 0 for the final page of a paginated walk", res.TTLMs)
	}
}

// Only tools/list is hinted; a call result must pass through untouched.
func TestCacheHintIgnoresOtherMethods(t *testing.T) {
	in := &mcp.CallToolResult{}
	if got := throughCacheHint(t, "tools/call", nil, in); got != mcp.Result(in) {
		t.Errorf("result was replaced for tools/call")
	}
}
