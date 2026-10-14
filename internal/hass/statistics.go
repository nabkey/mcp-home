package hass

import (
	"sort"
	"strings"
	"time"
)

// Long-term statistics shaping for get_long_term_statistics.
//
// recorder/statistics_during_period returns, per period, start/end as epoch
// milliseconds plus state, sum, last_reset and change (or mean/min/max). A
// model asked "what used the most power this week" had to decode epoch
// milliseconds, notice that period=week buckets are calendar weeks that do not
// line up with "the last 168 hours", and add the changes up itself. The
// shaped result gives each statistic its window total (or mean/min/max),
// local-time period starts, and ranks a search by total.

// maxStatisticsSearch bounds how many statistics one search may fetch.
const maxStatisticsSearch = 60

var statisticPeriods = []string{"5minute", "hour", "day", "week", "month"}

// defaultPeriod picks a bucket that keeps a window to a few dozen rows.
func defaultPeriod(window time.Duration) string {
	switch {
	case window <= 48*time.Hour:
		return "hour"
	case window <= 62*24*time.Hour:
		return "day"
	default:
		return "month"
	}
}

// statisticCandidates returns the entity ids among states that the recorder
// keeps long-term statistics for (those with a state_class) and that match
// search on entity_id, name or area name. Diagnostic/config entities are left
// out.
func statisticCandidates(h *homeContext, states []State, search string) []string {
	search = strings.ToLower(search)
	var out []string
	for _, s := range states {
		if sc, _ := s.Attributes["state_class"].(string); sc == "" {
			continue
		}
		info := h.entities[s.EntityID]
		if info.category != "" || info.hidden {
			continue
		}
		name, _ := s.Attributes["friendly_name"].(string)
		if strings.Contains(strings.ToLower(s.EntityID), search) ||
			strings.Contains(strings.ToLower(name), search) ||
			strings.Contains(strings.ToLower(h.areaNames[info.area]), search) {
			out = append(out, s.EntityID)
		}
	}
	return out
}

func msTime(v any) time.Time {
	switch n := v.(type) {
	case float64:
		return time.UnixMilli(int64(n))
	case string:
		t, _ := time.Parse(time.RFC3339Nano, n)
		return t
	}
	return time.Time{}
}

// shapeStatistics turns the raw recorder result into one summary per
// statistic. withRows keeps the per-period rows; without them only window
// totals remain, ranked, which is what a "which uses the most" search wants.
func shapeStatistics(h *homeContext, raw map[string]any, states []State, withRows bool) []map[string]any {
	meta := make(map[string]State, len(states))
	for _, s := range states {
		meta[s.EntityID] = s
	}
	out := make([]map[string]any, 0, len(raw))
	for id, v := range raw {
		rowsRaw, _ := v.([]any)
		rec := map[string]any{"statistic_id": id}
		if s, ok := meta[id]; ok {
			if n, _ := s.Attributes["friendly_name"].(string); n != "" {
				rec["name"] = n
			}
			if u, _ := s.Attributes["unit_of_measurement"].(string); u != "" {
				rec["unit"] = u
			}
		}
		var total float64
		hasChange, hasMean := false, false
		var meanSum, minV, maxV float64
		meanN := 0
		rows := make([][]any, 0, len(rowsRaw))
		for _, r := range rowsRaw {
			m, ok := r.(map[string]any)
			if !ok {
				continue
			}
			start := h.localTime(msTime(m["start"]))
			if c, ok := m["change"].(float64); ok {
				hasChange = true
				total += c
				rows = append(rows, []any{start, roundNum(c)})
				continue
			}
			if mean, ok := m["mean"].(float64); ok {
				lo, _ := m["min"].(float64)
				hi, _ := m["max"].(float64)
				if meanN == 0 || lo < minV {
					minV = lo
				}
				if meanN == 0 || hi > maxV {
					maxV = hi
				}
				meanSum += mean
				meanN++
				hasMean = true
				rows = append(rows, []any{start, roundNum(mean), roundNum(lo), roundNum(hi)})
			}
		}
		switch {
		case hasChange:
			rec["total_change"] = roundNum(total)
			rec["columns"] = []string{"start", "change"}
		case hasMean:
			rec["mean"] = roundNum(meanSum / float64(meanN))
			rec["min"] = roundNum(minV)
			rec["max"] = roundNum(maxV)
			rec["columns"] = []string{"start", "mean", "min", "max"}
		}
		if withRows {
			rec["rows"] = rows
		} else {
			delete(rec, "columns")
		}
		out = append(out, rec)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ti, _ := out[i]["total_change"].(float64)
		tj, _ := out[j]["total_change"].(float64)
		if ti != tj {
			return ti > tj
		}
		return str(out[i], "statistic_id") < str(out[j], "statistic_id")
	})
	return out
}
