package media

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeArr is a Radarr or Sonarr that serves recorded lookup results and an
// in-memory library, and records every add.
type fakeArr struct {
	t       *testing.T
	kind    string // "movie" or "series"
	idField string // "tmdbId" or "tvdbId"
	lookups map[string][]map[string]any

	mu       sync.Mutex
	library  []map[string]any
	profiles []QualityProfile
	folders  []RootFolder
	queue    string
	posts    []map[string]any
}

func loadLookups(t *testing.T, file string) map[string][]map[string]any {
	t.Helper()
	b, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string][]map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func newFakeRadarr(t *testing.T) *fakeArr {
	return &fakeArr{t: t, kind: "movie", idField: "tmdbId", lookups: loadLookups(t, "radarr_lookup.json"),
		profiles: []QualityProfile{{1, "Any"}, {4, "HD-1080p"}, {5, "Ultra-HD"}},
		folders:  []RootFolder{{1, "/data/movies", true, 4 << 40}},
	}
}

func newFakeSonarr(t *testing.T) *fakeArr {
	return &fakeArr{t: t, kind: "series", idField: "tvdbId", lookups: loadLookups(t, "sonarr_lookup.json"),
		profiles: []QualityProfile{{1, "Any"}, {4, "HD-1080p"}, {6, "HD - 720p/1080p"}},
		folders:  []RootFolder{{1, "/data/tv", true, 2 << 40}, {2, "/data/anime", true, 1 << 40}},
	}
}

func idOf(item map[string]any, field string) int {
	f, _ := item[field].(float64)
	return int(f)
}

func (f *fakeArr) find(id int) map[string]any {
	for _, rows := range f.lookups {
		for _, r := range rows {
			if idOf(r, f.idField) == id {
				// Mark library items as the real lookup does.
				for _, l := range f.library {
					if idOf(l, f.idField) == id {
						return l
					}
				}
				return r
			}
		}
	}
	return nil
}

func (f *fakeArr) write(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeArr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("X-Api-Key") != "test-key" {
		http.Error(w, `{"message":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	base := "/api/v3/" + f.kind
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/api/v3/rootfolder":
		f.write(w, f.folders)
	case r.URL.Path == "/api/v3/qualityprofile":
		f.write(w, f.profiles)
	case r.URL.Path == "/api/v3/queue":
		w.Header().Set("Content-Type", "application/json")
		if f.queue == "" {
			_, _ = w.Write([]byte(`{"page":1,"pageSize":20,"totalRecords":0,"records":[]}`))
			return
		}
		_, _ = w.Write([]byte(f.queue))
	case r.URL.Path == base+"/lookup/tmdb":
		id, _ := strconv.Atoi(q.Get("tmdbId"))
		if item := f.find(id); item != nil {
			f.write(w, item)
			return
		}
		http.Error(w, `{"message":"Movie not found"}`, http.StatusNotFound)
	case r.URL.Path == base+"/lookup":
		term := q.Get("term")
		if strings.HasPrefix(term, "tvdb:") {
			id, _ := strconv.Atoi(strings.TrimPrefix(term, "tvdb:"))
			if item := f.find(id); item != nil {
				f.write(w, []map[string]any{item})
				return
			}
			f.write(w, []any{})
			return
		}
		rows := []map[string]any{}
		for _, r := range f.lookups[term] {
			// Real lookups mark library items with their id and state.
			row := map[string]any{}
			for k, v := range r {
				row[k] = v
			}
			for _, l := range f.library {
				if idOf(l, f.idField) == idOf(r, f.idField) {
					for _, k := range []string{"id", "hasFile", "monitored", "statistics"} {
						if v, ok := l[k]; ok {
							row[k] = v
						}
					}
				}
			}
			rows = append(rows, row)
		}
		f.write(w, rows)
	case r.URL.Path == base && r.Method == http.MethodGet:
		out := []map[string]any{}
		for _, l := range f.library {
			if v := q.Get(f.idField); v == "" || strconv.Itoa(idOf(l, f.idField)) == v {
				out = append(out, l)
			}
		}
		f.write(w, out)
	case r.URL.Path == base && r.Method == http.MethodPost:
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			f.t.Errorf("add body: %v", err)
		}
		for _, l := range f.library {
			if idOf(l, f.idField) == idOf(body, f.idField) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`[{"propertyName":"` + f.idField + `","errorMessage":"This ` + f.kind + ` has already been added","severity":"error"}]`))
				return
			}
		}
		f.posts = append(f.posts, body)
		added := map[string]any{}
		for k, v := range body {
			added[k] = v
		}
		added["id"] = float64(1000 + len(f.posts))
		f.library = append(f.library, added)
		f.write(w, added)
	default:
		http.Error(w, `{"message":"NotFound"}`, http.StatusNotFound)
	}
}

func (f *fakeArr) postCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func (f *fakeArr) lastPost() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.posts) == 0 {
		return nil
	}
	return f.posts[len(f.posts)-1]
}

// connect serves the fakes behind real clients and returns an MCP client
// session onto the registered tools.
func connect(t *testing.T, radarr, sonarr *fakeArr, now time.Time) *mcp.ClientSession {
	t.Helper()
	var rURL, sURL string
	if radarr != nil {
		srv := httptest.NewServer(radarr)
		t.Cleanup(srv.Close)
		rURL = srv.URL
	}
	if sonarr != nil {
		srv := httptest.NewServer(sonarr)
		t.Cleanup(srv.Close)
		sURL = srv.URL
	}
	key := func(u string) string {
		if u == "" {
			return ""
		}
		return "test-key"
	}
	tools, err := NewTools(sURL, key(sURL), rURL, key(rURL))
	if err != nil {
		t.Fatal(err)
	}
	tools.now = func() time.Time { return now }
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

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}
