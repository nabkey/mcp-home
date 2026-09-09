package hass

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
)

// --- manage_entity_state ---

type manageEntityStateArgs struct {
	Action     string         `json:"action" jsonschema:"set to create/overwrite an entity's state, or delete to remove the entity from the state machine."`
	EntityID   string         `json:"entity_id" jsonschema:"Entity ID to write, e.g. sensor.rainfall_total."`
	State      string         `json:"state,omitempty" jsonschema:"New state value. Required for action=set."`
	Attributes map[string]any `json:"attributes,omitempty" jsonschema:"Optional attributes to store alongside the state (unit_of_measurement, friendly_name, device_class, ...)."`
}

func (t *Tools) registerManageEntityState(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "manage_entity_state",
		Description: "Write or remove an entity's state directly in Home Assistant's state machine. " +
			"This does NOT control a device: it is for virtual entities fed by an external source. " +
			"To actually turn something on or off use home_turn_on/home_turn_off or call_home_service. " +
			"An entity owned by an integration will be restored on its next update.",
		Annotations: mcputil.Destructive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args manageEntityStateArgs) (*mcp.CallToolResult, any, error) {
		switch strings.ToLower(strings.TrimSpace(args.Action)) {
		case "set":
			if args.State == "" {
				return mcputil.Errorf("state is required for action=set"), nil, nil
			}
			result, err := t.client.SetState(ctx, args.EntityID, args.State, args.Attributes)
			if err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.JSONResult(result)
		case "delete":
			if err := t.client.DeleteState(ctx, args.EntityID); err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			return mcputil.TextResult("Deleted " + args.EntityID + " from the state machine."), nil, nil
		default:
			return mcputil.Errorf("unknown action %q (use set or delete)", args.Action), nil, nil
		}
	})
}

// --- fire_home_event ---

type fireHomeEventArgs struct {
	EventType string         `json:"event_type" jsonschema:"Event type to fire, e.g. my_custom_event. Automations can trigger on this."`
	Data      map[string]any `json:"data,omitempty" jsonschema:"Optional event data payload."`
}

func (t *Tools) registerFireHomeEvent(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "fire_home_event",
		Description: "Fire an event on the Home Assistant event bus. Useful for triggering automations that listen " +
			"for a custom event type. Use get_home_events to see what has fired recently.",
		Annotations: mcputil.Additive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args fireHomeEventArgs) (*mcp.CallToolResult, any, error) {
		msg, err := t.client.FireEvent(ctx, args.EventType, args.Data)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		if msg == "" {
			msg = "Fired " + args.EventType + "."
		}
		return mcputil.TextResult(msg), nil, nil
	})
}

// --- get_home_config ---

type getHomeConfigArgs struct {
	Kind string `json:"kind,omitempty" jsonschema:"What to return: core (version, unit system, time zone, location), components (loaded integrations), check (validate configuration.yaml), or all. Defaults to core."`
}

func (t *Tools) registerGetHomeConfig(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_home_config",
		Description: "Inspect the Home Assistant installation: core config (version, unit system, time zone, latitude/longitude, config dir), " +
			"the list of loaded components, and a configuration.yaml validation check. Use kind=check before reloading after a config edit.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getHomeConfigArgs) (*mcp.CallToolResult, any, error) {
		kind := strings.ToLower(strings.TrimSpace(args.Kind))
		if kind == "" {
			kind = "core"
		}
		if kind != "core" && kind != "components" && kind != "check" && kind != "all" {
			return mcputil.Errorf("unknown kind %q (use core, components, check, or all)", args.Kind), nil, nil
		}

		out := map[string]any{}
		var errs []string

		if kind == "core" || kind == "all" {
			config, err := t.client.GetConfig(ctx)
			if err != nil {
				errs = append(errs, err.Error())
			} else {
				out["core"] = config
			}
		}
		if kind == "components" || kind == "all" {
			components, err := t.client.GetComponents(ctx)
			if err != nil {
				errs = append(errs, err.Error())
			} else {
				out["components"] = components
			}
		}
		if kind == "check" || kind == "all" {
			check, err := t.client.CheckConfig(ctx)
			if err != nil {
				errs = append(errs, err.Error())
			} else {
				out["check_config"] = check
			}
		}

		// A single-kind failure is a hard error; a partial failure under
		// kind=all still returns what did work.
		if len(out) == 0 {
			return mcputil.Errorf("%s", strings.Join(errs, "; ")), nil, nil
		}
		if len(errs) > 0 {
			out["errors"] = errs
		}
		return mcputil.JSONResult(out)
	})
}
