package server

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
)

// confirmServer serves one destructive tool through the production HTTP
// handler and counts how often the underlying action ran.
func confirmServer(t *testing.T) (endpoint string, ran *atomic.Int32) {
	t.Helper()
	ran = new(atomic.Int32)
	srv := newServer("test", nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "wipe"}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		if ok, res := mcputil.Confirm(req, "Wipe everything"); !ok {
			return res, nil, nil
		}
		ran.Add(1)
		return mcputil.TextResult("wiped"), nil, nil
	})
	hs := httptest.NewServer(NewHTTPHandler(srv, nil))
	t.Cleanup(hs.Close)
	return hs.URL + "/mcp", ran
}

func callWipe(t *testing.T, endpoint string, opts *mcp.ClientOptions) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, opts).
		Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "wipe"})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	return res.Content[0].(*mcp.TextContent).Text
}

// The whole point: over the real stateless HTTP transport, a client that can
// answer a form is asked once, and the action runs only after it says yes.
// The SDK client fulfils the input request and retries transparently, so the
// call site sees one result.
func TestConfirmOverStatelessHTTP(t *testing.T) {
	endpoint, ran := confirmServer(t)
	var asked atomic.Int32
	res := callWipe(t, endpoint, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, r *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			asked.Add(1)
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
		},
	})

	if asked.Load() != 1 {
		t.Errorf("asked %d times, want 1", asked.Load())
	}
	if ran.Load() != 1 || text(res) != "wiped" {
		t.Errorf("ran=%d text=%q, want the action to run once after accept", ran.Load(), text(res))
	}
}

func TestDeclineOverStatelessHTTP(t *testing.T) {
	endpoint, ran := confirmServer(t)
	res := callWipe(t, endpoint, &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "decline"}, nil
		},
	})

	if ran.Load() != 0 {
		t.Errorf("action ran %d times after a decline", ran.Load())
	}
	if res.IsError || text(res) == "" {
		t.Errorf("result = %+v, want a plain explanation of the decline", res)
	}
}

// A client that never advertised elicitation gets today's behaviour: the
// action runs without a question. This is the path any client that has not
// adopted the feature takes, so it must never error.
func TestNoElicitationCapabilityRunsDirectly(t *testing.T) {
	endpoint, ran := confirmServer(t)
	res := callWipe(t, endpoint, nil)

	if ran.Load() != 1 || text(res) != "wiped" {
		t.Errorf("ran=%d text=%q, want the action to run unprompted", ran.Load(), text(res))
	}
}
