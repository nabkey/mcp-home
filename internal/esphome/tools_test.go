package esphome

import (
	"reflect"
	"testing"
)

func TestSlimDevices(t *testing.T) {
	raw := []map[string]any{{
		"name":                             "rs485-bench",
		"friendly_name":                    "RS485 Bench",
		"configuration":                    "bench-tester.yaml",
		"address":                          "rs485-bench.local",
		"ip":                               "192.168.1.200",
		"area":                             "",
		"comment":                          nil,
		"target_platform":                  "esp32",
		"board_id":                         "esp32-s3-devkitc-1",
		"current_version":                  "2026.8.2",
		"update_available":                 true,
		"has_pending_changes":              false,
		"loaded_integrations":              []any{"api", "wifi"},
		"directly_referenced_integrations": []any{"api"},
		"build_size_bytes":                 222732333,
		"runtime_state": map[string]any{
			"state":            "offline",
			"deployed_version": "2026.6.2",
			"active_source":    "ping",
		},
	}, {
		"name": "bare",
	}}
	got := slimDevices(raw)
	want := []map[string]any{{
		"name":                "rs485-bench",
		"friendly_name":       "RS485 Bench",
		"configuration":       "bench-tester.yaml",
		"address":             "rs485-bench.local",
		"ip":                  "192.168.1.200",
		"target_platform":     "esp32",
		"board_id":            "esp32-s3-devkitc-1",
		"current_version":     "2026.8.2",
		"update_available":    true,
		"has_pending_changes": false,
		"state":               "offline",
		"deployed_version":    "2026.6.2",
	}, {
		"name": "bare",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}
