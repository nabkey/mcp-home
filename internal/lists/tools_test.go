package lists

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/hass"
)

const listStates = `[
	{"entity_id":"todo.shopping_list","state":"0","attributes":{"friendly_name":"Shopping list"}},
	{"entity_id":"todo.costco","state":"1","attributes":{"friendly_name":"Costco"}},
	{"entity_id":"todo.ivys_chores","state":"2","attributes":{"friendly_name":"Ivy’s Chores"}},
	{"entity_id":"light.kitchen","state":"on","attributes":{"friendly_name":"Kitchen"}}
]`

func connect(t *testing.T) (*mcp.ClientSession, *[]string, *[]map[string]any) {
	t.Helper()
	var paths []string
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/states":
			_, _ = w.Write([]byte(listStates))
		default:
			paths = append(paths, r.URL.Path)
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			bodies = append(bodies, body)
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)

	client, err := hass.NewClient(srv.URL, "t")
	if err != nil {
		t.Fatal(err)
	}
	tools, err := NewTools(client)
	if err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tools.Register(server)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, &paths, &bodies
}

// "Add milk to the shopping list" names the list; the tool must not need a
// get_lists call first to learn todo.shopping_list.
func TestModifyListItemAcceptsAListName(t *testing.T) {
	cases := map[string]string{
		"todo.costco":       "todo.costco",
		"Shopping list":     "todo.shopping_list",
		"the shopping list": "todo.shopping_list",
		"shopping":          "todo.shopping_list",
		"costco":            "todo.costco",
		"Ivy's chores":      "todo.ivys_chores", // straight apostrophe for a curly one
	}
	for ref, want := range cases {
		session, paths, bodies := connect(t)
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "modify_list_item",
			Arguments: map[string]any{"action": "add", "entity_id": ref, "item": "milk"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError {
			t.Errorf("%q: %v", ref, res.Content[0].(*mcp.TextContent).Text)
			continue
		}
		if len(*paths) != 1 || (*paths)[0] != "/api/services/todo/add_item" {
			t.Errorf("%q: calls = %v", ref, *paths)
			continue
		}
		if got := (*bodies)[0]["entity_id"]; got != want {
			t.Errorf("%q resolved to %v, want %s", ref, got, want)
		}
	}
}

// An unknown name is an error that lists what exists, and never calls a
// service on a guessed entity.
func TestModifyListItemUnknownListListsChoices(t *testing.T) {
	session, paths, _ := connect(t)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "modify_list_item",
		Arguments: map[string]any{"action": "add", "entity_id": "groceries", "item": "milk"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("expected an error")
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "Shopping list (todo.shopping_list)") {
		t.Errorf("error should list the lists: %q", text)
	}
	if len(*paths) != 0 {
		t.Errorf("called %v for an unknown list", *paths)
	}
}

func TestModifyListItemRejectsBadActionBeforeLookup(t *testing.T) {
	session, paths, _ := connect(t)
	res, _ := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "modify_list_item",
		Arguments: map[string]any{"action": "delete", "entity_id": "todo.costco", "item": "milk"},
	})
	if res == nil || !res.IsError {
		t.Error("expected an invalid action to be an error result")
	}
	if len(*paths) != 0 {
		t.Errorf("called %v", *paths)
	}
}
