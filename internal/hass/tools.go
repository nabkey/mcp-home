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
	t.registerManageScripts(server)
	t.registerManageScenes(server)
	t.registerGetHomeRegistry(server)
	t.registerListHomeServices(server)
	t.registerGetStateHistory(server)
	t.registerRenderTemplate(server)
	t.registerGetLongTermStatistics(server)
	t.registerGetCalendarEvents(server)
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

// --- manage_scripts ---

type manageScriptsArgs struct {
	Action string         `json:"action" jsonschema:"Action: list (all scripts) get_config (get config) create update delete"`
	ID     string         `json:"id,omitempty" jsonschema:"Script entity_id (for get_config, e.g. script.alert) or object_id without script. prefix (for create/update/delete, e.g. alert)"`
	Config map[string]any `json:"config,omitempty" jsonschema:"Script configuration (for create/update). Top-level keys: alias, sequence, mode, max, fields, icon, description."`
}

func (t *Tools) registerManageScripts(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "manage_scripts",
		Description: "Manage Home Assistant scripts. List all, get config, create, update, or delete scripts.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args manageScriptsArgs) (*mcp.CallToolResult, any, error) {
		switch args.Action {
		case "list":
			states, err := t.client.GetStates(ctx)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			var scripts []map[string]any
			for _, s := range states {
				if strings.HasPrefix(s.EntityID, "script.") {
					scripts = append(scripts, map[string]any{
						"entity_id":      s.EntityID,
						"state":          s.State,
						"friendly_name":  s.Attributes["friendly_name"],
						"last_triggered": s.Attributes["last_triggered"],
					})
				}
			}
			return mcputil.JSONResult(map[string]any{"scripts": scripts})

		case "get_config":
			if args.ID == "" {
				return mcputil.TextResult("Error: id (entity_id) is required for get_config"), nil, nil
			}
			wsClient := t.client.NewWebsocketClient()
			if err := wsClient.Dial(); err != nil {
				return mcputil.Errorf("connecting: %v", err), nil, nil
			}
			defer func() { _ = wsClient.Close() }()

			config, err := wsClient.GetScriptConfig(args.ID)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"config": config})

		case "create":
			if args.ID == "" {
				return mcputil.TextResult("Error: id (object_id without script. prefix) is required for create"), nil, nil
			}
			if args.Config == nil {
				return mcputil.TextResult("Error: config is required for create"), nil, nil
			}
			resp, err := t.client.CreateScript(ctx, args.ID, args.Config)
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
			resp, err := t.client.UpdateScript(ctx, args.ID, args.Config)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"config": resp, "status": "updated"})

		case "delete":
			if args.ID == "" {
				return mcputil.TextResult("Error: id is required for delete"), nil, nil
			}
			if err := t.client.DeleteScript(ctx, args.ID); err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"status": "deleted"})

		default:
			return mcputil.TextResult(fmt.Sprintf("Unknown action: %s (use list, get_config, create, update, delete)", args.Action)), nil, nil
		}
	})
}

// --- manage_scenes ---

type manageScenesArgs struct {
	Action string         `json:"action" jsonschema:"Action: list (all scenes) get_config (raw config) create update delete activate"`
	ID     string         `json:"id,omitempty" jsonschema:"Scene entity_id (for get_config and activate, e.g. scene.movie_night) or object_id without scene. prefix (for create/update/delete, e.g. movie_night)"`
	Config map[string]any `json:"config,omitempty" jsonschema:"Scene configuration (for create/update). Top-level keys: name, entities (map of entity_id to desired state/attributes), icon."`
}

func (t *Tools) registerManageScenes(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "manage_scenes",
		Description: "Manage Home Assistant scenes. List all, get config, create, update, delete, or activate a scene. Activating turns on the scene's entity_id.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args manageScenesArgs) (*mcp.CallToolResult, any, error) {
		switch args.Action {
		case "list":
			states, err := t.client.GetStates(ctx)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			var scenes []map[string]any
			for _, s := range states {
				if strings.HasPrefix(s.EntityID, "scene.") {
					scenes = append(scenes, map[string]any{
						"entity_id":     s.EntityID,
						"state":         s.State,
						"friendly_name": s.Attributes["friendly_name"],
					})
				}
			}
			return mcputil.JSONResult(map[string]any{"scenes": scenes})

		case "get_config":
			if args.ID == "" {
				return mcputil.TextResult("Error: id (entity_id) is required for get_config"), nil, nil
			}
			wsClient := t.client.NewWebsocketClient()
			if err := wsClient.Dial(); err != nil {
				return mcputil.Errorf("connecting: %v", err), nil, nil
			}
			defer func() { _ = wsClient.Close() }()

			config, err := wsClient.GetSceneConfig(args.ID)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"config": config})

		case "create":
			if args.ID == "" {
				return mcputil.TextResult("Error: id (object_id without scene. prefix) is required for create"), nil, nil
			}
			if args.Config == nil {
				return mcputil.TextResult("Error: config is required for create"), nil, nil
			}
			resp, err := t.client.CreateScene(ctx, args.ID, args.Config)
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
			resp, err := t.client.UpdateScene(ctx, args.ID, args.Config)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"config": resp, "status": "updated"})

		case "delete":
			if args.ID == "" {
				return mcputil.TextResult("Error: id is required for delete"), nil, nil
			}
			if err := t.client.DeleteScene(ctx, args.ID); err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"status": "deleted"})

		case "activate":
			if args.ID == "" {
				return mcputil.TextResult("Error: id (entity_id, e.g. scene.movie_night) is required for activate"), nil, nil
			}
			states, err := t.client.CallService(ctx, "scene", "turn_on", map[string]any{"entity_id": args.ID})
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"status": "activated", "affected_states": states})

		default:
			return mcputil.TextResult(fmt.Sprintf("Unknown action: %s (use list, get_config, create, update, delete, activate)", args.Action)), nil, nil
		}
	})
}

// --- get_home_registry ---

type getHomeRegistryArgs struct {
	Kind string `json:"kind" jsonschema:"Registry to fetch: areas, devices, entities, labels, floors, or all (returns the full topology in one call)."`
}

func (t *Tools) registerGetHomeRegistry(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_home_registry",
		Description: "Get Home Assistant topology data (areas, devices, entities, labels, floors). Use kind=all to fetch the full home topology in one call so the agent can map entities to rooms, devices, and floors.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getHomeRegistryArgs) (*mcp.CallToolResult, any, error) {
		kind := strings.ToLower(strings.TrimSpace(args.Kind))
		if kind == "" {
			kind = "all"
		}

		wsClient := t.client.NewWebsocketClient()
		if err := wsClient.Dial(); err != nil {
			return mcputil.Errorf("connecting: %v", err), nil, nil
		}
		defer func() { _ = wsClient.Close() }()

		switch kind {
		case "areas":
			items, err := wsClient.ListAreas()
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"areas": items, "count": len(items)})
		case "devices":
			items, err := wsClient.ListDevices()
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"devices": items, "count": len(items)})
		case "entities":
			items, err := wsClient.ListEntityRegistry()
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"entities": items, "count": len(items)})
		case "labels":
			items, err := wsClient.ListLabels()
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"labels": items, "count": len(items)})
		case "floors":
			items, err := wsClient.ListFloors()
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"floors": items, "count": len(items)})
		case "all":
			areas, err := wsClient.ListAreas()
			if err != nil {
				return mcputil.Errorf("areas: %v", err), nil, nil
			}
			devices, err := wsClient.ListDevices()
			if err != nil {
				return mcputil.Errorf("devices: %v", err), nil, nil
			}
			entities, err := wsClient.ListEntityRegistry()
			if err != nil {
				return mcputil.Errorf("entities: %v", err), nil, nil
			}
			labels, err := wsClient.ListLabels()
			if err != nil {
				return mcputil.Errorf("labels: %v", err), nil, nil
			}
			floors, err := wsClient.ListFloors()
			if err != nil {
				return mcputil.Errorf("floors: %v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{
				"areas":    areas,
				"devices":  devices,
				"entities": entities,
				"labels":   labels,
				"floors":   floors,
			})
		default:
			return mcputil.TextResult(fmt.Sprintf("Unknown kind: %s (use areas, devices, entities, labels, floors, all)", args.Kind)), nil, nil
		}
	})
}

// --- list_home_services ---

type listHomeServicesArgs struct {
	Domain string `json:"domain,omitempty" jsonschema:"Optional domain to filter services (e.g. light, climate, vacuum). Omit to get all domains."`
}

func (t *Tools) registerListHomeServices(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_home_services",
		Description: "List available Home Assistant services with their fields and target selectors. Use this before call_home_service to discover valid (domain, service) pairs and required parameters.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args listHomeServicesArgs) (*mcp.CallToolResult, any, error) {
		services, err := t.client.GetServices(ctx)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		if args.Domain != "" {
			filtered := make([]map[string]any, 0)
			for _, s := range services {
				if d, _ := s["domain"].(string); d == args.Domain {
					filtered = append(filtered, s)
				}
			}
			services = filtered
		}

		return mcputil.JSONResult(map[string]any{
			"services": services,
			"count":    len(services),
		})
	})
}

// --- get_state_history ---

type getStateHistoryArgs struct {
	EntityID        string `json:"entity_id" jsonschema:"Entity ID(s) to fetch history for. Comma-separate to query multiple at once (e.g. sensor.temp,sensor.humidity)."`
	Hours           int    `json:"hours,omitempty" jsonschema:"Hours to look back (1-168, default 24)."`
	MinimalResponse *bool  `json:"minimal_response,omitempty" jsonschema:"If true (default), HA omits attributes from intermediate points to keep responses small."`
	SignificantOnly *bool  `json:"significant_changes_only,omitempty" jsonschema:"If true, HA returns only significant state changes (not every poll)."`
}

func (t *Tools) registerGetStateHistory(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_state_history",
		Description: "Get the time-series state history for one or more entities. Use this for numeric trends (e.g. temperature over the night, door open/closed timeline). For human-readable event descriptions use get_home_events instead.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getStateHistoryArgs) (*mcp.CallToolResult, any, error) {
		if args.EntityID == "" {
			return mcputil.TextResult("Error: entity_id is required"), nil, nil
		}
		hours := args.Hours
		if hours <= 0 {
			hours = 24
		}
		if hours > 168 {
			hours = 168
		}

		minimal := true
		if args.MinimalResponse != nil {
			minimal = *args.MinimalResponse
		}
		significant := false
		if args.SignificantOnly != nil {
			significant = *args.SignificantOnly
		}

		entityIDs := make([]string, 0)
		for _, id := range strings.Split(args.EntityID, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				entityIDs = append(entityIDs, id)
			}
		}

		end := time.Now()
		start := end.Add(-time.Duration(hours) * time.Hour)
		history, err := t.client.GetHistory(ctx, start, end, entityIDs, minimal, significant)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return mcputil.JSONResult(map[string]any{
			"history":      history,
			"entity_ids":   entityIDs,
			"start":        start.UTC().Format(time.RFC3339),
			"end":          end.UTC().Format(time.RFC3339),
			"series_count": len(history),
		})
	})
}

// --- render_template ---

type renderTemplateArgs struct {
	Template  string         `json:"template" jsonschema:"Jinja2 template to render. Has access to states(), is_state(), state_attr(), now(), etc. Example: {{ states('person.alice') }}."`
	Variables map[string]any `json:"variables,omitempty" jsonschema:"Optional variables to expose to the template."`
}

func (t *Tools) registerRenderTemplate(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "render_template",
		Description: "Render a Home Assistant Jinja2 template against current state. Use this for compound questions (is anyone home? average of all temperature sensors? any door open?) that would otherwise require fetching and post-processing many states.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args renderTemplateArgs) (*mcp.CallToolResult, any, error) {
		if args.Template == "" {
			return mcputil.TextResult("Error: template is required"), nil, nil
		}
		out, err := t.client.RenderTemplate(ctx, args.Template, args.Variables)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		return mcputil.JSONResult(map[string]any{"result": out})
	})
}

// --- get_long_term_statistics ---

type getLongTermStatisticsArgs struct {
	StatisticIDs string `json:"statistic_ids" jsonschema:"Statistic IDs to query, comma-separated (typically entity IDs of measurement sensors, e.g. sensor.energy_daily)."`
	Hours        int    `json:"hours,omitempty" jsonschema:"Hours to look back (1-8760 i.e. up to a year, default 168 = 1 week)."`
	Period       string `json:"period,omitempty" jsonschema:"Aggregation bucket: 5minute, hour (default), day, week, or month."`
}

func (t *Tools) registerGetLongTermStatistics(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_long_term_statistics",
		Description: "Query Home Assistant's long-term statistics (energy, gas, water, measurement-class sensors) aggregated by 5minute/hour/day/week/month. Required for energy-usage and trend questions beyond the 10-day short-term history window.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getLongTermStatisticsArgs) (*mcp.CallToolResult, any, error) {
		if args.StatisticIDs == "" {
			return mcputil.TextResult("Error: statistic_ids is required"), nil, nil
		}
		ids := make([]string, 0)
		for _, id := range strings.Split(args.StatisticIDs, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			return mcputil.TextResult("Error: at least one statistic_id is required"), nil, nil
		}

		hours := args.Hours
		if hours <= 0 {
			hours = 168
		}
		if hours > 8760 {
			hours = 8760
		}
		period := args.Period
		if period == "" {
			period = "hour"
		}

		end := time.Now()
		start := end.Add(-time.Duration(hours) * time.Hour)

		wsClient := t.client.NewWebsocketClient()
		if err := wsClient.Dial(); err != nil {
			return mcputil.Errorf("connecting: %v", err), nil, nil
		}
		defer func() { _ = wsClient.Close() }()

		stats, err := wsClient.StatisticsDuringPeriod(start, end, ids, period)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		return mcputil.JSONResult(map[string]any{
			"statistics": stats,
			"start":      start.UTC().Format(time.RFC3339),
			"end":        end.UTC().Format(time.RFC3339),
			"period":     period,
		})
	})
}

// --- get_calendar_events ---

type getCalendarEventsArgs struct {
	EntityID string `json:"entity_id,omitempty" jsonschema:"Calendar entity ID (e.g. calendar.work). Omit to list all calendar entities instead of fetching events."`
	Hours    int    `json:"hours,omitempty" jsonschema:"Window size in hours, looking forward from now (1-720, default 168 = 1 week)."`
}

func (t *Tools) registerGetCalendarEvents(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_calendar_events",
		Description: "Get upcoming events from a Home Assistant calendar entity. Omit entity_id to list available calendars first; supply it to fetch events in a forward-looking window.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getCalendarEventsArgs) (*mcp.CallToolResult, any, error) {
		if args.EntityID == "" {
			calendars, err := t.client.GetCalendars(ctx)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(map[string]any{"calendars": calendars, "count": len(calendars)})
		}

		hours := args.Hours
		if hours <= 0 {
			hours = 168
		}
		if hours > 720 {
			hours = 720
		}

		start := time.Now()
		end := start.Add(time.Duration(hours) * time.Hour)
		events, err := t.client.GetCalendarEvents(ctx, args.EntityID, start, end)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		return mcputil.JSONResult(map[string]any{
			"events": events,
			"count":  len(events),
			"start":  start.UTC().Format(time.RFC3339),
			"end":    end.UTC().Format(time.RFC3339),
		})
	})
}
