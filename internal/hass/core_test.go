package hass

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestSetState(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.Method != "POST" || r.URL.Path != "/api/states/sensor.rainfall" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"entity_id":"sensor.rainfall","state":"12.4"}`))
	})

	result, err := c.SetState(context.Background(), "sensor.rainfall", "12.4",
		map[string]any{"unit_of_measurement": "mm"})
	if err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if gotBody["state"] != "12.4" {
		t.Errorf("state = %v, want 12.4", gotBody["state"])
	}
	attrs, _ := gotBody["attributes"].(map[string]any)
	if attrs["unit_of_measurement"] != "mm" {
		t.Errorf("attributes = %v", gotBody["attributes"])
	}
	if result["entity_id"] != "sensor.rainfall" {
		t.Errorf("result = %v", result)
	}
}

func TestSetStateOmitsEmptyAttributes(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{}`))
	})

	if _, err := c.SetState(context.Background(), "sensor.x", "on", nil); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	if _, ok := gotBody["attributes"]; ok {
		t.Errorf("attributes should be omitted when empty, got %v", gotBody)
	}
}

func TestSetStateRejectsPathInjection(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request should not have been sent: %s", r.URL.Path)
	})
	if _, err := c.SetState(context.Background(), "../config/core/check_config", "on", nil); err == nil {
		t.Error("expected an error for an entity_id containing a path traversal")
	}
}

func TestDeleteState(t *testing.T) {
	called := false
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != "DELETE" || r.URL.Path != "/api/states/sensor.stale" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"message":"Entity removed."}`))
	})

	if err := c.DeleteState(context.Background(), "sensor.stale"); err != nil {
		t.Fatalf("DeleteState: %v", err)
	}
	if !called {
		t.Error("handler was never called")
	}
}

func TestDeleteStateRejectsPathInjection(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request should not have been sent: %s", r.URL.Path)
	})
	if err := c.DeleteState(context.Background(), "a/b"); err == nil {
		t.Error("expected an error for an entity_id containing a slash")
	}
}

func TestFireEvent(t *testing.T) {
	var gotBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/events/my_custom_event" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"message":"Event my_custom_event fired."}`))
	})

	msg, err := c.FireEvent(context.Background(), "my_custom_event", map[string]any{"level": 3})
	if err != nil {
		t.Fatalf("FireEvent: %v", err)
	}
	if msg != "Event my_custom_event fired." {
		t.Errorf("message = %q", msg)
	}
	if gotBody["level"] != 3.0 {
		t.Errorf("body = %v, want the data sent as the payload", gotBody)
	}
}

func TestFireEventRejectsPathInjection(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request should not have been sent: %s", r.URL.Path)
	})
	if _, err := c.FireEvent(context.Background(), "../states/light.x", nil); err == nil {
		t.Error("expected an error for an event_type containing a path traversal")
	}
}

func TestGetConfigAndComponents(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/config":
			_, _ = w.Write([]byte(`{"version":"2026.9.1","time_zone":"America/New_York"}`))
		case "/api/components":
			_, _ = w.Write([]byte(`["sun","light","automation"]`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	config, err := c.GetConfig(context.Background())
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if config["version"] != "2026.9.1" {
		t.Errorf("version = %v", config["version"])
	}

	components, err := c.GetComponents(context.Background())
	if err != nil {
		t.Fatalf("GetComponents: %v", err)
	}
	if len(components) != 3 || components[1] != "light" {
		t.Errorf("components = %v", components)
	}
}

func TestCheckConfig(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/config/core/check_config" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"result":"invalid","errors":"Integration 'foo' not found."}`))
	})

	result, err := c.CheckConfig(context.Background())
	if err != nil {
		t.Fatalf("CheckConfig: %v", err)
	}
	if result["result"] != "invalid" {
		t.Errorf("result = %v", result)
	}
	if result["errors"] == nil {
		t.Error("expected errors to be surfaced")
	}
}

func TestScriptDescriptionFallbacksAndAliases(t *testing.T) {
	// A described script keeps its description and gains its aliases.
	got := scriptDescription("movie_night",
		map[string]any{"description": "Dims the lights and starts the projector."},
		[]string{"cinema mode", "film time"})
	if !contains(got, "Dims the lights") {
		t.Errorf("lost the description: %q", got)
	}
	if !contains(got, "cinema mode") || !contains(got, "film time") {
		t.Errorf("lost the aliases: %q", got)
	}

	// With no description, fall back to the name, then to the object id.
	got = scriptDescription("movie_night", map[string]any{"name": "Movie Night"}, nil)
	if got != "Movie Night" {
		t.Errorf("got %q, want the name", got)
	}
	got = scriptDescription("movie_night", map[string]any{}, nil)
	if !contains(got, "movie_night") {
		t.Errorf("got %q, want a fallback mentioning the object id", got)
	}
}
