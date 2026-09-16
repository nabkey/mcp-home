package hass

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestFilterStates(t *testing.T) {
	states := []State{
		{EntityID: "light.kitchen", State: "on", Attributes: map[string]any{"friendly_name": "Kitchen Light"}},
		{EntityID: "light.porch", State: "off", Attributes: map[string]any{"friendly_name": "Porch"}},
		{EntityID: "switch.kitchen_fan", State: "on", Attributes: map[string]any{"friendly_name": "Fan"}},
		{EntityID: "sensor.temp", State: "21.5", Attributes: map[string]any{"unit_of_measurement": "°C"}},
	}
	ids := func(ss []State) []string {
		var out []string
		for _, s := range ss {
			out = append(out, s.EntityID)
		}
		return out
	}
	cases := []struct {
		name            string
		domains, entIDs []string
		search          string
		want            []string
	}{
		{"no filter", nil, nil, "", []string{"light.kitchen", "light.porch", "switch.kitchen_fan", "sensor.temp"}},
		{"domain", []string{"light"}, nil, "", []string{"light.kitchen", "light.porch"}},
		{"two domains", []string{"light", "switch"}, nil, "", []string{"light.kitchen", "light.porch", "switch.kitchen_fan"}},
		{"entity ids", nil, []string{"sensor.temp", "light.porch"}, "", []string{"light.porch", "sensor.temp"}},
		{"search id and name", nil, nil, "KITCHEN", []string{"light.kitchen", "switch.kitchen_fan"}},
		{"search by friendly name only", nil, nil, "fan", []string{"switch.kitchen_fan"}},
		{"combined", []string{"switch"}, nil, "kitchen", []string{"switch.kitchen_fan"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ids(filterStates(states, c.domains, c.entIDs, c.search))
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestSlimStates(t *testing.T) {
	ts := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	states := []State{{
		EntityID:    "sensor.temp",
		State:       "21.5",
		Attributes:  map[string]any{"friendly_name": "Temp", "unit_of_measurement": "°C", "device_class": "temperature", "icon": "mdi:x"},
		LastChanged: ts,
		LastUpdated: ts,
	}, {
		EntityID: "light.kitchen", State: "on", Attributes: map[string]any{"brightness": 200},
	}}
	b, err := json.Marshal(slimStates(states))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"entity_id":"sensor.temp","state":"21.5","name":"Temp","unit":"°C","last_changed":"2026-09-15T12:00:00Z"},` +
		`{"entity_id":"light.kitchen","state":"on","last_changed":"0001-01-01T00:00:00Z"}]`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList(" light, switch ,,climate "); !reflect.DeepEqual(got, []string{"light", "switch", "climate"}) {
		t.Errorf("got %v", got)
	}
	if got := splitList(""); got != nil {
		t.Errorf("got %v want nil", got)
	}
}

func TestSlimEntitiesInheritsDeviceArea(t *testing.T) {
	devices := []map[string]any{
		{"id": "dev1", "name": "Hue Bridge", "name_by_user": "Bridge", "area_id": "hall", "manufacturer": "Signify", "config_entries": []any{"x"}, "created_at": 1.0},
	}
	entities := []map[string]any{
		{"entity_id": "light.hall", "device_id": "dev1", "area_id": nil, "original_name": "Hall", "name": nil, "platform": "hue", "unique_id": "abc", "options": map[string]any{"a": 1}, "labels": []any{}},
		{"entity_id": "light.kitchen", "device_id": "dev1", "area_id": "kitchen", "name": "Kitchen", "platform": "hue", "entity_category": "config", "disabled_by": "user"},
		{"entity_id": "sensor.orphan", "platform": "template"},
	}
	got := slimEntities(entities, devices)
	want := []map[string]any{
		{"entity_id": "light.hall", "device_id": "dev1", "area_id": "hall", "name": "Hall", "platform": "hue"},
		{"entity_id": "light.kitchen", "device_id": "dev1", "area_id": "kitchen", "name": "Kitchen", "platform": "hue", "entity_category": "config", "disabled_by": "user"},
		{"entity_id": "sensor.orphan", "platform": "template"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}

	// Without devices there is nothing to inherit from.
	if a := slimEntities(entities[:1], nil)[0]["area_id"]; a != nil {
		t.Errorf("area_id = %v, want unset", a)
	}

	gotDev := slimDevices(devices)
	wantDev := []map[string]any{{"id": "dev1", "name": "Bridge", "area_id": "hall", "manufacturer": "Signify"}}
	if !reflect.DeepEqual(gotDev, wantDev) {
		t.Errorf("devices got %v want %v", gotDev, wantDev)
	}
}

func TestRegistryFilters(t *testing.T) {
	recs := []map[string]any{
		{"entity_id": "light.a", "area_id": "kitchen"},
		{"entity_id": "switch.b", "area_id": "kitchen"},
		{"entity_id": "light.c", "area_id": "hall"},
		{"entity_id": "sensor.d"},
	}
	if got := filterByArea(recs, ""); len(got) != 4 {
		t.Errorf("empty area filter dropped rows: %d", len(got))
	}
	if got := filterByArea(recs, "kitchen"); len(got) != 2 || got[1]["entity_id"] != "switch.b" {
		t.Errorf("area filter: %v", got)
	}
	if got := filterEntitiesByDomain(recs, []string{"light"}); len(got) != 2 || got[1]["entity_id"] != "light.c" {
		t.Errorf("domain filter: %v", got)
	}
	if got := filterEntitiesByDomain(filterByArea(recs, "kitchen"), []string{"light"}); len(got) != 1 || got[0]["entity_id"] != "light.a" {
		t.Errorf("combined: %v", got)
	}
}

func TestServiceNames(t *testing.T) {
	services := []map[string]any{
		{"domain": "light", "services": map[string]any{"turn_on": map[string]any{"fields": 1}, "toggle": map[string]any{}, "turn_off": map[string]any{}}},
		{"domain": "lock", "services": map[string]any{"lock": map[string]any{}}},
		{"services": map[string]any{"ignored": map[string]any{}}},
	}
	got := serviceNames(services)
	want := map[string][]string{"light": {"toggle", "turn_off", "turn_on"}, "lock": {"lock"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}
