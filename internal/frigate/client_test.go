package frigate

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"cameras":{"front_door":{"name":"front_door","enabled":true},"garage":{"name":"garage","enabled":false}}}`))
	})

	cfg, err := c.GetConfig(context.Background())
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if len(cfg.Cameras) != 2 || !cfg.Cameras["front_door"].Enabled {
		t.Errorf("unexpected config: %+v", cfg)
	}
}

func TestGetEvents(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/events" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if got := q["cameras"]; len(got) != 1 || got[0] != "front_door" {
			t.Errorf("cameras = %v", got)
		}
		if got := q.Get("limit"); got != "10" {
			t.Errorf("limit = %q, want 10", got)
		}
		if q.Get("after") == "" {
			t.Error("after should be set")
		}
		_, _ = w.Write([]byte(`[{"id":"123-abc","camera":"front_door","label":"person","top_score":0.92}]`))
	})

	after := time.Now().Add(-time.Hour)
	events, err := c.GetEvents(context.Background(), []string{"front_door"}, nil, 10, &after)
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(events) != 1 || events[0].Label != "person" {
		t.Errorf("unexpected events: %+v", events)
	}
}

func TestGetLatestFrameValidatesCamera(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s", r.URL.Path)
	})
	if _, err := c.GetLatestFrame(context.Background(), "../config"); err == nil {
		t.Error("expected validation error for path-injection camera name")
	}
	if _, err := c.GetEventSnapshot(context.Background(), ""); err == nil {
		t.Error("expected validation error for empty event id")
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
