package hass

import "context"

// Diagnostics surfaces Home Assistant's own health and error reporting: the
// REST error log plus the WebSocket system health, repair issues, and
// persistent notifications.

// GetErrorLog returns the full Home Assistant error log as plain text.
func (c *Client) GetErrorLog(ctx context.Context) (string, error) {
	body, err := c.doRequest(ctx, "GET", "/api/error_log", nil, nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// wsCommandResult sends a field-less WebSocket command and returns its raw
// result. The result shape varies by command (object or array), so it is
// returned as-is for the caller to serialize.
func (c *WebsocketClient) wsCommandResult(commandType string) (any, error) {
	resp, err := c.wsCommand(commandType, nil)
	if err != nil {
		return nil, err
	}
	return resp["result"], nil
}

// SystemHealthInfo returns Home Assistant's system health information, keyed by
// integration domain. Some values may still be resolving when first read.
func (c *WebsocketClient) SystemHealthInfo() (any, error) {
	return c.wsCommandResult("system_health/info")
}

// RepairsListIssues returns the current repair issues (the "Repairs" surfaced
// in the HA UI).
func (c *WebsocketClient) RepairsListIssues() (any, error) {
	return c.wsCommandResult("repairs/list_issues")
}

// SystemLog returns the deduplicated recent warnings and errors Home
// Assistant keeps in memory (system_log/list). Unlike /api/error_log it works
// when logging to file is off, which is the default on Home Assistant OS.
func (c *WebsocketClient) SystemLog() ([]map[string]any, error) {
	return c.listResultMaps("system_log/list")
}

// PersistentNotifications returns Home Assistant's active persistent
// notifications.
func (c *WebsocketClient) PersistentNotifications() (any, error) {
	return c.wsCommandResult("persistent_notification/get")
}
