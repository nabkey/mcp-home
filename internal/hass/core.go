package hass

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nabkey/mcp-home/internal/validate"
)

// Core covers the remaining documented REST endpoints that had no client
// method: the state write/delete pair, event firing, config inspection, and
// config validation.

// SetState creates or updates an entity's state via POST /api/states/<id>.
// This writes only to the state machine — it does not talk to the device, so
// it is for virtual/externally-fed entities. Use CallService (or an intent) to
// actually control something.
func (c *Client) SetState(ctx context.Context, entityID, state string, attributes map[string]any) (map[string]any, error) {
	if err := validate.Identifier("entity_id", entityID); err != nil {
		return nil, err
	}
	payload := map[string]any{"state": state}
	if len(attributes) > 0 {
		payload["attributes"] = attributes
	}

	body, err := c.doRequest(ctx, "POST", "/api/states/"+entityID, nil, payload)
	if err != nil {
		return nil, fmt.Errorf("setting state of %s: %w", entityID, err)
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decoding state response: %w", err)
	}
	return result, nil
}

// DeleteState removes an entity from the state machine via
// DELETE /api/states/<id>. An entity backed by an integration will be
// recreated on the next update.
func (c *Client) DeleteState(ctx context.Context, entityID string) error {
	if err := validate.Identifier("entity_id", entityID); err != nil {
		return err
	}
	if _, err := c.doRequest(ctx, "DELETE", "/api/states/"+entityID, nil, nil); err != nil {
		return fmt.Errorf("deleting state of %s: %w", entityID, err)
	}
	return nil
}

// FireEvent fires an event on the event bus via POST /api/events/<type>.
func (c *Client) FireEvent(ctx context.Context, eventType string, data map[string]any) (string, error) {
	if err := validate.Identifier("event_type", eventType); err != nil {
		return "", err
	}

	var payload any
	if len(data) > 0 {
		payload = data
	}
	body, err := c.doRequest(ctx, "POST", "/api/events/"+eventType, nil, payload)
	if err != nil {
		return "", fmt.Errorf("firing event %s: %w", eventType, err)
	}

	var result struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decoding event response: %w", err)
	}
	return result.Message, nil
}

// GetConfig returns the core configuration (version, unit system, time zone,
// location, config directory) via GET /api/config.
func (c *Client) GetConfig(ctx context.Context) (map[string]any, error) {
	body, err := c.doRequest(ctx, "GET", "/api/config", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("getting config: %w", err)
	}

	var config map[string]any
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, fmt.Errorf("decoding config: %w", err)
	}
	return config, nil
}

// GetComponents returns the loaded components via GET /api/components.
func (c *Client) GetComponents(ctx context.Context) ([]string, error) {
	body, err := c.doRequest(ctx, "GET", "/api/components", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("getting components: %w", err)
	}

	var components []string
	if err := json.Unmarshal(body, &components); err != nil {
		return nil, fmt.Errorf("decoding components: %w", err)
	}
	return components, nil
}

// CheckConfig validates configuration.yaml via POST /api/config/core/check_config.
// The result carries "result" ("valid" or "invalid") and "errors".
func (c *Client) CheckConfig(ctx context.Context) (map[string]any, error) {
	body, err := c.doRequest(ctx, "POST", "/api/config/core/check_config", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("checking config: %w", err)
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decoding check_config response: %w", err)
	}
	return result, nil
}
