package hass

import (
	"sort"
	"strings"
	"time"
)

// Slim projections of Home Assistant payloads for tool results.
//
// The raw REST/WebSocket records are built for the frontend: an entity
// registry entry carries ids, timestamps, options blobs and translation keys
// that an LLM never needs, and a full state dump of a mid-sized home is a
// few hundred kilobytes. Tool results go straight into a model's context,
// so by default the tools return only the fields an agent acts on and offer
// a flag for the full record.

// SlimState is the default per-entity record from get_home_states.
type SlimState struct {
	EntityID    string    `json:"entity_id"`
	State       string    `json:"state"`
	Name        string    `json:"name,omitempty"`
	Unit        string    `json:"unit,omitempty"`
	LastChanged time.Time `json:"last_changed"`
}

// slimStates projects states to their essentials.
func slimStates(states []State) []SlimState {
	out := make([]SlimState, 0, len(states))
	for _, s := range states {
		name, _ := s.Attributes["friendly_name"].(string)
		unit, _ := s.Attributes["unit_of_measurement"].(string)
		out = append(out, SlimState{EntityID: s.EntityID, State: s.State, Name: name, Unit: unit, LastChanged: s.LastChanged})
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

// filterStates applies the get_home_states filters: domains (entity_id
// prefix), exact entity ids, and a case-insensitive substring match on
// entity_id or friendly_name. Empty filters match everything.
func filterStates(states []State, domains, entityIDs []string, search string) []State {
	ids := make(map[string]bool, len(entityIDs))
	for _, id := range entityIDs {
		ids[id] = true
	}
	search = strings.ToLower(search)
	out := make([]State, 0, len(states))
	for _, s := range states {
		if len(domains) > 0 && !hasDomain(s.EntityID, domains) {
			continue
		}
		if len(ids) > 0 && !ids[s.EntityID] {
			continue
		}
		if search != "" {
			name, _ := s.Attributes["friendly_name"].(string)
			if !strings.Contains(strings.ToLower(s.EntityID), search) && !strings.Contains(strings.ToLower(name), search) {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

func hasDomain(entityID string, domains []string) bool {
	for _, d := range domains {
		if strings.HasPrefix(entityID, d+".") {
			return true
		}
	}
	return false
}

// pick copies the named keys from rec, dropping empty strings, nils and
// empty lists so the result carries only what is set.
func pick(rec map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		v, ok := rec[k]
		if !ok || v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			if t == "" {
				continue
			}
		case []any:
			if len(t) == 0 {
				continue
			}
		}
		out[k] = v
	}
	return out
}

func str(rec map[string]any, key string) string {
	s, _ := rec[key].(string)
	return s
}

// slimAreas keeps area_id, name, floor_id and aliases.
func slimAreas(areas []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(areas))
	for _, a := range areas {
		out = append(out, pick(a, "area_id", "name", "floor_id", "aliases"))
	}
	return out
}

// slimFloors keeps floor_id, name, level and aliases.
func slimFloors(floors []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(floors))
	for _, f := range floors {
		out = append(out, pick(f, "floor_id", "name", "level", "aliases"))
	}
	return out
}

// slimLabels keeps label_id and name.
func slimLabels(labels []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(labels))
	for _, l := range labels {
		out = append(out, pick(l, "label_id", "name"))
	}
	return out
}

// slimDevices keeps the identifying fields; name is the user-assigned name
// when one exists, else the integration's.
func slimDevices(devices []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(devices))
	for _, d := range devices {
		rec := pick(d, "id", "area_id", "manufacturer", "model", "via_device_id", "disabled_by", "labels")
		if n := str(d, "name_by_user"); n != "" {
			rec["name"] = n
		} else if n := str(d, "name"); n != "" {
			rec["name"] = n
		}
		out = append(out, rec)
	}
	return out
}

// slimEntities keeps the fields an agent maps rooms and devices with. The
// area_id is the effective one: an entity without its own area inherits its
// device's, which is how Home Assistant resolves it too. devices may be nil,
// in which case no inheritance happens.
func slimEntities(entities, devices []map[string]any) []map[string]any {
	deviceArea := make(map[string]string, len(devices))
	for _, d := range devices {
		if id := str(d, "id"); id != "" {
			deviceArea[id] = str(d, "area_id")
		}
	}
	out := make([]map[string]any, 0, len(entities))
	for _, e := range entities {
		rec := pick(e, "entity_id", "area_id", "device_id", "platform", "entity_category", "disabled_by", "hidden_by", "labels")
		if n := str(e, "name"); n != "" {
			rec["name"] = n
		} else if n := str(e, "original_name"); n != "" {
			rec["name"] = n
		}
		if _, ok := rec["area_id"]; !ok {
			if a := deviceArea[str(e, "device_id")]; a != "" {
				rec["area_id"] = a
			}
		}
		out = append(out, rec)
	}
	return out
}

// filterByArea keeps records whose area_id equals areaID. Empty areaID
// matches everything.
func filterByArea(recs []map[string]any, areaID string) []map[string]any {
	if areaID == "" {
		return recs
	}
	out := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		if str(r, "area_id") == areaID {
			out = append(out, r)
		}
	}
	return out
}

// filterEntitiesByDomain keeps registry entries whose entity_id is in one of
// domains. Empty domains matches everything.
func filterEntitiesByDomain(recs []map[string]any, domains []string) []map[string]any {
	if len(domains) == 0 {
		return recs
	}
	out := make([]map[string]any, 0, len(recs))
	for _, r := range recs {
		if hasDomain(str(r, "entity_id"), domains) {
			out = append(out, r)
		}
	}
	return out
}

// serviceNames reduces the GET /api/services payload to domain → sorted
// service names, for discovery without every field schema.
func serviceNames(services []map[string]any) map[string][]string {
	out := make(map[string][]string, len(services))
	for _, s := range services {
		domain := str(s, "domain")
		if domain == "" {
			continue
		}
		svcs, _ := s["services"].(map[string]any)
		names := make([]string, 0, len(svcs))
		for name := range svcs {
			names = append(names, name)
		}
		sort.Strings(names)
		out[domain] = names
	}
	return out
}
