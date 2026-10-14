package hass

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
)

// Tools holds a set of Home Assistant MCP tools configured with a client.
type Tools struct {
	client *Client
	// now is the clock time windows are measured from; nil means time.Now.
	// Tests replaying a captured instance pin it to the capture time.
	now func() time.Time
}

func (t *Tools) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// NewTools creates a new Tools instance. denyServices is an optional list of
// domain.service patterns to refuse (see ServicePolicy).
func NewTools(baseURL, token string, denyServices []string) (*Tools, error) {
	client, err := NewClient(baseURL, token)
	if err != nil {
		return nil, err
	}
	policy, err := NewServicePolicy(denyServices)
	if err != nil {
		return nil, err
	}
	client.SetServicePolicy(policy)
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
	t.registerManageDashboards(server)
	t.registerManageDashboardResources(server)
	t.registerManageRegistry(server)
	t.registerExecuteScript(server)
	t.registerGetDiagnostics(server)
	t.registerManageEntityState(server)
	t.registerFireHomeEvent(server)
	t.registerGetHomeConfig(server)
}

// --- get_home_states ---

type getStatesArgs struct {
	Search      string `json:"search,omitempty" jsonschema:"Case-insensitive substring of entity_id, name or area name (e.g. garage, kitchen, temperature)."`
	Domain      string `json:"domain,omitempty" jsonschema:"Domain(s), comma-separated (e.g. light or light,switch)."`
	EntityID    string `json:"entity_id,omitempty" jsonschema:"Exact entity id(s), comma-separated."`
	Area        string `json:"area,omitempty" jsonschema:"Area id or name (e.g. kitchen)."`
	DeviceClass string `json:"device_class,omitempty" jsonschema:"device_class(es), comma-separated: door,window,garage_door,opening,motion,temperature,power,battery..."`
	State       string `json:"state,omitempty" jsonschema:"Only entities currently in these state(s), comma-separated (e.g. on,open,unavailable)."`
	All         bool   `json:"all,omitempty" jsonschema:"Include diagnostic/config entities (signal strength, firmware, restart buttons), hidden by default."`
	Attributes  bool   `json:"attributes,omitempty" jsonschema:"Return full attributes (brightness, color, options, forecasts...) instead of the compact record."`
}

func (t *Tools) registerGetStates(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_home_states",
		Description: "Current state of Home Assistant entities, with area and last_changed (home's local time). " +
			"Filters combine (AND). Without any filter it returns only entity counts per domain and area. " +
			"Example: open doors = device_class=door,window,garage_door,opening state=on,open.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getStatesArgs) (*mcp.CallToolResult, any, error) {
		states, err := t.client.GetStates(ctx)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		home := t.client.HomeContext(ctx)

		f := stateFilter{
			domains:       splitList(args.Domain),
			entityIDs:     splitList(args.EntityID),
			deviceClasses: splitList(args.DeviceClass),
			states:        splitList(args.State),
			search:        strings.TrimSpace(args.Search),
			all:           args.All,
		}
		if args.Area != "" {
			f.area = home.areaID(args.Area)
		}
		if f.empty() && !args.All {
			out := stateIndex(home, states)
			out["note"] = "No filter given: counts only. Call again with search, domain, area, device_class or state."
			return mcputil.JSONResult(out)
		}

		kept, secondary := filterStates(home, states, f)
		out := map[string]any{"count": len(kept)}
		if args.Attributes {
			out["states"] = kept
		} else {
			out["states"] = slimStates(home, kept)
		}
		if secondary > 0 {
			out["omitted_diagnostic"] = secondary
		}
		if len(kept) == 0 {
			out["note"] = noMatchNote(home, f, secondary)
		}
		return mcputil.JSONResult(out)
	})
}

// noMatchNote tells the model why a state query came back empty, so it
// retries with a better filter instead of concluding the thing does not exist.
func noMatchNote(h *homeContext, f stateFilter, secondary int) string {
	switch {
	case secondary > 0:
		return "Only diagnostic/config entities matched; set all=true to see them."
	case f.area != "" && h.areaNames[f.area] == "" && h.registryErr == nil:
		names := make([]string, 0, len(h.areaNames))
		for _, n := range h.areaNames {
			names = append(names, n)
		}
		sort.Strings(names)
		return "Unknown area. Areas: " + strings.Join(names, ", ")
	default:
		return "Nothing matched. Try a shorter search, or no domain/device_class filter."
	}
}

// --- get_home_events ---

type getEventsArgs struct {
	Hours    int    `json:"hours,omitempty" jsonschema:"Hours back from now (default 1, max 168). Ignored when start is set."`
	Start    string `json:"start,omitempty" jsonschema:"Window start in the home's local time (2026-10-13 22:00) or RFC3339."`
	End      string `json:"end,omitempty" jsonschema:"Window end, same formats (default now)."`
	EntityID string `json:"entity_id,omitempty" jsonschema:"Entity id(s), comma-separated; listed in full."`
	Domain   string `json:"domain,omitempty" jsonschema:"Domain(s), comma-separated (e.g. automation, cover,lock, person)."`
	Area     string `json:"area,omitempty" jsonschema:"Area id or name."`
	Search   string `json:"search,omitempty" jsonschema:"Case-insensitive substring of entity_id, name, message or area."`
	All      bool   `json:"all,omitempty" jsonschema:"Include diagnostic/config entities (network trackers, firmware, buttons), dropped by default."`
}

func (t *Tools) registerGetEvents(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_home_events",
		Description: "Logbook: what happened and when (state changes, automations triggered and why), oldest first, local time. " +
			"Entities with many entries are folded into 'frequent' (count, first, last); pass their entity_id to list them. " +
			"Check omitted_* counts before concluding nothing happened.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getEventsArgs) (*mcp.CallToolResult, any, error) {
		home := t.client.HomeContext(ctx)
		start, end, err := resolveWindow(home, t.clock(), args.Hours, 168, args.Start, args.End, 1)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		ids := splitList(args.EntityID)
		entries, err := t.client.GetLogbook(ctx, start, end, ids)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		f := logbookFilter{domains: splitList(args.Domain), entityIDs: ids, search: strings.TrimSpace(args.Search), all: args.All}
		if args.Area != "" {
			f.area = home.areaID(args.Area)
		}
		out := shapeLogbook(home, entries, f)
		out["start"] = home.localTime(start)
		out["end"] = home.localTime(end)
		return mcputil.JSONResult(out)
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
		Annotations: mcputil.Destructive(),
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
		Annotations: mcputil.ReadOnly(),
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
		Annotations: mcputil.Destructive(),
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
			if err := wsClient.Dial(ctx); err != nil {
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
			if ok, res := mcputil.Confirm(req, "Delete automation "+args.ID); !ok {
				return res, nil, nil
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
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getAutomationTracesArgs) (*mcp.CallToolResult, any, error) {
		if args.AutomationID == "" {
			return mcputil.TextResult("Error: automation_id is required"), nil, nil
		}

		wsClient := t.client.NewWebsocketClient()
		if err := wsClient.Dial(ctx); err != nil {
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
		Annotations: mcputil.Destructive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args manageHelpersArgs) (*mcp.CallToolResult, any, error) {
		if args.HelperType == "" {
			return mcputil.TextResult("Error: helper_type is required"), nil, nil
		}
		if !IsHelperType(args.HelperType) {
			return mcputil.TextResult(fmt.Sprintf("Error: unsupported helper_type %q (use one of: %s)", args.HelperType, strings.Join(HelperTypes, ", "))), nil, nil
		}

		wsClient := t.client.NewWebsocketClient()
		if err := wsClient.Dial(ctx); err != nil {
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
		Annotations: mcputil.Destructive(),
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
			if err := wsClient.Dial(ctx); err != nil {
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
		Annotations: mcputil.Destructive(),
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
			if err := wsClient.Dial(ctx); err != nil {
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
	Kind   string `json:"kind,omitempty" jsonschema:"Registry to fetch: areas, devices, entities, labels, floors, or all (default)."`
	AreaID string `json:"area_id,omitempty" jsonschema:"Only areas, devices and entities in this area (id or name; entities inherit their device's area)."`
	Domain string `json:"domain,omitempty" jsonschema:"Only entities in these domain(s), comma-separated (e.g. light,switch)."`
	All    bool   `json:"all,omitempty" jsonschema:"Include disabled devices/entities and diagnostic/config entities, left out by default."`
	Full   bool   `json:"full,omitempty" jsonschema:"Return the raw registry records (ids, timestamps, options, config entries). Default returns a compact projection."`
}

func (t *Tools) registerGetHomeRegistry(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_home_registry",
		Description: "Home Assistant topology: areas, floors, labels, and which devices and entities are in each area. " +
			"For current states by room use get_home_states area=... instead. Without area_id or domain, kind=all returns per-area counts rather than every device and entity.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getHomeRegistryArgs) (*mcp.CallToolResult, any, error) {
		kind := strings.ToLower(strings.TrimSpace(args.Kind))
		if kind == "" {
			kind = "all"
		}
		switch kind {
		case "all", "areas", "devices", "entities", "labels", "floors":
		default:
			return mcputil.Errorf("unknown kind %q (use areas, devices, entities, labels, floors, all)", args.Kind), nil, nil
		}
		domains := splitList(args.Domain)

		wsClient := t.client.NewWebsocketClient()
		if err := wsClient.Dial(ctx); err != nil {
			return mcputil.Errorf("connecting: %v", err), nil, nil
		}
		defer func() { _ = wsClient.Close() }()

		result := map[string]any{}
		want := func(k string) bool { return kind == "all" || kind == k }
		// An overview: the unfiltered compact kind=all, which would otherwise
		// list every device and entity in the house.
		overview := kind == "all" && !args.Full && args.AreaID == "" && len(domains) == 0

		areas, err := wsClient.ListAreas()
		if err != nil {
			return mcputil.Errorf("areas: %v", err), nil, nil
		}
		areaID := resolveAreaArg(areas, args.AreaID)
		if want("areas") {
			items := areas
			if !args.Full {
				items = slimAreas(items)
			}
			result["areas"] = filterByArea(items, areaID)
		}
		var devices []map[string]any
		if want("devices") || (want("entities") && !args.Full) {
			if devices, err = wsClient.ListDevices(); err != nil {
				return mcputil.Errorf("devices: %v", err), nil, nil
			}
		}
		omitted := 0
		if want("devices") {
			items := devices
			if !args.Full {
				items = slimDevices(items)
				if !args.All {
					var n int
					items, n = dropRecords(items, "disabled_by")
					omitted += n
				}
			}
			items = filterByArea(items, areaID)
			if overview {
				result["devices_per_area"] = countByArea(items)
			} else {
				result["devices"] = items
			}
		}
		if want("entities") {
			items, err := wsClient.ListEntityRegistry()
			if err != nil {
				return mcputil.Errorf("entities: %v", err), nil, nil
			}
			if !args.Full {
				items = slimEntities(items, devices)
				if !args.All {
					var n int
					items, n = dropRecords(items, "disabled_by", "entity_category", "hidden_by")
					omitted += n
				}
			}
			items = filterEntitiesByDomain(filterByArea(items, areaID), domains)
			if overview {
				result["entities_per_area"] = countByArea(items)
			} else {
				result["entities"] = items
			}
		}
		if want("labels") {
			items, err := wsClient.ListLabels()
			if err != nil {
				return mcputil.Errorf("labels: %v", err), nil, nil
			}
			if !args.Full {
				items = slimLabels(items)
			}
			result["labels"] = items
		}
		if want("floors") {
			items, err := wsClient.ListFloors()
			if err != nil {
				return mcputil.Errorf("floors: %v", err), nil, nil
			}
			if !args.Full {
				items = slimFloors(items)
			}
			result["floors"] = items
		}
		if kind != "all" {
			result["count"] = len(result[kind].([]map[string]any))
		}
		if omitted > 0 {
			result["omitted_disabled_or_diagnostic"] = omitted
		}
		if overview {
			result["note"] = "Counts only; pass area_id, domain or kind=devices/entities for the records."
		}
		return mcputil.JSONResult(result)
	})
}

// --- list_home_services ---

type listHomeServicesArgs struct {
	Domain  string `json:"domain,omitempty" jsonschema:"Domain to describe in full (e.g. light, climate, vacuum), with every service's fields and target selector. Omit for a names-only index of all domains."`
	Service string `json:"service,omitempty" jsonschema:"With domain: describe only this service (e.g. turn_on)."`
}

func (t *Tools) registerListHomeServices(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_home_services",
		Description: "Discover Home Assistant services before call_home_service. Without domain it returns just domain → service names; pass domain (and optionally service) for the field schemas.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args listHomeServicesArgs) (*mcp.CallToolResult, any, error) {
		services, err := t.client.GetServices(ctx)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		if args.Domain == "" {
			names := serviceNames(services)
			return mcputil.JSONResult(map[string]any{
				"domains": names,
				"count":   len(names),
			})
		}

		for _, s := range services {
			if d, _ := s["domain"].(string); d != args.Domain {
				continue
			}
			svcs, _ := s["services"].(map[string]any)
			if args.Service != "" {
				svc, ok := svcs[args.Service]
				if !ok {
					return mcputil.Errorf("no service %s.%s", args.Domain, args.Service), nil, nil
				}
				return mcputil.JSONResult(map[string]any{"domain": args.Domain, "service": args.Service, "definition": svc})
			}
			return mcputil.JSONResult(map[string]any{"domain": args.Domain, "services": svcs, "count": len(svcs)})
		}
		return mcputil.Errorf("no services in domain %q", args.Domain), nil, nil
	})
}

// --- get_state_history ---

type getStateHistoryArgs struct {
	EntityID string `json:"entity_id" jsonschema:"Entity id(s), comma-separated (e.g. sensor.living_room_temperature,light.porch)."`
	Hours    int    `json:"hours,omitempty" jsonschema:"Hours back from now (default 24, max 240). Ignored when start is set."`
	Start    string `json:"start,omitempty" jsonschema:"Window start in the home's local time (2026-10-13 22:00) or RFC3339. Use for 'last night' or 'yesterday'."`
	End      string `json:"end,omitempty" jsonschema:"Window end, same formats as start (default now)."`
	Raw      bool   `json:"raw,omitempty" jsonschema:"Return every recorded sample instead of the summary. Large; only when the summary is not enough."`
}

// maxHistoryHours is the recorder's default retention (10 days).
const maxHistoryHours = 240

// resolveWindow turns hours/start/end arguments into a time window ending no
// later than now.
func resolveWindow(h *homeContext, now time.Time, hours, maxHours int, startArg, endArg string, defHours int) (time.Time, time.Time, error) {
	end := now
	if endArg != "" {
		t, ok := h.parseUserTime(endArg)
		if !ok {
			return time.Time{}, time.Time{}, fmt.Errorf("cannot parse end %q (use 2026-10-13 22:00 or RFC3339)", endArg)
		}
		end = t
	}
	if hours <= 0 {
		hours = defHours
	}
	if hours > maxHours {
		hours = maxHours
	}
	start := end.Add(-time.Duration(hours) * time.Hour)
	if startArg != "" {
		t, ok := h.parseUserTime(startArg)
		if !ok {
			return time.Time{}, time.Time{}, fmt.Errorf("cannot parse start %q (use 2026-10-13 22:00 or RFC3339)", startArg)
		}
		start = t
	}
	if !start.Before(end) {
		return time.Time{}, time.Time{}, fmt.Errorf("start must be before end")
	}
	if end.Sub(start) > time.Duration(maxHours)*time.Hour {
		start = end.Add(-time.Duration(maxHours) * time.Hour)
	}
	return start, end, nil
}

func (t *Tools) registerGetStateHistory(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_state_history",
		Description: "How entities changed over a time window. Numeric sensors come back as time-weighted min/mean/max " +
			"(per bucket when long); on/off and other states as transitions plus time_in_state. Times are the home's local time. " +
			"For energy totals or windows beyond 10 days use get_long_term_statistics.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getStateHistoryArgs) (*mcp.CallToolResult, any, error) {
		entityIDs := splitList(args.EntityID)
		if len(entityIDs) == 0 {
			return mcputil.Errorf("entity_id is required (find ids with get_home_states search=...)"), nil, nil
		}
		home := t.client.HomeContext(ctx)
		start, end, err := resolveWindow(home, t.clock(), args.Hours, maxHistoryHours, args.Start, args.End, 24)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		history, err := t.client.GetHistory(ctx, start, end, entityIDs, true, false)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		out := map[string]any{"start": home.localTime(start), "end": home.localTime(end)}
		if args.Raw {
			out["history"] = history
			return mcputil.JSONResult(out)
		}
		series := make([]map[string]any, 0, len(history))
		found := map[string]bool{}
		for _, s := range history {
			sum := summarizeSeries(home, s, start, end)
			found[str(sum, "entity_id")] = true
			series = append(series, sum)
		}
		out["series"] = series
		var missing []string
		for _, id := range entityIDs {
			if !found[id] {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			out["no_history"] = missing
		}
		return mcputil.JSONResult(out)
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
		Annotations: mcputil.ReadOnly(),
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
	Search       string `json:"search,omitempty" jsonschema:"Find statistics by substring of entity id, name or area (e.g. energy, power, temperature) and rank them by window total. Use instead of statistic_ids when you do not know the ids."`
	StatisticIDs string `json:"statistic_ids,omitempty" jsonschema:"Statistic ids (usually sensor entity ids), comma-separated; returns per-period rows."`
	Hours        int    `json:"hours,omitempty" jsonschema:"Hours back from now (default 168, max 8760). Ignored when start is set."`
	Start        string `json:"start,omitempty" jsonschema:"Window start in the home's local time (2026-10-01 or 2026-10-01 00:00) or RFC3339."`
	End          string `json:"end,omitempty" jsonschema:"Window end, same formats (default now)."`
	Period       string `json:"period,omitempty" jsonschema:"Row bucket: 5minute, hour, day, week, month. Default fits the window (hour up to 2 days, day up to 2 months)."`
	Raw          bool   `json:"raw,omitempty" jsonschema:"Return the recorder's raw rows (epoch-ms times, sum, state, last_reset)."`
}

func (t *Tools) registerGetLongTermStatistics(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_long_term_statistics",
		Description: "Long-term statistics (kept indefinitely) for sensors with a state_class: energy, water, gas, power, temperature. " +
			"Each statistic gets its window total_change (counters such as kWh) or mean/min/max (measurements). " +
			"For 'what used the most energy this week' pass search=energy: results are ranked by total_change. Times are local.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getLongTermStatisticsArgs) (*mcp.CallToolResult, any, error) {
		ids := splitList(args.StatisticIDs)
		search := strings.TrimSpace(args.Search)
		if len(ids) == 0 && search == "" {
			return mcputil.Errorf("pass search (e.g. energy) or statistic_ids"), nil, nil
		}
		home := t.client.HomeContext(ctx)
		start, end, err := resolveWindow(home, t.clock(), args.Hours, 8760, args.Start, args.End, 168)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		period := strings.ToLower(strings.TrimSpace(args.Period))
		if period == "" {
			period = defaultPeriod(end.Sub(start))
		}
		if !slices.Contains(statisticPeriods, period) {
			return mcputil.Errorf("unknown period %q (use %s)", args.Period, strings.Join(statisticPeriods, ", ")), nil, nil
		}

		states, err := t.client.GetStates(ctx)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		if search != "" && len(ids) == 0 {
			ids = statisticCandidates(home, states, search)
			if len(ids) == 0 {
				return mcputil.Errorf("no sensor with long-term statistics matches %q; try a broader word (energy, power, temperature)", search), nil, nil
			}
			if len(ids) > maxStatisticsSearch {
				return mcputil.Errorf("%d statistics match %q; narrow the search (e.g. energy_today, or an area name)", len(ids), search), nil, nil
			}
		}

		wsClient := t.client.NewWebsocketClient()
		if err := wsClient.Dial(ctx); err != nil {
			return mcputil.Errorf("connecting: %v", err), nil, nil
		}
		defer func() { _ = wsClient.Close() }()

		var types []string
		if !args.Raw {
			types = []string{"change", "mean", "min", "max"}
		}
		stats, err := wsClient.StatisticsDuringPeriod(start, end, ids, period, types...)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		out := map[string]any{"start": home.localTime(start), "end": home.localTime(end), "period": period}
		if args.Raw {
			out["statistics"] = stats
			return mcputil.JSONResult(out)
		}
		// A search ranks totals; the per-period rows are for named ids.
		named := len(splitList(args.StatisticIDs)) > 0
		shaped := shapeStatistics(home, stats, states, named)
		if !named {
			// Idle counters (a disconnected circuit) only pad a ranking.
			kept := shaped[:0]
			idle := 0
			for _, s := range shaped {
				if v, ok := s["total_change"].(float64); ok && v == 0 {
					idle++
					continue
				}
				kept = append(kept, s)
			}
			shaped = kept
			if idle > 0 {
				out["unchanged"] = idle
			}
		}
		out["statistics"] = shaped
		var missing []string
		for _, id := range ids {
			if _, ok := stats[id]; !ok {
				missing = append(missing, id)
			}
		}
		if len(missing) > 0 {
			out["no_statistics"] = missing
		}
		return mcputil.JSONResult(out)
	})
}

// --- get_calendar_events ---

type getCalendarEventsArgs struct {
	EntityID string `json:"entity_id,omitempty" jsonschema:"Calendar entity id(s), comma-separated (e.g. calendar.family). Omit for every calendar."`
	Hours    int    `json:"hours,omitempty" jsonschema:"Hours ahead of start to cover (default 168, max 720)."`
	Start    string `json:"start,omitempty" jsonschema:"Window start in the home's local time (2026-10-13 or 2026-10-13 08:00) or RFC3339; default now."`
}

// maxCalendarEvents caps the merged event list.
const maxCalendarEvents = 100

func (t *Tools) registerGetCalendarEvents(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_calendar_events",
		Description: "Upcoming events from Home Assistant calendars, merged and sorted by start (local time). Omit entity_id to read every calendar.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getCalendarEventsArgs) (*mcp.CallToolResult, any, error) {
		home := t.client.HomeContext(ctx)
		hours := args.Hours
		if hours <= 0 {
			hours = 168
		}
		if hours > 720 {
			hours = 720
		}
		start := t.clock()
		if args.Start != "" {
			s, ok := home.parseUserTime(args.Start)
			if !ok {
				return mcputil.Errorf("cannot parse start %q (use 2026-10-13 08:00 or RFC3339)", args.Start), nil, nil
			}
			start = s
		}
		end := start.Add(time.Duration(hours) * time.Hour)

		ids := splitList(args.EntityID)
		names := map[string]string{}
		if len(ids) == 0 {
			calendars, err := t.client.GetCalendars(ctx)
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
				// /api/calendars exists only once the calendar integration is
				// loaded, i.e. when some integration provides a calendar.
				return mcputil.JSONResult(map[string]any{"events": []any{}, "count": 0,
					"note": "No calendar integration is set up in Home Assistant, so there are no calendars to read."})
			}
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			for _, c := range calendars {
				ids = append(ids, c.EntityID)
				names[c.EntityID] = c.Name
			}
			if len(ids) == 0 {
				return mcputil.JSONResult(map[string]any{"events": []any{}, "count": 0, "note": "Home Assistant has no calendar entities."})
			}
		}

		type row struct {
			at  time.Time
			rec map[string]any
		}
		var rows []row
		var errs []string
		for _, id := range ids {
			events, err := t.client.GetCalendarEvents(ctx, id, start, end)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			for _, e := range events {
				r := calendarRow(home, e)
				if len(ids) > 1 {
					r["calendar"] = id
				}
				rows = append(rows, row{at: calendarTime(home, e.Start), rec: r})
			}
		}
		if len(rows) == 0 && len(errs) > 0 {
			return mcputil.Errorf("%s", strings.Join(errs, "; ")), nil, nil
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].at.Before(rows[j].at) })
		out := map[string]any{"start": home.localTime(start), "end": home.localTime(end), "count": len(rows)}
		if len(rows) > maxCalendarEvents {
			out["omitted_later"] = len(rows) - maxCalendarEvents
			rows = rows[:maxCalendarEvents]
		}
		list := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			list = append(list, r.rec)
		}
		out["events"] = list
		if len(names) > 1 {
			out["calendars"] = names
		}
		if len(errs) > 0 {
			out["errors"] = errs
		}
		return mcputil.JSONResult(out)
	})
}

// calendarTime reads an event boundary: {"dateTime": RFC3339} for timed
// events, {"date": "2006-01-02"} for all-day ones (local midnight).
func calendarTime(h *homeContext, m map[string]any) time.Time {
	if s, _ := m["dateTime"].(string); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t
		}
	}
	if s, _ := m["date"].(string); s != "" {
		if t, err := time.ParseInLocation("2006-01-02", s, h.loc); err == nil {
			return t
		}
	}
	return time.Time{}
}

// calendarRow flattens an event to local start/end strings; an all-day event
// keeps its dates and says so.
func calendarRow(h *homeContext, e CalendarEvent) map[string]any {
	r := map[string]any{"summary": e.Summary}
	if d, _ := e.Start["date"].(string); d != "" {
		r["all_day"] = true
		r["start"] = d
		if ed, _ := e.End["date"].(string); ed != "" {
			r["end"] = ed
		}
	} else {
		r["start"] = h.localTime(calendarTime(h, e.Start))
		r["end"] = h.localTime(calendarTime(h, e.End))
	}
	if e.Location != "" {
		r["location"] = e.Location
	}
	if e.Description != "" {
		d := e.Description
		if len(d) > 300 {
			d = d[:300] + "..."
		}
		r["description"] = d
	}
	return r
}
