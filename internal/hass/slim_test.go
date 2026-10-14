package hass

import (
	"reflect"
	"testing"
)

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
