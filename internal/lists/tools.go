// Package lists provides MCP tools for managing to-do lists via Home Assistant.
package lists

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/hass"
	"github.com/nabkey/mcp-home/internal/mcputil"
)

// Tools holds the list tools and their configuration.
type Tools struct {
	client *hass.Client
}

// NewTools creates a new Tools instance.
func NewTools(client *hass.Client) (*Tools, error) {
	if client == nil {
		return nil, fmt.Errorf("home assistant client is required")
	}
	return &Tools{client: client}, nil
}

// Register adds all list management tools to the given MCP server.
func (t *Tools) Register(server *mcp.Server) {
	t.registerGetLists(server)
	t.registerGetListItems(server)
	t.registerModifyListItem(server)
}

// --- get_lists ---

type getListsArgs struct{}

func (t *Tools) registerGetLists(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_lists",
		Description: "Retrieve all available to-do lists (e.g., shopping lists, task lists).",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getListsArgs) (*mcp.CallToolResult, any, error) {
		lists, err := t.client.GetTodoLists(ctx)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		type listInfo struct {
			EntityID string `json:"entity_id"`
			Name     string `json:"name"`
		}

		result := make([]listInfo, 0, len(lists))
		for _, l := range lists {
			name := l.EntityID
			if friendlyName, ok := l.Attributes["friendly_name"].(string); ok {
				name = friendlyName
			}
			result = append(result, listInfo{
				EntityID: l.EntityID,
				Name:     name,
			})
		}

		return mcputil.JSONResult(map[string]any{"lists": result})
	})
}

// resolveList turns what a caller said into a todo entity id. "Add milk to
// the shopping list" names the list, not todo.shopping_list; requiring the
// id cost a get_lists round trip on every list request. An id passes
// through; a name is matched against the lists' friendly names and object
// ids, ignoring case, typographic apostrophes and a leading "the".
func (t *Tools) resolveList(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", fmt.Errorf("entity_id is required (a todo entity id or the list's name)")
	}
	if strings.HasPrefix(ref, "todo.") {
		return ref, nil
	}
	lists, err := t.client.GetTodoLists(ctx)
	if err != nil {
		return "", err
	}
	want := normalizeListName(ref)
	var matches, names []string
	for _, l := range lists {
		name, _ := l.Attributes["friendly_name"].(string)
		names = append(names, fmt.Sprintf("%s (%s)", name, l.EntityID))
		objectID := strings.TrimPrefix(l.EntityID, "todo.")
		if normalizeListName(name) == want || normalizeListName(objectID) == want ||
			normalizeListName(strings.ReplaceAll(objectID, "_", " ")) == want {
			matches = append(matches, l.EntityID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no to-do list called %q; lists: %s", ref, strings.Join(names, ", "))
	}
	return "", fmt.Errorf("%q matches several lists (%s); pass the entity id", ref, strings.Join(matches, ", "))
}

func normalizeListName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("’", "'", "‘", "'", "`", "'").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimPrefix(s, "the ")
	return strings.TrimSuffix(s, " list")
}

// --- get_list_items ---

type getListItemsArgs struct {
	EntityID string `json:"entity_id" jsonschema:"The list: its entity ID (e.g. todo.shopping_list) or its name (e.g. Shopping list)"`
}

func (t *Tools) registerGetListItems(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_list_items",
		Description: "Retrieve all items in a specific to-do list.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getListItemsArgs) (*mcp.CallToolResult, any, error) {
		entityID, err := t.resolveList(ctx, args.EntityID)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		items, err := t.client.GetTodoItems(ctx, entityID)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return mcputil.JSONResult(map[string]any{"items": items})
	})
}

// --- modify_list_item ---

type modifyListItemArgs struct {
	Action   string `json:"action" jsonschema:"Action: add remove complete or incomplete"`
	EntityID string `json:"entity_id" jsonschema:"The list: its entity ID (e.g. todo.shopping_list) or its name (e.g. Shopping list)"`
	Item     string `json:"item" jsonschema:"The name or summary of the item"`
}

func (t *Tools) registerModifyListItem(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "modify_list_item",
		Description: "Add, remove, or update items in a specific to-do list.",
		Annotations: mcputil.Destructive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args modifyListItemArgs) (*mcp.CallToolResult, any, error) {
		if args.Item == "" {
			return mcputil.Errorf("item is required"), nil, nil
		}
		switch args.Action {
		case "add", "remove", "complete", "incomplete":
		default:
			return mcputil.Errorf("invalid action '%s' (use add, remove, complete, incomplete)", args.Action), nil, nil
		}
		entityID, err := t.resolveList(ctx, args.EntityID)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		var service string
		data := map[string]any{
			"entity_id": entityID,
		}

		switch args.Action {
		case "add":
			service = "add_item"
			data["item"] = args.Item
		case "remove":
			service = "remove_item"
			data["item"] = args.Item
		case "complete":
			service = "update_item"
			data["item"] = args.Item
			data["status"] = "completed"
		case "incomplete":
			service = "update_item"
			data["item"] = args.Item
			data["status"] = "needs_action"
		}

		if _, err := t.client.CallService(ctx, "todo", service, data); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return mcputil.JSONResult(map[string]any{"status": "success", "list": entityID})
	})
}
