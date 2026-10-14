package hass

import "strings"

// SlimState is the default per-entity record from get_home_states.
type SlimState struct {
	EntityID    string `json:"entity_id"`
	State       string `json:"state"`
	Name        string `json:"name,omitempty"`
	Unit        string `json:"unit,omitempty"`
	Area        string `json:"area,omitempty"`
	LastChanged string `json:"last_changed,omitempty"`
}

// slimStates projects states to their essentials, placing each in its area
// and stamping last_changed in the home's local time.
func slimStates(h *homeContext, states []State) []SlimState {
	out := make([]SlimState, 0, len(states))
	for _, s := range states {
		name, _ := s.Attributes["friendly_name"].(string)
		unit, _ := s.Attributes["unit_of_measurement"].(string)
		out = append(out, SlimState{
			EntityID:    s.EntityID,
			State:       s.State,
			Name:        name,
			Unit:        unit,
			Area:        h.areaNames[h.entities[s.EntityID].area],
			LastChanged: h.localTime(s.LastChanged),
		})
	}
	return out
}

// splitList parses a comma-separated argument into trimmed, non-empty items.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// stateFilter is the get_home_states selection. Empty fields match
// everything.
type stateFilter struct {
	domains, entityIDs, deviceClasses, states []string
	search                                    string
	area                                      string // area id, already resolved
	// all keeps diagnostic, config and hidden entities, which are otherwise
	// dropped unless named by entity_id.
	all bool
}

func (f stateFilter) empty() bool {
	return len(f.domains) == 0 && len(f.entityIDs) == 0 && len(f.deviceClasses) == 0 &&
		len(f.states) == 0 && f.search == "" && f.area == ""
}

// filterStates applies f, returning the kept states and how many otherwise
// matching entities were left out for being diagnostic, config or hidden.
// search is a case-insensitive substring of the entity_id, friendly name or
// area name.
func filterStates(h *homeContext, states []State, f stateFilter) (kept []State, secondary int) {
	ids := make(map[string]bool, len(f.entityIDs))
	for _, id := range f.entityIDs {
		ids[id] = true
	}
	search := strings.ToLower(f.search)
	kept = make([]State, 0, len(states))
	for _, s := range states {
		info := h.entities[s.EntityID]
		if len(f.domains) > 0 && !hasDomain(s.EntityID, f.domains) {
			continue
		}
		if len(ids) > 0 && !ids[s.EntityID] {
			continue
		}
		if f.area != "" && info.area != f.area {
			continue
		}
		if len(f.deviceClasses) > 0 {
			dc, _ := s.Attributes["device_class"].(string)
			if !containsFold(f.deviceClasses, dc) {
				continue
			}
		}
		if len(f.states) > 0 && !containsFold(f.states, s.State) {
			continue
		}
		if search != "" {
			name, _ := s.Attributes["friendly_name"].(string)
			if !strings.Contains(strings.ToLower(s.EntityID), search) &&
				!strings.Contains(strings.ToLower(name), search) &&
				!strings.Contains(strings.ToLower(h.areaNames[info.area]), search) {
				continue
			}
		}
		if !f.all && len(ids) == 0 && (info.category != "" || info.hidden) {
			secondary++
			continue
		}
		kept = append(kept, s)
	}
	return kept, secondary
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

// stateIndex summarises a whole state dump as primary-entity counts per
// domain and per area, for an unfiltered get_home_states call: enough to pick
// the next filter without paying for every entity.
func stateIndex(h *homeContext, states []State) map[string]any {
	domains := map[string]int{}
	areas := map[string]int{}
	for _, s := range states {
		info := h.entities[s.EntityID]
		if info.category != "" || info.hidden {
			continue
		}
		domains[strings.SplitN(s.EntityID, ".", 2)[0]]++
		if name := h.areaNames[info.area]; name != "" {
			areas[name]++
		}
	}
	out := map[string]any{"total_entities": len(states), "domains": domains}
	if len(areas) > 0 {
		out["areas"] = areas
	}
	return out
}
