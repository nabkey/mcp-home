package hass

import "fmt"

// ExecuteScript runs an ad-hoc action sequence via the execute_script
// WebSocket command without creating a stored script. sequence is a list of
// action steps using Home Assistant's script syntax; variables are optional
// values exposed to the sequence. The result contains the run context and any
// response data produced by the actions.
func (c *WebsocketClient) ExecuteScript(sequence []any, variables map[string]any) (map[string]any, error) {
	if len(sequence) == 0 {
		return nil, fmt.Errorf("sequence is required")
	}
	fields := map[string]any{"sequence": sequence}
	if len(variables) > 0 {
		fields["variables"] = variables
	}
	resp, err := c.wsCommand("execute_script", fields)
	if err != nil {
		return nil, err
	}
	return resultMap(resp), nil
}
