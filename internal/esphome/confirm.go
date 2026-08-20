package esphome

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
)

// confirmFlashKey identifies the flash-confirmation input request. The client
// echoes it back in InputResponses.
const confirmFlashKey = "confirm_flash"

// flashDecision is the outcome of the OTA confirmation handshake.
type flashDecision int

const (
	// flashAsk means the handler must return an input request and wait for the
	// user's answer before doing anything.
	flashAsk flashDecision = iota
	// flashProceed means the flash is authorised, either because the user
	// accepted or because the client cannot be asked.
	flashProceed
	// flashDeclined means the user said no.
	flashDeclined
)

// decideFlash runs the confirmation handshake for a flash request.
//
// Flashing rewrites a device's firmware over the air; a bad image, or the
// wrong device, leaves it unreachable until someone walks over with a USB
// cable. Protocol version 2026-07-28 (SEP-2322, go-sdk v1.7.0) lets a tool ask
// the user directly: the handler returns an input request instead of a result,
// the client puts the question to the user, and the SDK re-invokes the handler
// with the answer in Params.InputResponses.
//
// Clients that do not advertise form elicitation cannot be asked, so the flash
// proceeds unchanged for them — the tool is still annotated destructive, and
// hosts prompt for tool approval in their own way.
func decideFlash(req *mcp.CallToolRequest) flashDecision {
	if req == nil || req.Params == nil {
		return flashProceed
	}
	if resp, ok := req.Params.InputResponses[confirmFlashKey]; ok {
		return readFlashAnswer(resp)
	}
	if len(req.Params.InputResponses) > 0 {
		// The client answered something, but not the question that was asked.
		// Never flash on an answer that cannot be read as consent.
		return flashDeclined
	}
	if !supportsFormElicitation(req.ClientCapabilities()) {
		return flashProceed
	}
	return flashAsk
}

// supportsFormElicitation reports whether the calling client can render a form
// elicitation. A client that advertises elicitation without naming a mode is
// treated as supporting forms, matching the SDK's own back-compatibility rule.
func supportsFormElicitation(caps *mcp.ClientCapabilities) bool {
	if caps == nil || caps.Elicitation == nil {
		return false
	}
	return caps.Elicitation.Form != nil || caps.Elicitation.URL == nil
}

// readFlashAnswer interprets the user's response to the confirmation.
func readFlashAnswer(resp mcp.InputResponse) flashDecision {
	res, ok := resp.(*mcp.ElicitResult)
	if !ok || res.Action != "accept" {
		return flashDeclined
	}
	if confirmed, ok := res.Content["confirm"].(bool); ok && confirmed {
		return flashProceed
	}
	return flashDeclined
}

// flashConfirmationRequest builds the input request asking the user to approve
// the flash.
func flashConfirmationRequest(args uploadArgs) *mcp.CallToolResult {
	target := "over the air"
	if args.Port != "" && args.Port != "OTA" {
		target = "over " + args.Port
	}
	return &mcp.CallToolResult{
		InputRequests: mcp.InputRequestMap{
			confirmFlashKey: &mcp.ElicitParams{
				Message: fmt.Sprintf(
					"Flash the latest build of %s to the device %s? This overwrites the running firmware, and the device is offline until it reboots.",
					args.Config, target,
				),
				RequestedSchema: &jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"confirm": {
							Type:        "boolean",
							Description: "Confirm flashing this device",
						},
					},
					Required: []string{"confirm"},
				},
			},
		},
		// Echoed back on the retry, so a handler that needs to resume work can
		// tell which flash was approved without per-session state.
		RequestState: "flash:" + args.Config,
	}
}

// flashDeclinedResult reports that the user refused the flash.
func flashDeclinedResult(args uploadArgs) *mcp.CallToolResult {
	return mcputil.TextResult(fmt.Sprintf(
		"Flash of %s cancelled: the user did not confirm. Nothing was queued.", args.Config,
	))
}
