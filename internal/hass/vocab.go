package hass

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"unicode"
)

// The house vocabulary is the set of names a generated tool can be targeted
// with: the friendly names and aliases of entities exposed to Assist, and the
// names and aliases of areas and floors. Home Assistant's intent matcher
// compares these exactly (after casefolding), so a model that has to guess
// "the left garage door" for an entity called "Garage Door Controller Left
// Garage Door" misses, and then spends a round trip looking the name up.
//
// Upstream solves this by putting the exposed entities in the LLM prompt (the
// "Static Context" in homeassistant/components/homeassistant/llm.py). An MCP
// server cannot rely on a client showing its instructions, so the generated
// tools carry the relevant slice of the vocabulary in their own parameter
// descriptions instead, and the same data canonicalises near-miss spellings
// (curly apostrophes, a leading "the") before they reach Home Assistant.

// assistantID is the Assist assistant whose exposure settings the generated
// intent tools honour, matching what Home Assistant's own LLM API passes.
const assistantID = "conversation"

// maxVocabEntries caps how many entity names one tool description lists. An
// instance with more matching entities gets the first ones and a pointer to
// get_home_states for the rest.
const maxVocabEntries = 150

// defaultExposedDomains is upstream's DEFAULT_EXPOSED_DOMAINS: the domains
// Home Assistant exposes to Assist when an entity has no explicit setting.
var defaultExposedDomains = map[string]bool{
	"climate": true, "cover": true, "fan": true, "humidifier": true, "light": true,
	"media_player": true, "scene": true, "switch": true, "todo": true,
	"vacuum": true, "water_heater": true,
}

type vocabEntity struct {
	ID     string
	Domain string
	// Names are the strings Home Assistant matches a name slot against: the
	// computed full name and any aliases, in registry order.
	Names   []string
	Area    string // area name, empty when the entity has none
	AreaID  string
	Exposed bool
}

type vocabArea struct {
	ID      string
	Name    string
	Aliases []string
	FloorID string
}

type vocabFloor struct {
	ID      string
	Name    string
	Aliases []string
}

// houseVocab is captured once at startup. It is guidance, not a gate: a name
// that is missing from it (an entity added later) is still passed through to
// Home Assistant, which has the final say.
type houseVocab struct {
	entities []vocabEntity
	areas    []vocabArea
	floors   []vocabFloor
}

// loadVocab builds the vocabulary from states plus the area, floor, device and
// entity registries and the Assist exposure list. It is best effort: any
// WebSocket failure yields nil, and the tools fall back to generic
// descriptions.
func (t *Tools) loadVocab(ctx context.Context, states []State) *houseVocab {
	ws := t.client.NewWebsocketClient()
	if err := ws.Dial(ctx); err != nil {
		slog.Debug("hass: skipping tool vocabulary", "error", err)
		return nil
	}
	defer func() { _ = ws.Close() }()

	v, err := buildVocab(ws, states)
	if err != nil {
		slog.Warn("hass: could not read registries for tool vocabulary; tool descriptions will not list names", "error", err)
		return nil
	}
	return v
}

func buildVocab(ws *WebsocketClient, states []State) (*houseVocab, error) {
	areas, err := ws.ListAreas()
	if err != nil {
		return nil, err
	}
	floors, err := ws.ListFloors()
	if err != nil {
		return nil, err
	}
	devices, err := ws.ListDevices()
	if err != nil {
		return nil, err
	}
	entries, err := ws.ListEntityRegistry()
	if err != nil {
		return nil, err
	}

	v := &houseVocab{}
	areaByID := map[string]vocabArea{}
	for _, a := range areas {
		id, _ := a["area_id"].(string)
		name, _ := a["name"].(string)
		if id == "" || name == "" {
			continue
		}
		floorID, _ := a["floor_id"].(string)
		va := vocabArea{ID: id, Name: name, Aliases: sortedStrings(a["aliases"]), FloorID: floorID}
		v.areas = append(v.areas, va)
		areaByID[id] = va
	}
	for _, f := range floors {
		id, _ := f["floor_id"].(string)
		name, _ := f["name"].(string)
		if id == "" || name == "" {
			continue
		}
		v.floors = append(v.floors, vocabFloor{ID: id, Name: name, Aliases: sortedStrings(f["aliases"])})
	}

	deviceArea := map[string]string{}
	for _, d := range devices {
		id, _ := d["id"].(string)
		area, _ := d["area_id"].(string)
		if id != "" && area != "" {
			deviceArea[id] = area
		}
	}
	registry := map[string]map[string]any{}
	for _, e := range entries {
		if id, _ := e["entity_id"].(string); id != "" {
			registry[id] = e
		}
	}

	// The exposure list is admin-only; without it, registry options and
	// Home Assistant's defaults still give a good approximation.
	exposedList, listOK := exposedEntities(ws)

	// Decide exposure first, then fetch aliases only for what is exposed: the
	// extended registry entries are large, and only exposed entities can be
	// matched by the generated tools anyway.
	type pending struct {
		state   State
		domain  string
		entry   map[string]any
		exposed bool
	}
	var items []pending
	var wantAliases []string
	for _, s := range states {
		domain, _, ok := strings.Cut(s.EntityID, ".")
		if !ok {
			continue
		}
		entry := registry[s.EntityID]
		if entry != nil && entry["disabled_by"] != nil {
			continue
		}
		exposed := isExposed(s, domain, entry, exposedList, listOK)
		items = append(items, pending{state: s, domain: domain, entry: entry, exposed: exposed})
		if exposed && entry != nil {
			wantAliases = append(wantAliases, s.EntityID)
		}
	}
	aliases := entityAliases(ws, wantAliases)

	for _, it := range items {
		ve := vocabEntity{ID: it.state.EntityID, Domain: it.domain, Exposed: it.exposed}
		friendly, _ := it.state.Attributes["friendly_name"].(string)
		friendly = strings.TrimSpace(friendly)
		if friendly == "" {
			friendly = it.state.EntityID
		}
		ve.Names = matchNames(friendly, aliases, it.state.EntityID)
		if it.entry != nil {
			areaID, _ := it.entry["area_id"].(string)
			if areaID == "" {
				if dev, _ := it.entry["device_id"].(string); dev != "" {
					areaID = deviceArea[dev]
				}
			}
			if a, ok := areaByID[areaID]; ok {
				ve.AreaID = a.ID
				ve.Area = a.Name
			}
		}
		v.entities = append(v.entities, ve)
	}
	sort.SliceStable(v.entities, func(i, j int) bool {
		return strings.ToLower(v.primaryName(v.entities[i])) < strings.ToLower(v.primaryName(v.entities[j]))
	})
	return v, nil
}

func (v *houseVocab) primaryName(e vocabEntity) string {
	if len(e.Names) > 0 {
		return e.Names[0]
	}
	return e.ID
}

// matchNames mirrors er.async_get_entity_aliases: with no alias list the
// computed name is the only name; otherwise each null entry stands for the
// computed name and strings are taken as-is. An empty list means the entity
// cannot be matched by name at all.
func matchNames(friendly string, aliases map[string][]any, entityID string) []string {
	list, ok := aliases[entityID]
	if !ok {
		return []string{friendly}
	}
	out := []string{}
	seen := map[string]bool{}
	for _, a := range list {
		name := friendly
		if s, ok := a.(string); ok {
			name = strings.TrimSpace(s)
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// isExposed reproduces async_should_expose for the conversation assistant
// from the data a REST/WebSocket client can see.
func isExposed(s State, domain string, entry map[string]any, list map[string]bool, listOK bool) bool {
	if entry != nil {
		if opts, ok := entry["options"].(map[string]any); ok {
			if conv, ok := opts[assistantID].(map[string]any); ok {
				if v, ok := conv["should_expose"].(bool); ok {
					return v
				}
			}
		}
	}
	if listOK {
		if list[s.EntityID] {
			return true
		}
		if entry == nil {
			return false
		}
	}
	if entry != nil && (entry["entity_category"] != nil || entry["hidden_by"] != nil) {
		return false
	}
	return defaultExposedDomains[domain]
}

// exposedEntities reads homeassistant/expose_entity/list: entity_id ->
// {assistant: true} for every entity exposed to at least one assistant.
func exposedEntities(ws *WebsocketClient) (map[string]bool, bool) {
	resp, err := ws.wsCommand("homeassistant/expose_entity/list", nil)
	if err != nil {
		slog.Debug("hass: expose_entity/list unavailable; using registry options", "error", err)
		return nil, false
	}
	if ok, _ := resp["success"].(bool); !ok {
		return nil, false
	}
	result := resultMap(resp)
	raw, _ := result["exposed_entities"].(map[string]any)
	out := make(map[string]bool, len(raw))
	for id, v := range raw {
		if m, ok := v.(map[string]any); ok && m[assistantID] == true {
			out[id] = true
		}
	}
	return out, true
}

// entityAliases fetches registry aliases. config/entity_registry/list omits
// them (they are only in the extended entry), so they come from get_entries.
// A null alias stands for the computed entity name. Best effort: on failure
// every entity is treated as having only its computed name.
func entityAliases(ws *WebsocketClient, ids []string) map[string][]any {
	out := map[string][]any{}
	if len(ids) == 0 {
		return out
	}
	resp, err := ws.wsCommand("config/entity_registry/get_entries", map[string]any{"entity_ids": ids})
	if err != nil {
		slog.Debug("hass: entity aliases unavailable", "error", err)
		return out
	}
	if ok, _ := resp["success"].(bool); !ok {
		return out
	}
	for id, v := range resultMap(resp) {
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if list, ok := entry["aliases"].([]any); ok {
			out[id] = list
		}
	}
	return out
}

func sortedStrings(v any) []string {
	out := stringList(v)
	sort.Strings(out)
	return out
}

// normalizeName folds the differences Home Assistant's own casefold
// comparison does not: typographic apostrophes and quotes, repeated
// whitespace and a leading article. "Ivy's room" and "the Ivy’s Room" both
// normalise to "ivy's room".
func normalizeName(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.TrimSpace(s) {
		switch r {
		case '‘', '’', '‛', 'ʼ', '`', '´':
			r = '\''
		case '“', '”', '″':
			r = '"'
		}
		if unicode.IsSpace(r) {
			if !space {
				b.WriteRune(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(unicode.ToLower(r))
	}
	out := b.String()
	out = strings.TrimPrefix(out, "the ")
	return out
}

// entitiesFor returns exposed entities in domains (all exposed entities when
// domains is empty) that can be matched by name or area.
func (v *houseVocab) entitiesFor(domains []string) []vocabEntity {
	want := map[string]bool{}
	for _, d := range domains {
		want[d] = true
	}
	var out []vocabEntity
	for _, e := range v.entities {
		if !e.Exposed {
			continue
		}
		if len(want) > 0 && !want[e.Domain] {
			continue
		}
		out = append(out, e)
	}
	return out
}

// canonicalName maps a model-supplied name to the house's exact spelling when
// it differs only in ways normalizeName folds, and the mapping is unique.
// Anything else is returned unchanged for Home Assistant to judge.
func (v *houseVocab) canonicalName(value string, domains []string) string {
	norm := normalizeName(value)
	match := ""
	for _, e := range v.entitiesFor(domains) {
		for _, n := range append([]string{e.ID}, e.Names...) {
			if normalizeName(n) != norm {
				continue
			}
			if match != "" && !strings.EqualFold(match, n) {
				return value
			}
			match = n
		}
	}
	if match == "" {
		return value
	}
	return match
}

// canonicalArea does the same for an area name, alias or id.
func (v *houseVocab) canonicalArea(value string) string {
	norm := normalizeName(value)
	match := ""
	for _, a := range v.areas {
		for _, n := range append([]string{a.ID, a.Name}, a.Aliases...) {
			if normalizeName(n) != norm {
				continue
			}
			if match != "" && match != a.Name {
				return value
			}
			match = a.Name
		}
	}
	if match == "" {
		return value
	}
	return match
}

// canonicalFloor does the same for a floor name, alias or id.
func (v *houseVocab) canonicalFloor(value string) string {
	norm := normalizeName(value)
	match := ""
	for _, f := range v.floors {
		for _, n := range append([]string{f.ID, f.Name}, f.Aliases...) {
			if normalizeName(n) != norm {
				continue
			}
			if match != "" && match != f.Name {
				return value
			}
			match = f.Name
		}
	}
	if match == "" {
		return value
	}
	return match
}

// targetDescriptions renders the vocabulary for a tool acting on domains: the
// entity names it can be aimed at, the areas holding them and the floors
// holding those areas. withDomain adds each entity's domain, which matters
// for cross-domain tools where "the garage light" may be a switch.
func (v *houseVocab) targetDescriptions(domains []string, withDomain bool) (names, areas, floors string) {
	ents := v.entitiesFor(domains)

	// Group entities sharing a name: Home Assistant refuses a name that
	// matches several entities unless an area disambiguates, so the model
	// should know a name is shared before it tries it.
	type group struct {
		name    string
		members []vocabEntity
	}
	var order []string
	groups := map[string]*group{}
	for _, e := range ents {
		for _, n := range e.Names {
			key := normalizeName(n)
			g, ok := groups[key]
			if !ok {
				g = &group{name: n}
				groups[key] = g
				order = append(order, key)
			}
			g.members = append(g.members, e)
		}
	}

	var parts []string
	for _, key := range order {
		g := groups[key]
		var meta []string
		if withDomain {
			meta = append(meta, uniqueField(g.members, func(e vocabEntity) string { return e.Domain })...)
		}
		meta = append(meta, uniqueField(g.members, func(e vocabEntity) string { return e.Area })...)
		entry := g.name
		if len(meta) > 0 {
			entry += " (" + strings.Join(meta, ", ") + ")"
		}
		if len(g.members) > 1 {
			entry += fmt.Sprintf(" [%d devices share this name; add area or it fails]", len(g.members))
		}
		parts = append(parts, entry)
	}
	if len(parts) > maxVocabEntries {
		extra := len(parts) - maxVocabEntries
		parts = append(parts[:maxVocabEntries], fmt.Sprintf("… and %d more (find them with get_home_states search)", extra))
	}
	if len(parts) > 0 {
		names = "Exposed names: " + strings.Join(parts, "; ") + "."
	}

	areaHas := map[string]bool{}
	for _, e := range ents {
		if e.AreaID != "" {
			areaHas[e.AreaID] = true
		}
	}
	var areaParts []string
	floorAreas := map[string][]string{}
	for _, a := range v.areas {
		if !areaHas[a.ID] {
			continue
		}
		entry := a.Name
		if len(a.Aliases) > 0 {
			entry += " (alias " + strings.Join(a.Aliases, ", ") + ")"
		}
		areaParts = append(areaParts, entry)
		if a.FloorID != "" {
			floorAreas[a.FloorID] = append(floorAreas[a.FloorID], a.Name)
		}
	}
	if len(areaParts) > 0 {
		areas = "Areas with matching devices: " + strings.Join(areaParts, "; ") + "."
	}

	var floorParts []string
	for _, f := range v.floors {
		inside := floorAreas[f.ID]
		if len(inside) == 0 {
			continue
		}
		entry := f.Name
		if len(f.Aliases) > 0 {
			entry += " (alias " + strings.Join(f.Aliases, ", ") + ")"
		}
		floorParts = append(floorParts, entry+": "+strings.Join(inside, ", "))
	}
	if len(floorParts) > 0 {
		floors = "Floors and the areas on them: " + strings.Join(floorParts, "; ") + ". Areas not listed under a floor are on none."
	}
	return names, areas, floors
}

func uniqueField(members []vocabEntity, f func(vocabEntity) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range members {
		v := f(m)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// suggestNames ranks the vocabulary's names for value by shared words, for an
// error message after Home Assistant fails to match. It returns at most n.
func (v *houseVocab) suggestNames(value string, domains []string, n int) []string {
	words := strings.Fields(normalizeName(value))
	if len(words) == 0 {
		return nil
	}
	type scored struct {
		name  string
		score int
	}
	var all []scored
	seen := map[string]bool{}
	for _, e := range v.entitiesFor(domains) {
		for _, name := range e.Names {
			if seen[name] {
				continue
			}
			seen[name] = true
			norm := normalizeName(name)
			score := 0
			for _, w := range words {
				if strings.Contains(norm, w) {
					score++
				}
			}
			if e.Area != "" {
				for _, w := range words {
					if strings.Contains(normalizeName(e.Area), w) {
						score++
					}
				}
			}
			if score > 0 {
				label := name
				if e.Area != "" {
					label += " (" + e.Area + ")"
				}
				all = append(all, scored{label, score})
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].score > all[j].score })
	var out []string
	for i := 0; i < len(all) && i < n; i++ {
		out = append(out, all[i].name)
	}
	return out
}
