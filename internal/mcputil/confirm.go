package mcputil

import (
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// confirmKey is the input-request id under which the confirmation travels.
// The client echoes it back in InputResponses on the retry.
const confirmKey = "confirm"

// Confirm gates a destructive action on explicit user approval.
//
// It uses elicitation embedded in the tool result (multi round-trip requests,
// SEP-2322): the handler returns an input-required result carrying a yes/no
// form, the client shows it to the user, and retries the same call with the
// answer attached. Nothing is held open between the two calls, which is what
// lets this work on the stateless transport.
//
// Call it before doing the work. There are three outcomes:
//
//   - ok is true and res is nil: proceed. Either the user accepted on this
//     retry, or the client cannot ask (a protocol older than 2026-07-28, or no
//     form-elicitation capability) and the tool runs as it always has. An
//     older client cannot be asked at all here: stateless mode forbids the
//     server-initiated elicitation/create the SDK would otherwise fall back
//     to, so gating on capability is what keeps those clients working.
//   - ok is false and res is the input-required result: return it as-is.
//   - ok is false and res is a plain result: the user declined or dismissed
//     the form; return it so the model learns the action did not happen.
//
// message is shown to the user and should name the exact thing about to
// happen ("Flash pump.yaml over the air").
func Confirm(req *mcp.CallToolRequest, message string) (ok bool, res *mcp.CallToolResult) {
	if req == nil || req.Params == nil {
		return true, nil
	}
	if resp, answered := req.Params.InputResponses[confirmKey]; answered {
		if accepted(resp) {
			return true, nil
		}
		return false, TextResult("Cancelled: the user declined to confirm. Not done: " + message)
	}
	if !CanElicit(req.Session) {
		return true, nil
	}
	return false, &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{
			confirmKey: &mcp.ElicitParams{
				Mode:    "form",
				Message: message + "\n\nThis cannot be undone. Proceed?",
				RequestedSchema: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						confirmKey: {
							Type:        "boolean",
							Title:       "Confirm",
							Description: "Tick to proceed",
						},
					},
					Required: []string{confirmKey},
				},
			},
		},
		RequestState: confirmKey,
	}
}

// accepted reports whether an elicitation answer is an explicit yes. A
// declined or cancelled form, a missing field, or anything but boolean true
// counts as no.
func accepted(resp mcp.InputResponse) bool {
	r, ok := resp.(*mcp.ElicitResult)
	if !ok || r.Action != "accept" {
		return false
	}
	v, _ := r.Content[confirmKey].(bool)
	return v
}

// CanElicit reports whether the session's client can answer an embedded form
// elicitation: it negotiated protocol 2026-07-28 or later, where input
// requests ride in the result, and it advertises form elicitation.
func CanElicit(ss *mcp.ServerSession) bool {
	if ss == nil {
		return false
	}
	p := ss.InitializeParams()
	if p == nil || p.ProtocolVersion < "2026-07-28" {
		return false
	}
	return p.Capabilities != nil && p.Capabilities.Elicitation != nil && p.Capabilities.Elicitation.Form != nil
}
