package hass

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func testHome(t *testing.T) *homeContext {
	t.Helper()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	return &homeContext{
		loc: loc,
		entities: map[string]entityInfo{
			"light.kitchen":       {area: "kitchen"},
			"switch.kitchen_fan":  {area: "kitchen"},
			"sensor.fridge_power": {area: "kitchen"},
			"sensor.fridge_rssi":  {area: "kitchen", category: "diagnostic"},
			"binary_sensor.front": {area: "hall"},
		},
		areaNames: map[string]string{"kitchen": "Kitchen", "hall": "Front Hall"},
	}
}

func TestFilterStates(t *testing.T) {
	h := testHome(t)
	states := []State{
		{EntityID: "light.kitchen", State: "on", Attributes: map[string]any{"friendly_name": "Kitchen Light"}},
		{EntityID: "light.porch", State: "off", Attributes: map[string]any{"friendly_name": "Porch"}},
		{EntityID: "switch.kitchen_fan", State: "on", Attributes: map[string]any{"friendly_name": "Fan"}},
		{EntityID: "sensor.fridge_power", State: "80", Attributes: map[string]any{"friendly_name": "Fridge", "device_class": "power"}},
		{EntityID: "sensor.fridge_rssi", State: "-60", Attributes: map[string]any{"friendly_name": "Fridge RSSI"}},
		{EntityID: "binary_sensor.front", State: "on", Attributes: map[string]any{"friendly_name": "Front Door", "device_class": "door"}},
	}
	ids := func(ss []State) []string {
		var out []string
		for _, s := range ss {
			out = append(out, s.EntityID)
		}
		return out
	}
	cases := []struct {
		name      string
		f         stateFilter
		want      []string
		secondary int
	}{
		{"domain", stateFilter{domains: []string{"light"}}, []string{"light.kitchen", "light.porch"}, 0},
		{"two domains", stateFilter{domains: []string{"light", "switch"}}, []string{"light.kitchen", "light.porch", "switch.kitchen_fan"}, 0},
		{"entity ids keep diagnostic", stateFilter{entityIDs: []string{"sensor.fridge_rssi", "light.porch"}}, []string{"light.porch", "sensor.fridge_rssi"}, 0},
		{"search id and name", stateFilter{search: "FAN"}, []string{"switch.kitchen_fan"}, 0},
		// The fridge has no "kitchen" in its id or name; its area does.
		{"search matches area name", stateFilter{search: "kitchen"}, []string{"light.kitchen", "switch.kitchen_fan", "sensor.fridge_power"}, 1},
		{"area", stateFilter{area: "kitchen"}, []string{"light.kitchen", "switch.kitchen_fan", "sensor.fridge_power"}, 1},
		{"area with all", stateFilter{area: "kitchen", all: true}, []string{"light.kitchen", "switch.kitchen_fan", "sensor.fridge_power", "sensor.fridge_rssi"}, 0},
		{"device class and state", stateFilter{deviceClasses: []string{"door", "window"}, states: []string{"on", "open"}}, []string{"binary_sensor.front"}, 0},
		{"state", stateFilter{domains: []string{"light"}, states: []string{"ON"}}, []string{"light.kitchen"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, secondary := filterStates(h, states, c.f)
			if !reflect.DeepEqual(ids(got), c.want) {
				t.Errorf("got %v want %v", ids(got), c.want)
			}
			if secondary != c.secondary {
				t.Errorf("secondary = %d, want %d", secondary, c.secondary)
			}
		})
	}
}

func TestSlimStates(t *testing.T) {
	h := testHome(t)
	ts := time.Date(2026, 9, 15, 12, 0, 0, 123, time.UTC)
	states := []State{{
		EntityID:    "light.kitchen",
		State:       "on",
		Attributes:  map[string]any{"friendly_name": "Kitchen Light", "brightness": 200},
		LastChanged: ts,
	}, {
		EntityID: "sensor.temp", State: "21.5", Attributes: map[string]any{"unit_of_measurement": "°C"},
	}}
	b, err := json.Marshal(slimStates(h, states))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"entity_id":"light.kitchen","state":"on","name":"Kitchen Light","area":"Kitchen","last_changed":"2026-09-15T08:00:00-04:00"},` +
		`{"entity_id":"sensor.temp","state":"21.5","unit":"°C"}]`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}

func TestAreaIDResolvesNames(t *testing.T) {
	h := testHome(t)
	for in, want := range map[string]string{"kitchen": "kitchen", "Front Hall": "hall", "front hall": "hall", "attic": "attic"} {
		if got := h.areaID(in); got != want {
			t.Errorf("areaID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseUserTime(t *testing.T) {
	h := testHome(t)
	cases := map[string]string{
		"2026-10-13 22:00":          "2026-10-14T02:00:00Z",
		"2026-10-13T22:00:30":       "2026-10-14T02:00:30Z",
		"2026-10-13":                "2026-10-13T04:00:00Z",
		"2026-10-13T22:00:00Z":      "2026-10-13T22:00:00Z",
		"2026-10-13T22:00:00-07:00": "2026-10-14T05:00:00Z",
	}
	for in, want := range cases {
		got, ok := h.parseUserTime(in)
		if !ok || got.UTC().Format(time.RFC3339) != want {
			t.Errorf("parseUserTime(%q) = %v, %v; want %s", in, got.UTC(), ok, want)
		}
	}
	if _, ok := h.parseUserTime("last night"); ok {
		t.Error("parsed free text")
	}
}
