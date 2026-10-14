package media

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
		_, _ = w.Write([]byte(`[{"title":"Alien","year":1979,"tmdbId":348,"id":12,"hasFile":true}]`))
	})

	results, err := c.SearchMovies(context.Background(), "alien")
	if err != nil {
		t.Fatalf("SearchMovies: %v", err)
	}
	if len(results) != 1 || results[0].Title != "Alien" || results[0].TmdbID != 348 || results[0].ID != 12 {
		t.Errorf("unexpected results: %+v", results)
	}
}

func TestAddMovieOverlaysLookup(t *testing.T) {
	c := newTestRadarr(t, func(w http.ResponseWriter, r *http.Request) {
		assertAPIKey(t, r)
		if r.Method != "POST" || r.URL.Path != "/api/v3/movie" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		if req["tmdbId"] != float64(348) || req["titleSlug"] != "alien-348" || req["qualityProfileId"] != float64(4) {
			t.Errorf("unexpected request body: %v", req)
		}
		if _, ok := req["id"]; ok {
			t.Error("lookup id must not be posted")
		}
		_, _ = w.Write([]byte(`{"id":1,"title":"Alien","year":1979,"tmdbId":348}`))
	})

	base := map[string]any{"id": 12.0, "tmdbId": 348.0, "title": "Alien", "titleSlug": "alien-348", "qualityProfileId": 0.0}
	result, err := c.AddMovie(context.Background(), AddMovieRequest{
		TmdbID:           348,
		Title:            "Alien",
		QualityProfileID: 4,
		AddOptions:       AddMovieOptions{SearchForMovie: true},
	}, base)
	if err != nil {
		t.Fatalf("AddMovie: %v", err)
	}
	if result.Title != "Alien" {
		t.Errorf("result = %+v", result)
	}
}

// Sonarr v5 serialises sizes as doubles; an int64 field would reject them.
func TestGetQueueProgressAndDoubles(t *testing.T) {
	c := newTestRadarr(t, func(w http.ResponseWriter, r *http.Request) {
		assertAPIKey(t, r)
		if r.URL.Path != "/api/v3/queue" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("includeMovie") != "true" || q.Get("includeEpisode") != "true" {
			t.Errorf("queue query = %v", q)
		}
		_, _ = w.Write([]byte(`{"page":1,"pageSize":50,"totalRecords":2,"records":[
			{"id":1,"title":"Half","size":1000.0,"sizeleft":500.5,"protocol":"torrent"},
			{"id":2,"title":"Unknown","size":0,"sizeleft":0,"protocol":"usenet"}
		]}`))
	})

	queue, err := c.GetQueue(context.Background(), 0)
	if err != nil {
		t.Fatalf("GetQueue: %v", err)
	}
	if queue.Records[0].Progress != 49.95 {
		t.Errorf("progress = %v, want 49.95", queue.Records[0].Progress)
	}
	if queue.Records[1].Progress != 0 {
		t.Errorf("zero-size progress = %v, want 0", queue.Records[1].Progress)
	}
}

// Every failure must tell a model what to do next.
func TestErrorMessages(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"401 names the key setting", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
		}, "check RADARR_API_KEY"},
		{"404 suggests a base path", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"NotFound"}`, http.StatusNotFound)
		}, "base path such as /radarr"},
		{"HTML login page", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("<!DOCTYPE html><html>Sign in</html>"))
		}, "returned HTML"},
		{"malformed JSON", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[{"title":`))
		}, "failed to parse Radarr movie lookup"},
		{"validation array", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`[{"propertyName":"Path","errorMessage":"Path is already configured for an existing movie"}]`))
		}, "Path is already configured"},
		{"500 suggests retry", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"TMDb API is unavailable"}`, http.StatusServiceUnavailable)
		}, "retry shortly"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestRadarr(t, tc.handler)
			_, err := c.SearchMovies(context.Background(), "alien")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestTimeoutAndUnreachable(t *testing.T) {
	block := make(chan struct{})
	c := newTestRadarr(t, func(w http.ResponseWriter, r *http.Request) { <-block })
	t.Cleanup(func() { close(block) })
	c.httpClient.Timeout = 50 * time.Millisecond
	if _, err := c.SearchMovies(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "did not respond") {
		t.Errorf("timeout err = %v", err)
	}

	s, _ := NewSonarrClient("http://127.0.0.1:1", "k")
	if _, err := s.SearchSeries(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "SONARR_URL") {
		t.Errorf("unreachable err = %v", err)
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

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"Shōgun":                           "shogun",
		"The Office (US)":                  "the office",
		"The Office (CA) (2012)":           "the office",
		"Battlestar Galactica (2003)":      "battlestar galactica",
		"Dune: Part Two":                   "dune part two",
		"Love, Death & Robots":             "love death and robots",
		"Fullmetal Alchemist: Brotherhood": "fullmetal alchemist brotherhood",
	}
	for in, want := range cases {
		if got := normalizeTitle(in); got != want {
			t.Errorf("normalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second: "under a minute",
		45 * time.Minute: "45m",
		90 * time.Minute: "1h 30m",
		50 * time.Hour:   "2d 2h",
	}
	for d, want := range cases {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
