package frigate

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Sizes measured on the live cameras at the tool defaults (see the
// constants in tools.go): a 360px q70 frame is 24-32 KB, a thumbnail ~9 KB.
const (
	fakeThumbBytes    = 9 * 1024
	fakeSnapshotBytes = 32 * 1024
)

// fakeFrigate serves recorded 0.18 responses. The config carries
// ui.timezone, as the house's would if set.
func fakeFrigate(t *testing.T, tz string) (*httptest.Server, *[]string) {
	t.Helper()
	events, err := os.ReadFile("testdata/events.json")
	if err != nil {
		t.Fatal(err)
	}
	var requests []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path+"?"+r.URL.RawQuery)
		mu.Unlock()
		p := r.URL.Path
		switch {
		case p == "/api/config":
			_, _ = w.Write([]byte(`{"ui":{"timezone":"` + tz + `"},"cameras":{
				"driveway":{"enabled":true,"zones":{"walkway":{},"apron":{}},"objects":{"track":["person","car"]}},
				"garage":{"enabled":true,"zones":{"workbench":{}},"objects":{"track":["person"]}},
				"old_cam":{"enabled":false}}}`))
		case p == "/api/events":
			_, _ = w.Write(events)
		case p == "/api/events/1791916547.05921-2c24kw":
			var all []json.RawMessage
			_ = json.Unmarshal(events, &all)
			_, _ = w.Write(all[2])
		case p == "/api/events/1791940000.100000-inprog":
			var all []json.RawMessage
			_ = json.Unmarshal(events, &all)
			_, _ = w.Write(all[4])
		case strings.HasSuffix(p, "/thumbnail.jpg"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(bytes.Repeat([]byte{0xff}, fakeThumbBytes))
		case strings.HasSuffix(p, "/snapshot.jpg"), strings.HasSuffix(p, "/latest.jpg"):
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(bytes.Repeat([]byte{0xff}, fakeSnapshotBytes))
		default:
			http.Error(w, `{"success":false,"message":"Event not found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

// connectTools registers the tools on a server and returns a client session.
func connectTools(t *testing.T, frigateURL string, now time.Time) *mcp.ClientSession {
	t.Helper()
	tools, err := NewTools(frigateURL)
	if err != nil {
		t.Fatal(err)
	}
	tools.client.now = func() time.Time { return now }
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	tools.Register(server)
	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func firstText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("first content is %T", res.Content[0])
	}
	return tc.Text
}

func imageBytes(res *mcp.CallToolResult) (n, count int) {
	for _, c := range res.Content {
		if img, ok := c.(*mcp.ImageContent); ok {
			n += len(img.Data)
			count++
		}
	}
	return n, count
}

// evening is 22:24 on 2026-10-13 in New York, already 10-14 in UTC.
var evening = time.Date(2026, 10, 14, 2, 24, 0, 0, time.UTC)

// "Who came up the driveway this afternoon?" in two calls and well under
// 150 KB of image: events with thumbnails, then one snapshot.
func TestDrivewayQuestionInTwoCalls(t *testing.T) {
	srv, requests := fakeFrigate(t, "America/New_York")
	cs := connectTools(t, srv.URL, evening)

	res := call(t, cs, "get_frigate_events", map[string]any{
		"camera": "driveway", "label": "person", "after": "12:00", "before": "18:00", "thumbnails": true,
	})
	if res.IsError {
		t.Fatalf("error: %s", firstText(t, res))
	}
	var out struct {
		Timezone string            `json:"timezone"`
		Window   map[string]string `json:"window"`
		Events   []eventSummary    `json:"events"`
	}
	if err := json.Unmarshal([]byte(firstText(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	if out.Timezone != "America/New_York" {
		t.Errorf("timezone = %s", out.Timezone)
	}
	if out.Window["after"] != "2026-10-13T12:00:00-04:00" || out.Window["before"] != "2026-10-13T18:00:00-04:00" {
		t.Errorf("window = %v", out.Window)
	}
	// Frigate was asked for exactly that window, in epoch seconds.
	var eventsReq string
	for _, r := range *requests {
		if strings.HasPrefix(r, "/api/events?") {
			eventsReq = r
		}
	}
	for _, want := range []string{"after=1791907200", "before=1791928800", "cameras=driveway", "labels=person"} {
		if !strings.Contains(eventsReq, want) {
			t.Errorf("events request %q missing %s", eventsReq, want)
		}
	}
	// The event the live tool reported as 18:35:47Z is 14:35:47 local.
	var found bool
	for _, e := range out.Events {
		if e.ID == "1791916547.05921-2c24kw" {
			found = true
			if e.Start != "2026-10-13T14:35:47-04:00" || e.Score != 0.75 || e.DurationSec == nil || *e.DurationSec != 4 {
				t.Errorf("summary = %+v", e)
			}
		}
	}
	if !found {
		t.Fatal("recorded event missing")
	}
	thumbBytes, thumbs := imageBytes(res)
	if thumbs == 0 || thumbs > maxThumbnails {
		t.Errorf("thumbnails = %d", thumbs)
	}

	snap := call(t, cs, "get_event_snapshot", map[string]any{"event_id": "1791916547.05921-2c24kw", "crop": true})
	if snap.IsError {
		t.Fatalf("snapshot error: %s", firstText(t, snap))
	}
	if !strings.Contains(firstText(t, snap), `"start":"2026-10-13T14:35:47-04:00"`) {
		t.Errorf("snapshot meta = %s", firstText(t, snap))
	}
	snapBytes, _ := imageBytes(snap)
	total := thumbBytes + snapBytes
	if total >= 150*1024 {
		t.Errorf("two-call image weight = %d bytes, want < 150 KB", total)
	}
	t.Logf("two-call image weight: %d KB (%d thumbnails + 1 snapshot)", total/1024, thumbs)

	var snapReq string
	for _, r := range *requests {
		if strings.Contains(r, "snapshot.jpg") {
			snapReq = r
		}
	}
	for _, want := range []string{"height=360", "quality=70", "crop=1"} {
		if !strings.Contains(snapReq, want) {
			t.Errorf("snapshot request %q missing %s", snapReq, want)
		}
	}
}

func TestToolErrorsGuideNextStep(t *testing.T) {
	srv, _ := fakeFrigate(t, "America/New_York")
	cs := connectTools(t, srv.URL, evening)

	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{"unknown camera lists real ones", "get_frigate_events", map[string]any{"camera": "front_door"}, "cameras are: driveway, garage, old_cam"},
		{"bad time format", "get_frigate_events", map[string]any{"after": "this afternoon"}, "YYYY-MM-DDTHH:MM"},
		{"bad timezone", "get_frigate_events", map[string]any{"timezone": "EST5"}, "IANA"},
		{"snapshot of unknown event", "get_event_snapshot", map[string]any{"event_id": "123.4-gone"}, "get_frigate_events"},
		{"event without snapshot", "get_event_snapshot", map[string]any{"event_id": "1791940000.100000-inprog"}, "thumbnails=true"},
		{"live frame of unknown camera", "get_camera_snapshot", map[string]any{"camera": "porch"}, "cameras are"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := call(t, cs, tc.tool, tc.args)
			if !res.IsError {
				t.Fatalf("expected IsError, got %s", firstText(t, res))
			}
			if txt := firstText(t, res); !strings.Contains(txt, tc.want) {
				t.Errorf("error %q does not contain %q", txt, tc.want)
			}
		})
	}
}

func TestEmptyAndTruncatedResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/config":
			_, _ = w.Write([]byte(`{"cameras":{"driveway":{"enabled":true,"objects":{"track":["person","car"]}}}}`))
		case "/api/events":
			if r.URL.Query().Get("limit") == "2" {
				_, _ = w.Write([]byte(`[{"id":"a","camera":"driveway","label":"car","start_time":1,"end_time":2},{"id":"b","camera":"driveway","label":"car","start_time":1,"end_time":2}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TZ", "")
	cs := connectTools(t, srv.URL, evening)

	txt := firstText(t, call(t, cs, "get_frigate_events", map[string]any{"label": "package"}))
	if !strings.Contains(txt, `not tracked`) {
		t.Errorf("empty result for untracked label: %s", txt)
	}
	txt = firstText(t, call(t, cs, "get_frigate_events", map[string]any{}))
	if !strings.Contains(txt, "Widen the window") || !strings.Contains(txt, `"events":[]`) {
		t.Errorf("empty result: %s", txt)
	}
	txt = firstText(t, call(t, cs, "get_frigate_events", map[string]any{"limit": 2}))
	if !strings.Contains(txt, `"more":true`) {
		t.Errorf("truncated result not flagged: %s", txt)
	}
}

// With no ui.timezone and no TZ on a UTC server, the result says so instead
// of silently presenting UTC as house time.
func TestUTCFallbackIsFlagged(t *testing.T) {
	if time.Local.String() != "UTC" && time.Local != time.UTC {
		t.Skip("server zone is not UTC")
	}
	srv, _ := fakeFrigate(t, "")
	t.Setenv("TZ", "")
	cs := connectTools(t, srv.URL, evening)
	txt := firstText(t, call(t, cs, "get_frigate_events", map[string]any{}))
	if !strings.Contains(txt, "timezone_note") {
		t.Errorf("UTC fallback not flagged: %s", txt)
	}
	txt = firstText(t, call(t, cs, "get_frigate_events", map[string]any{"timezone": "America/New_York"}))
	if strings.Contains(txt, "timezone_note") || !strings.Contains(txt, "-04:00") {
		t.Errorf("explicit timezone not applied: %s", txt)
	}
}

func TestListCameras(t *testing.T) {
	srv, _ := fakeFrigate(t, "America/New_York")
	cs := connectTools(t, srv.URL, evening)
	txt := firstText(t, call(t, cs, "list_frigate_cameras", nil))
	for _, want := range []string{`"name":"driveway","zones":["apron","walkway"],"objects":["person","car"]`, `"disabled":["old_cam"]`, `"timezone":"America/New_York"`, `"now":"2026-10-13T22:24:00-04:00"`} {
		if !strings.Contains(txt, want) {
			t.Errorf("list output missing %s: %s", want, txt)
		}
	}
}
