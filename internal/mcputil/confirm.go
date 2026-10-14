package mcputil

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// confirmKey is the input-request id under which the confirmation travels.
// The client echoes it back in InputResponses on the retry.
const confirmKey = "confirm"

// confirmTTL is how long a confirmation prompt stays answerable.
const confirmTTL = 15 * time.Minute

var (
	confirmMu     sync.RWMutex
	confirmSecret = randomSecret()
	confirmNow    = time.Now
)

func randomSecret() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("mcputil: crypto/rand: " + err.Error())
	}
	return b
}

// SetConfirmSecret sets the key that binds a confirmation answer to the call
// it was asked about. The default is random per process, which means a prompt
// answered after a redeploy is asked again; a server that derives this from
// stable secret material keeps answers valid across restarts. Empty input is
// ignored.
func SetConfirmSecret(secret []byte) {
	if len(secret) == 0 {
		return
	}
	sum := sha256.Sum256(append([]byte("mcp-home confirm v1\x00"), secret...))
	confirmMu.Lock()
	confirmSecret = sum[:]
	confirmMu.Unlock()
}

// Confirm gates a destructive action on explicit user approval.
//
// It uses elicitation embedded in the tool result (multi round-trip requests,
// SEP-2322): the handler returns an input-required result carrying a yes/no
// form, the client shows it to the user, and retries the same call with the
// answer attached. Nothing is held open between the two calls, which is what
// lets this work on the stateless transport.
//
// The question carries a requestState that binds it to this tool and these
// exact arguments, signed and time-limited. A "yes" counts only alongside a
// valid state for the same call: an answer sent without being asked, or one
// replayed onto a retry whose arguments changed (flash pump.yaml, then
// garage.yaml with the same yes), is ignored and the user is asked again. A
// "no" is always honoured.
//
// Call it before doing the work. There are three outcomes:
//
//   - ok is true and res is nil: proceed. Either the user accepted this exact
//     call, or the client cannot ask (a protocol older than 2026-07-28, or no
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
		if !accepted(resp) {
			return false, TextResult("Cancelled: the user declined to confirm. Not done: " + message)
		}
		if verifyState(req.Params) {
			return true, nil
		}
		// An unverifiable yes: fall through and ask properly.
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
		RequestState: signState(req.Params.Name, req.Params.Arguments, confirmNow().Add(confirmTTL)),
	}
}

// ConfirmationAnswer describes the confirmation answer a tool call carries,
// for the audit log: "" when there is none, "declined", "accepted" for a yes
// bound to this exact call, or "accepted_unverified" for a yes that Confirm
// will not honour (no or invalid requestState).
func ConfirmationAnswer(p *mcp.CallToolParamsRaw) string {
	if p == nil {
		return ""
	}
	resp, answered := p.InputResponses[confirmKey]
	switch {
	case !answered:
		return ""
	case !accepted(resp):
		return "declined"
	case verifyState(p):
		return "accepted"
	default:
		return "accepted_unverified"
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

// signState returns base64url(expiry || mac), mac over the tool name, the
// canonical arguments and the expiry.
func signState(name string, args json.RawMessage, expiry time.Time) string {
	var exp [8]byte
	binary.BigEndian.PutUint64(exp[:], uint64(expiry.Unix()))
	return base64.RawURLEncoding.EncodeToString(append(exp[:], stateMAC(name, args, exp[:])...))
}

func verifyState(p *mcp.CallToolParamsRaw) bool {
	raw, err := base64.RawURLEncoding.DecodeString(p.RequestState)
	if err != nil || len(raw) != 8+sha256.Size {
		return false
	}
	exp, mac := raw[:8], raw[8:]
	if confirmNow().Unix() > int64(binary.BigEndian.Uint64(exp)) {
		return false
	}
	return hmac.Equal(mac, stateMAC(p.Name, p.Arguments, exp))
}

func stateMAC(name string, args json.RawMessage, exp []byte) []byte {
	confirmMu.RLock()
	m := hmac.New(sha256.New, confirmSecret)
	confirmMu.RUnlock()
	m.Write([]byte(name))
	m.Write([]byte{0})
	m.Write(canonical(args))
	m.Write([]byte{0})
	m.Write(exp)
	return m.Sum(nil)
}

// canonical re-encodes JSON so that a client re-serializing the same
// arguments in a different key order or spacing still matches.
func canonical(args json.RawMessage) []byte {
	if len(args) == 0 {
		return []byte("null")
	}
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return args
	}
	b, err := json.Marshal(v)
	if err != nil {
		return args
	}
	return b
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
