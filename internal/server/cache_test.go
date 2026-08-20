package server

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func runCacheHint(t *testing.T, method string, result mcp.Result, retErr error) (mcp.Result, error) {
	t.Helper()
	next := func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		return result, retErr
	}
	req := &mcp.ServerRequest[*mcp.ListToolsParams]{Params: &mcp.ListToolsParams{}}
	return cacheHintMiddleware()(next)(context.Background(), method, req)
}

func TestCacheHintSetsToolListTTL(t *testing.T) {
	res, err := runCacheHint(t, "tools/list", &mcp.ListToolsResult{}, nil)
	if err != nil {
		t.Fatalf("middleware: %v", err)
	}
	list, ok := res.(*mcp.ListToolsResult)
	if !ok {
		t.Fatalf("result type = %T", res)
	}
	if want := int(toolListTTL.Milliseconds()); list.TTLMs != want {
		t.Errorf("TTLMs = %d, want %d", list.TTLMs, want)
	}
}

func TestCacheHintLeavesOtherMethodsAlone(t *testing.T) {
	in := &mcp.CallToolResult{}
	res, err := runCacheHint(t, "tools/call", in, nil)
	if err != nil {
		t.Fatalf("middleware: %v", err)
	}
	if res != mcp.Result(in) {
		t.Errorf("result was replaced: %#v", res)
	}
}

func TestCacheHintPropagatesErrors(t *testing.T) {
	want := errors.New("boom")
	res, err := runCacheHint(t, "tools/list", &mcp.ListToolsResult{}, want)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	// A failed list must not be advertised as cacheable.
	if list, ok := res.(*mcp.ListToolsResult); ok && list.TTLMs != 0 {
		t.Errorf("TTLMs = %d on error, want 0", list.TTLMs)
	}
}
