package mcputil

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connect pairs a server with a client over in-memory transports and returns
// the server-side session, so tests can hand a real ServerSession to Confirm.
// The in-memory pair speaks the newest protocol the SDK knows, which is what
// a capable client looks like.
func connect(t *testing.T, opts *mcp.ClientOptions) *mcp.ServerSession {
	t.Helper()
	ctx := context.Background()
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, opts).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return ss
}

func withElicitation() *mcp.ClientOptions {
	return &mcp.ClientOptions{
		ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept"}, nil
		},
	}
}

func callReq(ss *mcp.ServerSession, responses mcp.InputResponseMap) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{
		Session: ss,
		Params:  &mcp.CallToolParamsRaw{Name: "x", InputResponses: responses},
	}
}

func TestConfirmAsksCapableClient(t *testing.T) {
	ok, res := Confirm(callReq(connect(t, withElicitation()), nil), "Flash pump.yaml")

	if ok {
		t.Fatal("proceeded without asking a client that can answer")
	}
	if res == nil || res.InputRequests[confirmKey] == nil {
		t.Fatalf("result = %+v, want an input request under %q", res, confirmKey)
	}
	if len(res.Content) != 0 {
		t.Error("input-required result must not carry content")
	}
	ep := res.InputRequests[confirmKey].(*mcp.ElicitParams)
	if !strings.Contains(ep.Message, "Flash pump.yaml") {
		t.Errorf("message %q does not name the action", ep.Message)
	}
}

// A client without the elicitation capability has no way to answer, and in
// stateless mode the SDK cannot fall back to asking it directly, so the tool
// must run as it did before confirmation existed.
func TestConfirmProceedsWhenClientCannotAnswer(t *testing.T) {
	ok, res := Confirm(callReq(connect(t, nil), nil), "Flash pump.yaml")

	if !ok || res != nil {
		t.Errorf("ok=%v res=%+v, want proceed", ok, res)
	}
}

func TestConfirmProceedsWithoutSession(t *testing.T) {
	if ok, _ := Confirm(&mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{}}, "x"); !ok {
		t.Error("nil session should proceed")
	}
}

func TestConfirmAccept(t *testing.T) {
	ss := connect(t, withElicitation())
	ok, res := Confirm(callReq(ss, mcp.InputResponseMap{
		confirmKey: &mcp.ElicitResult{Action: "accept", Content: map[string]any{confirmKey: true}},
	}), "Flash pump.yaml")

	if !ok || res != nil {
		t.Errorf("ok=%v res=%+v, want proceed after accept", ok, res)
	}
}

func TestConfirmRefusals(t *testing.T) {
	ss := connect(t, withElicitation())
	cases := map[string]mcp.InputResponse{
		"decline":        &mcp.ElicitResult{Action: "decline"},
		"cancel":         &mcp.ElicitResult{Action: "cancel"},
		"accept false":   &mcp.ElicitResult{Action: "accept", Content: map[string]any{confirmKey: false}},
		"accept missing": &mcp.ElicitResult{Action: "accept"},
		"accept string":  &mcp.ElicitResult{Action: "accept", Content: map[string]any{confirmKey: "true"}},
	}
	for name, resp := range cases {
		t.Run(name, func(t *testing.T) {
			ok, res := Confirm(callReq(ss, mcp.InputResponseMap{confirmKey: resp}), "Flash pump.yaml")
			if ok {
				t.Fatal("proceeded on a refusal")
			}
			if res == nil || res.InputRequests != nil {
				t.Fatalf("result = %+v, want a plain result, not another question", res)
			}
			if res.IsError {
				t.Error("a refusal is an outcome, not a tool error")
			}
			text := res.Content[0].(*mcp.TextContent).Text
			if !strings.Contains(text, "declined") || !strings.Contains(text, "Flash pump.yaml") {
				t.Errorf("text %q should say the user declined and what was not done", text)
			}
		})
	}
}
