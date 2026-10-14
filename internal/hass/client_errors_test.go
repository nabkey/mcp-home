package hass

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The errors a real instance (or what sits in front of it) produces, and
// whether what comes back tells a model what to do next. A model cannot look
// at Home Assistant itself; the message is all it has.
func TestClientErrorsAreActionable(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    []string
		notWant []string
	}{
		{
			name: "bad token",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "401: Unauthorized", http.StatusUnauthorized)
			},
			want: []string{"401", "HASS_TOKEN"},
		},
		{
			name: "missing endpoint",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "404: Not Found", http.StatusNotFound)
			},
			want:    []string{"GET /api/states", "404", "not loaded"},
			notWant: []string{"404: Not Found"},
		},
		{
			name: "proxy 502 with an HTML page",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte("<html><body><h1>502 Bad Gateway</h1>" + strings.Repeat("x", 5000) + "</body></html>"))
			},
			want:    []string{"502", "retry"},
			notWant: []string{"<html>"},
		},
		{
			name: "login page instead of the API",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write([]byte("<!doctype html><title>Sign in</title>"))
			},
			want: []string{"HTML", "HASS_URL"},
		},
		{
			name: "HA's own error message",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"message":"Invalid filter_entity_id"}`))
			},
			want: []string{"400", "Invalid filter_entity_id"},
		},
		{
			name: "server error with a long body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(strings.Repeat("traceback line\n", 1000)))
			},
			want: []string{"500", "get_diagnostics", "..."},
		},
		{
			name: "truncated JSON",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`[{"entity_id":"light.a","state":"on"`))
			},
			want: []string{"states"},
		},
		{
			name: "oversized body",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("[" + strings.Repeat(`{"entity_id":"sensor.x","state":"1"},`, maxJSONResponseSize/36+10)))
			},
			want: []string{"exceeds 5 MB", "narrow"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := newTestClient(t, c.handler)
			_, err := client.GetStates(context.Background())
			if err == nil {
				t.Fatal("expected an error")
			}
			msg := err.Error()
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error %q does not mention %q", msg, w)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(msg, w) {
					t.Errorf("error %q should not contain %q", msg, w)
				}
			}
			if len(msg) > 600 {
				t.Errorf("error is %d bytes; a model only needs the gist", len(msg))
			}
		})
	}
}

func TestClientSlowResponse(t *testing.T) {
	release := make(chan struct{})
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	client.httpClient.Timeout = 50 * time.Millisecond
	_, err := client.GetStates(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no answer") || !strings.Contains(err.Error(), "retry") {
		t.Errorf("err = %v, want a timeout the model can retry", err)
	}
}

func TestWSErrorMessages(t *testing.T) {
	c, _ := newTestWSClient(t, func(cmd map[string]any) map[string]any {
		return map[string]any{"success": false, "error": map[string]any{"code": "unknown_command", "message": "Unknown command."}}
	})
	_, err := c.ListAreas()
	if err == nil || !strings.Contains(err.Error(), "does not support") || strings.Contains(err.Error(), "map[") {
		t.Errorf("err = %v", err)
	}
}

// When the registries cannot be read (WebSocket refused), the state tools
// still answer, just without areas.
func TestStatesWithoutRegistry(t *testing.T) {
	fake := &fakeHA{states: `[{"entity_id":"light.porch","state":"on","attributes":{"friendly_name":"Porch"},"last_changed":"2026-10-13T21:00:00Z"}]`}
	session := connectRegistered(t, fake)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_home_states", Arguments: map[string]any{"search": "porch"}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if res.IsError || !strings.Contains(text, `"entity_id":"light.porch"`) {
		t.Errorf("result = %s", text)
	}
	// /api/config 404s here too, so times fall back to UTC.
	if !strings.Contains(text, `"last_changed":"2026-10-13T21:00:00Z"`) {
		t.Errorf("result = %s, want UTC times when the time zone is unknown", text)
	}
}

// connectRegistered is connect for the hand-written tools.
func connectRegistered(t *testing.T, fake *fakeHA) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)
	tools, err := NewTools(srv.URL, "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tools.Register(server)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := server.Connect(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// A base URL with a path prefix (Home Assistant behind a reverse proxy at
// /ha) must reach the WebSocket under the same prefix.
func TestWebsocketKeepsPathPrefix(t *testing.T) {
	f := &replayFixture{now: time.Now(), areas: []map[string]any{{"area_id": "kitchen", "name": "Kitchen"}}}
	srv := f.server(t)
	mux := http.NewServeMux()
	mux.Handle("/ha/", http.StripPrefix("/ha", srv.Config.Handler))
	prefixed := httptest.NewServer(mux)
	t.Cleanup(prefixed.Close)
	c, err := NewClient(prefixed.URL+"/ha", "t")
	if err != nil {
		t.Fatal(err)
	}
	ws := c.NewWebsocketClient()
	if err := ws.Dial(context.Background()); err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = ws.Close() }()
	areas, err := ws.ListAreas()
	if err != nil || len(areas) != 1 {
		t.Errorf("areas = %v, %v", areas, err)
	}
}
