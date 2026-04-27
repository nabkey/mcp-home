package hass

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nabkey/mcp-home/internal/mcputil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tools holds a set of Home Assistant MCP tools configured with a client.
type Tools struct {
	client *Client
}

// NewTools creates a new Tools instance.
func NewTools(baseURL, token string) (*Tools, error) {
	client, err := NewClient(baseURL, token)
	if err != nil {
		return nil, err
	}
	return &Tools{client: client}, nil
}

// Client returns the underlying Home Assistant client.
func (t *Tools) Client() *Client { return t.client }

// Register adds all Home Assistant tools to the given MCP server.
func (t *Tools) Register(server *mcp.Server) {
	t.registerGetStates(server)
	t.registerGetEvents(server)
	t.registerCallService(server)
	t.registerGetTodoItems(server)
	t.registerManageAutomations(server)
	t.registerGetAutomationTraces(server)
	t.registerManageHelpers(server)
}

// --- get_home_states ---

type getStatesArgs struct {
	Domain string `json:"domain,omitempty" jsonschema:"Optional domain to filter entities (e.g. light switch sensor climate cover lock media_player todo)"`
}

func (t *Tools) registerGetStates(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_home_states",
		Description: "Get current states of Home Assistant entities. Use domain filter to get specific types: light, switch, sensor, climate, cover, lock, media_player, todo.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getStatesArgs) (*mcp.CallToolResult, any, error) {
		states, err := t.client.GetStates(ctx)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		if args.Domain != "" {
			prefix := args.Domain + "."
			filtered := make([]State, 0)
			for _, s := range states {
				if strings.HasPrefix(s.EntityID, prefix) {
					filtered = append(filtered, s)
				}
			}
			states = filtered
		}

		return mcputil.JSONResult(map[string]any{
			"states": states,
			"count":  len(states),
		})
	})
}

// --- get_home_events ---

type getEventsArgs struct {
	Hours    int    `json:"hours" jsonschema:"Number of hours to look back (1-24)"`
	EntityID string `json:"entity_id,omitempty" jsonschema:"Optional entity ID to filter events (e.g. light.living_room)"`
}

func (t *Tools) registerGetEvents(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_home_events",
		Description: "Get recent events from Home Assistant logbook. Shows state changes for smart home devices.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getEventsArgs) (*mcp.CallToolResult, any, error) {
		hours := args.Hours
		if hours <= 0 {
			hours = 1
		}
		if hours > 24 {
			hours = 24
		}

		since := time.Now().Add(-time.Duration(hours) * time.Hour)
		entries, err := t.client.GetLogbook(ctx, since, args.EntityID)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return mcputil.JSONResult(map[string]any{
			"events": entries,
			"count":  len(entries),
		})
	})
}

// --- call_home_service ---

type callServiceArgs struct {
	Domain   string         `json:"domain" jsonschema:"Service domain (e.g. light switch climate cover lock media_player)"`
	Service  string         `json:"service" jsonschema:"Service name (e.g. turn_on turn_off toggle set_temperature open_cover close_cover lock unlock)"`
	EntityID string         `json:"entity_id,omitempty" jsonschema:"Target entity ID (e.g. light.living_room). Required for most services."`
	Data     map[string]any `json:"data,omitempty" jsonschema:"Additional service data (e.g. brightness for lights or temperature for climate)"`
}

func (t *Tools) registerCallService(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "call_home_service",
		Description: "Control Home Assistant devices by calling services. Turn on/off lights, set thermostat temperature, open/close covers, lock/unlock doors, play/pause media.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args callServiceArgs) (*mcp.CallToolResult, any, error) {
		if args.Domain == "" {
			return mcputil.TextResult("Error: domain is required"), nil, nil
		}
		if args.Service == "" {
			return mcputil.TextResult("Error: service is required"), nil, nil
		}

		data := args.Data
		if data == nil {
			data = make(map[string]any)
		}
		if args.EntityID != "" {
			data["entity_id"] = args.EntityID
		}

		states, err := t.client.CallService(ctx, args.Domain, args.Service, data)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return mcputil.JSONResult(map[string]any{
			"success":         true,
			"affected_states": states,
		})
	})
}

// --- get_todo_items ---

type getTodoItemsArgs struct {
	EntityID string `json:"entity_id" jsonschema:"The todo list entity ID (e.g. todo.shopping_list). Use get_home_states with domain=todo to see available lists."`
}

func (t *Tools) registerGetTodoItems(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_todo_items",
		Description: "Get items from a Home Assistant todo list. Use get_home_states with domain=todo to discover available lists first.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getTodoItemsArgs) (*mcp.CallToolResult, any, error) {
		if args.EntityID == "" {
			return mcputil.TextResult("Error: entity_id is required"), nil, nil
		}

		items, err := t.client.GetTodoItems(ctx, args.EntityID)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return mcputil.JSONResult(map[string]any{
			"items": items,
			"count": len(items),
		})
	})
}

// --- manage_automations ---

type manageAutomationsArgs struct {
	Action string         `json:"action" jsonschema:"Action: list (all automations) get_config (get config) create update delete"`
	ID     string         `json:"id,omitempty" jsonschema:"Automation entity_id (for get_config) or config id (for update/delete)"`
	Config map[string]any `json:"config,omitempty" jsonschema:"Automation configuration (for create/update). Include alias trigger condition action."`
}

func (t *Tools) registerManageAutomations(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "manage_automations",
		Description: "Manage Home Assistant automations. List all, get config, create, update, or delete automations.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args manageAutomationsArgs) (*mcp.CallToolResult, any, error) {
		switch args.Action {
		case "list":
			states, err := t.client.GetStates(ctx)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			var automations []map[string]any
			for _, s := range states {
				if strings.HasPrefix(s.EntityID, "automation.") {
					automations = append(automations, map[string]any{
						"entity_id":      s.EntityID,
						"state":          s.State,
						"friendly_name":  s.Attributes["friendly_name"],
						"last_triggered": s.Attributes["last_triggered"],
					})
				}
			}
			return mcputil.JSONResult(map[string]any{"automations": automations})

		case "get_config":
			if args.ID == "" {
				return mcputil.TextResult("Error: id (entity_id) is required for get_config"), nil, nil
			}
			wsClient := t.client.NewWebsocketClient()
			if err := wsClient.Dial(); err != nil {
				return mcputil.Errorf("connecting: %v", err), nil, nil
			}
			defer func() { _ = wsClient.Close() }()

			config, err := wsClient.GetAutomationConfig(args.ID)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"config": config})

		case "create":
			if args.Config == nil {
				return mcputil.TextResult("Error: config is required for create"), nil, nil
			}
			resp, err := t.client.CreateAutomation(ctx, args.Config)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"config": resp, "status": "created"})

		case "update":
			if args.ID == "" {
				return mcputil.TextResult("Error: id is required for update"), nil, nil
			}
			if args.Config == nil {
				return mcputil.TextResult("Error: config is required for update"), nil, nil
			}
			resp, err := t.client.UpdateAutomation(ctx, args.ID, args.Config)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"config": resp, "status": "updated"})

		case "delete":
			if args.ID == "" {
				return mcputil.TextResult("Error: id is required for delete"), nil, nil
			}
			if err := t.client.DeleteAutomation(ctx, args.ID); err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"status": "deleted"})

		default:
			return mcputil.TextResult(fmt.Sprintf("Unknown action: %s (use list, get_config, create, update, delete)", args.Action)), nil, nil
		}
	})
}

// --- get_automation_traces ---

type getAutomationTracesArgs struct {
	AutomationID string `json:"automation_id" jsonschema:"The automation unique ID (from config not entity_id). Use manage_automations with action=list to find IDs."`
	RunID        string `json:"run_id,omitempty" jsonschema:"Specific run ID for full trace details. If empty lists recent traces."`
}

func (t *Tools) registerGetAutomationTraces(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_automation_traces",
		Description: "Get execution traces for a Home Assistant automation. Shows trigger info, conditions evaluated, and actions executed. Useful for debugging.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getAutomationTracesArgs) (*mcp.CallToolResult, any, error) {
		if args.AutomationID == "" {
			return mcputil.TextResult("Error: automation_id is required"), nil, nil
		}

		wsClient := t.client.NewWebsocketClient()
		if err := wsClient.Dial(); err != nil {
			return mcputil.Errorf("connecting: %v", err), nil, nil
		}
		defer func() { _ = wsClient.Close() }()

		if args.RunID != "" {
			trace, err := wsClient.TraceGet("automation", args.AutomationID, args.RunID)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"trace": trace})
		}

		traces, err := wsClient.TraceList("automation", args.AutomationID)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		return mcputil.JSONResult(map[string]any{"traces": traces})
	})
}

// --- manage_helpers ---

type manageHelpersArgs struct {
	Action     string         `json:"action" jsonschema:"Action: list create update delete"`
	HelperType string         `json:"helper_type" jsonschema:"Helper domain: input_boolean input_button input_datetime input_number input_select input_text counter timer schedule"`
	HelperID   string         `json:"helper_id,omitempty" jsonschema:"Helper storage id without domain prefix (e.g. my_toggle not input_boolean.my_toggle). Required for update and delete."`
	Config     map[string]any `json:"config,omitempty" jsonschema:"Helper configuration (for create/update). Common fields: name icon. Type-specific fields vary (e.g. input_number takes min max step; input_select takes options; timer takes duration)."`
}

func (t *Tools) registerManageHelpers(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "manage_helpers",
		Description: "Manage Home Assistant helpers (input_boolean, input_button, input_datetime, input_number, input_select, input_text, counter, timer, schedule). List, create, update, or delete helpers.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args manageHelpersArgs) (*mcp.CallToolResult, any, error) {
		if args.HelperType == "" {
			return mcputil.TextResult("Error: helper_type is required"), nil, nil
		}
		if !IsHelperType(args.HelperType) {
			return mcputil.TextResult(fmt.Sprintf("Error: unsupported helper_type %q (use one of: %s)", args.HelperType, strings.Join(HelperTypes, ", "))), nil, nil
		}

		wsClient := t.client.NewWebsocketClient()
		if err := wsClient.Dial(); err != nil {
			return mcputil.Errorf("connecting: %v", err), nil, nil
		}
		defer func() { _ = wsClient.Close() }()

		switch args.Action {
		case "list":
			helpers, err := wsClient.ListHelpers(args.HelperType)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"helpers": helpers, "count": len(helpers)})

		case "create":
			if args.Config == nil {
				return mcputil.TextResult("Error: config is required for create"), nil, nil
			}
			resp, err := wsClient.CreateHelper(args.HelperType, args.Config)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"helper": resp, "status": "created"})

		case "update":
			if args.HelperID == "" {
				return mcputil.TextResult("Error: helper_id is required for update"), nil, nil
			}
			if args.Config == nil {
				return mcputil.TextResult("Error: config is required for update"), nil, nil
			}
			resp, err := wsClient.UpdateHelper(args.HelperType, args.HelperID, args.Config)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"helper": resp, "status": "updated"})

		case "delete":
			if args.HelperID == "" {
				return mcputil.TextResult("Error: helper_id is required for delete"), nil, nil
			}
			if err := wsClient.DeleteHelper(args.HelperType, args.HelperID); err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"status": "deleted"})

		default:
			return mcputil.TextResult(fmt.Sprintf("Unknown action: %s (use list, create, update, delete)", args.Action)), nil, nil
		}
	})
}
