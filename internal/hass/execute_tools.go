package hass

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
)

// --- execute_script ---

type executeScriptArgs struct {
	Sequence  []any          `json:"sequence" jsonschema:"List of action steps to run, using Home Assistant script syntax (the same schema as a script's sequence). Example: [{\"action\":\"light.turn_on\",\"target\":{\"entity_id\":\"light.kitchen\"}},{\"delay\":\"00:00:05\"}]."`
	Variables map[string]any `json:"variables,omitempty" jsonschema:"Optional variables exposed to the sequence."`
}

func (t *Tools) registerExecuteScript(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "execute_script",
		Description: "Run an ad-hoc sequence of Home Assistant actions without creating a stored script. Useful for one-off multi-step actions (e.g. turn several things on in order with delays). For reusable logic, create a script with manage_scripts instead.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args executeScriptArgs) (*mcp.CallToolResult, any, error) {
		if len(args.Sequence) == 0 {
			return mcputil.TextResult("Error: sequence is required (a non-empty list of action steps)"), nil, nil
		}

		wsClient := t.client.NewWebsocketClient()
		if err := wsClient.Dial(ctx); err != nil {
			return mcputil.Errorf("connecting: %v", err), nil, nil
		}
		defer func() { _ = wsClient.Close() }()

		result, err := wsClient.ExecuteScript(args.Sequence, args.Variables)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		return mcputil.JSONResult(map[string]any{"status": "executed", "result": result})
	})
}
