package hass

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A replay of a Home Assistant instance: the raw REST/WebSocket payloads the
// tools consume, served by an httptest server so a question set can be run
// end to end through the real MCP tool handlers and its result sizes counted.
//
// Two sources fill it. syntheticFixture builds a home with the shape of a real
// one (domain mix, diagnostic/config share, unavailable share, chatty motion
// sensors) and backs the committed budget tests. loadReplayDir reads captures
// of a live instance, which are personal data and never committed; point
// HASS_REPLAY_DIR at them to rerun the question set against the real thing.
type replayFixture struct {
	// now is when the capture was taken. Requested time windows are shifted by
	// (wall clock - now) so a capture replays as if it were current.
	now      time.Time
	config   map[string]any
	states   []map[string]any
	entities []map[string]any
	devices  []map[string]any
	areas    []map[string]any
	floors   []map[string]any
	logbook  []map[string]any
	// history holds raw recorder series for entities whose history is not
	// derivable from the logbook (numeric sensors are not logbooked).
	history map[string][]map[string]any
	// stats is recorder/statistics_during_period output keyed by statistic_id.
	stats map[string][]map[string]any
}

func (f *replayFixture) shift(t time.Time) time.Time {
	return t.Add(-time.Since(f.now))
}

func parseHATime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999-07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func (f *replayFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/api/states", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, f.states) })
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, f.config) })
	mux.HandleFunc("/api/logbook/", func(w http.ResponseWriter, r *http.Request) {
		since, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/logbook/"))
		start := f.shift(parseHATime(since))
		end := f.now
		if e := r.URL.Query().Get("end_time"); e != "" {
			end = f.shift(parseHATime(e))
		}
		want := splitList(r.URL.Query().Get("entity"))
		out := []map[string]any{}
		for _, e := range f.logbook {
			when := parseHATime(str(e, "when"))
			if when.Before(start) || when.After(end) {
				continue
			}
			if len(want) > 0 && !inList(want, str(e, "entity_id")) {
				continue
			}
			out = append(out, e)
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/api/history/period/", func(w http.ResponseWriter, r *http.Request) {
		since, _ := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/history/period/"))
		start := f.shift(parseHATime(since))
		end := f.now
		if e := r.URL.Query().Get("end_time"); e != "" {
			end = f.shift(parseHATime(e))
		}
		out := [][]map[string]any{}
		for _, id := range splitList(r.URL.Query().Get("filter_entity_id")) {
			if s := f.historyFor(id, start, end); len(s) > 0 {
				out = append(out, s)
			}
		}
		writeJSON(w, out)
	})
	mux.HandleFunc("/api/calendars", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404: Not Found", http.StatusNotFound)
	})
	mux.HandleFunc("/api/error_log", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "404: Not Found", http.StatusNotFound)
	})
	mux.HandleFunc("/api/websocket", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.WriteJSON(map[string]any{"type": "auth_required"})
		var auth map[string]any
		if conn.ReadJSON(&auth) != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "auth_ok"})
		for {
			var cmd map[string]any
			if conn.ReadJSON(&cmd) != nil {
				return
			}
			resp := map[string]any{"id": cmd["id"], "type": "result", "success": true}
			switch cmd["type"] {
			case "config/entity_registry/list":
				resp["result"] = f.entities
			case "config/device_registry/list":
				resp["result"] = f.devices
			case "config/area_registry/list":
				resp["result"] = f.areas
			case "config/floor_registry/list":
				resp["result"] = f.floors
			case "config/label_registry/list", "repairs/list_issues", "persistent_notification/get", "system_log/list":
				resp["result"] = []any{}
			case "recorder/statistics_during_period":
				res := map[string]any{}
				ids, _ := cmd["statistic_ids"].([]any)
				for _, id := range ids {
					if s, ok := f.stats[fmt.Sprint(id)]; ok {
						res[fmt.Sprint(id)] = s
					}
				}
				resp["result"] = res
			default:
				resp["success"] = false
				resp["error"] = map[string]any{"code": "unknown_command", "message": "Unknown command."}
			}
			_ = conn.WriteJSON(resp)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func inList(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// historyFor builds a recorder series for id over [start, end]: from the raw
// history when the fixture has one, else from the entity's logbook entries,
// seeded with the state in force at start as the recorder does.
func (f *replayFixture) historyFor(id string, start, end time.Time) []map[string]any {
	var points []map[string]any
	if raw, ok := f.history[id]; ok {
		points = raw
	} else {
		for _, e := range f.logbook {
			if str(e, "entity_id") == id && str(e, "state") != "" {
				points = append(points, map[string]any{"state": e["state"], "last_changed": e["when"]})
			}
		}
	}
	var attrs map[string]any
	initial := ""
	for _, s := range f.states {
		if str(s, "entity_id") == id {
			attrs, _ = s["attributes"].(map[string]any)
			initial = str(s, "state")
		}
	}
	if attrs == nil && len(points) == 0 {
		return nil
	}
	var out []map[string]any
	for _, p := range points {
		when := parseHATime(str(p, "last_changed"))
		if when.Before(start) {
			initial = str(p, "state")
			continue
		}
		if when.After(end) {
			break
		}
		out = append(out, map[string]any{"state": p["state"], "last_changed": p["last_changed"]})
	}
	if len(out) > 0 && f.history[id] == nil {
		// The state before the first logbooked change is the opposite of it
		// for most of what is logbooked; good enough for a replay seed.
		initial = str(out[0], "state")
	}
	first := map[string]any{
		"entity_id":    id,
		"state":        initial,
		"attributes":   attrs,
		"last_changed": start.UTC().Format(time.RFC3339),
		"last_updated": start.UTC().Format(time.RFC3339),
	}
	return append([]map[string]any{first}, out...)
}

// loadReplayDir reads live captures: states_full.json and registry_full.json
// (get_home_states attributes=true / get_home_registry full=true output),
// logbook24.json (get_home_events hours=24), config.json (GET /api/config),
// and optional hist_*.json / stats_*.json (get_state_history and
// get_long_term_statistics output).
func loadReplayDir(t *testing.T, dir string) *replayFixture {
	t.Helper()
	read := func(name string, v any) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
	}
	f := &replayFixture{history: map[string][]map[string]any{}, stats: map[string][]map[string]any{}}
	var states struct {
		States []map[string]any `json:"states"`
	}
	read("states_full.json", &states)
	f.states = states.States
	var reg struct {
		Entities, Devices, Areas, Floors []map[string]any
	}
	read("registry_full.json", &reg)
	f.entities, f.devices, f.areas, f.floors = reg.Entities, reg.Devices, reg.Areas, reg.Floors
	var lb struct {
		Events []map[string]any `json:"events"`
	}
	read("logbook24.json", &lb)
	f.logbook = lb.Events
	read("config.json", &f.config)
	// The capture time is the newest thing observed.
	for _, s := range f.states {
		if t := parseHATime(str(s, "last_updated")); t.After(f.now) {
			f.now = t
		}
	}
	hist, _ := filepath.Glob(filepath.Join(dir, "hist_*.json"))
	for _, p := range hist {
		var h struct {
			History [][]map[string]any `json:"history"`
		}
		read(filepath.Base(p), &h)
		for _, series := range h.History {
			if len(series) > 0 {
				f.history[str(series[0], "entity_id")] = series
			}
		}
	}
	stats, _ := filepath.Glob(filepath.Join(dir, "stats_*.json"))
	for _, p := range stats {
		var s struct {
			Statistics map[string][]map[string]any `json:"statistics"`
		}
		read(filepath.Base(p), &s)
		for k, v := range s.Statistics {
			f.stats[k] = v
		}
	}
	return f
}

// replaySession registers the hand-written HA tools against the fixture.
func replaySession(t *testing.T, f *replayFixture) *mcp.ClientSession {
	t.Helper()
	srv := f.server(t)
	tools, err := NewTools(srv.URL, "test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "replay", Version: "0"}, nil)
	tools.Register(server)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

type replayCall struct {
	tool string
	args map[string]any
}

type replayQuestion struct {
	q     string
	calls []replayCall
}

type replayOutcome struct {
	q       string
	calls   int
	bytes   int
	errored bool
	texts   []string
}

func runQuestions(t *testing.T, cs *mcp.ClientSession, qs []replayQuestion) []replayOutcome {
	t.Helper()
	var out []replayOutcome
	for _, q := range qs {
		o := replayOutcome{q: q.q}
		for _, c := range q.calls {
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: c.tool, Arguments: c.args})
			if err != nil {
				t.Fatalf("%s: %s: %v", q.q, c.tool, err)
			}
			o.calls++
			for _, ct := range res.Content {
				if tc, ok := ct.(*mcp.TextContent); ok {
					o.bytes += len(tc.Text)
					o.texts = append(o.texts, tc.Text)
				}
			}
			if res.IsError {
				o.errored = true
			}
		}
		out = append(out, o)
	}
	return out
}

func logOutcomes(t *testing.T, outs []replayOutcome) {
	t.Helper()
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-58s %5s %9s %8s\n", "question", "calls", "bytes", "~tokens")
	total := 0
	for _, o := range outs {
		flag := ""
		if o.errored {
			flag = " (error)"
		}
		fmt.Fprintf(&b, "%-58s %5d %9d %8d%s\n", o.q, o.calls, o.bytes, o.bytes/4, flag)
		total += o.bytes
	}
	fmt.Fprintf(&b, "%-58s %5s %9d %8d\n", "total", "", total, total/4)
	t.Log(b.String())
}

// TestReplayLiveCaptures reruns the household question set against captures
// of a real instance. Opt-in: set HASS_REPLAY_DIR (see loadReplayDir).
func TestReplayLiveCaptures(t *testing.T) {
	dir := os.Getenv("HASS_REPLAY_DIR")
	if dir == "" {
		t.Skip("HASS_REPLAY_DIR not set")
	}
	f := loadReplayDir(t, dir)
	cs := replaySession(t, f)
	outs := runQuestions(t, cs, liveQuestions())
	logOutcomes(t, outs)
	if os.Getenv("HASS_REPLAY_DUMP") != "" {
		for _, o := range outs {
			for i, txt := range o.texts {
				_ = os.WriteFile(filepath.Join(os.Getenv("HASS_REPLAY_DUMP"), fmt.Sprintf("%s.%d.json", slug(o.q), i)), []byte(txt), 0o600)
			}
		}
	}
}

func slug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

var _ = sort.Strings
