package hass

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient builds a Client pointing at an httptest server. The handler is
// expected to inspect the incoming request and return a canned response.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := NewClient(srv.URL, "test-token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, srv
}

func assertAuth(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer test-token")
	}
}

func TestGetServices(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.Method != "GET" || r.URL.Path != "/api/services" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"domain":"light","services":{"turn_on":{}}},{"domain":"switch","services":{"toggle":{}}}]`))
	})

	services, err := c.GetServices(context.Background())
	if err != nil {
		t.Fatalf("GetServices: %v", err)
	}
	if len(services) != 2 {
		t.Fatalf("len(services) = %d, want 2", len(services))
	}
	if services[0]["domain"] != "light" {
		t.Errorf("services[0].domain = %v, want light", services[0]["domain"])
	}
}

func TestGetHistory(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if !strings.HasPrefix(r.URL.Path, "/api/history/period/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("filter_entity_id") != "sensor.temp,sensor.humidity" {
			t.Errorf("filter_entity_id = %q, want sensor.temp,sensor.humidity", q.Get("filter_entity_id"))
		}
		if _, ok := q["minimal_response"]; !ok {
			t.Errorf("minimal_response flag missing")
		}
		if _, ok := q["significant_changes_only"]; ok {
			t.Errorf("significant_changes_only should not be set")
		}
		if q.Get("end_time") == "" {
			t.Errorf("end_time should be set")
		}
		_, _ = w.Write([]byte(`[[{"entity_id":"sensor.temp","state":"21.5"}],[{"entity_id":"sensor.humidity","state":"40"}]]`))
	})

	since := time.Now().Add(-2 * time.Hour)
	end := time.Now()
	history, err := c.GetHistory(context.Background(), since, end, []string{"sensor.temp", "sensor.humidity"}, true, false)
	if err != nil {
		t.Fatalf("GetHistory: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("len(history) = %d, want 2", len(history))
	}
	if history[0][0]["entity_id"] != "sensor.temp" {
		t.Errorf("history[0][0].entity_id = %v", history[0][0]["entity_id"])
	}
}

func TestRenderTemplate(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.Method != "POST" || r.URL.Path != "/api/template" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if payload["template"] != "{{ states('sun.sun') }}" {
			t.Errorf("template = %v", payload["template"])
		}
		_, _ = w.Write([]byte("above_horizon"))
	})

	out, err := c.RenderTemplate(context.Background(), "{{ states('sun.sun') }}", nil)
	if err != nil {
		t.Fatalf("RenderTemplate: %v", err)
	}
	if out != "above_horizon" {
		t.Errorf("out = %q, want above_horizon", out)
	}
}

func TestRenderTemplateRejectsEmpty(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
	})
	if _, err := c.RenderTemplate(context.Background(), "", nil); err == nil {
		t.Error("expected error for empty template")
	}
}

func TestSceneCRUD(t *testing.T) {
	type call struct {
		method string
		path   string
	}
	var got []call

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		got = append(got, call{r.Method, r.URL.Path})
		_, _ = w.Write([]byte(`{"result":"ok"}`))
	})

	if _, err := c.CreateScene(context.Background(), "movie_night", map[string]any{"name": "Movie Night"}); err != nil {
		t.Fatalf("CreateScene: %v", err)
	}
	if _, err := c.UpdateScene(context.Background(), "movie_night", map[string]any{"name": "Cinema"}); err != nil {
		t.Fatalf("UpdateScene: %v", err)
	}
	if err := c.DeleteScene(context.Background(), "movie_night"); err != nil {
		t.Fatalf("DeleteScene: %v", err)
	}

	want := []call{
		{"POST", "/api/config/scene/config/movie_night"},
		{"POST", "/api/config/scene/config/movie_night"},
		{"DELETE", "/api/config/scene/config/movie_night"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d calls, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSceneRejectsBadID(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
	})
	if _, err := c.CreateScene(context.Background(), "../etc/passwd", map[string]any{}); err == nil {
		t.Error("expected validation error for path-injection id")
	}
	if err := c.DeleteScene(context.Background(), ""); err == nil {
		t.Error("expected validation error for empty id")
	}
}

func TestGetCalendars(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.URL.Path != "/api/calendars" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"entity_id":"calendar.work","name":"Work"},{"entity_id":"calendar.home","name":"Home"}]`))
	})

	cals, err := c.GetCalendars(context.Background())
	if err != nil {
		t.Fatalf("GetCalendars: %v", err)
	}
	if len(cals) != 2 || cals[0].EntityID != "calendar.work" {
		t.Errorf("unexpected calendars: %+v", cals)
	}
}

func TestGetCalendarEvents(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.URL.Path != "/api/calendars/calendar.work" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("start") == "" || q.Get("end") == "" {
			t.Errorf("start/end query missing: %v", q)
		}
		_, _ = w.Write([]byte(`[{"summary":"Standup","start":{"dateTime":"2026-05-02T09:00:00Z"},"end":{"dateTime":"2026-05-02T09:30:00Z"}}]`))
	})

	start := time.Now()
	end := start.Add(24 * time.Hour)
	events, err := c.GetCalendarEvents(context.Background(), "calendar.work", start, end)
	if err != nil {
		t.Fatalf("GetCalendarEvents: %v", err)
	}
	if len(events) != 1 || events[0].Summary != "Standup" {
		t.Errorf("unexpected events: %+v", events)
	}
}

func TestGetCalendarEventsRequiresEntityID(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s", r.URL.Path)
	})
	if _, err := c.GetCalendarEvents(context.Background(), "", time.Now(), time.Now().Add(time.Hour)); err == nil {
		t.Error("expected error for empty entity_id")
	}
}
