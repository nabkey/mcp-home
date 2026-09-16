package server

import (
	"context"
	"log/slog"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func listRequest(cursor string) mcp.Request {
	return &mcp.ServerRequest[*mcp.ListToolsParams]{
		Params: &mcp.ListToolsParams{Cursor: cursor},
	}
}

func TestCacheHintOnToolList(t *testing.T) {
	var c mcp.Cacheable
	setCacheable(context.Background(), listRequest(""), &c)

	if c.TTLMs != int(toolListTTL.Milliseconds()) {
		t.Errorf("TTLMs = %d, want %d", c.TTLMs, toolListTTL.Milliseconds())
	}
	// "public" would let any intermediary cache a description of this home.
	if c.CacheScope != "private" {
		t.Errorf("CacheScope = %q, want private", c.CacheScope)
	}
}

func TestCacheHintOnDiscover(t *testing.T) {
	var c mcp.Cacheable
	setCacheable(context.Background(), &mcp.ServerRequest[*mcp.DiscoverParams]{Params: &mcp.DiscoverParams{}}, &c)

	if c.TTLMs == 0 || c.CacheScope != "private" {
		t.Errorf("discover hint = %+v, want private with a TTL", c)
	}
}

// A page is only meaningful next to its cursor, so a later page gets no hint.
func TestNoCacheHintOnPaginatedPage(t *testing.T) {
	var c mcp.Cacheable
	setCacheable(context.Background(), listRequest("page-2"), &c)

	if c.TTLMs != 0 {
		t.Errorf("TTLMs = %d, want 0 for a paginated page", c.TTLMs)
	}
}

// Only tools/list and server/discover are hinted; other cacheable results are
// left as their handler produced them.
func TestCacheHintIgnoresOtherMethods(t *testing.T) {
	var c mcp.Cacheable
	setCacheable(context.Background(), &mcp.ServerRequest[*mcp.ListPromptsParams]{Params: &mcp.ListPromptsParams{}}, &c)

	if c.TTLMs != 0 || c.CacheScope != "" {
		t.Errorf("prompts/list hint = %+v, want untouched", c)
	}
}

// The hook hints the first page without seeing the result, so it is only
// correct while tools/list is a single page. Pin that through a real
// round-trip: the whole list comes back hinted with no NextCursor.
func TestToolListIsSinglePage(t *testing.T) {
	ctx := context.Background()
	srv := newServer("test", slog.New(slog.DiscardHandler))
	for _, name := range []string{"alpha", "beta", "gamma"} {
		srv.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{}, nil
			})
	}

	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.NextCursor != "" {
		t.Fatalf("tools/list paginated (NextCursor=%q); setCacheable assumes a single page", res.NextCursor)
	}
	if len(res.Tools) != 3 {
		t.Errorf("got %d tools, want 3", len(res.Tools))
	}
	if res.TTLMs != int(toolListTTL.Milliseconds()) || res.CacheScope != "private" {
		t.Errorf("hint = ttl %d scope %q, want %d private", res.TTLMs, res.CacheScope, toolListTTL.Milliseconds())
	}
}
