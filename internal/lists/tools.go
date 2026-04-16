// Package lists provides MCP tools for managing to-do lists via Home Assistant.
package lists

import (
	"context"
	"fmt"

	"github.com/nabkey/mcp-home/internal/hass"
	"github.com/nabkey/mcp-home/internal/mcputil"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// --- get_list_items ---

type getListItemsArgs struct {
	EntityID string `json:"entity_id" jsonschema:"The entity ID of the list (e.g. todo.shopping_list)"`
}

func (t *Tools) registerGetListItems(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_list_items",
		Description: "Retrieve all items in a specific to-do list.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getListItemsArgs) (*mcp.CallToolResult, any, error) {
		if args.EntityID == "" {
			return mcputil.TextResult("Error: entity_id is required"), nil, nil
		}

		items, err := t.client.GetTodoItems(ctx, args.EntityID)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return mcputil.JSONResult(map[string]any{"items": items})
	})
}

// --- modify_list_item ---

type modifyListItemArgs struct {
	Action   string `json:"action" jsonschema:"Action: add remove complete or incomplete"`
	EntityID string `json:"entity_id" jsonschema:"The entity ID of the list (e.g. todo.shopping_list)"`
	Item     string `json:"item" jsonschema:"The name or summary of the item"`
}

func (t *Tools) registerModifyListItem(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "modify_list_item",
		Description: "Add, remove, or update items in a specific to-do list.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args modifyListItemArgs) (*mcp.CallToolResult, any, error) {
		if args.EntityID == "" {
			return mcputil.TextResult("Error: entity_id is required"), nil, nil
		}
		if args.Item == "" {
			return mcputil.TextResult("Error: item is required"), nil, nil
		}

		var service string
		data := map[string]any{
			"entity_id": args.EntityID,
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
		default:
			return mcputil.TextResult(fmt.Sprintf("Error: invalid action '%s' (use add, remove, complete, incomplete)", args.Action)), nil, nil
		}

		_, err := t.client.CallService(ctx, "todo", service, data)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		return mcputil.JSONResult(map[string]any{"status": "success"})
	})
}
