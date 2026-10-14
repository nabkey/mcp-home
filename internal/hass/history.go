package hass

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"
)

// History summarisation for get_state_history.
//
// The recorder returns every sample. A power sensor reporting each minute is
// ~800 points a day at ~80 bytes each, so two sensors over one night was
// ~75 KB of output on the live instance — far past what a model can use, and
// the question ("was the heating on all night?") needs only the shape. So
// numeric series are reduced to time-weighted statistics, bucketed when long,
// and state series to their transitions plus time spent in each state.

const (
	// maxNumericPoints is the most raw numeric samples returned before a
	// series is bucketed instead.
	maxNumericPoints = 24
	// targetBuckets is roughly how many buckets a long numeric series becomes.
	targetBuckets = 24
	// maxChanges caps the transitions listed for a state series; the most
	// recent are kept.
	maxChanges = 60
)

// bucketSizes are the bucket widths a series may use, smallest first. Each
// divides a day so buckets align to the local clock.
var bucketSizes = []time.Duration{
	5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

func bucketSize(window time.Duration) time.Duration {
	for _, s := range bucketSizes {
		if window/s <= targetBuckets {
			return s
		}
	}
	return bucketSizes[len(bucketSizes)-1]
}

// histPoint is one recorder sample.
type histPoint struct {
	at    time.Time
	state string
}

// isUnset reports states that carry no value.
func isUnset(s string) bool {
	return s == "unavailable" || s == "unknown" || s == ""
}

// roundNum keeps a number readable: two decimals, or three significant digits
// below 1.
func roundNum(v float64) float64 {
	if v == 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	if math.Abs(v) >= 1 {
		return math.Round(v*100) / 100
	}
	p := math.Pow(10, 2-math.Floor(math.Log10(math.Abs(v))))
	return math.Round(v*p) / p
}

// fmtDuration renders a duration as e.g. 7h12m, 45m or 30s.
func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour:
		h := int(d / time.Hour)
		m := int((d % time.Hour) / time.Minute)
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}

// parsePoints turns one raw recorder series into ordered samples. The first
// sample is the state in force at the window start.
func parsePoints(series []map[string]any) []histPoint {
	out := make([]histPoint, 0, len(series))
	for _, p := range series {
		ts := str(p, "last_changed")
		if ts == "" {
			ts = str(p, "last_updated")
		}
		at, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			continue
		}
		out = append(out, histPoint{at: at, state: str(p, "state")})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// summarizeSeries reduces one raw recorder series over [start, end].
func summarizeSeries(h *homeContext, series []map[string]any, start, end time.Time) map[string]any {
	out := map[string]any{}
	if len(series) == 0 {
		return out
	}
	first := series[0]
	out["entity_id"] = str(first, "entity_id")
	attrs, _ := first["attributes"].(map[string]any)
	if n, _ := attrs["friendly_name"].(string); n != "" {
		out["name"] = n
	}
	unit, _ := attrs["unit_of_measurement"].(string)
	if unit != "" {
		out["unit"] = unit
	}
	pts := parsePoints(series)
	if len(pts) == 0 {
		return out
	}
	if pts[0].at.Before(start) {
		pts[0].at = start
	}

	numeric := false
	allNumeric := true
	for _, p := range pts {
		if isUnset(p.state) {
			continue
		}
		if _, err := strconv.ParseFloat(p.state, 64); err != nil {
			allNumeric = false
			break
		}
		numeric = true
	}
	if numeric && allNumeric {
		summarizeNumeric(h, out, pts, start, end)
	} else {
		summarizeStates(h, out, pts, end)
	}
	return out
}

// summarizeNumeric adds time-weighted min/mean/max, the latest value, and
// either the raw points or per-bucket statistics.
func summarizeNumeric(h *homeContext, out map[string]any, pts []histPoint, start, end time.Time) {
	type seg struct {
		from, to time.Time
		v        float64
		ok       bool
	}
	segs := make([]seg, 0, len(pts))
	for i, p := range pts {
		to := end
		if i+1 < len(pts) {
			to = pts[i+1].at
		}
		if !to.After(p.at) {
			continue
		}
		v, err := strconv.ParseFloat(p.state, 64)
		segs = append(segs, seg{from: p.at, to: to, v: v, ok: err == nil})
	}

	type acc struct {
		min, max, integral float64
		covered            time.Duration
		n                  int
	}
	add := func(a *acc, v float64, d time.Duration) {
		if a.n == 0 || v < a.min {
			a.min = v
		}
		if a.n == 0 || v > a.max {
			a.max = v
		}
		a.n++
		a.integral += v * d.Seconds()
		a.covered += d
	}
	var total acc
	var unavailable time.Duration
	for _, s := range segs {
		if s.ok {
			add(&total, s.v, s.to.Sub(s.from))
		} else {
			unavailable += s.to.Sub(s.from)
		}
	}
	for i := len(pts) - 1; i >= 0; i-- {
		if v, err := strconv.ParseFloat(pts[i].state, 64); err == nil {
			out["last"] = roundNum(v)
			out["last_changed"] = h.localTime(pts[i].at)
			break
		}
	}
	if total.n > 0 {
		out["min"] = roundNum(total.min)
		out["max"] = roundNum(total.max)
		if total.covered > 0 {
			out["mean"] = roundNum(total.integral / total.covered.Seconds())
		}
	}
	if unavailable > 0 {
		out["unavailable_for"] = fmtDuration(unavailable)
	}

	numericPts := 0
	for _, s := range segs {
		if s.ok {
			numericPts++
		}
	}
	if numericPts <= maxNumericPoints {
		points := make([][]any, 0, len(segs))
		for _, s := range segs {
			if s.ok {
				points = append(points, []any{h.localTime(s.from), roundNum(s.v)})
			} else {
				points = append(points, []any{h.localTime(s.from), nil})
			}
		}
		out["points"] = points
		return
	}

	size := bucketSize(end.Sub(start))
	local := start.In(h.loc)
	anchor := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, h.loc)
	first := anchor.Add(start.Sub(anchor) / size * size)
	n := int(end.Sub(first)/size) + 1
	buckets := make([]acc, n)
	for _, s := range segs {
		if !s.ok {
			continue
		}
		for t := s.from; t.Before(s.to); {
			i := int(t.Sub(first) / size)
			if i < 0 || i >= n {
				break
			}
			bEnd := first.Add(time.Duration(i+1) * size)
			to := s.to
			if bEnd.Before(to) {
				to = bEnd
			}
			add(&buckets[i], s.v, to.Sub(t))
			t = to
		}
	}
	rows := make([][]any, 0, n)
	for i, b := range buckets {
		if b.n == 0 || b.covered == 0 {
			continue
		}
		rows = append(rows, []any{h.localTime(first.Add(time.Duration(i) * size)), roundNum(b.min), roundNum(b.integral / b.covered.Seconds()), roundNum(b.max)})
	}
	out["bucket"] = fmtDuration(size)
	out["buckets"] = rows
}

// summarizeStates adds the transitions (state at the window start first) and
// the total time spent in each state.
func summarizeStates(h *homeContext, out map[string]any, pts []histPoint, end time.Time) {
	durations := map[string]time.Duration{}
	changes := make([][]any, 0, len(pts))
	prev := ""
	for i, p := range pts {
		to := end
		if i+1 < len(pts) {
			to = pts[i+1].at
		}
		if to.After(p.at) {
			durations[p.state] += to.Sub(p.at)
		}
		if i > 0 && p.state == prev {
			continue
		}
		changes = append(changes, []any{h.localTime(p.at), p.state})
		prev = p.state
	}
	if len(changes) > maxChanges {
		out["omitted_changes"] = len(changes) - maxChanges
		changes = changes[len(changes)-maxChanges:]
	}
	out["changes"] = changes
	out["last"] = pts[len(pts)-1].state
	d := make(map[string]string, len(durations))
	for s, v := range durations {
		d[s] = fmtDuration(v)
	}
	out["time_in_state"] = d
}
