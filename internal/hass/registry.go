package hass

import "fmt"

// Registry writes via the WebSocket API. Reads live in get_home_registry /
// the List* methods; this adds create/update/delete for the area, label,
// floor, entity, and device registries so the assistant can organize the home
// (assign areas, rename, apply labels, set floors).

// RegistryCommand sends a config/<registry>_registry/<action> command with the
// given fields and returns the result object (empty for delete/remove, which
// return no payload). registry is one of area, label, floor, entity, device;
// action is the WebSocket verb (create, update, delete, remove).
func (c *WebsocketClient) RegistryCommand(registry, action string, fields map[string]any) (map[string]any, error) {
	cmd := fmt.Sprintf("config/%s_registry/%s", registry, action)
	resp, err := c.wsCommand(cmd, fields)
	if err != nil {
		return nil, err
	}
	return resultMap(resp), nil
}
