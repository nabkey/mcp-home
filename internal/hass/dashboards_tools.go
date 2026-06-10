package hass

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
)

// --- manage_dashboards ---

type manageDashboardsArgs struct {
	Action      string         `json:"action" jsonschema:"Action: list (all dashboards), get_config (read a dashboard's views/cards), save_config (overwrite the full config), delete_config (revert to auto-generated), create, update, delete"`
	URLPath     string         `json:"url_path,omitempty" jsonschema:"Dashboard url_path (e.g. lovelace-cameras). Omit/empty for the default overview dashboard. Used by get_config, save_config, delete_config."`
	DashboardID string         `json:"dashboard_id,omitempty" jsonschema:"Storage dashboard id (from the list action). Required for update and delete."`
	Config      map[string]any `json:"config,omitempty" jsonschema:"Full dashboard config (top-level keys: views, etc.) for save_config. This OVERWRITES the entire dashboard, so read with get_config first, modify, then save the whole object."`
	Properties  map[string]any `json:"properties,omitempty" jsonschema:"Dashboard metadata for create/update: url_path (must contain a hyphen), title, icon, show_in_sidebar, require_admin."`
	Force       bool           `json:"force,omitempty" jsonschema:"For get_config: bypass the cache and force a fresh read."`
}

func (t *Tools) registerManageDashboards(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "manage_dashboards",
		Description: "Manage Home Assistant Lovelace dashboards. List dashboards, read a dashboard's full config (views/cards), overwrite it, revert it to auto-generated, or create/update/delete storage dashboards. save_config replaces the whole config: always get_config first, then modify and save the entire object.",
		Annotations: mcputil.Destructive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args manageDashboardsArgs) (*mcp.CallToolResult, any, error) {
		wsClient := t.client.NewWebsocketClient()
		if err := wsClient.Dial(ctx); err != nil {
			return mcputil.Errorf("connecting: %v", err), nil, nil
		}
		defer func() { _ = wsClient.Close() }()

		switch args.Action {
		case "list":
			dashboards, err := wsClient.ListDashboards()
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"dashboards": dashboards, "count": len(dashboards)})

		case "get_config":
			config, err := wsClient.GetDashboardConfig(args.URLPath, args.Force)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"config": config, "url_path": args.URLPath})

		case "save_config":
			if args.Config == nil {
				return mcputil.TextResult("Error: config is required for save_config (read get_config first, then save the whole modified object)"), nil, nil
			}
			if err := wsClient.SaveDashboardConfig(args.URLPath, args.Config); err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"status": "saved", "url_path": args.URLPath})

		case "delete_config":
			if err := wsClient.DeleteDashboardConfig(args.URLPath); err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"status": "reverted_to_auto_generated", "url_path": args.URLPath})

		case "create":
			if args.Properties == nil {
				return mcputil.TextResult("Error: properties is required for create (include url_path with a hyphen and title)"), nil, nil
			}
			dashboard, err := wsClient.CreateDashboard(args.Properties)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"dashboard": dashboard, "status": "created"})

		case "update":
			if args.DashboardID == "" {
				return mcputil.TextResult("Error: dashboard_id is required for update"), nil, nil
			}
			if args.Properties == nil {
				return mcputil.TextResult("Error: properties is required for update"), nil, nil
			}
			dashboard, err := wsClient.UpdateDashboard(args.DashboardID, args.Properties)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"dashboard": dashboard, "status": "updated"})

		case "delete":
			if args.DashboardID == "" {
				return mcputil.TextResult("Error: dashboard_id is required for delete"), nil, nil
			}
			if err := wsClient.DeleteDashboard(args.DashboardID); err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"status": "deleted"})

		default:
			return mcputil.TextResult(fmt.Sprintf("Unknown action: %s (use list, get_config, save_config, delete_config, create, update, delete)", args.Action)), nil, nil
		}
	})
}

// --- manage_dashboard_resources ---

type manageDashboardResourcesArgs struct {
	Action     string `json:"action" jsonschema:"Action: list, create, update, delete"`
	ResourceID string `json:"resource_id,omitempty" jsonschema:"Resource id (from the list action). Required for update and delete."`
	URL        string `json:"url,omitempty" jsonschema:"Resource URL (e.g. /local/my-card.js). Required for create."`
	ResType    string `json:"res_type,omitempty" jsonschema:"Resource type: module, css, js, or html. Required for create."`
}

func (t *Tools) registerManageDashboardResources(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "manage_dashboard_resources",
		Description: "Manage Home Assistant Lovelace dashboard resources (custom JS/CSS modules loaded by the frontend). List, create, update, or delete resources.",
		Annotations: mcputil.Destructive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args manageDashboardResourcesArgs) (*mcp.CallToolResult, any, error) {
		wsClient := t.client.NewWebsocketClient()
		if err := wsClient.Dial(ctx); err != nil {
			return mcputil.Errorf("connecting: %v", err), nil, nil
		}
		defer func() { _ = wsClient.Close() }()

		switch args.Action {
		case "list":
			resources, err := wsClient.ListDashboardResources()
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"resources": resources, "count": len(resources)})

		case "create":
			if args.URL == "" || args.ResType == "" {
				return mcputil.TextResult("Error: url and res_type are required for create"), nil, nil
			}
			resource, err := wsClient.CreateDashboardResource(args.URL, args.ResType)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"resource": resource, "status": "created"})

		case "update":
			if args.ResourceID == "" {
				return mcputil.TextResult("Error: resource_id is required for update"), nil, nil
			}
			if args.URL == "" && args.ResType == "" {
				return mcputil.TextResult("Error: provide url and/or res_type to update"), nil, nil
			}
			resource, err := wsClient.UpdateDashboardResource(args.ResourceID, args.URL, args.ResType)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"resource": resource, "status": "updated"})

		case "delete":
			if args.ResourceID == "" {
				return mcputil.TextResult("Error: resource_id is required for delete"), nil, nil
			}
			if err := wsClient.DeleteDashboardResource(args.ResourceID); err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"status": "deleted"})

		default:
			return mcputil.TextResult(fmt.Sprintf("Unknown action: %s (use list, create, update, delete)", args.Action)), nil, nil
		}
	})
}
