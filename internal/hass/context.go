package hass

import (
	"context"
	"strings"
	"sync"
	"time"

	// The image is distroless; embed the zone database so the home's time
	// zone always resolves.
	_ "time/tzdata"
)

// homeContextTTL bounds how stale the cached registry view may be. Rooms and
// entity categories change rarely; states are never cached.
const homeContextTTL = 5 * time.Minute

// entityInfo is what the state, history and logbook tools need from the
// registries to place and rank an entity.
type entityInfo struct {
	// area is the effective area id: the entity's own, else its device's.
	area string
	// category is "config" or "diagnostic" for entities Home Assistant itself
	// keeps off the default dashboards; empty for primary entities.
	category string
	hidden   bool
}

// homeContext is the slowly changing view of the home the read tools join
// against: its time zone and the entity/area registries.
type homeContext struct {
	loc       *time.Location
	entities  map[string]entityInfo
	areaNames map[string]string // area_id → display name
	fetched   time.Time
	// registryErr is set when the registries could not be read; tools then
	// work without areas and categories rather than failing.
	registryErr error
}

// areaID resolves an area argument given as an id or a display name
// (case-insensitive). Unknown areas resolve to themselves, so a filter on them
// simply matches nothing.
func (h *homeContext) areaID(s string) string {
	s = strings.TrimSpace(s)
	if _, ok := h.areaNames[s]; ok {
		return s
	}
	for id, name := range h.areaNames {
		if strings.EqualFold(name, s) || strings.EqualFold(id, s) {
			return id
		}
	}
	return s
}

// homeContextCache memoises homeContext per Client.
type homeContextCache struct {
	mu  sync.Mutex
	cur *homeContext
}

// HomeContext returns the cached home context, refreshing it when older than
// homeContextTTL. It never fails: a time zone it cannot learn is UTC, and
// registries it cannot read leave entities unplaced (see registryErr).
func (c *Client) HomeContext(ctx context.Context) *homeContext {
	c.homeCtx.mu.Lock()
	defer c.homeCtx.mu.Unlock()
	if cur := c.homeCtx.cur; cur != nil && time.Since(cur.fetched) < homeContextTTL {
		return cur
	}
	h := &homeContext{loc: time.UTC, entities: map[string]entityInfo{}, areaNames: map[string]string{}, fetched: time.Now()}
	if cfg, err := c.GetConfig(ctx); err == nil {
		if tz, _ := cfg["time_zone"].(string); tz != "" {
			if loc, err := time.LoadLocation(tz); err == nil {
				h.loc = loc
			}
		}
	}
	h.registryErr = c.loadRegistries(ctx, h)
	// Keep a failed registry read only briefly so the next call retries.
	if h.registryErr != nil {
		h.fetched = time.Now().Add(-homeContextTTL + 30*time.Second)
	}
	c.homeCtx.cur = h
	return h
}

func (c *Client) loadRegistries(ctx context.Context, h *homeContext) error {
	ws := c.NewWebsocketClient()
	if err := ws.Dial(ctx); err != nil {
		return err
	}
	defer func() { _ = ws.Close() }()
	areas, err := ws.ListAreas()
	if err != nil {
		return err
	}
	for _, a := range areas {
		h.areaNames[str(a, "area_id")] = str(a, "name")
	}
	devices, err := ws.ListDevices()
	if err != nil {
		return err
	}
	deviceArea := make(map[string]string, len(devices))
	for _, d := range devices {
		deviceArea[str(d, "id")] = str(d, "area_id")
	}
	entities, err := ws.ListEntityRegistry()
	if err != nil {
		return err
	}
	for _, e := range entities {
		info := entityInfo{area: str(e, "area_id"), category: str(e, "entity_category"), hidden: str(e, "hidden_by") != ""}
		if info.area == "" {
			info.area = deviceArea[str(e, "device_id")]
		}
		h.entities[str(e, "entity_id")] = info
	}
	return nil
}

// localTime formats t in the home's time zone at second precision with its
// offset, so a model reading "last night" against it needs no conversion.
func (h *homeContext) localTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(h.loc).Format(time.RFC3339)
}

// parseUserTime accepts RFC3339, or a local date/time without offset
// ("2026-10-13 22:00", "2026-10-13T22:00", "2026-10-13") read in the home's
// time zone.
func (h *homeContext) parseUserTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, h.loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
