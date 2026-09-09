package hass

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/jsonschema-go/jsonschema"
)

// Intent support mirrors Home Assistant's own LLM tool layer (the "assist" API
// in homeassistant/helpers/llm.py plus each integration's llm.py platform).
//
// Upstream deliberately does NOT generate one tool per service: their generic
// ActionTool carries a comment saying _get_cached_action_parameters "only works
// for services which add their description directly to the service description
// cache. This is not the case for most services, but it is for scripts." The
// curated intent set is what they expose instead, so it is what we port.
//
// Intents are cross-domain verbs: HassTurnOn covers light, switch, fan, lock
// and media_player in one tool. They also resolve friendly names, areas and
// floors server-side, so callers say "kitchen" instead of looking up an
// area_id first.
//
// Timer intents (HassStartTimer et al.) are intentionally absent. Upstream
// gates them on llm_context.device_id belonging to a voice satellite that
// supports timers; a REST caller has no such device, so those intents would
// always fail to match.

// IntentSlot is one argument of an intent.
type IntentSlot struct {
	Name        string
	Description string
	Schema      *jsonschema.Schema
	Required    bool
}

// IntentDef describes an intent exposed as an MCP tool.
type IntentDef struct {
	// ToolName is the MCP tool name.
	ToolName string
	// Intent is the Home Assistant intent type (e.g. HassTurnOn).
	Intent string
	// Description is the tool description, taken from upstream's handler.
	Description string
	// Domains lists the entity domains this intent can act on. The tool is
	// only registered when the instance has at least one entity in one of
	// them. An empty list means always register.
	Domains []string
	// Targeted adds the standard name/area/floor targeting slots, one of
	// which must be supplied.
	Targeted bool
	// Slots are intent-specific arguments beyond the targeting ones.
	Slots []IntentSlot
	// Services lists every domain.service this intent can dispatch inside
	// Home Assistant. HASS_DENY_SERVICES is enforced against these at
	// registration time, because the intent endpoint takes an intent name
	// rather than a service and so bypasses the check in CallService.
	Services []string
	// Destructive marks intents that change state in a hard-to-reverse way.
	Destructive bool
}

func strSchema(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Description: desc}
}

func pctSchema(desc string) *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "integer",
		Description: desc,
		Minimum:     ptrFloat(0),
		Maximum:     ptrFloat(100),
	}
}

// onOffDeviceClasses is HA's ONOFF_DEVICE_CLASSES, the device classes that
// HassTurnOn/HassTurnOff accept for narrowing a target.
var onOffDeviceClasses = []string{
	"awning", "blind", "curtain", "damper", "door", "garage", "gate",
	"shade", "shutter", "window", "outlet", "switch", "receiver", "speaker", "tv",
}

// coverValveDeviceClasses covers HassSetPosition and HassStopMoving.
var coverValveDeviceClasses = []string{
	"awning", "blind", "curtain", "damper", "door", "garage", "gate",
	"shade", "shutter", "window", "water", "gas",
}

// IntentCatalog is the set of intents we expose, ported from the integrations
// that ship an llm.py platform upstream.
var IntentCatalog = []IntentDef{
	{
		ToolName:    "home_turn_on",
		Intent:      "HassTurnOn",
		Services:    []string{"homeassistant.turn_on", "button.press", "input_button.press", "cover.open_cover", "lock.lock", "valve.open_valve"},
		Description: "Turns on, opens, presses, or starts a device or entity. For locks this locks. Works across light, switch, fan, cover, lock, media_player and more — prefer this over call_home_service for simple on/open commands.",
		Targeted:    true,
		Slots:       deviceClassSlot(onOffDeviceClasses),
	},
	{
		ToolName:    "home_turn_off",
		Intent:      "HassTurnOff",
		Services:    []string{"homeassistant.turn_off", "cover.close_cover", "lock.unlock", "valve.close_valve"},
		Description: "Turns off, closes, or stops a device or entity. For locks this unlocks. Works across light, switch, fan, cover, lock, media_player and more.",
		Targeted:    true,
		Slots:       deviceClassSlot(onOffDeviceClasses),
	},
	{
		ToolName:    "home_set_position",
		Intent:      "HassSetPosition",
		Services:    []string{"cover.set_cover_position", "valve.set_valve_position"},
		Description: "Sets the position of a cover or valve (0 is fully closed, 100 fully open).",
		Domains:     []string{"cover", "valve"},
		Targeted:    true,
		Slots: append(
			[]IntentSlot{{
				Name:     "position",
				Schema:   pctSchema("Target position percentage, 0 (closed) to 100 (open)."),
				Required: true,
			}},
			deviceClassSlot(coverValveDeviceClasses)...,
		),
	},
	{
		ToolName:    "home_stop_moving",
		Intent:      "HassStopMoving",
		Services:    []string{"cover.stop_cover", "valve.stop_valve"},
		Description: "Stops a moving cover or valve.",
		Domains:     []string{"cover", "valve"},
		Targeted:    true,
		Slots:       deviceClassSlot(coverValveDeviceClasses),
	},
	{
		ToolName:    "home_light_set",
		Intent:      "HassLightSet",
		Services:    []string{"light.turn_on"},
		Description: "Sets the brightness percentage or color of a light.",
		Domains:     []string{"light"},
		Targeted:    true,
		Slots: []IntentSlot{
			{Name: "brightness", Schema: pctSchema("The brightness percentage of the light between 0 and 100, where 0 is off and 100 is fully lit.")},
			{Name: "color", Schema: strSchema("A CSS color name, e.g. red, warmwhite, dodgerblue.")},
			{Name: "temperature", Schema: &jsonschema.Schema{Type: "integer", Description: "Color temperature in Kelvin.", Minimum: ptrFloat(0)}},
		},
	},
	{
		ToolName:    "home_climate_set_temperature",
		Intent:      "HassClimateSetTemperature",
		Services:    []string{"climate.set_temperature"},
		Description: "Sets the target temperature of a climate device (thermostat).",
		Domains:     []string{"climate"},
		Targeted:    true,
		Slots: []IntentSlot{
			{Name: "temperature", Schema: &jsonschema.Schema{Type: "number", Description: "Target temperature, in the system's configured unit."}, Required: true},
		},
	},
	{
		ToolName:    "home_fan_set_speed",
		Intent:      "HassFanSetSpeed",
		Services:    []string{"fan.turn_on"},
		Description: "Sets a fan's speed by percentage.",
		Domains:     []string{"fan"},
		Targeted:    true,
		Slots: []IntentSlot{
			{Name: "percentage", Schema: pctSchema("The speed percentage of the fan."), Required: true},
		},
	},
	{
		ToolName:    "home_media_pause",
		Intent:      "HassMediaPause",
		Services:    []string{"media_player.media_pause"},
		Description: "Pauses a media player.",
		Domains:     []string{"media_player"},
		Targeted:    true,
	},
	{
		ToolName:    "home_media_unpause",
		Intent:      "HassMediaUnpause",
		Services:    []string{"media_player.media_play"},
		Description: "Resumes a paused media player.",
		Domains:     []string{"media_player"},
		Targeted:    true,
	},
	{
		ToolName:    "home_media_next",
		Intent:      "HassMediaNext",
		Services:    []string{"media_player.media_next_track"},
		Description: "Skips a media player to the next item.",
		Domains:     []string{"media_player"},
		Targeted:    true,
	},
	{
		ToolName:    "home_media_previous",
		Intent:      "HassMediaPrevious",
		Services:    []string{"media_player.media_previous_track"},
		Description: "Replays the previous item on a media player.",
		Domains:     []string{"media_player"},
		Targeted:    true,
	},
	{
		ToolName:    "home_set_volume",
		Intent:      "HassSetVolume",
		Services:    []string{"media_player.volume_set"},
		Description: "Sets the volume percentage of a media player.",
		Domains:     []string{"media_player"},
		Targeted:    true,
		Slots: []IntentSlot{
			{Name: "volume_level", Schema: pctSchema("The volume percentage of the media player, 0 to 100."), Required: true},
		},
	},
	{
		ToolName:    "home_set_volume_relative",
		Intent:      "HassSetVolumeRelative",
		Services:    []string{"media_player.volume_set"},
		Description: "Increases or decreases the volume of a media player.",
		Domains:     []string{"media_player"},
		Targeted:    true,
		Slots: []IntentSlot{
			{
				Name:     "volume_step",
				Required: true,
				Schema: &jsonschema.Schema{
					Description: `Either the string "up" or "down", or a percentage change from -100 to 100.`,
					AnyOf: []*jsonschema.Schema{
						{Type: "string", Enum: []any{"up", "down"}},
						{Type: "integer", Minimum: ptrFloat(-100), Maximum: ptrFloat(100)},
					},
				},
			},
		},
	},
	{
		ToolName:    "home_media_player_mute",
		Intent:      "HassMediaPlayerMute",
		Services:    []string{"media_player.volume_mute"},
		Description: "Mutes a media player.",
		Domains:     []string{"media_player"},
		Targeted:    true,
	},
	{
		ToolName:    "home_media_player_unmute",
		Intent:      "HassMediaPlayerUnmute",
		Services:    []string{"media_player.volume_mute"},
		Description: "Unmutes a media player.",
		Domains:     []string{"media_player"},
		Targeted:    true,
	},
	{
		ToolName:    "home_media_search_and_play",
		Intent:      "HassMediaSearchAndPlay",
		Services:    []string{"media_player.play_media"},
		Description: "Searches for media and plays the first result on a media player.",
		Domains:     []string{"media_player"},
		Targeted:    true,
		Slots: []IntentSlot{
			{Name: "search_query", Schema: strSchema("What to search for, e.g. an artist, album or show name."), Required: true},
			{Name: "media_class", Schema: strSchema("Optional media class to narrow the search, e.g. album, artist, playlist, tv_show, movie.")},
		},
	},
	{
		ToolName:    "home_vacuum_start",
		Intent:      "HassVacuumStart",
		Services:    []string{"vacuum.start"},
		Description: "Starts a vacuum.",
		Domains:     []string{"vacuum"},
		Targeted:    true,
	},
	{
		ToolName:    "home_vacuum_return_to_base",
		Intent:      "HassVacuumReturnToBase",
		Services:    []string{"vacuum.return_to_base"},
		Description: "Sends a vacuum back to its base.",
		Domains:     []string{"vacuum"},
		Targeted:    true,
	},
	{
		ToolName:    "home_vacuum_clean_area",
		Intent:      "HassVacuumCleanArea",
		Services:    []string{"vacuum.clean_area"},
		Description: "Sends a vacuum to clean a specific area.",
		Domains:     []string{"vacuum"},
		Slots: []IntentSlot{
			{Name: "area", Schema: strSchema("Name of the area to clean."), Required: true},
			{Name: "name", Schema: strSchema("Name of the vacuum to use.")},
		},
	},
	{
		ToolName:    "home_humidifier_setpoint",
		Intent:      "HassHumidifierSetpoint",
		Services:    []string{"humidifier.set_humidity", "humidifier.turn_on"},
		Description: "Sets a humidifier's target humidity percentage.",
		Domains:     []string{"humidifier"},
		Slots: []IntentSlot{
			{Name: "name", Schema: strSchema("Name of the humidifier."), Required: true},
			{Name: "humidity", Schema: pctSchema("Target humidity percentage, 0 to 100."), Required: true},
		},
	},
	{
		ToolName:    "home_humidifier_mode",
		Intent:      "HassHumidifierMode",
		Services:    []string{"humidifier.set_mode", "humidifier.turn_on"},
		Description: "Sets a humidifier's mode.",
		Domains:     []string{"humidifier"},
		Slots: []IntentSlot{
			{Name: "name", Schema: strSchema("Name of the humidifier."), Required: true},
			{Name: "mode", Schema: strSchema("The mode to set, e.g. normal, eco, away."), Required: true},
		},
	},
	{
		ToolName:    "home_list_add_item",
		Intent:      "HassListAddItem",
		Services:    []string{"todo.add_item"},
		Description: "Adds an item to a to-do or shopping list.",
		Domains:     []string{"todo"},
		Slots: []IntentSlot{
			{Name: "item", Schema: strSchema("The item to add."), Required: true},
			{Name: "name", Schema: strSchema("Name of the list to add to."), Required: true},
		},
	},
	{
		ToolName:    "home_list_complete_item",
		Intent:      "HassListCompleteItem",
		Services:    []string{"todo.update_item"},
		Description: "Marks an item on a to-do or shopping list as completed.",
		Domains:     []string{"todo"},
		Slots: []IntentSlot{
			{Name: "item", Schema: strSchema("The item to complete."), Required: true},
			{Name: "name", Schema: strSchema("Name of the list holding the item."), Required: true},
		},
	},
	{
		ToolName:    "home_list_remove_item",
		Intent:      "HassListRemoveItem",
		Services:    []string{"todo.remove_item"},
		Description: "Removes an item from a to-do or shopping list.",
		Domains:     []string{"todo"},
		Destructive: true,
		Slots: []IntentSlot{
			{Name: "item", Schema: strSchema("The item to remove."), Required: true},
			{Name: "name", Schema: strSchema("Name of the list holding the item."), Required: true},
		},
	},
}

// deviceClassSlot builds the optional device_class narrowing slot that HA's
// DynamicServiceIntentHandler adds when a handler declares device classes.
func deviceClassSlot(classes []string) []IntentSlot {
	return []IntentSlot{{
		Name: "device_class",
		Schema: &jsonschema.Schema{
			Type:        "array",
			Description: "Optional device classes to narrow the target, e.g. door or window.",
			Items:       &jsonschema.Schema{Type: "string", Enum: enumOf(classes)},
		},
	}}
}

// InputSchema builds the JSON Schema for an intent's arguments.
func (d IntentDef) InputSchema() *jsonschema.Schema {
	props := map[string]*jsonschema.Schema{}
	var required []string

	if d.Targeted {
		props["name"] = strSchema("Name of a specific entity to target, e.g. \"kitchen sink light\". Resolved by Home Assistant against friendly names and aliases.")
		props["area"] = strSchema("Name of an area to target, e.g. \"kitchen\". Targets every matching entity in the area.")
		props["floor"] = strSchema("Name of a floor to target, e.g. \"upstairs\".")
		props["domain"] = &jsonschema.Schema{
			Type:        "array",
			Description: "Entity domains to target, e.g. [\"light\"]. Narrows a name/area/floor target, or on its own targets every entity in those domains (\"turn off all the lights\").",
			Items:       &jsonschema.Schema{Type: "string"},
		}
	}

	for _, slot := range d.Slots {
		s := slot.Schema
		if s == nil {
			s = strSchema(slot.Description)
		} else if slot.Description != "" && s.Description == "" {
			c := *s
			c.Description = slot.Description
			s = &c
		}
		props[slot.Name] = s
		if slot.Required {
			required = append(required, slot.Name)
		}
	}
	sort.Strings(required)

	schema := &jsonschema.Schema{
		Type:       "object",
		Properties: props,
		Required:   required,
	}

	// Require at least one targeting slot. This is deliberately stricter than
	// upstream: vol.Any("name", "area", "floor") in DynamicServiceIntentHandler
	// is a *key matcher*, not a requirement, and unmarked voluptuous keys are
	// optional, so Home Assistant accepts an intent with no target at all and
	// matches every entity in scope. For a voice assistant that is a
	// reasonable "turn off the lights" default; for a tool call it means one
	// under-specified argument list can act on the whole house.
	//
	// "domain" counts as a target, so a deliberate broad command still works
	// (domain: ["light"] to turn off every light) while a call carrying no
	// targeting information at all is rejected.
	if d.Targeted {
		schema.AnyOf = []*jsonschema.Schema{
			{Required: []string{"name"}},
			{Required: []string{"area"}},
			{Required: []string{"floor"}},
			{Required: []string{"domain"}},
		}
	}
	return schema
}

// IntentResult is the useful part of an intent response. IntentResponse.as_dict
// puts either "code" (on an error) or "success"/"failed" under "data", and
// nothing else, so those are the only fields worth lifting out.
type IntentResult struct {
	Speech  string           `json:"speech,omitempty"`
	Success []map[string]any `json:"success,omitempty"`
	Failed  []map[string]any `json:"failed,omitempty"`
	Raw     map[string]any   `json:"raw,omitempty"`
}

// HandleIntent fires an intent via POST /api/intent/handle. Home Assistant
// resolves name/area/floor slots to entities server-side.
func (c *Client) HandleIntent(ctx context.Context, name string, slots map[string]any) (*IntentResult, error) {
	payload := map[string]any{"name": name}
	if len(slots) > 0 {
		payload["data"] = slots
	}

	body, err := c.doRequest(ctx, "POST", "/api/intent/handle", nil, payload)
	if err != nil {
		return nil, fmt.Errorf("handling intent %s: %w", name, err)
	}

	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decoding intent response: %w", err)
	}

	result := &IntentResult{Raw: raw}
	if speech, ok := raw["speech"].(map[string]any); ok {
		if plain, ok := speech["plain"].(map[string]any); ok {
			result.Speech, _ = plain["speech"].(string)
		}
	}
	if data, ok := raw["data"].(map[string]any); ok {
		result.Success = mapSlice(data["success"])
		result.Failed = mapSlice(data["failed"])
		// An intent that matched nothing still returns HTTP 200 with an error
		// response type; surface that as an error so callers do not read a
		// no-op as success.
		if code, ok := data["code"].(string); ok {
			msg := result.Speech
			if msg == "" {
				msg = code
			}
			return result, fmt.Errorf("intent %s failed (%s): %s", name, code, msg)
		}
		// An action_done response carries no code even when every target
		// failed (an unavailable device, an unsupported feature). Reporting
		// that as success would tell the caller the action took effect.
		if len(result.Success) == 0 && len(result.Failed) > 0 {
			msg := result.Speech
			if msg == "" {
				msg = fmt.Sprintf("%d target(s) failed", len(result.Failed))
			}
			return result, fmt.Errorf("intent %s affected nothing: %s", name, msg)
		}
	}
	return result, nil
}

func mapSlice(v any) []map[string]any {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
