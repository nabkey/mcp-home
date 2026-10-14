package hass

import (
	"fmt"
	"testing"
	"time"
)

func TestShapeLogbook(t *testing.T) {
	h := testHome(t)
	h.entities["device_tracker.laptop"] = entityInfo{category: "diagnostic"}
	h.entities["binary_sensor.driveway_motion"] = entityInfo{area: "hall"}
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	var entries []LogbookEntry
	at := func(m int) time.Time { return base.Add(time.Duration(m) * time.Minute) }
	for i := 0; i < 40; i++ {
		entries = append(entries,
			LogbookEntry{When: at(i), EntityID: "binary_sensor.driveway_motion", Name: "Driveway Motion", State: []string{"on", "off"}[i%2]},
			LogbookEntry{When: at(i), EntityID: "device_tracker.laptop", Name: "Laptop", State: "home"},
		)
	}
	entries = append(entries,
		LogbookEntry{When: at(50), EntityID: "light.kitchen", Name: "Kitchen", State: "on"},
		LogbookEntry{When: at(51), EntityID: "automation.night", Name: "Night mode", Message: "triggered by time"},
	)

	t.Run("whole house folds chatty entities and drops diagnostic", func(t *testing.T) {
		out := shapeLogbook(h, entries, logbookFilter{})
		rows := out["events"].([][]string)
		if len(rows) != 2 || rows[0][1] != "light.kitchen" || rows[1][2] != "triggered by time" {
			t.Errorf("events = %v", rows)
		}
		if rows[0][0] != "2026-10-13T08:50:00-04:00" {
			t.Errorf("when = %s, want local time", rows[0][0])
		}
		freq := out["frequent"].([][]any)
		if len(freq) != 1 || freq[0][0] != "binary_sensor.driveway_motion" || freq[0][1] != 40 || freq[0][4] != "off" {
			t.Errorf("frequent = %v", freq)
		}
		if out["omitted_diagnostic"] != 40 {
			t.Errorf("omitted_diagnostic = %v", out["omitted_diagnostic"])
		}
		names := out["names"].(map[string]string)
		if _, ok := names["light.kitchen"]; ok {
			t.Error("a name restating its entity id was listed")
		}
		if names["binary_sensor.driveway_motion"] != "" {
			t.Error("Driveway Motion restates binary_sensor.driveway_motion and should be dropped")
		}
		if names["automation.night"] != "Night mode" {
			t.Errorf("names = %v", names)
		}
	})

	t.Run("a filter keeps what it chose", func(t *testing.T) {
		out := shapeLogbook(h, entries, logbookFilter{domains: []string{"binary_sensor"}})
		if n := len(out["events"].([][]string)); n != 40 {
			t.Errorf("%d events, want all 40 motion changes when filtered to their domain", n)
		}
		if _, ok := out["frequent"]; ok {
			t.Error("folded entities the filter selected")
		}
	})

	t.Run("entity id keeps diagnostic entities", func(t *testing.T) {
		out := shapeLogbook(h, entries, logbookFilter{entityIDs: []string{"device_tracker.laptop"}})
		if n := len(out["events"].([][]string)); n != 40 {
			t.Errorf("%d events, want 40", n)
		}
	})

	t.Run("area", func(t *testing.T) {
		out := shapeLogbook(h, entries, logbookFilter{area: "kitchen"})
		if rows := out["events"].([][]string); len(rows) != 1 || rows[0][1] != "light.kitchen" {
			t.Errorf("events = %v", rows)
		}
	})
}

func TestShapeLogbookCapsRows(t *testing.T) {
	h := testHome(t)
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	var entries []LogbookEntry
	// 300 distinct entities changing once each: nothing to fold, so the
	// oldest rows go.
	for i := 0; i < 300; i++ {
		entries = append(entries, LogbookEntry{When: base.Add(time.Duration(i) * time.Second), EntityID: fmt.Sprintf("light.l%d", i), State: "on"})
	}
	out := shapeLogbook(h, entries, logbookFilter{})
	rows := out["events"].([][]string)
	if len(rows) != maxLogbookRows || rows[len(rows)-1][1] != "light.l299" {
		t.Errorf("%d rows ending %v", len(rows), rows[len(rows)-1])
	}
	if out["omitted_older"] != 300-maxLogbookRows {
		t.Errorf("omitted_older = %v", out["omitted_older"])
	}
}

func TestNameRestatesID(t *testing.T) {
	for _, c := range []struct {
		id, name string
		want     bool
	}{
		{"light.night_light", "Night Light", true},
		{"light.office_light_2", "Office Light", true},
		{"media_player.50_tcl", `50" tcl`, true},
		{"binary_sensor.lumi_lumi_sensor_magnet_aq2_opening", "Freezer Door Opening", false},
		{"sensor.garage_plugs_energy_today", "Master Bathroom Energy Today", false},
		{"light.kitchen", "", false},
	} {
		if got := nameRestatesID(c.id, c.name); got != c.want {
			t.Errorf("nameRestatesID(%q, %q) = %v", c.id, c.name, got)
		}
	}
}
