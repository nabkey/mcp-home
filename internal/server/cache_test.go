package server

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func throughCacheHint(t *testing.T, method string, result mcp.Result) mcp.Result {
	t.Helper()
	next := func(context.Context, string, mcp.Request) (mcp.Result, error) {
		return result, nil
	}
	got, err := cacheHintMiddleware()(next)(context.Background(), method, nil)
	if err != nil {
		t.Fatalf("middleware: %v", err)
	}
	return got
}

func TestCacheHintOnToolList(t *testing.T) {
	got := throughCacheHint(t, "tools/list", &mcp.ListToolsResult{
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
	got := throughCacheHint(t, "tools/list", &mcp.ListToolsResult{
		Tools:      []*mcp.Tool{{Name: "ping"}},
		NextCursor: "more",
	})

	if res := got.(*mcp.ListToolsResult); res.TTLMs != 0 {
		t.Errorf("TTLMs = %d, want 0 for a paginated page", res.TTLMs)
	}
}

// Only tools/list is hinted; a call result must pass through untouched.
func TestCacheHintIgnoresOtherMethods(t *testing.T) {
	in := &mcp.CallToolResult{}
	if got := throughCacheHint(t, "tools/call", in); got != mcp.Result(in) {
		t.Errorf("result was replaced for tools/call")
	}
}
