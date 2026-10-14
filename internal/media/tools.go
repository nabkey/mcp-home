package media

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
)

const (
	defaultSearchLimit = 10
	maxSearchLimit     = 20
	overviewLen        = 160
	defaultQueueLimit  = 20
)

// Tools holds media management tools for Sonarr and Radarr.
type Tools struct {
	sonarr *SonarrClient
	radarr *RadarrClient
	now    func() time.Time
}

// NewTools creates a new Tools instance. Either or both services can be configured.
func NewTools(sonarrURL, sonarrKey, radarrURL, radarrKey string) (*Tools, error) {
	t := &Tools{now: time.Now}

	sonarr, err := NewSonarrClient(sonarrURL, sonarrKey)
	if err == nil {
		t.sonarr = sonarr
	}

	radarr, err := NewRadarrClient(radarrURL, radarrKey)
	if err == nil {
		t.radarr = radarr
	}

	if t.sonarr == nil && t.radarr == nil {
		return nil, fmt.Errorf("neither sonarr nor radarr configured")
	}

	return t, nil
}

// Register adds all available media tools to the given MCP server.
// Tools for unconfigured services are skipped.
func (t *Tools) Register(server *mcp.Server) {
	if t.radarr != nil {
		t.registerSearchMovies(server)
		t.registerAddMovie(server)
	}
	if t.sonarr != nil {
		t.registerSearchSeries(server)
		t.registerAddSeries(server)
	}
	t.registerGetDownloadQueue(server)
}

// clampLimit applies a default and maximum to a limit argument.
func clampLimit(n, def, maxN int) int {
	if n <= 0 {
		return def
	}
	return min(n, maxN)
}

// yearOK reports whether a result's year is within one of the wanted year.
// Used for filtering search results only; adds match the year exactly.
func yearOK(want, got int) bool {
	return want == 0 || (got != 0 && got >= want-1 && got <= want+1)
}

// dateOnly trims an ISO timestamp to its date.
func dateOnly(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// --- search_movies ---

type searchMoviesArgs struct {
	Term  string `json:"term" jsonschema:"Movie title to search for"`
	Year  int    `json:"year,omitempty" jsonschema:"Only results released within a year of this (e.g. 2019 for 'the one from 2019')"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum results (default 10, max 20)"`
}

type movieSummary struct {
	Title          string `json:"title"`
	Year           int    `json:"year"`
	TmdbID         int    `json:"tmdb_id"`
	ImdbID         string `json:"imdb_id,omitempty"`
	Studio         string `json:"studio,omitempty"`
	Language       string `json:"language,omitempty"`
	Runtime        int    `json:"runtime_min,omitempty"`
	Status         string `json:"status"`
	InLibrary      bool   `json:"in_library"`
	Downloaded     bool   `json:"downloaded,omitempty"`
	Monitored      bool   `json:"monitored,omitempty"`
	DigitalRelease string `json:"digital_release,omitempty"`
	Kind           string `json:"kind,omitempty"`
	Overview       string `json:"overview,omitempty"`
}

func summarizeMovie(m Movie) movieSummary {
	s := movieSummary{
		Title:     m.Title,
		Year:      m.Year,
		TmdbID:    m.TmdbID,
		ImdbID:    m.ImdbID,
		Studio:    m.Studio,
		Runtime:   m.Runtime,
		Status:    m.Status,
		InLibrary: m.ID != 0,
		Overview:  truncate(m.Overview, overviewLen),
	}
	if m.OriginalLanguage != nil && m.OriginalLanguage.Name != "English" {
		s.Language = m.OriginalLanguage.Name
	}
	if s.InLibrary {
		s.Downloaded = m.HasFile
		s.Monitored = m.Monitored
	}
	if m.Status != "released" {
		s.DigitalRelease = dateOnly(m.DigitalRelease)
	}
	// Lookups are padded with shorts, making-ofs and sing-alongs that share
	// the title. Saying so stops a model reading them as the film.
	if m.Runtime > 0 && m.Runtime < 40 {
		s.Kind = "short"
	}
	return s
}

func (t *Tools) registerSearchMovies(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "search_movies",
		Description: "Search Radarr (TMDB) for movies. Each result has year, tmdb_id, studio, runtime and whether it is already in the library/downloaded. " +
			"Many titles have remakes or same-name films: when 'ambiguous' is present, pick by year (ask the user if they did not say) and pass that year to add_movie. Use this to answer 'is it downloaded yet' too.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args searchMoviesArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Term) == "" {
			return mcputil.Errorf("term is required"), nil, nil
		}
		results, err := t.radarr.SearchMovies(ctx, args.Term)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		limit := clampLimit(args.Limit, defaultSearchLimit, maxSearchLimit)
		var out []movieSummary
		for _, m := range results {
			if !yearOK(args.Year, m.Year) {
				continue
			}
			out = append(out, summarizeMovie(m))
			if len(out) == limit {
				break
			}
		}

		titles := make([]string, len(out))
		labels := make([]string, len(out))
		for i, m := range out {
			titles[i] = m.Title
			labels[i] = fmt.Sprintf("%d tmdb:%d", m.Year, m.TmdbID)
		}
		resp := map[string]any{
			"results": nonNil(out),
			"count":   len(out),
			"found":   len(results),
		}
		if g := ambiguousGroups(titles, labels); len(g) > 0 {
			resp["ambiguous"] = g
		}
		if len(out) == 0 {
			resp["hint"] = noResultsHint(args.Term, args.Year)
		}
		return mcputil.JSONResult(resp)
	})
}

func noResultsHint(term string, year int) string {
	if year != 0 {
		return fmt.Sprintf("Nothing titled like %q within a year of %d; retry without year, or with the original-language title.", term, year)
	}
	return fmt.Sprintf("Nothing found for %q; try fewer words, the original-language title, or check the spelling.", term)
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// --- add_movie ---

type addMovieArgs struct {
	TmdbID              int    `json:"tmdb_id" jsonschema:"TMDB id from search_movies"`
	Title               string `json:"title" jsonschema:"Title as shown in search_movies"`
	Year                int    `json:"year" jsonschema:"Release year as shown in search_movies. The add is refused if the tmdb_id is a different year, which catches picking a remake or same-name film by mistake"`
	QualityProfileID    int    `json:"quality_profile_id,omitempty" jsonschema:"Quality profile id (omit to use the one most of the library uses)"`
	RootFolderPath      string `json:"root_folder_path,omitempty" jsonschema:"Root folder path (omit to use the one most of the library uses)"`
	MinimumAvailability string `json:"minimum_availability,omitempty" jsonschema:"When Radarr may grab it: announced, inCinemas or released (default released)"`
	SearchNow           *bool  `json:"search_now,omitempty" jsonschema:"Search indexers right after adding (default true)"`
}

var validAvailability = map[string]bool{"announced": true, "inCinemas": true, "released": true}

func (t *Tools) registerAddMovie(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "add_movie",
		Description: "Add a movie to Radarr and start searching for it. Requires tmdb_id, title and year from search_movies; the server re-checks all three against TMDB and refuses a mismatch. " +
			"If the movie is already in the library nothing is added and its download state is returned instead. Quality profile and root folder default to what the existing library uses.",
		Annotations: mcputil.Additive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args addMovieArgs) (*mcp.CallToolResult, any, error) {
		if args.TmdbID <= 0 || strings.TrimSpace(args.Title) == "" || args.Year <= 0 {
			return mcputil.Errorf("tmdb_id, title and year are all required; take them from one search_movies result"), nil, nil
		}
		minAvail := args.MinimumAvailability
		if minAvail == "" {
			minAvail = "released"
		}
		if !validAvailability[minAvail] {
			return mcputil.Errorf("minimum_availability must be announced, inCinemas or released"), nil, nil
		}

		base, movie, err := t.radarr.LookupMovie(ctx, args.TmdbID)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		if msg := checkIdentity("tmdb_id", args.TmdbID, args.Title, args.Year, movie.Title, movie.Year, movieTitles(movie)...); msg != "" {
			return mcputil.Errorf("%s", msg), nil, nil
		}

		if existing, err := t.existingMovie(ctx, movie); err != nil {
			return mcputil.Errorf("checking the library: %v", err), nil, nil
		} else if existing != nil {
			return mcputil.JSONResult(movieInLibrary(*existing))
		}

		d, err := chooseDefaults(ctx, t.radarr.Client, t.radarr.Library, args.RootFolderPath, args.QualityProfileID, false)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		search := args.SearchNow == nil || *args.SearchNow
		added, err := t.radarr.AddMovie(ctx, AddMovieRequest{
			TmdbID:              movie.TmdbID,
			Title:               movie.Title,
			QualityProfileID:    d.profile.ID,
			RootFolderPath:      d.root,
			Monitored:           true,
			MinimumAvailability: minAvail,
			AddOptions:          AddMovieOptions{SearchForMovie: search},
		}, base)
		if err != nil {
			// Added between our check and the post (or the check missed it).
			if isAlreadyAdded(err) {
				if existing, _ := t.radarr.GetMovieByTmdb(ctx, movie.TmdbID); existing != nil {
					return mcputil.JSONResult(movieInLibrary(*existing))
				}
			}
			return mcputil.Errorf("%v", err), nil, nil
		}

		out := map[string]any{
			"added":           true,
			"title":           added.Title,
			"year":            added.Year,
			"tmdb_id":         added.TmdbID,
			"quality_profile": d.profile.Name,
			"root_folder":     d.root,
			"defaults":        d.reasons,
			"searching":       search,
		}
		out["ready"] = movieReadiness(*movie, minAvail, search)
		return mcputil.JSONResult(out)
	})
}

func movieTitles(m *Movie) []string {
	titles := []string{m.Title, m.OriginalTitle}
	for _, a := range m.AlternateTitles {
		titles = append(titles, a.Title)
	}
	return titles
}

// checkIdentity refuses an add whose id names a different title or year
// than the caller believes. This is the guard against wrong-title adds:
// the year is what separates a film from its remake.
func checkIdentity(idName string, id int, title string, year int, gotTitle string, gotYear int, titles ...string) string {
	if year != gotYear {
		return fmt.Sprintf("refusing to add: %s %d is %q (%d), not %d. Search again and use the id of the %d release, or confirm the year with the user",
			idName, id, gotTitle, gotYear, year, year)
	}
	if !titleMatches(title, titles...) {
		return fmt.Sprintf("refusing to add: %s %d is %q (%d), not %q. Use the id from the search result for the title you mean",
			idName, id, gotTitle, gotYear, title)
	}
	return ""
}

// existingMovie returns the library entry for m, or nil. The lookup marks
// library items with their id; the library endpoint is asked too, since it
// carries the current file and monitoring state.
func (t *Tools) existingMovie(ctx context.Context, m *Movie) (*Movie, error) {
	existing, err := t.radarr.GetMovieByTmdb(ctx, m.TmdbID)
	if err != nil {
		return nil, err
	}
	if existing == nil && m.ID != 0 {
		existing = m
	}
	return existing, nil
}

func isAlreadyAdded(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(apiErr.Message), "already")
}

func movieInLibrary(m Movie) map[string]any {
	out := map[string]any{
		"added":              false,
		"already_in_library": true,
		"title":              m.Title,
		"year":               m.Year,
		"tmdb_id":            m.TmdbID,
		"downloaded":         m.HasFile,
		"monitored":          m.Monitored,
	}
	switch {
	case m.HasFile:
		out["ready"] = "Already downloaded and in the library."
	case m.Monitored:
		out["ready"] = "In the library and monitored but not downloaded yet; check get_download_queue. Radarr grabs it when a release that fits the quality profile appears."
	default:
		out["ready"] = "In the library but not monitored, so Radarr will not download it; it must be set to monitored in Radarr."
	}
	return out
}

// movieReadiness says when the movie can realistically be ready.
func movieReadiness(m Movie, minAvail string, search bool) string {
	if m.Status != "released" && minAvail == "released" {
		when := dateOnly(m.DigitalRelease)
		if when == "" {
			when = dateOnly(m.PhysicalRelease)
		}
		if when != "" {
			return fmt.Sprintf("Not released for home viewing yet (status %s); Radarr will grab it after %s.", m.Status, when)
		}
		return fmt.Sprintf("Not released for home viewing yet (status %s, no release date announced); Radarr will grab it once it is.", m.Status)
	}
	if !search {
		return "Added without searching; Radarr will pick it up on its next RSS sync or a manual search."
	}
	return "Searching now. If a release is found it appears in get_download_queue within a minute or two, with an ETA."
}

// --- search_series ---

type searchSeriesArgs struct {
	Term  string `json:"term" jsonschema:"Series title to search for"`
	Year  int    `json:"year,omitempty" jsonschema:"Only series that started within a year of this"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum results (default 10, max 20)"`
}

type seriesSummary struct {
	Title      string `json:"title"`
	Year       int    `json:"year"`
	TvdbID     int    `json:"tvdb_id"`
	ImdbID     string `json:"imdb_id,omitempty"`
	Network    string `json:"network,omitempty"`
	Type       string `json:"type"`
	Language   string `json:"language,omitempty"`
	Status     string `json:"status"`
	Seasons    int    `json:"seasons,omitempty"`
	InLibrary  bool   `json:"in_library"`
	Monitored  bool   `json:"monitored,omitempty"`
	Episodes   string `json:"episodes_downloaded,omitempty"`
	NextAiring string `json:"next_airing,omitempty"`
	Overview   string `json:"overview,omitempty"`
}

func seriesType(s Series) string {
	if s.IsAnime() {
		return "anime"
	}
	if s.SeriesType != "" {
		return s.SeriesType
	}
	return "standard"
}

func summarizeSeries(s Series) seriesSummary {
	out := seriesSummary{
		Title:      s.Title,
		Year:       s.Year,
		TvdbID:     s.TvdbID,
		ImdbID:     s.ImdbID,
		Network:    s.Network,
		Type:       seriesType(s),
		Status:     s.Status,
		Seasons:    s.SeasonCount(),
		InLibrary:  s.ID != 0,
		NextAiring: dateOnly(s.NextAiring),
		Overview:   truncate(s.Overview, overviewLen),
	}
	if s.OriginalLanguage != nil && s.OriginalLanguage.Name != "English" {
		out.Language = s.OriginalLanguage.Name
	}
	if out.InLibrary {
		out.Monitored = s.Monitored
		if st := s.Statistics; st != nil && st.EpisodeCount > 0 {
			out.Episodes = fmt.Sprintf("%d/%d", st.EpisodeFileCount, st.EpisodeCount)
		}
	}
	return out
}

func (t *Tools) registerSearchSeries(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "search_series",
		Description: "Search Sonarr (TVDB) for TV series. Each result has start year, tvdb_id, network, type (anime/standard/daily), season count and library state. " +
			"Same-name shows are common (UK vs US versions, reboots, anime vs live action): when 'ambiguous' is present, pick by year and network and pass that year to add_series.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args searchSeriesArgs) (*mcp.CallToolResult, any, error) {
		if strings.TrimSpace(args.Term) == "" {
			return mcputil.Errorf("term is required"), nil, nil
		}
		results, err := t.sonarr.SearchSeries(ctx, args.Term)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		limit := clampLimit(args.Limit, defaultSearchLimit, maxSearchLimit)
		var out []seriesSummary
		for _, s := range results {
			if !yearOK(args.Year, s.Year) {
				continue
			}
			out = append(out, summarizeSeries(s))
			if len(out) == limit {
				break
			}
		}

		titles := make([]string, len(out))
		labels := make([]string, len(out))
		for i, s := range out {
			titles[i] = s.Title
			labels[i] = fmt.Sprintf("%d %s %s tvdb:%d", s.Year, s.Network, s.Type, s.TvdbID)
		}
		resp := map[string]any{
			"results": nonNil(out),
			"count":   len(out),
			"found":   len(results),
		}
		if g := ambiguousGroups(titles, labels); len(g) > 0 {
			resp["ambiguous"] = g
		}
		if len(out) == 0 {
			resp["hint"] = noResultsHint(args.Term, args.Year)
		}
		return mcputil.JSONResult(resp)
	})
}

// --- add_series ---

type addSeriesArgs struct {
	TvdbID           int    `json:"tvdb_id" jsonschema:"TVDB id from search_series"`
	Title            string `json:"title" jsonschema:"Title as shown in search_series"`
	Year             int    `json:"year" jsonschema:"Start year as shown in search_series. The add is refused if the tvdb_id is a different year, which catches picking the wrong version of a same-name show"`
	Monitor          string `json:"monitor,omitempty" jsonschema:"Episodes to download: all (default), latestSeason (for 'get me the new season'), future, missing, existing, firstSeason, lastSeason, pilot, recent, none"`
	SeriesType       string `json:"series_type,omitempty" jsonschema:"standard, daily or anime (default: detected from the lookup; anime matters for episode numbering)"`
	QualityProfileID int    `json:"quality_profile_id,omitempty" jsonschema:"Quality profile id (omit to use the one most of the library uses)"`
	RootFolderPath   string `json:"root_folder_path,omitempty" jsonschema:"Root folder path (omit to use the one most of the library uses; anime prefers an anime folder)"`
	SearchNow        *bool  `json:"search_now,omitempty" jsonschema:"Search for monitored episodes right after adding (default true)"`
}

var validMonitorOptions = map[string]bool{
	"all": true, "future": true, "missing": true, "existing": true,
	"pilot": true, "firstSeason": true, "lastSeason": true, "latestSeason": true,
	"recent": true, "none": true,
}

var validSeriesTypes = map[string]bool{"standard": true, "daily": true, "anime": true}

func (t *Tools) registerAddSeries(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "add_series",
		Description: "Add a TV series to Sonarr and start searching. Requires tvdb_id, title and year from search_series; the server re-checks them and refuses a mismatch. " +
			"If the series is already in the library nothing is added; its per-season download state is returned instead. Quality profile, root folder and anime type default from the library and the lookup.",
		Annotations: mcputil.Additive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args addSeriesArgs) (*mcp.CallToolResult, any, error) {
		if args.TvdbID <= 0 || strings.TrimSpace(args.Title) == "" || args.Year <= 0 {
			return mcputil.Errorf("tvdb_id, title and year are all required; take them from one search_series result"), nil, nil
		}
		monitor := args.Monitor
		if monitor == "" {
			monitor = "all"
		}
		if !validMonitorOptions[monitor] {
			return mcputil.Errorf("invalid monitor %q; use all, latestSeason, future, missing, existing, firstSeason, lastSeason, pilot, recent or none", monitor), nil, nil
		}
		if args.SeriesType != "" && !validSeriesTypes[args.SeriesType] {
			return mcputil.Errorf("series_type must be standard, daily or anime"), nil, nil
		}

		base, series, err := t.sonarr.LookupSeries(ctx, args.TvdbID)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		if msg := checkIdentity("tvdb_id", args.TvdbID, args.Title, args.Year, series.Title, series.Year, series.Title); msg != "" {
			return mcputil.Errorf("%s", msg), nil, nil
		}

		existing, err := t.sonarr.GetSeriesByTvdb(ctx, series.TvdbID)
		if err != nil {
			return mcputil.Errorf("checking the library: %v", err), nil, nil
		}
		if existing == nil && series.ID != 0 {
			existing = series
		}
		if existing != nil {
			return mcputil.JSONResult(seriesInLibrary(*existing))
		}

		sType := args.SeriesType
		if sType == "" {
			sType = seriesType(*series)
		}
		d, err := chooseDefaults(ctx, t.sonarr.Client, t.sonarr.Library, args.RootFolderPath, args.QualityProfileID, sType == "anime")
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}

		search := args.SearchNow == nil || *args.SearchNow
		added, err := t.sonarr.AddSeries(ctx, AddSeriesRequest{
			TvdbID:           series.TvdbID,
			Title:            series.Title,
			QualityProfileID: d.profile.ID,
			RootFolderPath:   d.root,
			Monitored:        true,
			SeasonFolder:     true,
			SeriesType:       sType,
			AddOptions: AddSeriesOptions{
				Monitor:                  monitor,
				SearchForMissingEpisodes: search && monitor != "none",
			},
		}, base)
		if err != nil {
			if isAlreadyAdded(err) {
				if existing, _ := t.sonarr.GetSeriesByTvdb(ctx, series.TvdbID); existing != nil {
					return mcputil.JSONResult(seriesInLibrary(*existing))
				}
			}
			return mcputil.Errorf("%v", err), nil, nil
		}

		out := map[string]any{
			"added":           true,
			"title":           added.Title,
			"year":            added.Year,
			"tvdb_id":         added.TvdbID,
			"series_type":     sType,
			"monitor":         monitor,
			"quality_profile": d.profile.Name,
			"root_folder":     d.root,
			"defaults":        d.reasons,
			"searching":       search && monitor != "none",
		}
		if series.Status == "upcoming" {
			out["ready"] = "The series has not premiered yet; Sonarr will grab episodes as they air."
		} else if search && monitor != "none" {
			out["ready"] = "Searching for monitored episodes now; found releases appear in get_download_queue within a minute or two, with ETAs."
		}
		return mcputil.JSONResult(out)
	})
}

type seasonState struct {
	Season     int    `json:"season"`
	Monitored  bool   `json:"monitored"`
	Downloaded string `json:"downloaded,omitempty"`
}

func seriesInLibrary(s Series) map[string]any {
	out := map[string]any{
		"added":              false,
		"already_in_library": true,
		"title":              s.Title,
		"year":               s.Year,
		"tvdb_id":            s.TvdbID,
		"monitored":          s.Monitored,
		"status":             s.Status,
	}
	var seasons []seasonState
	latest := -1
	for _, season := range s.Seasons {
		if season.SeasonNumber == 0 {
			continue
		}
		st := seasonState{Season: season.SeasonNumber, Monitored: season.Monitored}
		if season.Statistics != nil {
			st.Downloaded = fmt.Sprintf("%d/%d", season.Statistics.EpisodeFileCount, season.Statistics.TotalEpisodeCount)
		}
		seasons = append(seasons, st)
		if season.SeasonNumber > latest {
			latest = season.SeasonNumber
		}
	}
	sort.Slice(seasons, func(i, j int) bool { return seasons[i].Season < seasons[j].Season })
	if len(seasons) > 0 {
		out["seasons"] = seasons
	}
	if s.NextAiring != "" {
		out["next_airing"] = s.NextAiring
	}
	if latest > 0 {
		last := seasons[len(seasons)-1]
		switch {
		case !s.Monitored || !last.Monitored:
			out["ready"] = fmt.Sprintf("Already in the library, but season %d is not monitored, so Sonarr will not download it; it must be monitored in Sonarr.", latest)
		default:
			out["ready"] = fmt.Sprintf("Already in the library and season %d is monitored; Sonarr downloads its episodes as they become available. Check get_download_queue for anything in progress.", latest)
		}
	}
	return out
}

// --- defaults ---

type defaults struct {
	root    string
	profile QualityProfile
	reasons []string
}

// chooseDefaults picks the root folder and quality profile for an add.
// "First in the list" is what the API returns, and on a stock install the
// first quality profile is "Any", which grabs whatever turns up first. The
// household's real preference is what the existing library uses, so that is
// the default; an explicit argument must name something that exists.
func chooseDefaults(ctx context.Context, c *Client, library func(context.Context) ([]LibraryItem, error), wantRoot string, wantProfile int, anime bool) (defaults, error) {
	var d defaults

	folders, err := c.GetRootFolders(ctx)
	if err != nil {
		return d, fmt.Errorf("reading root folders: %w", err)
	}
	profiles, err := c.GetQualityProfiles(ctx)
	if err != nil {
		return d, fmt.Errorf("reading quality profiles: %w", err)
	}
	if len(folders) == 0 {
		return d, fmt.Errorf("%s has no root folder configured; add one under Settings > Media Management first", c.name)
	}
	if len(profiles) == 0 {
		return d, fmt.Errorf("%s has no quality profiles configured", c.name)
	}

	// The library is only needed when something is left to infer, and a
	// failure to read it degrades to heuristics rather than failing the add.
	var items []LibraryItem
	if wantRoot == "" || wantProfile == 0 {
		if items, err = library(ctx); err != nil {
			d.reasons = append(d.reasons, "could not read the library to infer defaults ("+err.Error()+"); used heuristics")
			items = nil
		}
	}
	if anime {
		var animeItems []LibraryItem
		for _, it := range items {
			if it.SeriesType == "anime" {
				animeItems = append(animeItems, it)
			}
		}
		if len(animeItems) > 0 {
			items = animeItems
		}
	}

	if d.root, err = pickRoot(folders, items, wantRoot, anime, &d.reasons); err != nil {
		return d, err
	}
	if d.profile, err = pickProfile(profiles, items, wantProfile, &d.reasons); err != nil {
		return d, err
	}
	return d, nil
}

func pickRoot(folders []RootFolder, items []LibraryItem, want string, anime bool, reasons *[]string) (string, error) {
	paths := make([]string, 0, len(folders))
	for _, f := range folders {
		paths = append(paths, f.Path)
	}
	if want != "" {
		for _, f := range folders {
			if strings.TrimSuffix(f.Path, "/") == strings.TrimSuffix(want, "/") {
				return f.Path, nil
			}
		}
		return "", fmt.Errorf("root folder %q does not exist; configured: %s", want, strings.Join(paths, ", "))
	}

	usable := folders[:0:0]
	for _, f := range folders {
		if f.Accessible {
			usable = append(usable, f)
		}
	}
	if len(usable) == 0 {
		return "", fmt.Errorf("no root folder is accessible (%s); the disk may be unmounted", strings.Join(paths, ", "))
	}
	if len(usable) == 1 {
		return usable[0].Path, nil
	}
	if anime {
		for _, f := range usable {
			if strings.Contains(strings.ToLower(f.Path), "anime") {
				*reasons = append(*reasons, fmt.Sprintf("root folder %s: anime series go in the anime folder", f.Path))
				return f.Path, nil
			}
		}
	}

	counts := map[string]int{}
	for _, it := range items {
		for _, f := range usable {
			if strings.HasPrefix(it.Path, strings.TrimSuffix(f.Path, "/")+"/") {
				counts[f.Path]++
				break
			}
		}
	}
	best := usable[0]
	for _, f := range usable[1:] {
		if counts[f.Path] > counts[best.Path] || (counts[f.Path] == counts[best.Path] && f.FreeSpace > best.FreeSpace) {
			best = f
		}
	}
	if counts[best.Path] > 0 {
		*reasons = append(*reasons, fmt.Sprintf("root folder %s: used by %d of %d library items", best.Path, counts[best.Path], len(items)))
	} else {
		*reasons = append(*reasons, fmt.Sprintf("root folder %s: most free space", best.Path))
	}
	return best.Path, nil
}

func pickProfile(profiles []QualityProfile, items []LibraryItem, want int, reasons *[]string) (QualityProfile, error) {
	byID := map[int]QualityProfile{}
	names := make([]string, 0, len(profiles))
	for _, p := range profiles {
		byID[p.ID] = p
		names = append(names, fmt.Sprintf("%d=%s", p.ID, p.Name))
	}
	if want != 0 {
		if p, ok := byID[want]; ok {
			return p, nil
		}
		return QualityProfile{}, fmt.Errorf("quality profile %d does not exist; configured: %s", want, strings.Join(names, ", "))
	}

	counts := map[int]int{}
	for _, it := range items {
		if _, ok := byID[it.QualityProfileID]; ok {
			counts[it.QualityProfileID]++
		}
	}
	bestID, bestN := 0, 0
	for _, p := range profiles {
		if counts[p.ID] > bestN {
			bestID, bestN = p.ID, counts[p.ID]
		}
	}
	if bestN > 0 {
		*reasons = append(*reasons, fmt.Sprintf("quality profile %q: used by %d of %d library items", byID[bestID].Name, bestN, len(items)))
		return byID[bestID], nil
	}

	// Empty library: prefer a 1080p profile, then anything but "Any".
	for _, p := range profiles {
		if strings.Contains(strings.ToLower(p.Name), "1080p") {
			*reasons = append(*reasons, fmt.Sprintf("quality profile %q: library is empty, chose the 1080p profile", p.Name))
			return p, nil
		}
	}
	for _, p := range profiles {
		if !strings.EqualFold(p.Name, "any") {
			*reasons = append(*reasons, fmt.Sprintf("quality profile %q: library is empty, skipped the catch-all \"Any\"", p.Name))
			return p, nil
		}
	}
	*reasons = append(*reasons, fmt.Sprintf("quality profile %q: the only one configured", profiles[0].Name))
	return profiles[0], nil
}

// --- get_download_queue ---

type getDownloadQueueArgs struct {
	Service string `json:"service,omitempty" jsonschema:"sonarr, radarr or both (default both)"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Maximum items per service (default 20)"`
}

type queueSummary struct {
	Service  string  `json:"service"`
	Name     string  `json:"name"`
	State    string  `json:"state"`
	Progress float64 `json:"progress_percent"`
	Size     string  `json:"size"`
	ETA      string  `json:"eta,omitempty"`
	ETAIn    string  `json:"eta_in,omitempty"`
	Quality  string  `json:"quality,omitempty"`
	Problem  string  `json:"problem,omitempty"`
	Action   string  `json:"action,omitempty"`
	Release  string  `json:"release"`
	Episodes int     `json:"episodes,omitempty"`
}

func (t *Tools) registerGetDownloadQueue(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "get_download_queue",
		Description: "What Sonarr (TV) and Radarr (movies) are downloading, named by title and episode. Each item has a state (downloading, stalled, queued, paused, delayed, " +
			"import_blocked, importing, failed...), progress, and an ETA only when the download is actually moving; stalled or blocked items say what is wrong. " +
			"A title missing here and not downloaded in search_movies/search_series has not been found yet.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getDownloadQueueArgs) (*mcp.CallToolResult, any, error) {
		service := args.Service
		if service == "" {
			service = "both"
		}
		switch service {
		case "both":
		case "sonarr":
			if t.sonarr == nil {
				return mcputil.Errorf("Sonarr is not configured on this server"), nil, nil
			}
		case "radarr":
			if t.radarr == nil {
				return mcputil.Errorf("Radarr is not configured on this server"), nil, nil
			}
		default:
			return mcputil.Errorf("service must be sonarr, radarr or both"), nil, nil
		}
		limit := clampLimit(args.Limit, defaultQueueLimit, 100)
		now := t.now()

		items := []queueSummary{}
		totals := map[string]int{}
		errs := map[string]string{}
		fetch := func(name string, c *Client) {
			queue, err := c.GetQueue(ctx, limit)
			if err != nil {
				errs[name] = err.Error()
				return
			}
			totals[name] = queue.TotalRecords
			items = append(items, buildQueueSummaries(name, queue.Records, now)...)
		}
		if (service == "sonarr" || service == "both") && t.sonarr != nil {
			fetch("sonarr", t.sonarr.Client)
		}
		if (service == "radarr" || service == "both") && t.radarr != nil {
			fetch("radarr", t.radarr.Client)
		}
		if len(errs) > 0 && len(totals) == 0 {
			var msgs []string
			for name, e := range errs {
				msgs = append(msgs, name+": "+e)
			}
			sort.Strings(msgs)
			return mcputil.Errorf("%s", strings.Join(msgs, "; ")), nil, nil
		}

		out := map[string]any{
			"items": items,
			"total": totals,
			"now":   now.UTC().Format(time.RFC3339),
		}
		// A queue that failed to load must not read as an empty queue.
		if len(errs) > 0 {
			out["errors"] = errs
		}
		for name, n := range totals {
			if n > limit {
				out["more"] = fmt.Sprintf("%s has %d queue records; raise limit to see them all", name, n)
			}
		}
		return mcputil.JSONResult(out)
	})
}

// classify turns the *arr status fields into one state a person
// understands, and says whether its ETA means anything.
func classify(item QueueItem) (state string, etaMeaningful bool) {
	switch item.TrackedDownloadState {
	case "importBlocked":
		return "import_blocked", false
	case "importPending":
		if item.TrackedDownloadStatus == "warning" || item.TrackedDownloadStatus == "error" {
			return "import_blocked", false
		}
		return "importing", false
	case "importing", "imported":
		return "importing", false
	case "failedPending", "failed":
		return "failed", false
	}
	switch item.Status {
	case "failed":
		return "failed", false
	case "warning":
		// qBittorrent/Transmission "stalled", SABnzbd missing articles.
		return "stalled", false
	case "paused":
		return "paused", false
	case "queued":
		return "queued", false
	case "delay":
		return "delayed", false
	case "downloadClientUnavailable":
		return "client_unavailable", false
	case "completed":
		return "importing", false
	}
	if item.TrackedDownloadStatus == "error" {
		return "failed", false
	}
	if item.TrackedDownloadStatus == "warning" {
		return "stalled", false
	}
	return "downloading", true
}

// maxCredibleETA is how far out an ETA can be and still be a promise; a
// torrent crawling at a few bytes a second reports ETAs of weeks.
const maxCredibleETA = 3 * 24 * time.Hour

func buildQueueSummaries(service string, records []QueueItem, now time.Time) []queueSummary {
	var items []queueSummary
	byDownload := map[string]int{}
	for _, item := range records {
		// A season pack is one download but one record per episode.
		if item.DownloadID != "" {
			if i, ok := byDownload[item.DownloadID]; ok {
				items[i].Episodes++
				continue
			}
		}

		state, etaOK := classify(item)
		s := queueSummary{
			Service:  service,
			Name:     queueName(item),
			State:    state,
			Progress: math.Round(item.Progress*10) / 10,
			Size:     formatBytes(int64(item.Size)),
			Release:  item.Title,
			Problem:  problemText(item),
		}
		if item.Quality != nil {
			s.Quality = item.Quality.Quality.Name
		}
		if item.Episode != nil {
			s.Episodes = 1
		}

		if eta := item.EstimatedCompletionTime; etaOK && eta != nil {
			left := eta.Sub(now)
			switch {
			case left <= 0:
				s.ETAIn = "any moment"
			case left > maxCredibleETA:
				s.State = "stalled"
				s.Problem = joinNonEmpty(s.Problem, fmt.Sprintf("download speed is near zero (estimated %s left)", humanDuration(left)))
			default:
				s.ETA = eta.UTC().Format(time.RFC3339)
				s.ETAIn = humanDuration(left)
			}
		} else if etaOK && item.Sizeleft > 0 {
			// Downloading with no estimate: no peers or not started.
			s.State = "stalled"
			s.Problem = joinNonEmpty(s.Problem, "no download speed reported, so no ETA")
		}
		s.Action = actionFor(s.State)

		if item.DownloadID != "" {
			byDownload[item.DownloadID] = len(items)
		}
		items = append(items, s)
	}
	for i := range items {
		if items[i].Episodes == 1 {
			items[i].Episodes = 0
		}
	}
	return items
}

func queueName(item QueueItem) string {
	switch {
	case item.Movie != nil && item.Movie.Title != "":
		return fmt.Sprintf("%s (%d)", item.Movie.Title, item.Movie.Year)
	case item.Series != nil && item.Series.Title != "" && item.Episode != nil:
		name := fmt.Sprintf("%s S%02dE%02d", item.Series.Title, item.Episode.SeasonNumber, item.Episode.EpisodeNumber)
		if item.Episode.Title != "" {
			name += " - " + item.Episode.Title
		}
		return name
	case item.Series != nil && item.Series.Title != "":
		return item.Series.Title
	}
	return item.Title
}

func problemText(item QueueItem) string {
	var parts []string
	if item.ErrorMessage != "" {
		parts = append(parts, item.ErrorMessage)
	}
	for _, sm := range item.StatusMessages {
		for _, m := range sm.Messages {
			if m != "" && !contains(parts, m) {
				parts = append(parts, m)
			}
		}
		if len(sm.Messages) == 0 && sm.Title != "" && sm.Title != item.Title {
			parts = append(parts, sm.Title)
		}
	}
	return truncate(strings.Join(parts, "; "), 300)
}

func actionFor(state string) string {
	switch state {
	case "import_blocked":
		return "Downloaded but not imported; needs a manual import or fix in Sonarr/Radarr (Activity > Queue)."
	case "stalled":
		return "Not progressing; it may never finish. Searching for a different release in Sonarr/Radarr usually fixes it."
	case "failed":
		return "Failed; Sonarr/Radarr normally blocklists it and searches for another release automatically."
	case "client_unavailable":
		return "The download client is unreachable; check that it is running."
	case "paused":
		return "Paused in the download client."
	case "delayed":
		return "Held by a delay profile waiting for a better release; it starts on its own."
	}
	return ""
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func joinNonEmpty(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// humanDuration renders a duration as "2h 15m" or "3d 4h".
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return "under a minute"
	}
	d = d.Round(time.Minute)
	days := int(d / (24 * time.Hour))
	hours := int(d % (24 * time.Hour) / time.Hour)
	mins := int(d % time.Hour / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%dm", mins)
	}
	return "under a minute"
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
