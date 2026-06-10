package hass

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
	"github.com/nabkey/mcp-home/internal/validate"
)

// registrySpec describes how a registry kind maps onto WebSocket commands:
// its registry name, the id field used by update/delete, and the supported
// actions mapped to their WebSocket verb (entity "delete" maps to "remove").
type registrySpec struct {
	registry string
	idField  string
	verbs    map[string]string
}

var registrySpecs = map[string]registrySpec{
	"area":   {"area", "area_id", map[string]string{"create": "create", "update": "update", "delete": "delete"}},
	"label":  {"label", "label_id", map[string]string{"create": "create", "update": "update", "delete": "delete"}},
	"floor":  {"floor", "floor_id", map[string]string{"create": "create", "update": "update", "delete": "delete"}},
	"entity": {"entity", "entity_id", map[string]string{"update": "update", "delete": "remove"}},
	"device": {"device", "device_id", map[string]string{"update": "update"}},
}

// supportedActions returns the sorted action names for a spec, for error messages.
func (s registrySpec) supportedActions() string {
	actions := make([]string, 0, len(s.verbs))
	for a := range s.verbs {
		actions = append(actions, a)
	}
	sort.Strings(actions)
	return strings.Join(actions, ", ")
}

// --- manage_registry ---

type manageRegistryArgs struct {
	Kind   string         `json:"kind" jsonschema:"Registry to modify: area, entity, device, label, or floor. Use get_home_registry to read current values."`
	Action string         `json:"action" jsonschema:"Action: create, update, or delete. area/label/floor support all three; entity supports update and delete; device supports update only."`
	ID     string         `json:"id,omitempty" jsonschema:"Registry id for update/delete: area_id, label_id, floor_id, entity_id (e.g. light.kitchen), or device_id. Not used for create."`
	Fields map[string]any `json:"fields,omitempty" jsonschema:"Properties to set. area: name, floor_id, aliases, icon, labels, picture. entity: name, icon, area_id, labels, new_entity_id, hidden_by, disabled_by. device: name_by_user, area_id, labels, disabled_by. label: name, color, icon, description. floor: name, aliases, icon, level."`
}

func (t *Tools) registerManageRegistry(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "manage_registry",
		Description: "Create, update, or delete entries in the Home Assistant area/entity/device/label/floor registries. Use this to organize the home: assign entities to areas, rename, apply labels, place areas on floors. Reads are done with get_home_registry.",
		Annotations: mcputil.Destructive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args manageRegistryArgs) (*mcp.CallToolResult, any, error) {
		kind := strings.ToLower(strings.TrimSpace(args.Kind))
		spec, ok := registrySpecs[kind]
		if !ok {
			return mcputil.TextResult(fmt.Sprintf("Error: unknown kind %q (use area, entity, device, label, floor)", args.Kind)), nil, nil
		}
		verb, ok := spec.verbs[args.Action]
		if !ok {
			return mcputil.TextResult(fmt.Sprintf("Error: %s registry does not support action %q (supported: %s)", kind, args.Action, spec.supportedActions())), nil, nil
		}

		fields := make(map[string]any, len(args.Fields)+1)
		for k, v := range args.Fields {
			fields[k] = v
		}

		if args.Action == "create" {
			if len(fields) == 0 {
				return mcputil.TextResult("Error: fields is required for create (e.g. name)"), nil, nil
			}
		} else {
			if args.ID == "" {
				return mcputil.TextResult(fmt.Sprintf("Error: id (%s) is required for %s", spec.idField, args.Action)), nil, nil
			}
			if err := validate.Identifier("id", args.ID); err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			fields[spec.idField] = args.ID
		}

		wsClient := t.client.NewWebsocketClient()
		if err := wsClient.Dial(ctx); err != nil {
			return mcputil.Errorf("connecting: %v", err), nil, nil
		}
		defer func() { _ = wsClient.Close() }()

		result, err := wsClient.RegistryCommand(spec.registry, verb, fields)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		status := args.Action + "d" // created, updated, deleted
		out := map[string]any{"status": status}
		if len(result) > 0 {
			out["result"] = result
		}
		return mcputil.JSONResult(out)
	})
}
