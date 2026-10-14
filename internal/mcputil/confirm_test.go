package mcputil

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

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

var yes = mcp.InputResponseMap{
	confirmKey: &mcp.ElicitResult{Action: "accept", Content: map[string]any{confirmKey: true}},
}

// ask runs the first round for a call with args and returns the requestState
// the server handed out.
func ask(t *testing.T, ss *mcp.ServerSession, args string) string {
	t.Helper()
	req := callReq(ss, nil)
	req.Params.Arguments = json.RawMessage(args)
	ok, res := Confirm(req, "Flash pump.yaml")
	if ok || res == nil || res.RequestState == "" {
		t.Fatalf("first round: ok=%v res=%+v, want a question with requestState", ok, res)
	}
	return res.RequestState
}

func retry(ss *mcp.ServerSession, args, state string) *mcp.CallToolRequest {
	req := callReq(ss, yes)
	req.Params.Arguments = json.RawMessage(args)
	req.Params.RequestState = state
	return req
}

func TestConfirmAccept(t *testing.T) {
	ss := connect(t, withElicitation())
	state := ask(t, ss, `{"file":"pump.yaml","force":true}`)

	// Same arguments, re-serialized in another key order and spacing.
	ok, res := Confirm(retry(ss, `{ "force": true, "file": "pump.yaml" }`, state), "Flash pump.yaml")
	if !ok || res != nil {
		t.Errorf("ok=%v res=%+v, want proceed after accept", ok, res)
	}
	if got := ConfirmationAnswer(retry(ss, `{"file":"pump.yaml","force":true}`, state).Params); got != "accepted" {
		t.Errorf("ConfirmationAnswer = %q, want accepted", got)
	}
}

// A yes the server never asked for, or one carried over to a call whose
// arguments changed, must not count: the user agreed to flash pump.yaml, not
// garage.yaml. The client is asked again instead.
func TestConfirmRejectsUnboundAccept(t *testing.T) {
	ss := connect(t, withElicitation())
	state := ask(t, ss, `{"file":"pump.yaml"}`)

	cases := map[string]*mcp.CallToolRequest{
		"no state (pre-answered)": retry(ss, `{"file":"pump.yaml"}`, ""),
		"garbage state":           retry(ss, `{"file":"pump.yaml"}`, "AAAA"),
		"different arguments":     retry(ss, `{"file":"garage.yaml"}`, state),
		"different tool": func() *mcp.CallToolRequest {
			r := retry(ss, `{"file":"pump.yaml"}`, state)
			r.Params.Name = "y"
			return r
		}(),
		"state tampered": retry(ss, `{"file":"pump.yaml"}`, state[:len(state)-2]+"AA"),
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			ok, res := Confirm(req, "Flash pump.yaml")
			if ok {
				t.Fatal("proceeded on an unbound yes")
			}
			if res == nil || res.InputRequests[confirmKey] == nil {
				t.Errorf("res = %+v, want the question asked again", res)
			}
			if got := ConfirmationAnswer(req.Params); got != "accepted_unverified" {
				t.Errorf("ConfirmationAnswer = %q, want accepted_unverified", got)
			}
		})
	}
}

func TestConfirmStateExpires(t *testing.T) {
	ss := connect(t, withElicitation())
	state := ask(t, ss, `{}`)
	defer func(orig func() time.Time) { confirmNow = orig }(confirmNow)
	confirmNow = func() time.Time { return time.Now().Add(confirmTTL + time.Minute) }
	if ok, _ := Confirm(retry(ss, `{}`, state), "x"); ok {
		t.Error("expired confirmation honoured")
	}
}

// A new secret (a restart without SetConfirmSecret) invalidates outstanding
// answers; the same derived secret keeps them valid.
func TestConfirmSecretAcrossRestart(t *testing.T) {
	ss := connect(t, withElicitation())
	defer func(orig []byte) { confirmMu.Lock(); confirmSecret = orig; confirmMu.Unlock() }(confirmSecret)

	SetConfirmSecret([]byte("stable"))
	state := ask(t, ss, `{}`)
	SetConfirmSecret([]byte("stable"))
	if ok, _ := Confirm(retry(ss, `{}`, state), "x"); !ok {
		t.Error("answer lost across a restart with the same secret")
	}
	SetConfirmSecret([]byte("rotated"))
	if ok, _ := Confirm(retry(ss, `{}`, state), "x"); ok {
		t.Error("answer honoured under a different secret")
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
