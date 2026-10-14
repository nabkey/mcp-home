package hass

import (
	"sort"
	"strings"
)

// Logbook shaping for get_home_events.
//
// A day of logbook on the live instance was 2,000+ entries (~300 KB): network
// device trackers coming and going, camera motion sensors toggling every
// minute, buttons and update entities going unavailable. The few entries a
// household question is about — a door, a person, an automation — were lost
// in it. So by default entries for diagnostic/config entities are dropped;
// when what is left still exceeds the row cap, the chattiest entities are
// folded into one summary row each; and only then are the oldest rows cut.
// Every omission is counted so the model narrows instead of concluding that
// nothing happened. Rows are arrays, with each entity's name given once.

const (
	// busyThreshold is the fewest entries an entity must have to be folded.
	busyThreshold = 6
	// maxLogbookRows caps the listed entries; the newest are kept.
	maxLogbookRows = 100
)

// logbookFilter is the get_home_events selection.
type logbookFilter struct {
	domains, entityIDs []string
	search, area       string
	all                bool
}

// nameRestatesID reports whether a friendly name adds nothing to the entity
// id: "Night Light" for light.night_light, or "Office Light" for
// light.office_light_2.
func nameRestatesID(entityID, name string) bool {
	_, object, ok := strings.Cut(entityID, ".")
	if !ok {
		return false
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "_"):
			b.WriteByte('_')
		}
	}
	s := strings.Trim(b.String(), "_")
	return s != "" && strings.HasPrefix(object, s) && len(object)-len(s) <= 2
}

// shapeLogbook filters, folds and caps entries (oldest first, as Home
// Assistant returns them) into the get_home_events result.
func shapeLogbook(h *homeContext, entries []LogbookEntry, f logbookFilter) map[string]any {
	ids := make(map[string]bool, len(f.entityIDs))
	for _, id := range f.entityIDs {
		ids[id] = true
	}
	search := strings.ToLower(f.search)
	secondary := 0
	kept := make([]LogbookEntry, 0, len(entries))
	for _, e := range entries {
		info := h.entities[e.EntityID]
		if len(ids) > 0 && !ids[e.EntityID] {
			continue
		}
		if len(f.domains) > 0 {
			d := e.Domain
			if e.EntityID != "" {
				d = strings.SplitN(e.EntityID, ".", 2)[0]
			}
			if !containsFold(f.domains, d) {
				continue
			}
		}
		if f.area != "" && info.area != f.area {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(e.EntityID), search) &&
			!strings.Contains(strings.ToLower(e.Name), search) &&
			!strings.Contains(strings.ToLower(e.Message), search) &&
			!strings.Contains(strings.ToLower(h.areaNames[info.area]), search) {
			continue
		}
		if !f.all && len(ids) == 0 && (info.category != "" || info.hidden) {
			secondary++
			continue
		}
		kept = append(kept, e)
	}

	// Entries without an entity (device triggers, integration events) are
	// keyed by their name so they fold and are named like entities.
	key := func(e LogbookEntry) string {
		if e.EntityID != "" {
			return e.EntityID
		}
		return e.Name
	}
	counts := map[string]int{}
	for _, e := range kept {
		counts[key(e)]++
	}

	// Fold the chattiest entities until the rest fit. An unfiltered query
	// also folds anything at or over busyThreshold: in a whole-house view an
	// entity changing that often is a motion sensor or a flapping device, not
	// the story. A filtered one keeps them, since the filter chose them. An
	// entity asked for by id is never folded.
	narrowed := len(f.domains) > 0 || len(ids) > 0 || f.search != "" || f.area != ""
	folded := map[string]bool{}
	remaining := len(kept)
	if remaining > maxLogbookRows || !narrowed {
		keys := make([]string, 0, len(counts))
		for k := range counts {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if counts[keys[i]] != counts[keys[j]] {
				return counts[keys[i]] > counts[keys[j]]
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys {
			if counts[k] < busyThreshold || (narrowed && remaining <= maxLogbookRows) {
				break
			}
			if ids[k] {
				continue
			}
			folded[k] = true
			remaining -= counts[k]
		}
	}

	names := map[string]string{}
	rows := make([][]string, 0, remaining)
	type fold struct {
		entries            int
		first, last, state string
	}
	folds := map[string]*fold{}
	var foldOrder []string
	for _, e := range kept {
		k := key(e)
		when := h.localTime(e.When)
		if folded[k] {
			fd, ok := folds[k]
			if !ok {
				fd = &fold{first: when}
				folds[k] = fd
				foldOrder = append(foldOrder, k)
			}
			fd.entries++
			fd.last = when
			if e.State != "" {
				fd.state = e.State
			}
			if e.Name != "" && e.EntityID != "" {
				names[k] = e.Name
			}
			continue
		}
		what := e.State
		if e.Message != "" {
			what = e.Message
		}
		rows = append(rows, []string{when, k, what})
		if e.Name != "" && e.EntityID != "" {
			names[k] = e.Name
		}
	}

	out := map[string]any{"columns": []string{"when", "entity", "state or message"}}
	if len(rows) > maxLogbookRows {
		out["omitted_older"] = len(rows) - maxLogbookRows
		rows = rows[len(rows)-maxLogbookRows:]
	}
	out["events"] = rows
	if len(foldOrder) > 0 {
		list := make([][]any, 0, len(foldOrder))
		for _, k := range foldOrder {
			fd := folds[k]
			list = append(list, []any{k, fd.entries, fd.first, fd.last, fd.state})
		}
		out["frequent"] = list
		out["frequent_columns"] = []string{"entity", "entries", "first", "last", "last_state"}
	}
	// Only names that are not just the entity id restated are worth listing.
	used := map[string]bool{}
	for _, r := range rows {
		used[r[1]] = true
	}
	for _, k := range foldOrder {
		used[k] = true
	}
	shown := map[string]string{}
	for k, n := range names {
		if used[k] && !nameRestatesID(k, n) {
			shown[k] = n
		}
	}
	if len(shown) > 0 {
		out["names"] = shown
	}
	if secondary > 0 {
		out["omitted_diagnostic"] = secondary
	}
	return out
}
