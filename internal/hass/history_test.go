package hass

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func rawSeries(id string, attrs map[string]any, start time.Time, pts ...any) []map[string]any {
	out := []map[string]any{{"entity_id": id, "attributes": attrs, "state": pts[1], "last_changed": start.Format(time.RFC3339)}}
	for i := 2; i+1 < len(pts); i += 2 {
		out = append(out, map[string]any{"last_changed": pts[i].(time.Time).Format(time.RFC3339Nano), "state": pts[i+1]})
	}
	return out
}

func TestSummarizeStateSeries(t *testing.T) {
	h := testHome(t)
	start := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC) // 22:00 local
	end := start.Add(10 * time.Hour)
	s := rawSeries("cover.garage", map[string]any{"friendly_name": "Garage"}, start,
		start, "closed",
		start.Add(time.Hour), "open",
		start.Add(time.Hour+30*time.Minute), "open", // repeated state is not a change
		start.Add(2*time.Hour), "closed",
	)
	got := summarizeSeries(h, s, start, end)
	b, _ := json.Marshal(got)
	want := `{"changes":[["2026-10-12T22:00:00-04:00","closed"],["2026-10-12T23:00:00-04:00","open"],["2026-10-13T00:00:00-04:00","closed"]],` +
		`"entity_id":"cover.garage","last":"closed","name":"Garage","time_in_state":{"closed":"9h","open":"1h"}}`
	if string(b) != want {
		t.Errorf("got  %s\nwant %s", b, want)
	}
}

func TestSummarizeNumericSeriesBuckets(t *testing.T) {
	h := testHome(t)
	start := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	end := start.Add(12 * time.Hour)
	// A power sensor sampled every minute: 1500 W for the first two hours,
	// then 0, with a 30-minute unavailable gap.
	pts := []any{start, "1500"}
	for m := 1; m < 12*60; m++ {
		at := start.Add(time.Duration(m) * time.Minute)
		v := "0"
		switch {
		case m < 120:
			v = "1500"
		case m >= 300 && m < 330:
			v = "unavailable"
		}
		pts = append(pts, at, v)
	}
	s := rawSeries("sensor.heat_pump_power", map[string]any{"friendly_name": "Heat Pump", "unit_of_measurement": "W"}, start, pts...)
	got := summarizeSeries(h, s, start, end)

	if got["bucket"] != "30m" {
		t.Errorf("bucket = %v, want 30m for a 12h window", got["bucket"])
	}
	buckets := got["buckets"].([][]any)
	if len(buckets) > targetBuckets+1 {
		t.Errorf("%d buckets, want at most %d", len(buckets), targetBuckets+1)
	}
	if first := buckets[0]; first[0] != "2026-10-12T22:00:00-04:00" || first[2] != 1500.0 {
		t.Errorf("first bucket = %v, want local 22:00 with mean 1500", first)
	}
	// Two hours at 1500 W out of 11.5 available hours.
	if mean := got["mean"].(float64); mean < 260 || mean > 262 {
		t.Errorf("mean = %v, want ~260.87 (time-weighted, excluding unavailable)", mean)
	}
	if got["unavailable_for"] != "30m" {
		t.Errorf("unavailable_for = %v", got["unavailable_for"])
	}
	b, _ := json.Marshal(got)
	if len(b) > 2500 {
		t.Errorf("summary is %d bytes for 720 samples, want under 2500", len(b))
	}
}

func TestSummarizeNumericSeriesFewPoints(t *testing.T) {
	h := testHome(t)
	start := time.Date(2026, 10, 13, 2, 0, 0, 0, time.UTC)
	s := rawSeries("sensor.t", map[string]any{"unit_of_measurement": "°F"}, start,
		start, "70.123456", start.Add(time.Hour), "72")
	got := summarizeSeries(h, s, start, start.Add(2*time.Hour))
	b, _ := json.Marshal(got["points"])
	if string(b) != `[["2026-10-12T22:00:00-04:00",70.12],["2026-10-12T23:00:00-04:00",72]]` {
		t.Errorf("points = %s", b)
	}
	if got["mean"] != 71.06 {
		t.Errorf("mean = %v", got["mean"])
	}
}

func TestSummarizeStateSeriesCapsChanges(t *testing.T) {
	h := testHome(t)
	start := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	pts := []any{start, "off"}
	for i := 1; i <= 200; i++ {
		pts = append(pts, start.Add(time.Duration(i)*time.Minute), map[bool]string{true: "on", false: "off"}[i%2 == 1])
	}
	got := summarizeSeries(h, rawSeries("binary_sensor.motion", nil, start, pts...), start, start.Add(4*time.Hour))
	if n := len(got["changes"].([][]any)); n != maxChanges {
		t.Errorf("%d changes, want %d", n, maxChanges)
	}
	if got["omitted_changes"] != 201-maxChanges {
		t.Errorf("omitted_changes = %v", got["omitted_changes"])
	}
}

func TestBucketSize(t *testing.T) {
	for window, want := range map[time.Duration]time.Duration{
		2 * time.Hour:       5 * time.Minute,
		12 * time.Hour:      30 * time.Minute,
		24 * time.Hour:      time.Hour,
		7 * 24 * time.Hour:  12 * time.Hour,
		60 * 24 * time.Hour: 24 * time.Hour,
	} {
		if got := bucketSize(window); got != want {
			t.Errorf("bucketSize(%v) = %v, want %v", window, got, want)
		}
	}
}

func TestRoundNum(t *testing.T) {
	for in, want := range map[float64]float64{1571.1534: 1571.15, 0.0123456: 0.0123, 0.527187: 0.527, -3.14159: -3.14, 0: 0} {
		if got := roundNum(in); got != want {
			t.Errorf("roundNum(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestResolveWindow(t *testing.T) {
	h := testHome(t)
	now := time.Date(2026, 10, 14, 2, 0, 0, 0, time.UTC)
	cases := []struct {
		hours      int
		start, end string
		want       string
	}{
		{0, "", "", "2026-10-13T02:00:00Z/2026-10-14T02:00:00Z"},
		{500, "", "", "2026-10-04T02:00:00Z/2026-10-14T02:00:00Z"},
		{0, "2026-10-13 22:00", "2026-10-14 07:00", "2026-10-14T02:00:00Z/2026-10-14T11:00:00Z"},
	}
	for _, c := range cases {
		s, e, err := resolveWindow(h, now, c.hours, maxHistoryHours, c.start, c.end, 24)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%s/%s", s.UTC().Format(time.RFC3339), e.UTC().Format(time.RFC3339)); got != c.want {
			t.Errorf("window(%d,%q,%q) = %s, want %s", c.hours, c.start, c.end, got, c.want)
		}
	}
	if _, _, err := resolveWindow(h, now, 0, 24, "yesterday", "", 24); err == nil || !strings.Contains(err.Error(), "2026-10-13 22:00") {
		t.Errorf("unparseable start: err = %v, want a format hint", err)
	}
	if _, _, err := resolveWindow(h, now, 0, 24, "2026-10-14 10:00", "2026-10-14 09:00", 24); err == nil {
		t.Error("start after end accepted")
	}
}
