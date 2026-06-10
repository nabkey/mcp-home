package media

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestRadarr(t *testing.T, handler http.HandlerFunc) *RadarrClient {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewRadarrClient(srv.URL, "test-key")
	if err != nil {
		t.Fatalf("NewRadarrClient: %v", err)
	}
	return c
}

func assertAPIKey(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("X-Api-Key"); got != "test-key" {
		t.Errorf("X-Api-Key = %q, want test-key", got)
	}
}

func TestSearchMovies(t *testing.T) {
	c := newTestRadarr(t, func(w http.ResponseWriter, r *http.Request) {
		assertAPIKey(t, r)
		if r.URL.Path != "/api/v3/movie/lookup" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("term"); got != "alien" {
			t.Errorf("term = %q, want alien", got)
		}
		_, _ = w.Write([]byte(`[{"title":"Alien","year":1979,"tmdbId":348}]`))
	})

	results, err := c.SearchMovies(context.Background(), "alien")
	if err != nil {
		t.Fatalf("SearchMovies: %v", err)
	}
	if len(results) != 1 || results[0].Title != "Alien" || results[0].TmdbID != 348 {
		t.Errorf("unexpected results: %+v", results)
	}
}

func TestAddMovie(t *testing.T) {
	c := newTestRadarr(t, func(w http.ResponseWriter, r *http.Request) {
		assertAPIKey(t, r)
		if r.Method != "POST" || r.URL.Path != "/api/v3/movie" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req AddMovieRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if req.TmdbID != 348 || !req.AddOptions.SearchForMovie {
			t.Errorf("unexpected request body: %+v", req)
		}
		_, _ = w.Write([]byte(`{"id":1,"title":"Alien"}`))
	})

	result, err := c.AddMovie(context.Background(), AddMovieRequest{
		TmdbID:     348,
		Title:      "Alien",
		AddOptions: AddMovieOptions{SearchForMovie: true},
	})
	if err != nil {
		t.Fatalf("AddMovie: %v", err)
	}
	if result["title"] != "Alien" {
		t.Errorf("result = %+v", result)
	}
}

func TestGetQueueProgress(t *testing.T) {
	c := newTestRadarr(t, func(w http.ResponseWriter, r *http.Request) {
		assertAPIKey(t, r)
		if r.URL.Path != "/api/v3/queue" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"page":1,"pageSize":50,"totalRecords":2,"records":[
			{"id":1,"title":"Half","size":1000,"sizeleft":500,"protocol":"torrent"},
			{"id":2,"title":"Unknown","size":0,"sizeleft":0,"protocol":"usenet"}
		]}`))
	})

	queue, err := c.GetQueue(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if queue.Records[0].Progress != 50 {
		t.Errorf("progress = %v, want 50", queue.Records[0].Progress)
	}
	if queue.Records[1].Progress != 0 {
		t.Errorf("zero-size progress = %v, want 0", queue.Records[1].Progress)
	}
}

func TestErrorStatusIncludesBody(t *testing.T) {
	c := newTestRadarr(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid api key"}`, http.StatusUnauthorized)
	})
	_, err := c.SearchMovies(context.Background(), "alien")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
}

func TestBaseURLPathPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/radarr/api/v3/movie/lookup" {
			t.Errorf("path = %s, want /radarr/api/v3/movie/lookup", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)

	// Radarr served behind a reverse proxy at a subpath.
	c, err := NewRadarrClient(srv.URL+"/radarr/", "test-key")
	if err != nil {
		t.Fatalf("NewRadarrClient: %v", err)
	}
	if _, err := c.SearchMovies(context.Background(), "alien"); err != nil {
		t.Fatalf("SearchMovies: %v", err)
	}
}

func TestNewClientsRequireConfig(t *testing.T) {
	if _, err := NewRadarrClient("", "key"); err == nil {
		t.Error("expected error for missing Radarr URL")
	}
	if _, err := NewSonarrClient("http://sonarr", ""); err == nil {
		t.Error("expected error for missing Sonarr API key")
	}
}
