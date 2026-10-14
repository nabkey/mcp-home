package frigate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestGetConfig(t *testing.T) {
	calls := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/config" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ui":{"timezone":"America/New_York"},"cameras":{"front_door":{"enabled":true,"zones":{"porch":{}},"objects":{"track":["person"]}},"garage":{"enabled":false}}}`))
	})

	cfg, err := c.GetConfig(context.Background())
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if len(cfg.Cameras) != 2 || !cfg.Cameras["front_door"].Enabled || cfg.UI.Timezone != "America/New_York" {
		t.Errorf("unexpected config: %+v", cfg)
	}
	if _, ok := cfg.Cameras["front_door"].Zones["porch"]; !ok {
		t.Errorf("zones not parsed: %+v", cfg.Cameras["front_door"])
	}
	if _, err := c.GetConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("config fetched %d times, want 1 (cached)", calls)
	}
}

// Frigate 0.14+ reads cameras/labels/zones as one comma-joined value; a
// repeated parameter keeps only the last, silently dropping the others.
func TestEventsQueryEncoding(t *testing.T) {
	after := time.Unix(1791900000, 0)
	before := time.Unix(1791910000, 0)
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		want := map[string]string{
			"cameras":            "driveway,garage",
			"labels":             "person,car",
			"zones":              "porch",
			"sub_labels":         "Alex",
			"after":              "1791900000",
			"before":             "1791910000",
			"limit":              "10",
			"min_score":          "0.7",
			"include_thumbnails": "0",
		}
		for k, v := range want {
			if got := q[k]; len(got) != 1 || got[0] != v {
				t.Errorf("%s = %v, want [%s]", k, got, v)
			}
		}
		_, _ = w.Write([]byte(`[]`))
	})

	_, err := c.GetEvents(context.Background(), EventsQuery{
		Cameras: []string{"driveway", "garage"}, Labels: []string{"person", "car"},
		Zones: []string{"porch"}, SubLabels: []string{"Alex"},
		After: after, Before: before, Limit: 10, MinScore: 0.7,
	})
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
}

func loadEvents(t *testing.T) []Event {
	t.Helper()
	b, err := os.ReadFile("testdata/events.json")
	if err != nil {
		t.Fatal(err)
	}
	var events []Event
	if err := json.Unmarshal(b, &events); err != nil {
		t.Fatal(err)
	}
	return events
}

// The live instance (0.18) returns top_score 0 for every event; the real
// score is in data. Older versions used top_score and entered_zones.
func TestEventFieldsAcrossVersions(t *testing.T) {
	events := loadEvents(t)
	cases := []struct {
		idx          int
		score        float64
		subLabel     string
		subLabelConf float64
		zones        []string
	}{
		{0, 0.68, "", 0, nil},
		{1, 0.83, "Alex", 0, []string{"workbench"}},
		{3, 0.9, "ABC1234", 0.91, []string{"apron", "walkway"}},
	}
	for _, tc := range cases {
		e := events[tc.idx]
		if e.Score() != tc.score {
			t.Errorf("%s: score = %v, want %v", e.ID, e.Score(), tc.score)
		}
		name, conf := e.SubLabel()
		if name != tc.subLabel || conf != tc.subLabelConf {
			t.Errorf("%s: sub label = %q %v, want %q %v", e.ID, name, conf, tc.subLabel, tc.subLabelConf)
		}
		if strings.Join(e.EventZones(), ",") != strings.Join(tc.zones, ",") {
			t.Errorf("%s: zones = %v, want %v", e.ID, e.EventZones(), tc.zones)
		}
	}

	var legacy Event
	if err := json.Unmarshal([]byte(`{"id":"x","top_score":0.77,"entered_zones":["porch"],"zones":[]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Score() != 0.77 || strings.Join(legacy.EventZones(), ",") != "porch" {
		t.Errorf("legacy event: score %v zones %v", legacy.Score(), legacy.EventZones())
	}
}

func TestImageOptionsAndOffline(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/api/events/1.2-abc/snapshot.jpg":
			if q.Get("height") != "360" || q.Get("quality") != "70" || q.Get("crop") != "1" || q.Get("bbox") != "0" {
				t.Errorf("snapshot query = %v", q)
			}
		case "/api/driveway/latest.jpg":
			if q.Has("crop") {
				t.Error("crop must not be sent for latest.jpg")
			}
			w.Header().Set("X-Frigate-Offline", "true")
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xff, 0xd8, 0xff})
	})
	no := false
	if _, err := c.GetEventSnapshot(context.Background(), "1.2-abc", ImageOptions{Height: 360, Quality: 70, Crop: true, BBox: &no}); err != nil {
		t.Fatal(err)
	}
	img, err := c.GetLatestFrame(context.Background(), "driveway", ImageOptions{Height: 360, Crop: true})
	if err != nil {
		t.Fatal(err)
	}
	if !img.Offline {
		t.Error("offline header not reported")
	}
}

func TestGetLatestFrameValidatesCamera(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s", r.URL.Path)
	})
	if _, err := c.GetLatestFrame(context.Background(), "../config", ImageOptions{}); err == nil {
		t.Error("expected validation error for path-injection camera name")
	}
	if _, err := c.GetEventSnapshot(context.Background(), "", ImageOptions{}); err == nil {
		t.Error("expected validation error for empty event id")
	}
	if _, err := c.GetEventThumbnail(context.Background(), "a/b"); err == nil {
		t.Error("expected validation error for thumbnail id")
	}
}

// Each failure mode must say what to do next, not just what went wrong.
func TestErrorMessages(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		call    func(*Client) error
		want    string
	}{
		{
			name: "401 points at the port",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			},
			call: func(c *Client) error { _, err := c.GetConfig(context.Background()); return err },
			want: "port (5000)",
		},
		{
			name: "HTML login page instead of JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("<!DOCTYPE html><html><body>Login</body></html>"))
			},
			call: func(c *Client) error {
				_, err := c.GetEvents(context.Background(), EventsQuery{})
				return err
			},
			want: "returned HTML",
		},
		{
			name: "malformed JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`[{"id":`))
			},
			call: func(c *Client) error {
				_, err := c.GetEvents(context.Background(), EventsQuery{})
				return err
			},
			want: "failed to parse Frigate events",
		},
		{
			name: "500 suggests retry",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			call: func(c *Client) error { _, err := c.GetConfig(context.Background()); return err },
			want: "retry",
		},
		{
			name: "image endpoint returning JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"success":false}`))
			},
			call: func(c *Client) error {
				_, err := c.GetEventSnapshot(context.Background(), "1.2-abc", ImageOptions{})
				return err
			},
			want: "instead of an image",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, tc.handler)
			err := tc.call(c)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestTimeoutMessage(t *testing.T) {
	block := make(chan struct{})
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-block
	})
	t.Cleanup(func() { close(block) })
	c.httpClient.Timeout = 50 * time.Millisecond
	_, err := c.GetConfig(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not respond") {
		t.Errorf("err = %v, want a timeout explanation", err)
	}
}

func TestUnreachableMessage(t *testing.T) {
	c, _ := NewClient("http://127.0.0.1:1")
	_, err := c.GetConfig(context.Background())
	if err == nil || !strings.Contains(err.Error(), "FRIGATE_URL") {
		t.Errorf("err = %v, want it to mention FRIGATE_URL", err)
	}
}

func TestBaseURLPathPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/frigate/api/config" {
			t.Errorf("path = %s, want /frigate/api/config", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"cameras":{}}`))
	}))
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL + "/frigate/")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.GetConfig(context.Background()); err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
}
