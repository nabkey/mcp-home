package hass

import (
	"reflect"
	"testing"
	"time"
)

func TestShapeStatistics(t *testing.T) {
	h := testHome(t)
	day := func(d int) float64 {
		return float64(time.Date(2026, 10, 10+d, 4, 0, 0, 0, time.UTC).UnixMilli())
	}
	raw := map[string]any{
		"sensor.dryer_energy": []any{
			map[string]any{"start": day(0), "end": day(1), "change": 4.5},
			map[string]any{"start": day(1), "end": day(2), "change": 1.25},
		},
		"sensor.oven_energy": []any{
			map[string]any{"start": day(0), "end": day(1), "change": 9.0},
		},
		"sensor.temp": []any{
			map[string]any{"start": day(0), "end": day(1), "mean": 20.0, "min": 18.0, "max": 23.0},
			map[string]any{"start": day(1), "end": day(2), "mean": 22.0, "min": 19.0, "max": 25.5},
		},
	}
	states := []State{
		{EntityID: "sensor.dryer_energy", Attributes: map[string]any{"friendly_name": "Dryer", "unit_of_measurement": "kWh"}},
	}

	got := shapeStatistics(h, raw, states, true)
	if ids := []string{str(got[0], "statistic_id"), str(got[1], "statistic_id"), str(got[2], "statistic_id")}; !reflect.DeepEqual(ids, []string{"sensor.oven_energy", "sensor.dryer_energy", "sensor.temp"}) {
		t.Errorf("order = %v, want ranked by total_change", ids)
	}
	dryer := got[1]
	if dryer["total_change"] != 5.75 || dryer["unit"] != "kWh" || dryer["name"] != "Dryer" {
		t.Errorf("dryer = %v", dryer)
	}
	if rows := dryer["rows"].([][]any); rows[0][0] != "2026-10-10T00:00:00-04:00" || rows[0][1] != 4.5 {
		t.Errorf("rows = %v, want local-time starts", rows)
	}
	temp := got[2]
	if temp["mean"] != 21.0 || temp["min"] != 18.0 || temp["max"] != 25.5 {
		t.Errorf("temp = %v", temp)
	}

	ranked := shapeStatistics(h, raw, states, false)
	if _, ok := ranked[0]["rows"]; ok {
		t.Error("ranking kept per-period rows")
	}
}

func TestStatisticCandidates(t *testing.T) {
	h := testHome(t)
	states := []State{
		{EntityID: "sensor.fridge_power", Attributes: map[string]any{"state_class": "measurement", "friendly_name": "Fridge"}},
		{EntityID: "sensor.fridge_rssi", Attributes: map[string]any{"state_class": "measurement"}},
		{EntityID: "sensor.dryer_energy_today", Attributes: map[string]any{"state_class": "total_increasing"}},
		{EntityID: "sensor.energy_price", Attributes: map[string]any{}},
	}
	if got := statisticCandidates(h, states, "energy"); !reflect.DeepEqual(got, []string{"sensor.dryer_energy_today"}) {
		t.Errorf("energy = %v (no state_class means no statistics)", got)
	}
	// By area name, skipping the diagnostic signal sensor.
	if got := statisticCandidates(h, states, "kitchen"); !reflect.DeepEqual(got, []string{"sensor.fridge_power"}) {
		t.Errorf("kitchen = %v", got)
	}
}

func TestDefaultPeriod(t *testing.T) {
	for w, want := range map[time.Duration]string{24 * time.Hour: "hour", 7 * 24 * time.Hour: "day", 365 * 24 * time.Hour: "month"} {
		if got := defaultPeriod(w); got != want {
			t.Errorf("defaultPeriod(%v) = %s, want %s", w, got, want)
		}
	}
}
