package media

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 14, 2, 30, 0, 0, time.UTC)

// trickyTitles are requests where a plain title search returns more than
// one plausible match on the live Radarr/Sonarr (results recorded in
// testdata). want is the right id; confusable is the one a model is most
// likely to pick instead.
var trickyTitles = []struct {
	request     string
	service     string
	term        string
	title       string
	year        int
	want        int
	confusable  int
	ambiguous   bool // results share a title a person would say
	anime       bool
	description string
}{
	{"Dune, the Villeneuve one", "radarr", "Dune", "Dune", 2021, 438631, 697620, true, false, "a 4-minute 2020 short is also titled Dune"},
	{"the original Dune", "radarr", "Dune", "Dune", 1984, 841, 438631, true, false, "1984 vs 2021"},
	{"The Lion King, 2019 not the cartoon", "radarr", "The Lion King", "The Lion King", 2019, 420818, 8587, true, false, "remake shares the title"},
	{"Little Women from 2019", "radarr", "Little Women", "Little Women", 2019, 331482, 9587, true, false, "five films share the title"},
	{"Halloween, the 2018 one", "radarr", "Halloween", "Halloween", 2018, 424139, 948, true, false, "1978, 2007, 2018 and a 2019 fan film"},
	{"the new Nosferatu", "radarr", "Nosferatu", "Nosferatu", 2024, 426063, 1528448, true, false, "an unrelated 2025 Nosferatu is announced"},
	{"the 1998 Godzilla", "radarr", "Godzilla", "Godzilla", 1998, 929, 124905, true, false, "four films titled Godzilla"},
	{"Total Recall, the remake", "radarr", "Total Recall", "Total Recall", 2012, 64635, 861, true, false, "remake shares the title"},
	{"The Office, the American one", "sonarr", "The Office", "The Office (US)", 2005, 73244, 78107, true, false, "UK original listed first"},
	{"the new Shogun", "sonarr", "Shogun", "Shogun", 2024, 392573, 80284, true, false, "accented title, 1980 miniseries"},
	{"One Piece live action", "sonarr", "One Piece", "One Piece", 2023, 392276, 81797, true, false, "anime listed first"},
	{"the new Doctor Who", "sonarr", "Doctor Who", "Doctor Who (2023)", 2024, 449991, 78804, true, false, "titled 2023, year 2024"},
	{"Death Note, the anime", "sonarr", "Death Note", "Death Note", 2006, 79481, 296956, true, true, "live-action drama on the same network"},
	{"the reimagined Battlestar Galactica", "sonarr", "Battlestar Galactica", "Battlestar Galactica", 2005, 73545, 71173, true, false, "titled 2003, year 2005"},
	{"House of Cards with Kevin Spacey", "sonarr", "House of Cards", "House of Cards (US)", 2013, 262980, 79861, true, false, "BBC original listed first"},
	{"Fullmetal Alchemist Brotherhood", "sonarr", "Fullmetal Alchemist", "Fullmetal Alchemist: Brotherhood", 2009, 85249, 75579, false, true, "2003 series has the shorter title"},
}

func idKey(service string) (search, add, idArg string) {
	if service == "radarr" {
		return "search_movies", "add_movie", "tmdb_id"
	}
	return "search_series", "add_series", "tvdb_id"
}

// The judge for wrong-title adds: for every tricky request, the search
// result separates the candidates, picking the confusable id with the year
// the user meant is refused without a write, and the right id is added
// exactly once with the right type.
func TestTrickyTitlesNeverAddTheWrongOne(t *testing.T) {
	for _, tc := range trickyTitles {
		t.Run(tc.request, func(t *testing.T) {
			radarr, sonarr := newFakeRadarr(t), newFakeSonarr(t)
			cs := connect(t, radarr, sonarr, testNow)
			fake := radarr
			if tc.service == "sonarr" {
				fake = sonarr
			}
			search, add, idArg := idKey(tc.service)

			// 1. The search shows both candidates with distinct years.
			txt, isErr := callTool(t, cs, search, map[string]any{"term": tc.term})
			if isErr {
				t.Fatalf("search: %s", txt)
			}
			var res struct {
				Results []map[string]any `json:"results"`
				Ambig   []titleGroup     `json:"ambiguous"`
			}
			if err := json.Unmarshal([]byte(txt), &res); err != nil {
				t.Fatal(err)
			}
			years := map[int]int{}
			for _, r := range res.Results {
				years[int(r[idArg].(float64))] = int(r["year"].(float64))
			}
			if years[tc.want] != tc.year {
				t.Errorf("right id %d shown with year %d, want %d", tc.want, years[tc.want], tc.year)
			}
			if _, ok := years[tc.confusable]; !ok {
				t.Errorf("confusable id %d not in results", tc.confusable)
			}
			if got := len(res.Ambig) > 0; got != tc.ambiguous {
				t.Errorf("ambiguous flagged = %v, want %v (%s): %s", got, tc.ambiguous, tc.description, txt)
			}

			// 2. The likely mistake is refused, and nothing is written.
			args := map[string]any{idArg: tc.confusable, "title": tc.title, "year": tc.year}
			txt, isErr = callTool(t, cs, add, args)
			if !isErr || !strings.Contains(txt, "refusing to add") {
				t.Fatalf("wrong id %d was not refused: %s", tc.confusable, txt)
			}
			if fake.postCount() != 0 {
				t.Fatalf("wrong id caused %d writes", fake.postCount())
			}

			// 3. The right one is added once, with the lookup's identity.
			args[idArg] = tc.want
			txt, isErr = callTool(t, cs, add, args)
			if isErr {
				t.Fatalf("right id refused: %s", txt)
			}
			post := fake.lastPost()
			if post == nil || idOf(post, fake.idField) != tc.want {
				t.Fatalf("posted %v, want id %d", post, tc.want)
			}
			if tc.service == "sonarr" {
				wantType := "standard"
				if tc.anime {
					wantType = "anime"
				}
				if post["seriesType"] != wantType {
					t.Errorf("seriesType = %v, want %s", post["seriesType"], wantType)
				}
			}

			// 4. Asking again does not add a duplicate.
			txt, isErr = callTool(t, cs, add, args)
			if isErr || !strings.Contains(txt, `"already_in_library":true`) {
				t.Errorf("second add: %s", txt)
			}
			if fake.postCount() != 1 {
				t.Errorf("writes = %d, want 1", fake.postCount())
			}
		})
	}
}

func TestSearchProjection(t *testing.T) {
	radarr, sonarr := newFakeRadarr(t), newFakeSonarr(t)
	cs := connect(t, radarr, sonarr, testNow)

	// Year filter answers "the one from 2019" in one call.
	txt, _ := callTool(t, cs, "search_movies", map[string]any{"term": "Little Women", "year": 2019})
	if !strings.Contains(txt, `"tmdb_id":331482`) || strings.Contains(txt, `"tmdb_id":9587`) {
		t.Errorf("year filter: %s", txt)
	}
	// Shorts and making-ofs are labelled, not presented as the film.
	txt, _ = callTool(t, cs, "search_movies", map[string]any{"term": "Dune"})
	if !strings.Contains(txt, `"tmdb_id":697620,"imdb_id":"tt12451788","studio":"Kecskemétfilm","runtime_min":4,"status":"announced","in_library":false,"kind":"short"`) {
		t.Errorf("short not labelled: %s", txt)
	}
	// Unreleased films say when they reach home release.
	if !strings.Contains(txt, `"digital_release":"2027-02-02"`) {
		t.Errorf("release date missing: %s", txt)
	}
	// Series carry network, type and a real season count (was always 0).
	txt, _ = callTool(t, cs, "search_series", map[string]any{"term": "One Piece"})
	for _, want := range []string{
		`"title":"One Piece","year":1999,"tvdb_id":81797,"imdb_id":"tt0388629","network":"Fuji TV","type":"anime","language":"Japanese","status":"continuing","seasons":3`,
		`"title":"ONE PIECE (2023)","year":2023,"tvdb_id":392276,"imdb_id":"tt11737520","network":"Netflix","type":"standard","status":"continuing","seasons":2`,
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("series projection missing %s in %s", want, txt)
		}
	}
	// Overviews are trimmed for the model's context.
	for _, r := range []string{"Dune", "The Lion King", "Halloween"} {
		txt, _ = callTool(t, cs, "search_movies", map[string]any{"term": r})
		var res struct {
			Results []movieSummary `json:"results"`
		}
		_ = json.Unmarshal([]byte(txt), &res)
		for _, m := range res.Results {
			if len(m.Overview) > overviewLen+3 {
				t.Errorf("%s overview %d bytes", m.Title, len(m.Overview))
			}
		}
	}
	// Empty results explain what to try.
	txt, _ = callTool(t, cs, "search_movies", map[string]any{"term": "Nonexistent Film"})
	if !strings.Contains(txt, `"results":[]`) || !strings.Contains(txt, "hint") {
		t.Errorf("empty search: %s", txt)
	}
}

func TestAddValidation(t *testing.T) {
	radarr, sonarr := newFakeRadarr(t), newFakeSonarr(t)
	cs := connect(t, radarr, sonarr, testNow)
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"add_movie", map[string]any{"tmdb_id": 420818, "title": "The Lion King"}, "missing properties: [\"year\"]"},
		{"add_movie", map[string]any{"tmdb_id": 999999, "title": "X", "year": 2000}, "no movie with tmdb_id 999999"},
		{"add_movie", map[string]any{"tmdb_id": 420818, "title": "Mufasa", "year": 2019}, "not \"Mufasa\""},
		{"add_movie", map[string]any{"tmdb_id": 420818, "title": "The Lion King", "year": 2019, "quality_profile_id": 99}, "quality profile 99 does not exist; configured: 1=Any, 4=HD-1080p, 5=Ultra-HD"},
		{"add_movie", map[string]any{"tmdb_id": 420818, "title": "The Lion King", "year": 2019, "root_folder_path": "/mnt/x"}, "root folder \"/mnt/x\" does not exist; configured: /data/movies"},
		{"add_movie", map[string]any{"tmdb_id": 420818, "title": "The Lion King", "year": 2019, "minimum_availability": "soon"}, "minimum_availability"},
		{"add_series", map[string]any{"tvdb_id": 73244, "title": "The Office", "year": 2005, "monitor": "newest"}, "invalid monitor"},
		{"add_series", map[string]any{"tvdb_id": 1, "title": "X", "year": 2000}, "no series with tvdb_id 1"},
	}
	for _, tc := range cases {
		txt, isErr := callTool(t, cs, tc.tool, tc.args)
		if !isErr || !strings.Contains(txt, tc.want) {
			t.Errorf("%s %v: got %q, want error containing %q", tc.tool, tc.args, txt, tc.want)
		}
	}
	if radarr.postCount()+sonarr.postCount() != 0 {
		t.Error("validation failure wrote to the service")
	}
}

// Defaults come from what the library uses, not list order ("Any" is
// first on a stock install and grabs whatever turns up).
func TestDefaultsFollowTheLibrary(t *testing.T) {
	t.Run("empty library avoids Any", func(t *testing.T) {
		radarr := newFakeRadarr(t)
		cs := connect(t, radarr, nil, testNow)
		txt, isErr := callTool(t, cs, "add_movie", map[string]any{"tmdb_id": 420818, "title": "The Lion King", "year": 2019})
		if isErr {
			t.Fatal(txt)
		}
		if got := radarr.lastPost()["qualityProfileId"]; got != float64(4) {
			t.Errorf("quality profile = %v, want 4 (HD-1080p): %s", got, txt)
		}
		if radarr.lastPost()["addOptions"].(map[string]any)["searchForMovie"] != true {
			t.Error("search should default on")
		}
	})
	t.Run("most used profile and folder win", func(t *testing.T) {
		radarr := newFakeRadarr(t)
		radarr.folders = append(radarr.folders, RootFolder{2, "/data/kids", true, 8 << 40})
		for i := range 5 {
			radarr.library = append(radarr.library, map[string]any{"id": float64(i + 1), "tmdbId": float64(100 + i), "qualityProfileId": float64(5), "path": fmt.Sprintf("/data/movies/M%d", i)})
		}
		radarr.library = append(radarr.library, map[string]any{"id": float64(9), "tmdbId": float64(200), "qualityProfileId": float64(4), "path": "/data/kids/K"})
		cs := connect(t, radarr, nil, testNow)
		txt, isErr := callTool(t, cs, "add_movie", map[string]any{"tmdb_id": 64635, "title": "Total Recall", "year": 2012})
		if isErr {
			t.Fatal(txt)
		}
		post := radarr.lastPost()
		if post["qualityProfileId"] != float64(5) || post["rootFolderPath"] != "/data/movies" {
			t.Errorf("post = %v", post)
		}
		if !strings.Contains(txt, `used by 5 of 6 library items`) {
			t.Errorf("reason missing: %s", txt)
		}
	})
	t.Run("anime goes to the anime folder", func(t *testing.T) {
		sonarr := newFakeSonarr(t)
		cs := connect(t, nil, sonarr, testNow)
		txt, isErr := callTool(t, cs, "add_series", map[string]any{"tvdb_id": 79481, "title": "Death Note", "year": 2006})
		if isErr {
			t.Fatal(txt)
		}
		if got := sonarr.lastPost()["rootFolderPath"]; got != "/data/anime" {
			t.Errorf("root = %v", got)
		}
	})
	t.Run("lookup record is posted, without an id", func(t *testing.T) {
		sonarr := newFakeSonarr(t)
		cs := connect(t, nil, sonarr, testNow)
		_, _ = callTool(t, cs, "add_series", map[string]any{"tvdb_id": 392573, "title": "Shogun", "year": 2024, "monitor": "latestSeason"})
		post := sonarr.lastPost()
		if post["network"] != "FX" || post["seasons"] == nil {
			t.Errorf("lookup fields not carried: %v", post)
		}
		if _, has := post["id"]; has {
			t.Error("id must not be posted")
		}
		if post["addOptions"].(map[string]any)["monitor"] != "latestSeason" {
			t.Errorf("monitor = %v", post["addOptions"])
		}
	})
}

func TestAlreadyInLibraryReportsState(t *testing.T) {
	radarr := newFakeRadarr(t)
	radarr.library = []map[string]any{{"id": float64(7), "title": "Dune", "year": float64(2021), "tmdbId": float64(438631), "hasFile": false, "monitored": true}}
	sonarr := newFakeSonarr(t)
	sonarr.library = []map[string]any{{"id": float64(3), "title": "Shōgun", "year": float64(2024), "tvdbId": float64(392573), "monitored": true, "status": "continuing",
		"seasons": []any{
			map[string]any{"seasonNumber": float64(1), "monitored": true, "statistics": map[string]any{"episodeFileCount": float64(10), "totalEpisodeCount": float64(10)}},
			map[string]any{"seasonNumber": float64(2), "monitored": false, "statistics": map[string]any{"episodeFileCount": float64(0), "totalEpisodeCount": float64(10)}},
		}}}
	cs := connect(t, radarr, sonarr, testNow)

	txt, isErr := callTool(t, cs, "add_movie", map[string]any{"tmdb_id": 438631, "title": "Dune", "year": 2021})
	if isErr || !strings.Contains(txt, `"already_in_library":true`) || !strings.Contains(txt, "monitored but not downloaded") {
		t.Errorf("movie: %s", txt)
	}
	txt, _ = callTool(t, cs, "search_movies", map[string]any{"term": "Dune"})
	if !strings.Contains(txt, `"tmdb_id":438631,"imdb_id":"tt1160419","studio":"Legendary Pictures","runtime_min":155,"status":"released","in_library":true`) {
		t.Errorf("search does not flag library item: %s", txt)
	}

	// "Get me the new season of Shōgun" when the show is already there but
	// season 2 is not monitored: the answer must say it will not download.
	txt, isErr = callTool(t, cs, "add_series", map[string]any{"tvdb_id": 392573, "title": "Shōgun", "year": 2024, "monitor": "latestSeason"})
	if isErr || !strings.Contains(txt, "season 2 is not monitored") || !strings.Contains(txt, `{"season":1,"monitored":true,"downloaded":"10/10"}`) {
		t.Errorf("series: %s", txt)
	}
	if radarr.postCount()+sonarr.postCount() != 0 {
		t.Error("duplicate add wrote to the service")
	}
}

func TestUnreleasedMovieReadiness(t *testing.T) {
	radarr := newFakeRadarr(t)
	cs := connect(t, radarr, nil, testNow)
	txt, isErr := callTool(t, cs, "add_movie", map[string]any{"tmdb_id": 1170608, "title": "Dune: Part Three", "year": 2026})
	if isErr || !strings.Contains(txt, "grab it after 2027-02-02") {
		t.Errorf("readiness: %s", txt)
	}
}

// Queue records synthesised from the Servarr v3 QueueResource schema (the
// live queue was empty when this was written): one healthy download, a
// stalled torrent, a crawling one whose ETA is weeks out, an import blocked
// on a sample file, a 3-episode season pack, and a delayed item.
const queueFixture = `{"page":1,"pageSize":20,"totalRecords":8,"records":[
 {"id":1,"movie":{"title":"The Lion King","year":2019},"title":"The.Lion.King.2019.1080p.WEB-DL","status":"downloading","trackedDownloadStatus":"ok","trackedDownloadState":"downloading","size":4.2e9,"sizeleft":1.05e9,"estimatedCompletionTime":"2026-10-14T03:15:00Z","downloadId":"A","protocol":"torrent","quality":{"quality":{"name":"WEBDL-1080p"}}},
 {"id":2,"movie":{"title":"Nosferatu","year":2024},"title":"Nosferatu.2024.2160p","status":"warning","trackedDownloadStatus":"warning","trackedDownloadState":"downloading","errorMessage":"The download is stalled with no connections","size":2.1e10,"sizeleft":2.0e10,"downloadId":"B","protocol":"torrent"},
 {"id":3,"movie":{"title":"Total Recall","year":2012},"title":"Total.Recall.2012.1080p","status":"downloading","trackedDownloadStatus":"ok","trackedDownloadState":"downloading","size":8e9,"sizeleft":7.9e9,"estimatedCompletionTime":"2026-11-20T00:00:00Z","downloadId":"C","protocol":"torrent"},
 {"id":4,"series":{"title":"Shōgun","year":2024},"episode":{"seasonNumber":2,"episodeNumber":1,"title":"Anjin"},"title":"Shogun.2024.S02E01.1080p","status":"completed","trackedDownloadStatus":"warning","trackedDownloadState":"importBlocked","statusMessages":[{"title":"Shogun.2024.S02E01.1080p","messages":["Sample file - only 52MB"]}],"size":3e9,"sizeleft":0,"downloadId":"D","protocol":"usenet"},
 {"id":5,"series":{"title":"The Office (US)","year":2005},"episode":{"seasonNumber":3,"episodeNumber":1,"title":"Gay Witch Hunt"},"title":"The.Office.US.S03.1080p","status":"downloading","trackedDownloadStatus":"ok","trackedDownloadState":"downloading","size":2e10,"sizeleft":1e10,"estimatedCompletionTime":"2026-10-14T04:00:00Z","downloadId":"E","protocol":"usenet"},
 {"id":6,"series":{"title":"The Office (US)","year":2005},"episode":{"seasonNumber":3,"episodeNumber":2,"title":"The Convention"},"title":"The.Office.US.S03.1080p","status":"downloading","trackedDownloadStatus":"ok","trackedDownloadState":"downloading","size":2e10,"sizeleft":1e10,"estimatedCompletionTime":"2026-10-14T04:00:00Z","downloadId":"E","protocol":"usenet"},
 {"id":7,"series":{"title":"The Office (US)","year":2005},"episode":{"seasonNumber":3,"episodeNumber":3,"title":"The Coup"},"title":"The.Office.US.S03.1080p","status":"downloading","trackedDownloadStatus":"ok","trackedDownloadState":"downloading","size":2e10,"sizeleft":1e10,"estimatedCompletionTime":"2026-10-14T04:00:00Z","downloadId":"E","protocol":"usenet"},
 {"id":8,"movie":{"title":"Halloween","year":2018},"title":"Halloween.2018.720p","status":"delay","trackedDownloadStatus":"ok","size":0,"sizeleft":0,"downloadId":"","protocol":"torrent"}
]}`

func TestQueueTellsTheTruth(t *testing.T) {
	radarr := newFakeRadarr(t)
	radarr.queue = queueFixture
	cs := connect(t, radarr, nil, testNow)
	txt, isErr := callTool(t, cs, "get_download_queue", map[string]any{"service": "radarr"})
	if isErr {
		t.Fatal(txt)
	}
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(txt), &out); err != nil {
		t.Fatal(err)
	}
	byName := map[string]map[string]any{}
	for _, it := range out.Items {
		byName[it["name"].(string)] = it
	}
	cases := []struct {
		name, state, etaIn, problem string
		progress                    float64
	}{
		{"The Lion King (2019)", "downloading", "45m", "", 75},
		{"Nosferatu (2024)", "stalled", "", "stalled with no connections", 4.8},
		{"Total Recall (2012)", "stalled", "", "near zero", 1.3},
		{"Shōgun S02E01 - Anjin", "import_blocked", "", "Sample file", 100},
		{"The Office (US) S03E01 - Gay Witch Hunt", "downloading", "1h 30m", "", 50},
		{"Halloween (2018)", "delayed", "", "", 0},
	}
	for _, tc := range cases {
		it, ok := byName[tc.name]
		if !ok {
			t.Errorf("missing %q in %s", tc.name, txt)
			continue
		}
		if it["state"] != tc.state {
			t.Errorf("%s state = %v, want %s", tc.name, it["state"], tc.state)
		}
		eta, _ := it["eta_in"].(string)
		if eta != tc.etaIn {
			t.Errorf("%s eta_in = %q, want %q", tc.name, eta, tc.etaIn)
		}
		prob, _ := it["problem"].(string)
		if tc.problem != "" && !strings.Contains(prob, tc.problem) {
			t.Errorf("%s problem = %q, want %q", tc.name, prob, tc.problem)
		}
		if it["progress_percent"] != tc.progress {
			t.Errorf("%s progress = %v, want %v", tc.name, it["progress_percent"], tc.progress)
		}
		if tc.state != "downloading" && it["action"] == nil {
			t.Errorf("%s has no action", tc.name)
		}
	}
	if pack := byName["The Office (US) S03E01 - Gay Witch Hunt"]; pack["episodes"] != float64(3) {
		t.Errorf("season pack not collapsed: %v", pack)
	}
	if len(out.Items) != 6 {
		t.Errorf("items = %d, want 6 (season pack collapsed)", len(out.Items))
	}
}

// A service that fails must not read as an empty queue.
func TestQueuePartialFailureIsReported(t *testing.T) {
	radarr := newFakeRadarr(t)
	sonarr := newFakeSonarr(t)
	sonarr.queue = `{"records":[{"id":` // malformed
	cs := connect(t, radarr, sonarr, testNow)
	txt, isErr := callTool(t, cs, "get_download_queue", nil)
	if isErr {
		t.Fatalf("one healthy service should still answer: %s", txt)
	}
	if !strings.Contains(txt, `"errors":{"sonarr":`) {
		t.Errorf("sonarr failure hidden: %s", txt)
	}
	if !strings.Contains(txt, `"items":[]`) {
		t.Errorf("empty radarr queue should be [] not null: %s", txt)
	}
}

func TestQueueServiceNotConfigured(t *testing.T) {
	cs := connect(t, newFakeRadarr(t), nil, testNow)
	txt, isErr := callTool(t, cs, "get_download_queue", map[string]any{"service": "sonarr"})
	if !isErr || !strings.Contains(txt, "Sonarr is not configured") {
		t.Errorf("got %s", txt)
	}
}
