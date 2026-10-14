package hass

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// syntheticFixture builds a home shaped like the live instance the question
// set was measured on (~900 states, ~45% of them diagnostic/config, a third
// unavailable, two camera motion sensors toggling ~300 times a day, a hundred
// network device trackers, a power sensor sampled every minute) without any
// of its personal data.
func syntheticFixture() *replayFixture {
	now := time.Date(2026, 10, 14, 2, 30, 0, 0, time.UTC)
	f := &replayFixture{
		now:     now,
		config:  map[string]any{"time_zone": "America/New_York", "version": "2026.10.1", "components": make([]any, 300)},
		history: map[string][]map[string]any{},
		stats:   map[string][]map[string]any{},
	}
	ts := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	areas := []string{"kitchen", "garage", "living_room", "master_bedroom", "office", "pool", "driveway", "storage_room"}
	for _, a := range areas {
		f.areas = append(f.areas, map[string]any{"area_id": a, "name": strings.ReplaceAll(strings.ToUpper(a[:1])+a[1:], "_", " ")})
	}
	addEntity := func(id, state, name, area, category string, attrs map[string]any) {
		if attrs == nil {
			attrs = map[string]any{}
		}
		attrs["friendly_name"] = name
		f.states = append(f.states, map[string]any{"entity_id": id, "state": state, "attributes": attrs,
			"last_changed": ts(now.Add(-3 * time.Hour)), "last_updated": ts(now.Add(-3 * time.Hour))})
		dev := "dev_" + id
		f.devices = append(f.devices, map[string]any{"id": dev, "area_id": area, "name": name})
		var cat any
		if category != "" {
			cat = category
		}
		f.entities = append(f.entities, map[string]any{"entity_id": id, "device_id": dev, "entity_category": cat, "platform": "demo"})
	}
	log := func(at time.Time, id, name, state, msg string) {
		f.logbook = append(f.logbook, map[string]any{"when": ts(at), "entity_id": id, "name": name, "state": state, "message": msg})
	}

	for _, p := range []string{"alex", "sam", "kid"} {
		addEntity("person."+p, "home", strings.ToUpper(p[:1])+p[1:], "", "", nil)
	}
	for i, a := range areas {
		addEntity(fmt.Sprintf("light.%s", a), []string{"on", "off"}[i%2], strings.ReplaceAll(a, "_", " ")+" light", a, "", nil)
		addEntity(fmt.Sprintf("sensor.%s_temperature", a), "70.5", a+" temperature", a, "", map[string]any{"device_class": "temperature", "unit_of_measurement": "°F", "state_class": "measurement"})
	}
	addEntity("cover.garage_door", "closed", "Garage Door", "garage", "", map[string]any{"device_class": "garage"})
	addEntity("binary_sensor.side_door", "on", "Side Door", "garage", "", map[string]any{"device_class": "door"})
	addEntity("binary_sensor.back_window", "off", "Back Window", "living_room", "", map[string]any{"device_class": "window"})
	addEntity("sensor.heat_pump_power", "0", "Heat Pump Power", "", "", map[string]any{"device_class": "power", "unit_of_measurement": "W", "state_class": "measurement"})
	for i, c := range []string{"house", "hot_water", "dryer", "oven", "dishwasher", "well_pump", "heat_pump", "spare_a", "spare_b"} {
		id := "sensor." + c + "_energy_today"
		addEntity(id, "1", c+" energy today", "", "", map[string]any{"device_class": "energy", "unit_of_measurement": "kWh", "state_class": "total_increasing"})
		var rows []map[string]any
		for d := 0; d < 7; d++ {
			change := float64(100/(i+1)) / 7
			if strings.HasPrefix(c, "spare") {
				change = 0
			}
			start := now.Add(time.Duration(-7+d) * 24 * time.Hour)
			rows = append(rows, map[string]any{"start": float64(start.UnixMilli()), "end": float64(start.Add(24 * time.Hour).UnixMilli()), "change": change, "sum": 1000.0, "state": 1.0})
		}
		f.stats[id] = rows
	}
	// Filler: primary sensors and switches, a third unavailable.
	for i := 0; i < 330; i++ {
		state := "12.5"
		if i%3 == 0 {
			state = "unavailable"
		}
		addEntity(fmt.Sprintf("sensor.misc_%d", i), state, fmt.Sprintf("Misc sensor %d", i), areas[i%len(areas)], "", map[string]any{"unit_of_measurement": "W"})
	}
	// Diagnostic and config entities: signal strength, firmware, restart
	// buttons, network trackers.
	for i := 0; i < 300; i++ {
		addEntity(fmt.Sprintf("sensor.device_%d_rssi", i), "-60", fmt.Sprintf("Device %d signal", i), areas[i%len(areas)], "diagnostic", nil)
	}
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("device_tracker.host_%d", i)
		addEntity(id, "home", fmt.Sprintf("host-%d", i), "", "diagnostic", nil)
		for k := 0; k < 4; k++ {
			log(now.Add(-time.Duration(k*5+i%5)*time.Hour), id, fmt.Sprintf("host-%d", i), []string{"home", "not_home"}[k%2], "")
		}
	}
	for _, cam := range []string{"driveway", "garage"} {
		id := "binary_sensor." + cam + "_motion"
		addEntity(id, "off", cam+" motion", cam, "", map[string]any{"device_class": "motion"})
		for k := 0; k < 290; k++ {
			log(now.Add(-24*time.Hour+time.Duration(k)*5*time.Minute), id, cam+" motion", []string{"on", "off"}[k%2], "")
		}
	}
	// The story: everyone leaves, the garage door closes behind them, an
	// automation runs, lights change.
	day := now.Add(-24 * time.Hour)
	log(day.Add(10*time.Hour), "person.alex", "Alex", "not_home", "")
	log(day.Add(10*time.Hour+2*time.Minute), "cover.garage_door", "Garage Door", "open", "")
	log(day.Add(10*time.Hour+5*time.Minute), "person.sam", "Sam", "not_home", "")
	log(day.Add(10*time.Hour+9*time.Minute), "cover.garage_door", "Garage Door", "closed", "")
	log(day.Add(18*time.Hour), "automation.lights_at_dusk", "Lights at dusk", "", "triggered by sun event sunset")
	for i, a := range areas {
		log(day.Add(18*time.Hour+time.Duration(i)*time.Minute), "light."+a, a+" light", "on", "")
	}
	// The heat pump drew 1500 W from 22:00 to 06:00 local, sampled every
	// minute.
	var series []map[string]any
	start := now.Add(-24 * time.Hour)
	series = append(series, map[string]any{"entity_id": "sensor.heat_pump_power", "state": "0", "last_changed": ts(start),
		"attributes": map[string]any{"friendly_name": "Heat Pump Power", "unit_of_measurement": "W"}})
	for m := 1; m < 24*60; m++ {
		at := start.Add(time.Duration(m) * time.Minute)
		local := at.In(mustLoad("America/New_York"))
		v := "3.2"
		if local.Hour() >= 22 || local.Hour() < 6 {
			v = "1500.4"
		}
		series = append(series, map[string]any{"state": v, "last_changed": ts(at)})
	}
	f.history["sensor.heat_pump_power"] = series
	return f
}

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// householdBudget is the target the question set is held to: each common
// question answered in at most two calls and ~5k tokens of tool output.
const (
	budgetCalls = 2
	budgetBytes = 20000 // ~5k tokens at 4 bytes per token
)

// TestHouseholdQuestionBudget runs the question set against the synthetic
// home and holds each answer to the budget, then checks the answers carry
// what the question needs. It is the regression guard for the result
// shaping: a change that inflates a result or hides the answer fails here.
func TestHouseholdQuestionBudget(t *testing.T) {
	f := syntheticFixture()
	cs := replaySession(t, f)
	questions := []replayQuestion{
		{"anyone home", []replayCall{{"get_home_states", map[string]any{"domain": "person"}}}},
		{"lights on", []replayCall{{"get_home_states", map[string]any{"domain": "light", "state": "on"}}}},
		{"garage after we left", []replayCall{{"get_home_events", map[string]any{"domain": "cover,person", "hours": 24}}}},
		{"heating all night", []replayCall{
			{"get_home_states", map[string]any{"search": "heat", "device_class": "power"}},
			{"get_state_history", map[string]any{"entity_id": "sensor.heat_pump_power", "hours": 24}},
		}},
		{"most energy this week", []replayCall{{"get_long_term_statistics", map[string]any{"search": "energy_today"}}}},
		{"doors open", []replayCall{{"get_home_states", map[string]any{"device_class": "door,window,garage_door,garage,opening", "state": "on,open"}}}},
		{"what happened today", []replayCall{{"get_home_events", map[string]any{"hours": 24}}}},
		{"kitchen", []replayCall{{"get_home_states", map[string]any{"area": "Kitchen"}}}},
		{"overview", []replayCall{{"get_home_states", map[string]any{}}}},
		{"rooms", []replayCall{{"get_home_registry", map[string]any{}}}},
		{"version", []replayCall{{"get_home_config", map[string]any{}}}},
	}
	outs := runQuestions(t, cs, questions)
	logOutcomes(t, outs)
	answers := map[string]string{}
	for _, o := range outs {
		if o.errored {
			t.Errorf("%s: tool error: %s", o.q, strings.Join(o.texts, " "))
		}
		if o.calls > budgetCalls || o.bytes > budgetBytes {
			t.Errorf("%s: %d calls, %d bytes; budget is %d calls, %d bytes", o.q, o.calls, o.bytes, budgetCalls, budgetBytes)
		}
		answers[o.q] = strings.Join(o.texts, "\n")
	}

	expect := func(q string, subs ...string) {
		t.Helper()
		for _, s := range subs {
			if !strings.Contains(answers[q], s) {
				t.Errorf("%s: answer lacks %q:\n%s", q, s, answers[q])
			}
		}
	}
	expect("anyone home", `"person.alex"`, `"home"`)
	// Departure then the door closing, in local time, in order.
	expect("garage after we left", `"person.alex","not_home"`, `"cover.garage_door","closed"`, "-04:00")
	// 1500 W overnight shows up in the night buckets, local time.
	expect("heating all night", `"bucket":"1h"`, `T23:00:00-04:00",1500.4,1500.4,1500.4`, `T12:00:00-04:00",3.2,3.2,3.2`)
	// Ranked with the biggest consumer first and idle counters dropped.
	if a := answers["most energy this week"]; strings.Index(a, "house_energy_today") > strings.Index(a, "hot_water_energy_today") || strings.Contains(a, "spare_a") {
		t.Errorf("energy ranking wrong:\n%s", a)
	}
	expect("doors open", "binary_sensor.side_door")
	if strings.Contains(answers["doors open"], "back_window") {
		t.Error("closed window listed as open")
	}
	expect("what happened today", "automation.lights_at_dusk", `"binary_sensor.driveway_motion",289`, "omitted_diagnostic")
	if strings.Contains(answers["what happened today"], "device_tracker.host_") {
		t.Error("network trackers leaked into the whole-house logbook")
	}
	expect("kitchen", "light.kitchen", "sensor.kitchen_temperature", `"area":"Kitchen"`)
	expect("overview", `"domains"`, `"light":8`)
	expect("rooms", `"entities_per_area"`, `"area_id":"garage"`)
	expect("version", "2026.10.1", "300 loaded")
}
