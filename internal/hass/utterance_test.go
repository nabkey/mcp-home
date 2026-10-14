package hass

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This file is the outside judge for the generated tool layer. It holds a
// fixture with the shape of a real household's registry (names anonymised,
// quirks kept: curly apostrophes, an empty "Bedroom" area next to "Master
// Bedroom", a speaker called "Kitchen" that lives in the Office area, a
// Plex client name shared by three entities, configuration switches that
// are not exposed to Assist, a config LED that is), a reference port of Home
// Assistant's intent matcher to stand in for the instance, and a table of
// plain-language requests with the call a model would plausibly make from
// the advertised schemas. Each row asserts what Home Assistant would then
// actually act on.

// --- fixture -----------------------------------------------------------

type fxEntity struct {
	id       string
	name     string
	area     string // area via the entity's device
	category string // entity_category
	exposed  bool
	state    string
	class    string // device_class
	features int
}

// Media player feature bits (MediaPlayerEntityFeature).
const (
	mpPause     = 1
	mpVolumeSet = 4
	mpMute      = 8
	mpPrevious  = 16
	mpNext      = 32
	mpPlayMedia = 512
	mpSearch    = 4194304
	mpAll       = mpPause | mpVolumeSet | mpMute | mpPrevious | mpNext | mpPlayMedia | mpSearch
	mpRoku      = mpPause | mpVolumeSet | mpMute | mpPrevious | mpNext | mpPlayMedia
)

var fxAreas = []vocabArea{
	{ID: "basement_bathroom", Name: "Basement Bathroom", FloorID: "basement"},
	{ID: "basement_living_room", Name: "Basement Living Room", FloorID: "basement"},
	{ID: "bedroom", Name: "Bedroom"},
	{ID: "dining_room", Name: "Dining Room", FloorID: "main_floor"},
	{ID: "driveway", Name: "Driveway"},
	{ID: "front_porch", Name: "Front Porch"},
	{ID: "garage", Name: "Garage"},
	{ID: "milo_s_room", Name: "Milo's Room"},
	{ID: "kitchen", Name: "Kitchen"},
	{ID: "living_room", Name: "Living Room"},
	{ID: "master_bedroom", Name: "Master Bedroom"},
	{ID: "media_cart", Name: "Media Cart"},
	{ID: "ivys_room", Name: "Ivy’s Room"},
	{ID: "office", Name: "Office"},
	{ID: "pool", Name: "Pool"},
	{ID: "theo_s_room", Name: "Theo's Room", FloorID: "basement"},
	{ID: "storage_room", Name: "Storage Room", FloorID: "basement"},
	{ID: "upstairs_hallway", Name: "Upstairs Hallway", FloorID: "upstairs"},
	{ID: "chicken_coop", Name: "Chicken Coop"},
}

var fxFloors = []vocabFloor{
	{ID: "main_floor", Name: "Main Floor"},
	{ID: "basement", Name: "Basement"},
	{ID: "upstairs", Name: "Upstairs"},
}

var fxEntities = []fxEntity{
	// Lights, all exposed. The AP LED is a config entity the owner exposed.
	{id: "light.bedroom_light_light", name: "Bedroom Overhead Light", area: "master_bedroom", exposed: true, state: "off"},
	{id: "light.living_room_lights_light", name: "Living Room Lights Light", area: "living_room", exposed: true, state: "off"},
	{id: "light.living_room_lamp_2", name: "Living Room Lamp", area: "living_room", exposed: true, state: "off"},
	{id: "light.dad_lamp", name: "Dad Lamp", area: "master_bedroom", exposed: true, state: "on"},
	{id: "light.mom_lamp", name: "Mom Lamp", area: "master_bedroom", exposed: true, state: "on"},
	{id: "light.bedroom_lamps", name: "Bedroom Lamps", area: "master_bedroom", exposed: true, state: "on"},
	{id: "light.milo_smart_bulb", name: "Milo Smart Bulb", area: "milo_s_room", exposed: true, state: "unavailable"},
	{id: "light.a1_chamber_light", name: "A1 Chamber light", area: "storage_room", exposed: true, state: "unavailable"},
	{id: "light.office_light", name: "Office Light", area: "office", exposed: true, state: "off"},
	{id: "light.night_light", name: "Night Light", area: "upstairs_hallway", exposed: true, state: "off"},
	{id: "light.master_bedroom_ap_led", name: "Master Bedroom AP LED", category: "config", exposed: true, state: "off"},
	{id: "light.pool_pool_slider_light_l", name: "Pool Slider Light L", area: "pool", exposed: true, state: "on"},
	{id: "light.pool_slider_light_r", name: "Pool Slider Light R", area: "pool", exposed: true, state: "on"},
	{id: "light.pool_slider_lights", name: "Pool Slider Lights", area: "pool", exposed: true, state: "on"},

	// Switches. Lamps on smart plugs are switches, not lights.
	{id: "switch.office_fan_switch", name: "Office Lamp Switch", area: "office", exposed: true, state: "off"},
	{id: "switch.garage_workbench_switch", name: "Garage Workbench Switch", area: "garage", exposed: true, state: "off"},
	{id: "switch.driveway_lights_switch", name: "Driveway Lights Switch", area: "driveway", exposed: true, state: "on"},
	{id: "switch.ivys_room_light_switch", name: "Ivys Room Light Switch", area: "ivys_room", exposed: true, state: "on"},
	{id: "switch.milo_plug_light_switch", name: "Milo Plug Light Switch", area: "milo_s_room", exposed: true, state: "off"},
	{id: "switch.porch_light_switch", name: "Porch Light Switch", area: "front_porch", exposed: true, state: "on"},
	{id: "switch.theo_alarm_switch", name: "Theo Alarm Switch", area: "theo_s_room", exposed: true, state: "off"},
	{id: "switch.driveway_light_plug", name: "Driveway Light Plug", area: "driveway", exposed: true, state: "on"},
	{id: "switch.garage_freezer_plug", name: "Garage Freezer Plug", area: "garage", exposed: true, state: "on"},
	{id: "switch.pool_pump_plug_switch_0", name: "Pool Pump Plug", area: "pool", exposed: true, state: "on"},
	{id: "switch.cph50_charging_schedule", name: "CPH50 Charging Schedule", area: "garage", exposed: true, state: "off"},
	// Configuration switches Assist does not expose.
	{id: "switch.frigate_garage_detect", name: "Frigate Garage Detect", area: "garage", category: "config", state: "on"},
	{id: "switch.frigate_garage_recordings", name: "Frigate Garage Recordings", area: "garage", category: "config", state: "on"},
	{id: "switch.frigate_driveway_detect", name: "Frigate Driveway Detect", area: "driveway", category: "config", state: "on"},
	{id: "switch.cat_door_motion_led", name: "Cat Door Motion LED trigger indicator", area: "garage", category: "config", state: "off"},
	{id: "switch.unifi_block_ivy_desktop", name: "UniFi Network Block Ivy Desktop", category: "config", state: "on"},

	// Buttons, none exposed. One has no entity category at all.
	{id: "button.cph50_restart_charger", name: "CPH50 Restart Charger", area: "garage", state: "unknown"},
	{id: "button.cph50_start_charging", name: "CPH50 Start Charging", area: "garage", state: "unknown"},
	{id: "button.garage_door_controller_restart", name: "Garage Door Controller Restart", area: "garage", category: "config", state: "unknown"},
	{id: "button.office_light_identify", name: "Office Light Identify", area: "office", category: "diagnostic", state: "unknown"},

	{id: "cover.garage_doors_left_garage_door", name: "Garage Door Controller Left Garage Door", area: "garage", exposed: true, state: "open", class: "garage"},
	{id: "cover.garage_doors_right_garage_door", name: "Garage Door Controller Right Garage Door", area: "garage", exposed: true, state: "open", class: "garage"},
	{id: "binary_sensor.freezer_door", name: "Freezer Door", area: "garage", exposed: true, state: "off", class: "door"},

	{id: "fan.a1_aux_fan", name: "A1 Aux fan", area: "storage_room", exposed: true, state: "unavailable"},
	{id: "fan.a1_cooling_fan", name: "A1 Cooling fan", area: "storage_room", exposed: true, state: "unavailable"},

	// Media players. Most have no area; one name is shared three ways.
	{id: "media_player.main_bedroom", name: "Main Bedroom", area: "master_bedroom", exposed: true, state: "idle", features: mpAll},
	{id: "media_player.kitchen_2", name: "Kitchen", area: "office", exposed: true, state: "idle", features: mpAll},
	{id: "media_player.milos_room", name: "Milo’s Room", exposed: true, state: "unavailable", features: mpAll},
	{id: "media_player.office", name: "Office", exposed: true, state: "idle", features: mpAll},
	{id: "media_player.bedroom", name: "Ivy’s room", exposed: true, state: "paused", features: mpAll},
	{id: "media_player.50in_tcl", name: "50in tcl", exposed: true, state: "idle", features: mpAll},
	{id: "media_player.77_oled", name: "77\" OLED", exposed: true, state: "idle", features: mpAll},
	{id: "media_player.living_room_tv", name: "Living Room TV", exposed: true, state: "unavailable", features: mpAll},
	{id: "media_player.tv_samsung_6_series_40", name: "[TV] Samsung 6 Series (40)", exposed: true, state: "unavailable", features: mpAll},
	{id: "media_player.50_tcl", name: "50\" tcl", area: "master_bedroom", exposed: true, state: "playing", features: mpRoku},
	{id: "media_player.gc888_12f50a", name: "GC888-12F50A", exposed: true, state: "unavailable", features: mpAll},
	{id: "media_player.gc888_12f50a_2", name: "GC888-12F50A", exposed: true, state: "unavailable", features: mpAll},
	{id: "media_player.plex_ios_1", name: "Plex (Plex for iOS - iPhone)", exposed: true, state: "unavailable", features: mpRoku},
	{id: "media_player.plex_ios_2", name: "Plex (Plex for iOS - iPhone)", exposed: true, state: "unavailable", features: mpRoku},
	{id: "media_player.plex_ios_3", name: "Plex (Plex for iOS - iPhone)", exposed: true, state: "unavailable", features: mpRoku},
	{id: "media_player.airplay_living_room", name: "AirPlay Living Room", exposed: true, state: "idle", features: mpAll},
	{id: "media_player.office_display", name: "Office Display", area: "office", exposed: true, state: "unavailable", features: mpAll},
	{id: "media_player.samsung_6_series_40", name: "Samsung 6 Series (40)", area: "media_cart", state: "off", features: mpAll},

	{id: "todo.shopping_list", name: "Shopping list", exposed: true, state: "0"},
	{id: "todo.costco", name: "Costco", exposed: true, state: "1"},
	{id: "todo.reminders", name: "Reminders", exposed: true, state: "1"},

	{id: "script.flash", name: "Light Flash", exposed: true, state: "off"},
	{id: "script.new_script", name: "Mood Lights", area: "master_bedroom", state: "off"},
	{id: "script.regular_lamps", name: "Regular Lamps", area: "master_bedroom", state: "off"},
	{id: "input_boolean.arm_clock", name: "Arm Clock", state: "off"},
}

const fxScriptServices = `[{"domain":"script","services":{
	"reload":{},"turn_on":{},"turn_off":{},"toggle":{},
	"flash":{"name":"Light Flash","description":"","fields":{}},
	"new_script":{"name":"Mood Lights","description":"Sets bedroom mode to Lamps Only: Red and starts Relaxing Massage playlist","fields":{}},
	"regular_lamps":{"name":"Regular Lamps","description":"Sets bedroom mode to Lamps Only: Warm (mode selector is source of truth)","fields":{}}
}}]`

func domainOf(id string) string {
	d, _, _ := strings.Cut(id, ".")
	return d
}

// --- reference matcher ---------------------------------------------------

// matchConstraints mirrors intent.MatchTargetsConstraints.
type matchConstraints struct {
	name, area, floor string
	domains           []string
	deviceClasses     []string
	features          int
	states            []string
	assistant         bool
	singleTarget      bool
}

func casefold(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// refMatch ports async_match_targets for this fixture: exact casefolded
// names, area and floor by name or id, exposure applied when an assistant
// is given, and the duplicate-name and single-target refusals.
func refMatch(c matchConstraints) ([]fxEntity, string) {
	var cands []fxEntity
	for _, e := range fxEntities {
		if c.domains != nil && !containsStr(c.domains, domainOf(e.id)) {
			continue
		}
		cands = append(cands, e)
	}
	if len(cands) == 0 {
		return nil, "domain"
	}
	if c.states != nil {
		cands = filter(cands, func(e fxEntity) bool { return containsStr(c.states, e.state) })
		if len(cands) == 0 {
			return nil, "state"
		}
	}
	if c.name != "" {
		cands = filter(cands, func(e fxEntity) bool { return e.id == c.name || casefold(e.name) == casefold(c.name) })
		if len(cands) == 0 {
			return nil, "name"
		}
	}
	if c.features != 0 {
		cands = filter(cands, func(e fxEntity) bool { return e.features&c.features == c.features })
		if len(cands) == 0 {
			return nil, "feature"
		}
	}
	if len(c.deviceClasses) > 0 {
		cands = filter(cands, func(e fxEntity) bool { return e.class != "" && containsStr(c.deviceClasses, e.class) })
		if len(cands) == 0 {
			return nil, "device_class"
		}
	}
	if c.floor != "" || c.area != "" {
		possible := map[string]bool{}
		for _, a := range fxAreas {
			possible[a.ID] = true
		}
		if c.floor != "" {
			var floors []string
			for _, f := range fxFloors {
				if f.ID == c.floor || casefold(f.Name) == casefold(c.floor) {
					floors = append(floors, f.ID)
				}
			}
			if len(floors) == 0 {
				return nil, "invalid_floor"
			}
			possible = map[string]bool{}
			for _, a := range fxAreas {
				if containsStr(floors, a.FloorID) {
					possible[a.ID] = true
				}
			}
			cands = filter(cands, func(e fxEntity) bool { return possible[e.area] })
			if len(cands) == 0 {
				return nil, "floor"
			}
		}
		if c.area != "" {
			matching := map[string]bool{}
			for _, a := range fxAreas {
				if a.ID == c.area || casefold(a.Name) == casefold(c.area) {
					matching[a.ID] = true
				}
			}
			if len(matching) == 0 {
				return nil, "invalid_area"
			}
			cands = filter(cands, func(e fxEntity) bool { return possible[e.area] && matching[e.area] })
			if len(cands) == 0 {
				return nil, "area"
			}
		}
	}
	if c.assistant {
		cands = filter(cands, func(e fxEntity) bool { return e.exposed })
		if len(cands) == 0 {
			return nil, "assistant"
		}
	}
	// Every name-matched candidate carries the query as matched_name, so
	// more than one left over is a duplicate.
	if c.name != "" && len(cands) > 1 {
		return nil, "duplicate_name"
	}
	if c.singleTarget && len(cands) > 1 {
		return nil, "multiple_targets"
	}
	return cands, ""
}

func filter(in []fxEntity, keep func(fxEntity) bool) []fxEntity {
	var out []fxEntity
	for _, e := range in {
		if keep(e) {
			out = append(out, e)
		}
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// onOffCapable is what has_service(domain, turn_on/off) answers here.
var onOffCapable = map[string]bool{"light": true, "switch": true, "fan": true, "media_player": true, "script": true, "input_boolean": true}

// refIntent runs one intent the way the upstream handler would, returning
// the entity ids acted on, or an error reason.
func refIntent(intentName string, slots map[string]any, assistant bool) ([]string, string) {
	str := func(k string) string { s, _ := slots[k].(string); return s }
	list := func(k string) []string { return stringSlice(slots[k]) }
	c := matchConstraints{name: str("name"), area: str("area"), floor: str("floor"), assistant: assistant}
	if c.name == "all" {
		c.name = ""
	}
	if v, ok := slots["domain"]; ok {
		c.domains = stringSlice(v)
	}
	c.deviceClasses = list("device_class")

	requireDomain := func(d string) string {
		if c.domains == nil {
			c.domains = []string{d}
			return ""
		}
		for _, x := range c.domains {
			if x != d {
				return "slot validation: domain " + x
			}
		}
		return ""
	}
	hasConstraints := func() bool {
		return c.name != "" || c.area != "" || c.floor != "" || len(c.domains) > 0 || len(c.deviceClasses) > 0 || c.features != 0 || len(c.states) > 0
	}
	ids := func(es []fxEntity) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.id)
		}
		sort.Strings(out)
		return out
	}

	switch intentName {
	case "HassTurnOn", "HassTurnOff":
		if !hasConstraints() {
			return nil, "cannot target all devices"
		}
		matched, why := refMatch(c)
		if why != "" {
			return nil, why
		}
		var ok []fxEntity
		for _, e := range matched {
			d := domainOf(e.id)
			switch {
			case d == "button":
				if intentName == "HassTurnOn" {
					ok = append(ok, e)
				}
			case d == "cover" || onOffCapable[d]:
				ok = append(ok, e)
			}
		}
		if len(ok) == 0 {
			return nil, "every target failed"
		}
		return ids(ok), ""
	case "HassLightSet":
		if why := requireDomain("light"); why != "" {
			return nil, why
		}
		matched, why := refMatch(c)
		if why != "" {
			return nil, why
		}
		return ids(matched), ""
	case "HassSetPosition", "HassStopMoving":
		if !hasConstraints() {
			return nil, "cannot target all devices"
		}
		matched, why := refMatch(c)
		if why != "" {
			return nil, why
		}
		for _, e := range matched {
			if d := domainOf(e.id); d != "cover" && d != "valve" {
				return nil, "domain not supported: " + d
			}
		}
		return ids(matched), ""
	case "HassMediaPause", "HassMediaNext", "HassMediaPrevious":
		if why := requireDomain("media_player"); why != "" {
			return nil, why
		}
		c.states = []string{"playing"}
		c.features = map[string]int{"HassMediaPause": mpPause, "HassMediaNext": mpNext, "HassMediaPrevious": mpPrevious}[intentName]
		matched, why := refMatch(c)
		if why != "" {
			return nil, why
		}
		return ids(matched), ""
	case "HassMediaUnpause", "HassSetVolume", "HassMediaPlayerMute", "HassMediaPlayerUnmute":
		if why := requireDomain("media_player"); why != "" {
			return nil, why
		}
		c.features = map[string]int{"HassMediaUnpause": 0, "HassSetVolume": mpVolumeSet, "HassMediaPlayerMute": mpMute, "HassMediaPlayerUnmute": mpMute}[intentName]
		matched, why := refMatch(c)
		if why != "" {
			return nil, why
		}
		return ids(matched), ""
	case "HassSetVolumeRelative":
		c.domains = []string{"media_player"} // the domain slot is ignored
		c.features = mpVolumeSet
		matched, why := refMatch(c)
		if why != "" {
			return nil, why
		}
		if len(matched) > 1 && c.name == "" {
			matched = filter(matched, func(e fxEntity) bool { return e.state == "playing" })
			if len(matched) == 0 {
				return nil, "state"
			}
		}
		return ids(matched), ""
	case "HassMediaSearchAndPlay":
		c.domains = []string{"media_player"}
		c.features = mpSearch | mpPlayMedia
		c.singleTarget = true
		matched, why := refMatch(c)
		if why != "" {
			return nil, why
		}
		return ids(matched), ""
	case "HassListAddItem", "HassListCompleteItem", "HassListRemoveItem":
		matched, why := refMatch(matchConstraints{name: c.name, domains: []string{"todo"}, assistant: assistant})
		if why != "" {
			return nil, why
		}
		return []string{matched[0].id}, ""
	}
	return nil, "unknown intent " + intentName
}

// --- fake instance -------------------------------------------------------

type refHA struct {
	mu sync.Mutex
	// acted collects every entity the reference matcher acted on.
	acted []string
	// posts records the intent bodies received.
	posts []map[string]any
}

func (h *refHA) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.acted, h.posts = nil, nil
}

func (h *refHA) snapshot() ([]string, []map[string]any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := append([]string(nil), h.acted...)
	sort.Strings(out)
	return out, append([]map[string]any(nil), h.posts...)
}

func fxStatesJSON() []map[string]any {
	var out []map[string]any
	for _, e := range fxEntities {
		attrs := map[string]any{"friendly_name": e.name}
		if e.class != "" {
			attrs["device_class"] = e.class
		}
		if e.features != 0 {
			attrs["supported_features"] = e.features
		}
		out = append(out, map[string]any{"entity_id": e.id, "state": e.state, "attributes": attrs})
	}
	return out
}

func (h *refHA) serve() http.HandlerFunc {
	upgrader := websocket.Upgrader{}
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/states":
			_ = json.NewEncoder(w).Encode(fxStatesJSON())
		case "/api/services":
			_, _ = w.Write([]byte(fxScriptServices))
		case "/api/intent/handle":
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			name, _ := body["name"].(string)
			slots, _ := body["data"].(map[string]any)
			assistant := body["assistant"] == "conversation"
			acted, why := refIntent(name, slots, assistant)
			h.mu.Lock()
			h.posts = append(h.posts, body)
			h.acted = append(h.acted, acted...)
			h.mu.Unlock()
			if why != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"response_type": "error",
					"speech":        map[string]any{"plain": map[string]any{"speech": why}},
					"data":          map[string]any{"code": "failed_to_handle"},
				})
				return
			}
			var success []map[string]any
			for _, id := range acted {
				success = append(success, map[string]any{"id": id, "type": "entity"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"response_type": "action_done",
				"speech":        map[string]any{"plain": map[string]any{"speech": "Done"}},
				"data":          map[string]any{"success": success, "failed": []any{}},
			})
		case "/api/websocket":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.Close() }()
			_ = conn.WriteJSON(map[string]any{"type": "auth_required"})
			var auth map[string]any
			if conn.ReadJSON(&auth) != nil {
				return
			}
			_ = conn.WriteJSON(map[string]any{"type": "auth_ok"})
			for {
				var cmd map[string]any
				if conn.ReadJSON(&cmd) != nil {
					return
				}
				_ = conn.WriteJSON(map[string]any{"id": cmd["id"], "type": "result", "success": true, "result": fxWS(cmd)})
			}
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}
}

// fxWS answers the registry commands the vocabulary loader sends. Every
// entity sits on a device that carries its area, as most real ones do.
func fxWS(cmd map[string]any) any {
	switch cmd["type"] {
	case "config/area_registry/list":
		var out []map[string]any
		for _, a := range fxAreas {
			var floor any
			if a.FloorID != "" {
				floor = a.FloorID
			}
			out = append(out, map[string]any{"area_id": a.ID, "name": a.Name, "floor_id": floor, "aliases": []any{}})
		}
		return out
	case "config/floor_registry/list":
		var out []map[string]any
		for _, f := range fxFloors {
			out = append(out, map[string]any{"floor_id": f.ID, "name": f.Name, "aliases": []any{}})
		}
		return out
	case "config/device_registry/list":
		var out []map[string]any
		for _, e := range fxEntities {
			out = append(out, map[string]any{"id": "dev_" + e.id, "area_id": e.area})
		}
		return out
	case "config/entity_registry/list":
		var out []map[string]any
		for _, e := range fxEntities {
			var cat any
			if e.category != "" {
				cat = e.category
			}
			out = append(out, map[string]any{
				"entity_id": e.id, "device_id": "dev_" + e.id, "area_id": nil,
				"entity_category": cat, "hidden_by": nil, "disabled_by": nil,
				"options": map[string]any{"conversation": map[string]any{"should_expose": e.exposed}},
			})
		}
		return out
	case "homeassistant/expose_entity/list":
		exposed := map[string]any{}
		for _, e := range fxEntities {
			if e.exposed {
				exposed[e.id] = map[string]any{"conversation": true}
			}
		}
		return map[string]any{"exposed_entities": exposed}
	case "config/entity_registry/get_entries":
		out := map[string]any{}
		for _, id := range stringSlice(cmd["entity_ids"]) {
			out[id] = map[string]any{"entity_id": id, "aliases": []any{nil}}
		}
		return out
	}
	return nil
}

func connectRef(t *testing.T, deny []string) (*mcp.ClientSession, *refHA) {
	t.Helper()
	h := &refHA{}
	srv := httptest.NewServer(h.serve())
	t.Cleanup(srv.Close)
	tools, err := NewTools(srv.URL, "t", deny)
	if err != nil {
		t.Fatal(err)
	}
	return connectTools(t, tools), h
}

// --- the table -------------------------------------------------------------

type utterance struct {
	say  string
	tool string
	args map[string]any
	// want is what Home Assistant acts on; nil with wantErr means the call
	// must fail without acting on anything.
	want    []string
	wantErr bool
	// typed marks args a user typed loosely (straight apostrophe, lower
	// case) rather than copied from the tool's name list.
	typed bool
}

var everythingOnOff = []any{"light", "switch", "fan", "media_player", "cover"}

var utterances = []utterance{
	{say: "dim the living room to 30%", tool: "home_light_set",
		args: map[string]any{"area": "Living Room", "brightness": 30},
		want: []string{"light.living_room_lamp_2", "light.living_room_lights_light"}},
	{say: "turn off the bedroom lamps", tool: "home_turn_off",
		args: map[string]any{"name": "Bedroom Lamps"},
		want: []string{"light.bedroom_lamps"}},
	{say: "turn off everything upstairs", tool: "home_turn_off",
		args: map[string]any{"floor": "Upstairs", "domain": everythingOnOff},
		want: []string{"light.night_light"}},
	{say: "turn off all the lights", tool: "home_turn_off",
		args: map[string]any{"domain": []any{"light"}},
		want: []string{
			"light.a1_chamber_light", "light.bedroom_lamps", "light.bedroom_light_light", "light.dad_lamp",
			"light.living_room_lamp_2", "light.living_room_lights_light", "light.master_bedroom_ap_led",
			"light.milo_smart_bulb", "light.mom_lamp", "light.night_light", "light.office_light",
			"light.pool_pool_slider_light_l", "light.pool_slider_light_r", "light.pool_slider_lights",
		}},
	{say: "pause the TV", tool: "home_media",
		args: map[string]any{"action": "pause", "domain": []any{"media_player"}},
		want: []string{"media_player.50_tcl"}},
	{say: "add milk to the shopping list", tool: "home_list_add_item",
		args: map[string]any{"item": "milk", "name": "Shopping list"},
		want: []string{"todo.shopping_list"}},
	{say: "close the garage door", tool: "home_turn_off",
		args: map[string]any{"area": "Garage", "device_class": []any{"garage"}},
		want: []string{"cover.garage_doors_left_garage_door", "cover.garage_doors_right_garage_door"}},
	{say: "open the left garage door", tool: "home_turn_on",
		args: map[string]any{"name": "Garage Door Controller Left Garage Door"},
		want: []string{"cover.garage_doors_left_garage_door"}},
	{say: "open the garage doors halfway", tool: "home_set_position",
		args: map[string]any{"area": "Garage", "position": 50},
		want: []string{"cover.garage_doors_left_garage_door", "cover.garage_doors_right_garage_door"}},
	{say: "stop the garage door", tool: "home_stop_moving",
		args: map[string]any{"area": "Garage"},
		want: []string{"cover.garage_doors_left_garage_door", "cover.garage_doors_right_garage_door"}},
	{say: "turn on the porch light", tool: "home_turn_on",
		args: map[string]any{"name": "Porch Light Switch"},
		want: []string{"switch.porch_light_switch"}},
	{say: "turn on the garage light", tool: "home_turn_on",
		args: map[string]any{"name": "Garage Workbench Switch"},
		want: []string{"switch.garage_workbench_switch"}},
	{say: "turn off the driveway lights", tool: "home_turn_off",
		args: map[string]any{"area": "Driveway", "domain": []any{"light", "switch"}},
		want: []string{"switch.driveway_light_plug", "switch.driveway_lights_switch"}},
	{say: "turn off everything in the office", tool: "home_turn_off",
		args: map[string]any{"area": "Office", "domain": everythingOnOff},
		want: []string{"light.office_light", "media_player.kitchen_2", "media_player.office_display", "switch.office_fan_switch"}},
	{say: "set the bedroom lights to warm white", tool: "home_light_set",
		args: map[string]any{"area": "Master Bedroom", "color": "warmwhite"},
		want: []string{"light.bedroom_lamps", "light.bedroom_light_light", "light.dad_lamp", "light.mom_lamp"}},
	{say: "turn on dad's lamp", tool: "home_turn_on",
		args: map[string]any{"name": "Dad Lamp"},
		want: []string{"light.dad_lamp"}},
	{say: "set the volume to 20", tool: "home_media",
		args: map[string]any{"action": "set_volume", "volume_level": 20, "domain": []any{"media_player"}},
		want: []string{"media_player.50_tcl"}},
	{say: "turn it up", tool: "home_media",
		args: map[string]any{"action": "set_volume_relative", "volume_step": "up", "domain": []any{"media_player"}},
		want: []string{"media_player.50_tcl"}},
	{say: "mute the kitchen speaker", tool: "home_media",
		args: map[string]any{"action": "mute", "name": "Kitchen"},
		want: []string{"media_player.kitchen_2"}},
	{say: "play Taylor Swift on the office speaker", tool: "home_media",
		args: map[string]any{"action": "search_and_play", "name": "Office", "search_query": "Taylor Swift", "media_class": "artist"},
		want: []string{"media_player.office"}},
	{say: "resume the music", tool: "home_media",
		args: map[string]any{"action": "unpause", "domain": []any{"media_player"}},
		want: []string{"media_player.bedroom"}},
	{say: "turn on the light in Ivy's room", tool: "home_turn_on", typed: true,
		args: map[string]any{"area": "Ivy's room", "domain": []any{"light", "switch"}},
		want: []string{"switch.ivys_room_light_switch"}},
	{say: "resume Ivy's speaker", tool: "home_media", typed: true,
		args: map[string]any{"action": "unpause", "name": "ivy's room"},
		want: []string{"media_player.bedroom"}},
	{say: "turn on the lights in Milo's room", tool: "home_turn_on",
		args: map[string]any{"area": "Milo's Room", "domain": []any{"light", "switch"}},
		want: []string{"light.milo_smart_bulb", "switch.milo_plug_light_switch"}},
	{say: "turn off the pool pump", tool: "home_turn_off",
		args: map[string]any{"name": "Pool Pump Plug"},
		want: []string{"switch.pool_pump_plug_switch_0"}},
	{say: "set the pool lights to 50%", tool: "home_light_set",
		args: map[string]any{"area": "Pool", "brightness": 50},
		want: []string{"light.pool_pool_slider_light_l", "light.pool_slider_light_r", "light.pool_slider_lights"}},
	{say: "add paper towels to costco", tool: "home_list_add_item", typed: true,
		args: map[string]any{"item": "paper towels", "name": "costco"},
		want: []string{"todo.costco"}},
	{say: "mark eggs done on the shopping list", tool: "home_list_complete_item",
		args: map[string]any{"item": "eggs", "name": "Shopping list"},
		want: []string{"todo.shopping_list"}},
	{say: "turn off the basement lights", tool: "home_turn_off",
		args: map[string]any{"floor": "Basement", "domain": []any{"light"}},
		want: []string{"light.a1_chamber_light"}},
	{say: "skip this song", tool: "home_media",
		args: map[string]any{"action": "next", "domain": []any{"media_player"}},
		want: []string{"media_player.50_tcl"}},
	{say: "turn on the night light", tool: "home_turn_on",
		args: map[string]any{"name": "Night Light"},
		want: []string{"light.night_light"}},
	{say: "turn off the office lamp", tool: "home_turn_off",
		args: map[string]any{"name": "Office Lamp Switch"},
		want: []string{"switch.office_fan_switch"}},
	{say: "turn off the TV in the bedroom", tool: "home_turn_off",
		args: map[string]any{"name": "50\" tcl"},
		want: []string{"media_player.50_tcl"}},
	{say: "turn off the lights in the garage", tool: "home_turn_off",
		args: map[string]any{"name": "Garage Workbench Switch"},
		want: []string{"switch.garage_workbench_switch"}},
	{say: "turn off the porch light and the driveway lights", tool: "home_turn_off",
		args: map[string]any{"name": []any{"Porch Light Switch", "Driveway Lights Switch", "Driveway Light Plug"}},
		want: []string{"switch.driveway_light_plug", "switch.driveway_lights_switch", "switch.porch_light_switch"}},
	{say: "turn on dad's and mom's lamps", tool: "home_turn_on",
		args: map[string]any{"name": []any{"Dad Lamp", "Mom Lamp"}},
		want: []string{"light.dad_lamp", "light.mom_lamp"}},

	// Calls that must not act at all. These are adversarial: a model would
	// not find these targets in the tool's lists, but a sloppy or
	// prompt-injected call must still not reach the wrong device.
	{say: "close the garage (area only)", tool: "home_turn_off",
		args: map[string]any{"area": "Garage"}, wantErr: true},
	{say: "turn everything off", tool: "home_turn_off",
		args: map[string]any{"name": "all", "area": "Garage"}, wantErr: true},
	{say: "turn off garage camera detection", tool: "home_turn_off", typed: true,
		args: map[string]any{"name": "Frigate Garage Detect"}, wantErr: true},
	{say: "press the garage buttons", tool: "home_turn_on",
		args: map[string]any{"area": "Garage", "domain": []any{"button"}}, wantErr: true},
	{say: "turn on the plex iphone", tool: "home_turn_on",
		args: map[string]any{"name": "Plex (Plex for iOS - iPhone)"}, wantErr: true},
	{say: "turn on the samsung TV (unexposed)", tool: "home_turn_on", typed: true,
		args: map[string]any{"name": "Samsung 6 Series (40)"}, wantErr: true},
	{say: "mute the office", tool: "home_media",
		args: map[string]any{"action": "mute", "area": "Office"},
		want: []string{"media_player.kitchen_2", "media_player.office_display"}},
}

// TestUtterances is the confusion table: each request, the call a model
// would make from the advertised tools, and what Home Assistant then acts
// on. A row passes only if exactly the expected entities are hit.
func TestUtterances(t *testing.T) {
	session, h := connectRef(t, nil)
	tools := toolNames(t, session)

	for _, u := range utterances {
		t.Run(u.say, func(t *testing.T) {
			tool, ok := tools[u.tool]
			if !ok {
				t.Fatalf("tool %s not registered", u.tool)
			}
			// One call, no lookup: names the model copies must be on offer
			// in the tool's own schema.
			if !u.typed {
				props := schemaJSON(t, tool.InputSchema)["properties"].(map[string]any)
				for _, key := range []string{"name", "area", "floor"} {
					prop, _ := props[key].(map[string]any)
					desc, _ := prop["description"].(string)
					for _, v := range stringSlice(u.args[key]) {
						if v != "all" && !strings.Contains(desc, v) {
							t.Errorf("%s %q is not listed in the tool's %s description, so the model would have to look it up", key, v, key)
						}
					}
				}
			}

			h.reset()
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: u.tool, Arguments: u.args})
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			acted, posts := h.snapshot()
			for _, p := range posts {
				if p["assistant"] != "conversation" {
					t.Errorf("intent sent without the Assist exposure filter: %v", p)
				}
			}
			if u.wantErr {
				if !res.IsError {
					t.Errorf("succeeded, want an error")
				}
				if len(acted) > 0 {
					t.Errorf("WRONG DEVICE: acted on %v", acted)
				}
				return
			}
			if res.IsError {
				t.Fatalf("error: %v (acted on %v)", res.Content[0].(*mcp.TextContent).Text, acted)
			}
			want := append([]string(nil), u.want...)
			sort.Strings(want)
			if strings.Join(acted, ",") != strings.Join(want, ",") {
				t.Errorf("acted on %v, want %v", acted, want)
			}
		})
	}
}

// The vocabulary must be what makes the table pass: unexposed entities are
// never offered, shared names are flagged, and an empty area is not listed.
func TestUtteranceVocabularyShape(t *testing.T) {
	session, _ := connectRef(t, nil)
	tools := toolNames(t, session)
	props := func(tool string) map[string]any {
		return schemaJSON(t, tools[tool].InputSchema)["properties"].(map[string]any)
	}
	desc := func(tool, key string) string {
		d, _ := props(tool)[key].(map[string]any)["description"].(string)
		return d
	}

	off := desc("home_turn_off", "name")
	for _, hidden := range []string{"Frigate Garage Detect", "CPH50 Restart Charger", "Samsung 6 Series (40) (media_player, Media Cart)", "Arm Clock"} {
		if strings.Contains(off, hidden) {
			t.Errorf("unexposed %q offered to the model", hidden)
		}
	}
	if !strings.Contains(off, "Garage Workbench Switch (switch, Garage)") {
		t.Errorf("cross-domain names should carry their domain and area: %q", off)
	}
	media := desc("home_media", "name")
	if !strings.Contains(media, "Plex (Plex for iOS - iPhone) [3 devices share this name") {
		t.Errorf("shared name not flagged: %q", media)
	}
	if strings.Contains(media, "Office Light") {
		t.Error("media tool lists lights")
	}
	areas := desc("home_light_set", "area")
	listed := map[string]bool{}
	if _, list, ok := strings.Cut(areas, "devices: "); ok {
		for _, a := range strings.Split(strings.TrimSuffix(list, "."), "; ") {
			listed[a] = true
		}
	}
	if listed["Bedroom"] || !listed["Master Bedroom"] {
		t.Errorf("light areas should list Master Bedroom and not the empty Bedroom: %q", areas)
	}
	if floors := desc("home_turn_off", "floor"); !strings.Contains(floors, "Upstairs: Upstairs Hallway") {
		t.Errorf("floors should show which areas they hold: %q", floors)
	}
	// Scripts keep their own name, so "mood lights" finds home_script_new_script.
	if d := tools["home_script_new_script"].Description; !strings.Contains(d, "Mood Lights") || !strings.Contains(d, "Master Bedroom") {
		t.Errorf("script description = %q", d)
	}
}

// A deny on switch.turn_off must keep home_turn_off off the freezer plug,
// even when the request names it directly.
func TestUtteranceDenyKeepsSwitchesOff(t *testing.T) {
	session, h := connectRef(t, []string{"switch.turn_off"})
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "home_turn_off", Arguments: map[string]any{"name": "Garage Freezer Plug"},
	})
	if err != nil {
		t.Fatal(err)
	}
	acted, _ := h.snapshot()
	if !res.IsError || len(acted) != 0 {
		t.Errorf("freezer plug reachable despite switch.turn_off deny: acted on %v", acted)
	}
	// Lights still work through the same tool.
	h.reset()
	res, _ = session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "home_turn_off", Arguments: map[string]any{"area": "Garage", "domain": []any{"switch", "cover"}},
	})
	acted, _ = h.snapshot()
	if res.IsError || strings.Join(acted, ",") != "cover.garage_doors_left_garage_door,cover.garage_doors_right_garage_door" {
		t.Errorf("acted on %v, want only the garage doors", acted)
	}
}
