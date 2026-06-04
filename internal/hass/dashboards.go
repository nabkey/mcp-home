package hass

import "github.com/nabkey/mcp-home/internal/validate"

// Dashboard (Lovelace) management via the WebSocket API.
//
// Per-dashboard configs are read with lovelace/config and overwritten wholesale
// with lovelace/config/save. The dashboard collection (storage-mode dashboards
// shown in the sidebar) is managed with the lovelace/dashboards/* commands, and
// custom JS/CSS resources with lovelace/resources/*.
//
// url_path identifies a dashboard; an empty url_path means the default overview
// dashboard, sent to Home Assistant as JSON null.

// ListDashboards returns all configured Lovelace dashboards.
func (c *WebsocketClient) ListDashboards() ([]map[string]any, error) {
	resp, err := c.wsCommand("lovelace/dashboards/list", nil)
	if err != nil {
		return nil, err
	}
	return resultList(resp), nil
}

// GetDashboardConfig returns the full stored config (views/cards) for a
// dashboard. An empty urlPath selects the default overview dashboard. force
// bypasses the in-memory cache. Home Assistant returns an error if the
// dashboard is in auto-generated mode (no stored config).
func (c *WebsocketClient) GetDashboardConfig(urlPath string, force bool) (map[string]any, error) {
	resp, err := c.wsCommand("lovelace/config", map[string]any{
		"url_path": nullable(urlPath),
		"force":    force,
	})
	if err != nil {
		return nil, err
	}
	return resultMap(resp), nil
}

// SaveDashboardConfig overwrites the entire stored config for a dashboard.
// Callers should read the current config first, mutate it, and save the whole
// object. An empty urlPath targets the default overview dashboard.
func (c *WebsocketClient) SaveDashboardConfig(urlPath string, config map[string]any) error {
	_, err := c.wsCommand("lovelace/config/save", map[string]any{
		"url_path": nullable(urlPath),
		"config":   config,
	})
	return err
}

// DeleteDashboardConfig deletes a dashboard's stored config, reverting it to
// auto-generated mode. An empty urlPath targets the default overview dashboard.
func (c *WebsocketClient) DeleteDashboardConfig(urlPath string) error {
	_, err := c.wsCommand("lovelace/config/delete", map[string]any{
		"url_path": nullable(urlPath),
	})
	return err
}

// CreateDashboard creates a new storage-mode dashboard. fields must include
// url_path (which Home Assistant requires to contain a hyphen) and title;
// optional fields are icon, show_in_sidebar, and require_admin.
func (c *WebsocketClient) CreateDashboard(fields map[string]any) (map[string]any, error) {
	if urlPath, _ := fields["url_path"].(string); urlPath != "" {
		if err := validate.Identifier("url_path", urlPath); err != nil {
			return nil, err
		}
	}
	resp, err := c.wsCommand("lovelace/dashboards/create", fields)
	if err != nil {
		return nil, err
	}
	return resultMap(resp), nil
}

// UpdateDashboard updates metadata (title, icon, show_in_sidebar,
// require_admin) for an existing storage-mode dashboard.
func (c *WebsocketClient) UpdateDashboard(dashboardID string, fields map[string]any) (map[string]any, error) {
	if err := validate.Identifier("dashboard_id", dashboardID); err != nil {
		return nil, err
	}
	merged := map[string]any{"dashboard_id": dashboardID}
	for k, v := range fields {
		if k == "dashboard_id" {
			continue
		}
		merged[k] = v
	}
	resp, err := c.wsCommand("lovelace/dashboards/update", merged)
	if err != nil {
		return nil, err
	}
	return resultMap(resp), nil
}

// DeleteDashboard removes a storage-mode dashboard.
func (c *WebsocketClient) DeleteDashboard(dashboardID string) error {
	if err := validate.Identifier("dashboard_id", dashboardID); err != nil {
		return err
	}
	_, err := c.wsCommand("lovelace/dashboards/delete", map[string]any{"dashboard_id": dashboardID})
	return err
}

// ListDashboardResources returns all registered Lovelace resources (custom
// JS/CSS modules).
func (c *WebsocketClient) ListDashboardResources() ([]map[string]any, error) {
	resp, err := c.wsCommand("lovelace/resources/list", nil)
	if err != nil {
		return nil, err
	}
	return resultList(resp), nil
}

// CreateDashboardResource registers a new resource. resType is one of module,
// css, js, or html.
func (c *WebsocketClient) CreateDashboardResource(url, resType string) (map[string]any, error) {
	resp, err := c.wsCommand("lovelace/resources/create", map[string]any{
		"url":      url,
		"res_type": resType,
	})
	if err != nil {
		return nil, err
	}
	return resultMap(resp), nil
}

// UpdateDashboardResource updates an existing resource. Empty url or resType
// fields are omitted from the request.
func (c *WebsocketClient) UpdateDashboardResource(resourceID, url, resType string) (map[string]any, error) {
	if err := validate.Identifier("resource_id", resourceID); err != nil {
		return nil, err
	}
	fields := map[string]any{"resource_id": resourceID}
	if url != "" {
		fields["url"] = url
	}
	if resType != "" {
		fields["res_type"] = resType
	}
	resp, err := c.wsCommand("lovelace/resources/update", fields)
	if err != nil {
		return nil, err
	}
	return resultMap(resp), nil
}

// DeleteDashboardResource removes a resource.
func (c *WebsocketClient) DeleteDashboardResource(resourceID string) error {
	if err := validate.Identifier("resource_id", resourceID); err != nil {
		return err
	}
	_, err := c.wsCommand("lovelace/resources/delete", map[string]any{"resource_id": resourceID})
	return err
}
