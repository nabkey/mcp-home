// Package frigate provides MCP tools for interacting with Frigate NVR.
package frigate

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
	"golang.org/x/sync/errgroup"
)

// Image sizing. Measured on the live cameras (1280x960 and 1920x1080), a
// full-resolution snapshot is 117-135 KB of JPEG before base64; at 360px
// and quality 70 the same frames are 24-32 KB, which still shows whether a
// person or car is present and where. crop=true gives detail for "who".
const (
	defaultImageHeight = 360
	maxImageHeight     = 1080
	minImageHeight     = 90
	defaultQuality     = 70

	defaultEventLimit = 20
	maxEventLimit     = 50

	// Thumbnails are Frigate's stored per-event object crops (~175px,
	// single-digit KB). maxThumbnails and thumbnailBudget cap what one
	// get_frigate_events call can add to a model's context.
	maxThumbnails   = 6
	thumbnailBudget = 64 * 1024
)

// Tools holds the Frigate tools and their configuration.
type Tools struct {
	client *Client
}

// NewTools creates a new Tools instance.
func NewTools(frigateURL string) (*Tools, error) {
	client, err := NewClient(frigateURL)
	if err != nil {
		return nil, err
	}
	return &Tools{client: client}, nil
}

// Register adds all Frigate tools to the given MCP server.
func (t *Tools) Register(server *mcp.Server) {
	t.registerListCameras(server)
	t.registerGetCameraSnapshot(server)
	t.registerGetEvents(server)
	t.registerGetEventSnapshot(server)
}

// zoneInfo is the resolved house time zone, reported with every result.
type zoneInfo struct {
	loc    *time.Location
	source string
}

func (z zoneInfo) fields(now time.Time) map[string]any {
	m := map[string]any{
		"timezone": z.loc.String(),
		"now":      formatTime(now, z.loc),
	}
	if z.source == tzSourceServer && (z.loc == time.UTC || z.loc.String() == "UTC") {
		m["timezone_note"] = "Times are UTC because neither Frigate ui.timezone nor TZ is set. If the house is elsewhere, pass timezone (e.g. America/New_York)."
	}
	return m
}

// zone resolves the house zone and returns the (cached) config used to
// validate camera names. A config failure is returned as is: /api/config
// always exists, so if it fails the call that follows would fail too, and
// failing here avoids waiting out a second timeout.
func (t *Tools) zone(ctx context.Context, arg string) (zoneInfo, *Config, error) {
	cfg, err := t.client.GetConfig(ctx)
	if err != nil {
		return zoneInfo{}, nil, err
	}
	loc, source, err := resolveLocation(arg, cfg.UI.Timezone)
	if err != nil {
		return zoneInfo{}, nil, err
	}
	return zoneInfo{loc: loc, source: source}, cfg, nil
}

func formatTime(t time.Time, loc *time.Location) string {
	return t.In(loc).Format(time.RFC3339)
}

// checkCameras rejects camera names the config does not know, listing the
// real ones. Frigate itself answers an unknown camera with an empty list,
// which a model reads as "nothing happened".
func checkCameras(cfg *Config, names []string) error {
	if cfg == nil {
		return nil
	}
	for _, n := range names {
		if _, ok := cfg.Cameras[n]; !ok {
			return fmt.Errorf("unknown camera %q; cameras are: %s", n, strings.Join(cameraNames(cfg), ", "))
		}
	}
	return nil
}

func cameraNames(cfg *Config) []string {
	names := make([]string, 0, len(cfg.Cameras))
	for n := range cfg.Cameras {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func clampHeight(h int) int {
	switch {
	case h <= 0:
		return defaultImageHeight
	case h < minImageHeight:
		return minImageHeight
	case h > maxImageHeight:
		return maxImageHeight
	}
	return h
}

func clampQuality(q int) int {
	switch {
	case q <= 0:
		return defaultQuality
	case q > 100:
		return 100
	}
	return q
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// --- list_frigate_cameras ---

type listCamerasArgs struct{}

type cameraSummary struct {
	Name    string   `json:"name"`
	Zones   []string `json:"zones,omitempty"`
	Objects []string `json:"objects,omitempty"`
}

func (t *Tools) registerListCameras(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_frigate_cameras",
		Description: "List enabled Frigate cameras with their zones (e.g. porch, driveway) and tracked object labels, plus the house time zone used for event times. Use it to map a place the user names to a camera or zone filter for get_frigate_events.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args listCamerasArgs) (*mcp.CallToolResult, any, error) {
		zi, config, err := t.zone(ctx, "")
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		cameras := make([]cameraSummary, 0, len(config.Cameras))
		var disabled []string
		for _, name := range cameraNames(config) {
			cam := config.Cameras[name]
			if !cam.Enabled {
				disabled = append(disabled, name)
				continue
			}
			s := cameraSummary{Name: name, Objects: cam.Objects.Track}
			for z := range cam.Zones {
				s.Zones = append(s.Zones, z)
			}
			sort.Strings(s.Zones)
			cameras = append(cameras, s)
		}

		out := zi.fields(t.client.now())
		out["cameras"] = cameras
		out["count"] = len(cameras)
		if len(disabled) > 0 {
			out["disabled"] = disabled
		}
		return mcputil.JSONResult(out)
	})
}

// --- get_camera_snapshot ---

type getCameraSnapshotArgs struct {
	Camera  string `json:"camera" jsonschema:"Camera name from list_frigate_cameras"`
	Height  int    `json:"height,omitempty" jsonschema:"Image height in pixels (default 360, max 1080). 360 shows what is in frame; raise it only to read fine detail"`
	Quality int    `json:"quality,omitempty" jsonschema:"JPEG quality 1-100 (default 70)"`
}

func (t *Tools) registerGetCameraSnapshot(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_camera_snapshot",
		Description: "Get the current live frame from a Frigate camera as a JPEG (360px tall by default, about 25-35 KB). For something that already happened use get_frigate_events, not this.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getCameraSnapshotArgs) (*mcp.CallToolResult, any, error) {
		if args.Camera == "" {
			return mcputil.Errorf("camera is required; call list_frigate_cameras for the names"), nil, nil
		}
		zi, cfg, err := t.zone(ctx, "")
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		if err := checkCameras(cfg, []string{args.Camera}); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		height := clampHeight(args.Height)
		img, err := t.client.GetLatestFrame(ctx, args.Camera, ImageOptions{Height: height, Quality: clampQuality(args.Quality)})
		if err != nil {
			if IsNotFound(err) {
				return mcputil.Errorf("camera %q not found in Frigate; call list_frigate_cameras for the names", args.Camera), nil, nil
			}
			return mcputil.Errorf("%v", err), nil, nil
		}

		meta := map[string]any{
			"camera":   args.Camera,
			"captured": formatTime(t.client.now(), zi.loc),
			"height":   height,
			"bytes":    len(img.Data),
		}
		if img.Offline {
			meta["offline"] = true
			meta["note"] = "The camera is offline; this is Frigate's last preview frame, not a live view."
		}
		return imageResult(meta, img.Data)
	})
}

// imageResult returns metadata as JSON text followed by the image.
func imageResult(meta map[string]any, data []byte) (*mcp.CallToolResult, any, error) {
	res, _, _ := mcputil.JSONResult(meta)
	res.Content = append(res.Content, &mcp.ImageContent{MIMEType: "image/jpeg", Data: data})
	return res, nil, nil
}

// --- get_frigate_events ---

type getEventsArgs struct {
	Camera     string  `json:"camera,omitempty" jsonschema:"Camera name, or several comma-separated (from list_frigate_cameras). Omit for all cameras"`
	Label      string  `json:"label,omitempty" jsonschema:"Object label(s), comma-separated: person, car, dog, cat, package..."`
	Zone       string  `json:"zone,omitempty" jsonschema:"Zone name(s), comma-separated, e.g. porch. Narrows a camera to the part of the frame the user means"`
	SubLabel   string  `json:"sub_label,omitempty" jsonschema:"Sub label(s), comma-separated: a recognised face name, licence plate or delivery company, if Frigate is configured to set them"`
	After      string  `json:"after,omitempty" jsonschema:"Start of window in house local time: YYYY-MM-DDTHH:MM, YYYY-MM-DD, or HH:MM (the most recent such time, so 22:00 asked in the morning is last night). RFC 3339 with offset also accepted"`
	Before     string  `json:"before,omitempty" jsonschema:"End of window, same formats. HH:MM means the first such time after 'after' (after=12:00 before=18:00 is this afternoon). Default now"`
	Hours      int     `json:"hours,omitempty" jsonschema:"Look back this many hours from before when after is not given (default 24). Windows are capped at 7 days"`
	MinScore   float64 `json:"min_score,omitempty" jsonschema:"Drop detections scoring below this (0-1). 0.7 removes most false positives"`
	Limit      int     `json:"limit,omitempty" jsonschema:"Maximum events, newest first (default 20, max 50)"`
	Thumbnails bool    `json:"thumbnails,omitempty" jsonschema:"Attach small object thumbnails (a few KB each) for the newest events, up to 6. Usually enough to answer who/what without a snapshot call"`
	Timezone   string  `json:"timezone,omitempty" jsonschema:"IANA zone for interpreting and reporting times (default: Frigate's ui.timezone, then the server's)"`
}

type eventSummary struct {
	ID            string   `json:"id"`
	Camera        string   `json:"camera"`
	Label         string   `json:"label"`
	SubLabel      string   `json:"sub_label,omitempty"`
	SubLabelScore float64  `json:"sub_label_score,omitempty"`
	Score         float64  `json:"score"`
	Start         string   `json:"start"`
	DurationSec   *int     `json:"duration_s,omitempty"`
	InProgress    bool     `json:"in_progress,omitempty"`
	Zones         []string `json:"zones,omitempty"`
	HasSnapshot   bool     `json:"has_snapshot"`
}

func summarize(e Event, loc *time.Location) eventSummary {
	s := eventSummary{
		ID:          e.ID,
		Camera:      e.Camera,
		Label:       e.Label,
		Score:       round2(e.Score()),
		Start:       formatTime(epochTime(e.StartTime), loc),
		Zones:       e.EventZones(),
		HasSnapshot: e.HasSnapshot,
	}
	s.SubLabel, s.SubLabelScore = e.SubLabel()
	s.SubLabelScore = round2(s.SubLabelScore)
	if e.EndTime != nil {
		d := int(math.Round(*e.EndTime - e.StartTime))
		s.DurationSec = &d
	} else {
		s.InProgress = true
	}
	return s
}

func (t *Tools) registerGetEvents(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_frigate_events",
		Description: "Find Frigate detection events (people, cars, animals, packages) by camera, zone, label, face/plate sub label and time window. " +
			"Times are in the house time zone, given and returned with their UTC offset; trust these rather than the clock burned into images, which is Frigate's (often UTC). " +
			"Set thumbnails=true to see small crops of the newest events in the same call; fetch get_event_snapshot only for the one event you need to show in detail.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getEventsArgs) (*mcp.CallToolResult, any, error) {
		zi, cfg, err := t.zone(ctx, args.Timezone)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		now := t.client.now()
		after, before, clamped, err := window(args.After, args.Before, args.Hours, zi.loc, now)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		limit := args.Limit
		if limit <= 0 {
			limit = defaultEventLimit
		}
		if limit > maxEventLimit {
			limit = maxEventLimit
		}

		q := EventsQuery{
			Cameras:   splitList(args.Camera),
			Labels:    splitList(args.Label),
			Zones:     splitList(args.Zone),
			SubLabels: splitList(args.SubLabel),
			After:     after,
			Before:    before,
			Limit:     limit,
			MinScore:  args.MinScore,
		}
		if err := checkCameras(cfg, q.Cameras); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		events, err := t.client.GetEvents(ctx, q)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		summaries := make([]eventSummary, 0, len(events))
		for _, e := range events {
			summaries = append(summaries, summarize(e, zi.loc))
		}

		out := zi.fields(now)
		out["window"] = map[string]string{"after": formatTime(after, zi.loc), "before": formatTime(before, zi.loc)}
		out["count"] = len(summaries)
		out["events"] = summaries
		var notes []string
		if clamped {
			notes = append(notes, "Window narrowed to the most recent 7 days.")
		}
		if len(events) == limit {
			out["more"] = true
			notes = append(notes, fmt.Sprintf("Showing the newest %d; narrow the window or filters, or raise limit, to see older events.", limit))
		}
		if len(events) == 0 {
			notes = append(notes, emptyHint(cfg, q))
		}

		res, _, _ := mcputil.JSONResult(withNotes(out, notes))
		if args.Thumbnails && len(events) > 0 {
			res.Content = append(res.Content, t.thumbnails(ctx, summaries)...)
		}
		return res, nil, nil
	})
}

func withNotes(out map[string]any, notes []string) map[string]any {
	if len(notes) > 0 {
		out["notes"] = notes
	}
	return out
}

// emptyHint explains an empty result in terms of what to change.
func emptyHint(cfg *Config, q EventsQuery) string {
	if cfg != nil && len(q.Labels) > 0 {
		tracked := map[string]bool{}
		for _, c := range cfg.Cameras {
			for _, o := range c.Objects.Track {
				tracked[o] = true
			}
		}
		for _, l := range q.Labels {
			if len(tracked) > 0 && !tracked[l] {
				return fmt.Sprintf("No events. Label %q is not tracked by any camera; list_frigate_cameras shows the tracked labels.", l)
			}
		}
	}
	return "No events in this window. Widen the window (hours/after) or drop camera, zone, label or min_score filters."
}

// thumbnails fetches the newest events' thumbnails, each preceded by a
// caption tying it to its event, within maxThumbnails and thumbnailBudget.
func (t *Tools) thumbnails(ctx context.Context, events []eventSummary) []mcp.Content {
	n := min(len(events), maxThumbnails)
	imgs := make([]*Image, n)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(3)
	var mu sync.Mutex
	failed := 0
	for i := range n {
		g.Go(func() error {
			img, err := t.client.GetEventThumbnail(gctx, events[i].ID)
			if err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				return nil
			}
			imgs[i] = img
			return nil
		})
	}
	_ = g.Wait()

	var content []mcp.Content
	used := 0
	for i, img := range imgs {
		if img == nil {
			continue
		}
		if used+len(img.Data) > thumbnailBudget {
			break
		}
		used += len(img.Data)
		e := events[i]
		content = append(content,
			&mcp.TextContent{Text: fmt.Sprintf("thumbnail %s: %s %s at %s", e.ID, e.Camera, e.Label, e.Start)},
			&mcp.ImageContent{MIMEType: "image/jpeg", Data: img.Data},
		)
	}
	if failed > 0 {
		content = append(content, &mcp.TextContent{Text: fmt.Sprintf("%d thumbnail(s) could not be fetched; use get_event_snapshot for those events.", failed)})
	}
	return content
}

// --- get_event_snapshot ---

type getEventSnapshotArgs struct {
	EventID  string `json:"event_id" jsonschema:"Event id from get_frigate_events"`
	Crop     bool   `json:"crop,omitempty" jsonschema:"Crop to the detected object for a close look at who or what it is (smaller and more detailed than the full frame)"`
	Height   int    `json:"height,omitempty" jsonschema:"Image height in pixels (default 360, max 1080)"`
	Quality  int    `json:"quality,omitempty" jsonschema:"JPEG quality 1-100 (default 70)"`
	BBox     *bool  `json:"bbox,omitempty" jsonschema:"Draw the detection box (default: Frigate's snapshot setting)"`
	Timezone string `json:"timezone,omitempty" jsonschema:"IANA zone for the reported time (default: Frigate's ui.timezone, then the server's)"`
}

func (t *Tools) registerGetEventSnapshot(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_event_snapshot",
		Description: "Get the snapshot for one Frigate event as a JPEG with its camera, label and local time. 360px tall by default (about 25-35 KB); crop=true zooms to the object. " +
			"The clock burned into the image is Frigate's and may be UTC; report the returned start time instead.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getEventSnapshotArgs) (*mcp.CallToolResult, any, error) {
		if args.EventID == "" {
			return mcputil.Errorf("event_id is required; get one from get_frigate_events"), nil, nil
		}
		zi, _, err := t.zone(ctx, args.Timezone)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		event, err := t.client.GetEvent(ctx, args.EventID)
		if err != nil {
			if IsNotFound(err) {
				return mcputil.Errorf("event %q not found; it may have expired from Frigate's retention. Call get_frigate_events for current ids", args.EventID), nil, nil
			}
			return mcputil.Errorf("%v", err), nil, nil
		}
		if !event.HasSnapshot {
			return mcputil.Errorf("event %q has no snapshot (snapshots are off for camera %q); call get_frigate_events with thumbnails=true to see it", args.EventID, event.Camera), nil, nil
		}

		height := clampHeight(args.Height)
		img, err := t.client.GetEventSnapshot(ctx, args.EventID, ImageOptions{
			Height:  height,
			Quality: clampQuality(args.Quality),
			Crop:    args.Crop,
			BBox:    args.BBox,
		})
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		s := summarize(*event, zi.loc)
		meta := map[string]any{
			"event_id": s.ID,
			"camera":   s.Camera,
			"label":    s.Label,
			"score":    s.Score,
			"start":    s.Start,
			"height":   height,
			"crop":     args.Crop,
			"bytes":    len(img.Data),
		}
		if s.SubLabel != "" {
			meta["sub_label"] = s.SubLabel
		}
		if len(s.Zones) > 0 {
			meta["zones"] = s.Zones
		}
		return imageResult(meta, img.Data)
	})
}
