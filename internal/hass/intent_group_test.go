package hass

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mediaFake() *fakeHA {
	return &fakeHA{
		states:   `[{"entity_id":"media_player.kitchen","state":"playing"}]`,
		services: `[]`,
	}
}

// The media intents register as one home_media tool, never as their own.
func TestMediaIntentsFoldIntoOneTool(t *testing.T) {
	tools := toolNames(t, connect(t, mediaFake()))
	tool, ok := tools["home_media"]
	if !ok {
		t.Fatal("home_media not registered for an instance with a media_player")
	}
	for name := range tools {
		if strings.HasPrefix(name, "home_media_") || strings.HasPrefix(name, "home_set_volume") {
			t.Errorf("%s registered on its own; it should only be a home_media action", name)
		}
	}

	got := schemaJSON(t, tool.InputSchema)
	props, _ := got["properties"].(map[string]any)
	action, _ := props["action"].(map[string]any)
	var enum []string
	for _, v := range action["enum"].([]any) {
		enum = append(enum, v.(string))
	}
	sort.Strings(enum)
	want := []string{"mute", "next", "pause", "previous", "search_and_play", "set_volume", "set_volume_relative", "unmute", "unpause"}
	if !reflect.DeepEqual(enum, want) {
		t.Errorf("actions = %v, want %v", enum, want)
	}
	if req, _ := got["required"].([]any); len(req) != 1 || req[0] != "action" {
		t.Errorf("required = %v, want [action]; per-action slots are checked at call time", got["required"])
	}
	// Targeting is shared, and slots from every action are present and say
	// which action needs them.
	for _, name := range []string{"name", "area", "floor", "domain", "volume_level", "volume_step", "search_query", "media_class"} {
		if _, ok := props[name]; !ok {
			t.Errorf("missing property %q", name)
		}
	}
	if anyOf, _ := got["anyOf"].([]any); len(anyOf) != 4 {
		t.Errorf("anyOf = %v, want the 4 target alternatives", got["anyOf"])
	}
	vol, _ := props["volume_level"].(map[string]any)
	if d, _ := vol["description"].(string); !strings.Contains(d, "required by set_volume") {
		t.Errorf("volume_level description = %q, want it to name set_volume", d)
	}
}

// An action dispatches its own intent with the targets and only its own slots.
func TestMediaGroupRoutesToTheActionsIntent(t *testing.T) {
	fake := mediaFake()
	session := connect(t, fake)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "home_media",
		Arguments: map[string]any{
			"action":       "set_volume",
			"name":         "kitchen",
			"volume_level": 30,
			"search_query": "stray slot from another action",
		},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error: %v", result.Content)
	}
	if fake.lastIntentBody["name"] != "HassSetVolume" {
		t.Errorf("intent = %v, want HassSetVolume", fake.lastIntentBody["name"])
	}
	data, _ := fake.lastIntentBody["data"].(map[string]any)
	want := map[string]any{"name": "kitchen", "volume_level": 30.0}
	if !reflect.DeepEqual(data, want) {
		t.Errorf("slots = %v, want %v (only the action's own slots)", data, want)
	}
}

func TestMediaGroupRejectsBadCalls(t *testing.T) {
	fake := mediaFake()
	session := connect(t, fake)
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing per-action slot", map[string]any{"action": "set_volume", "name": "kitchen"}, `requires "volume_level"`},
		{"unknown action", map[string]any{"action": "explode", "name": "kitchen"}, "invalid arguments"},
		{"no target", map[string]any{"action": "pause"}, "invalid arguments"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake.lastIntentBody = nil
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "home_media", Arguments: c.args})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if !result.IsError {
				t.Fatalf("expected an error result, got %v", result.Content)
			}
			text := result.Content[0].(*mcp.TextContent).Text
			if !strings.Contains(text, c.want) {
				t.Errorf("error = %q, want it to contain %q", text, c.want)
			}
			if fake.lastIntentBody != nil {
				t.Errorf("Home Assistant was called with %v; a rejected call must not reach it", fake.lastIntentBody)
			}
		})
	}
}

// A denied service removes just that action from the enum.
func TestMediaGroupDropsDeniedActions(t *testing.T) {
	session := connectWithDeny(t, mediaFake(), []string{"media_player.play_media", "media_player.volume_mute"})

	tool, ok := toolNames(t, session)["home_media"]
	if !ok {
		t.Fatal("home_media should survive with its remaining actions")
	}
	props := schemaJSON(t, tool.InputSchema)["properties"].(map[string]any)
	var enum []string
	for _, v := range props["action"].(map[string]any)["enum"].([]any) {
		enum = append(enum, v.(string))
	}
	sort.Strings(enum)
	want := []string{"next", "pause", "previous", "set_volume", "set_volume_relative", "unpause"}
	if !reflect.DeepEqual(enum, want) {
		t.Errorf("actions = %v, want %v (mute, unmute and search_and_play denied)", enum, want)
	}
}
