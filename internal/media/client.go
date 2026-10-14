// Package media provides MCP tools for interacting with Sonarr and Radarr.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// maxJSONResponseSize is the maximum size of a JSON API response (5 MB).
	maxJSONResponseSize = 5 * 1024 * 1024
	// maxLibraryResponseSize bounds a full library listing, which is
	// streamed into a few fields per item rather than held whole.
	maxLibraryResponseSize = 128 * 1024 * 1024
	requestTimeout         = 30 * time.Second
)

// Client is a generic *arr API client.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	// name and env identify the service in error messages ("Radarr",
	// "RADARR") so a model can tell the user which setting to fix.
	name string
	env  string
}

// NewClient creates a new *arr API client.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: requestTimeout,
		},
		name: "*arr",
		env:  "the service",
	}
}

// APIError is a non-2xx response from Sonarr or Radarr.
type APIError struct {
	Service    string
	Env        string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	switch e.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("%s rejected the API key (HTTP %d); check %s_API_KEY (Settings > General > API Key in %s)", e.Service, e.StatusCode, e.Env, e.Service)
	case http.StatusNotFound:
		return fmt.Sprintf("%s returned 404 (%s); if every call fails this way, %s_URL is missing a base path such as /%s", e.Service, e.Message, e.Env, strings.ToLower(e.Service))
	}
	if e.StatusCode >= 500 {
		return fmt.Sprintf("%s server error (HTTP %d): %s. It may be restarting or its indexers/metadata source may be down; retry shortly", e.Service, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("%s rejected the request (HTTP %d): %s", e.Service, e.StatusCode, e.Message)
}

// errorMessage extracts the human part of an *arr error body: either a
// {"message": ...} object or a validation array of {"errorMessage": ...}.
func errorMessage(body []byte) string {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '<' {
		return "HTML page instead of an API response; the URL may point at a login page or reverse proxy"
	}
	var obj struct {
		Message     string `json:"message"`
		Description string `json:"description"`
	}
	if json.Unmarshal(trimmed, &obj) == nil && obj.Message != "" {
		if obj.Description != "" && obj.Description != obj.Message {
			return obj.Message + ": " + obj.Description
		}
		return obj.Message
	}
	var validation []struct {
		PropertyName string `json:"propertyName"`
		ErrorMessage string `json:"errorMessage"`
	}
	if json.Unmarshal(trimmed, &validation) == nil && len(validation) > 0 {
		msgs := make([]string, 0, len(validation))
		for _, v := range validation {
			msgs = append(msgs, v.ErrorMessage)
		}
		return strings.Join(msgs, "; ")
	}
	s := string(trimmed)
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, payload any) (*http.Request, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid %s_URL: %w", c.env, err)
	}
	// Append to any path prefix in the base URL (e.g. https://host/sonarr).
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}

	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal payload: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do sends a request and returns the open response on success. The caller
// closes the body.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, payload any) (*http.Response, error) {
	req, err := c.newRequest(ctx, method, path, query, payload)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("request cancelled: %w", ctx.Err())
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return nil, fmt.Errorf("%s did not respond within %s; it may be busy (large library scan) or down, retry shortly", c.name, requestTimeout)
		}
		return nil, fmt.Errorf("cannot reach %s (%v); check that it is running and %s_URL is correct", c.name, err, c.env)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
		return nil, &APIError{Service: c.name, Env: c.env, StatusCode: resp.StatusCode, Message: errorMessage(body)}
	}
	return resp, nil
}

func (c *Client) doRequest(ctx context.Context, method, path string, query url.Values, payload any) ([]byte, error) {
	resp, err := c.do(ctx, method, path, query, payload)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read %s response: %w", c.name, err)
	}
	if len(body) > maxJSONResponseSize {
		return nil, fmt.Errorf("%s response exceeds %d bytes", c.name, maxJSONResponseSize)
	}
	return body, nil
}

// getJSON GETs path and decodes the body into v.
func (c *Client) getJSON(ctx context.Context, path string, query url.Values, v any, what string) error {
	body, err := c.doRequest(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return err
	}
	return c.decode(body, v, what)
}

func (c *Client) decode(body []byte, v any, what string) error {
	if err := json.Unmarshal(body, v); err != nil {
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 && trimmed[0] == '<' {
			return fmt.Errorf("%s returned HTML instead of %s JSON; %s_URL probably points at a login page or reverse proxy rather than the API", c.name, what, c.env)
		}
		return fmt.Errorf("failed to parse %s %s response: %w", c.name, what, err)
	}
	return nil
}

// RootFolder is a configured library root.
type RootFolder struct {
	ID         int    `json:"id"`
	Path       string `json:"path"`
	Accessible bool   `json:"accessible"`
	FreeSpace  int64  `json:"freeSpace"`
}

// GetRootFolders returns the configured root folders.
func (c *Client) GetRootFolders(ctx context.Context) ([]RootFolder, error) {
	var results []RootFolder
	return results, c.getJSON(ctx, "/api/v3/rootfolder", nil, &results, "root folder")
}

// QualityProfile is a configured quality profile.
type QualityProfile struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// GetQualityProfiles returns the configured quality profiles.
func (c *Client) GetQualityProfiles(ctx context.Context) ([]QualityProfile, error) {
	var results []QualityProfile
	return results, c.getJSON(ctx, "/api/v3/qualityprofile", nil, &results, "quality profile")
}

// LibraryItem is the slice of a library entry needed to infer defaults.
type LibraryItem struct {
	ID               int    `json:"id"`
	TmdbID           int    `json:"tmdbId"`
	TvdbID           int    `json:"tvdbId"`
	QualityProfileID int    `json:"qualityProfileId"`
	Path             string `json:"path"`
	SeriesType       string `json:"seriesType"`
}

// getLibrary streams /api/v3/movie or /api/v3/series, keeping only the
// fields in LibraryItem. A large library is several MB of JSON.
func (c *Client) getLibrary(ctx context.Context, path string) ([]LibraryItem, error) {
	resp, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	dec := json.NewDecoder(io.LimitReader(resp.Body, maxLibraryResponseSize))
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("failed to read %s library: %w", c.name, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, fmt.Errorf("unexpected %s library response; %s_URL may point at the wrong service", c.name, c.env)
	}
	var items []LibraryItem
	for dec.More() {
		var item LibraryItem
		if err := dec.Decode(&item); err != nil {
			return nil, fmt.Errorf("failed to parse %s library: %w", c.name, err)
		}
		items = append(items, item)
	}
	return items, nil
}

// --- Radarr ---

// RadarrClient wraps the generic client for Radarr-specific operations.
type RadarrClient struct {
	*Client
}

// NewRadarrClient creates a new Radarr client.
func NewRadarrClient(baseURL, apiKey string) (*RadarrClient, error) {
	if baseURL == "" || apiKey == "" {
		return nil, fmt.Errorf("radarr URL and API key are both required")
	}
	c := NewClient(baseURL, apiKey)
	c.name, c.env = "Radarr", "RADARR"
	return &RadarrClient{Client: c}, nil
}

// Movie is a movie from Radarr's lookup or library API. ID is non-zero when
// the movie is already in the library.
type Movie struct {
	ID               int      `json:"id"`
	Title            string   `json:"title"`
	OriginalTitle    string   `json:"originalTitle,omitempty"`
	Year             int      `json:"year"`
	TmdbID           int      `json:"tmdbId"`
	ImdbID           string   `json:"imdbId,omitempty"`
	Overview         string   `json:"overview"`
	Status           string   `json:"status"`
	Studio           string   `json:"studio,omitempty"`
	Runtime          int      `json:"runtime"`
	IsAvailable      bool     `json:"isAvailable"`
	HasFile          bool     `json:"hasFile"`
	Monitored        bool     `json:"monitored"`
	Genres           []string `json:"genres,omitempty"`
	Certification    string   `json:"certification,omitempty"`
	QualityProfileID int      `json:"qualityProfileId,omitempty"`
	InCinemas        string   `json:"inCinemas,omitempty"`
	DigitalRelease   string   `json:"digitalRelease,omitempty"`
	PhysicalRelease  string   `json:"physicalRelease,omitempty"`
	SizeOnDisk       int64    `json:"sizeOnDisk,omitempty"`
	OriginalLanguage *struct {
		Name string `json:"name"`
	} `json:"originalLanguage,omitempty"`
	AlternateTitles []struct {
		Title string `json:"title"`
	} `json:"alternateTitles,omitempty"`
	Collection *struct {
		Title string `json:"title"`
	} `json:"collection,omitempty"`
}

// MovieLookupResult is kept as an alias for callers of the old name.
type MovieLookupResult = Movie

// SearchMovies searches for movies by term.
func (c *RadarrClient) SearchMovies(ctx context.Context, term string) ([]Movie, error) {
	var results []Movie
	return results, c.getJSON(ctx, "/api/v3/movie/lookup", url.Values{"term": {term}}, &results, "movie lookup")
}

// LookupMovie fetches one movie by TMDB id, as raw JSON (to post back on
// add, as Radarr's own UI does) and parsed.
func (c *RadarrClient) LookupMovie(ctx context.Context, tmdbID int) (map[string]any, *Movie, error) {
	body, err := c.doRequest(ctx, http.MethodGet, "/api/v3/movie/lookup/tmdb", url.Values{"tmdbId": {strconv.Itoa(tmdbID)}}, nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil, nil, fmt.Errorf("no movie with tmdb_id %d; use the tmdb_id from search_movies", tmdbID)
		}
		return nil, nil, err
	}
	var raw map[string]any
	var movie Movie
	if err := c.decode(body, &raw, "movie lookup"); err != nil {
		return nil, nil, err
	}
	if err := c.decode(body, &movie, "movie lookup"); err != nil {
		return nil, nil, err
	}
	if movie.TmdbID == 0 {
		return nil, nil, fmt.Errorf("no movie with tmdb_id %d; use the tmdb_id from search_movies", tmdbID)
	}
	return raw, &movie, nil
}

// GetMovieByTmdb returns the library entry for a TMDB id, or nil.
func (c *RadarrClient) GetMovieByTmdb(ctx context.Context, tmdbID int) (*Movie, error) {
	var results []Movie
	if err := c.getJSON(ctx, "/api/v3/movie", url.Values{"tmdbId": {strconv.Itoa(tmdbID)}}, &results, "movie"); err != nil {
		return nil, err
	}
	for i := range results {
		if results[i].TmdbID == tmdbID {
			return &results[i], nil
		}
	}
	return nil, nil
}

// Library returns the slim library listing.
func (c *RadarrClient) Library(ctx context.Context) ([]LibraryItem, error) {
	return c.getLibrary(ctx, "/api/v3/movie")
}

// AddMovieRequest is the request body for adding a movie.
type AddMovieRequest struct {
	TmdbID              int             `json:"tmdbId"`
	Title               string          `json:"title"`
	QualityProfileID    int             `json:"qualityProfileId"`
	RootFolderPath      string          `json:"rootFolderPath"`
	Monitored           bool            `json:"monitored"`
	MinimumAvailability string          `json:"minimumAvailability"`
	AddOptions          AddMovieOptions `json:"addOptions"`
}

// AddMovieOptions contains options for adding a movie.
type AddMovieOptions struct {
	SearchForMovie bool `json:"searchForMovie"`
}

// AddMovie adds a movie to Radarr. base, if non-nil, is the lookup record
// the request is laid over (images, year, titleSlug...), which is what
// Radarr's own add dialog posts.
func (c *RadarrClient) AddMovie(ctx context.Context, req AddMovieRequest, base map[string]any) (*Movie, error) {
	payload := overlay(base, req)
	body, err := c.doRequest(ctx, http.MethodPost, "/api/v3/movie", nil, payload)
	if err != nil {
		return nil, err
	}
	var result Movie
	return &result, c.decode(body, &result, "add movie")
}

// --- Sonarr ---

// SonarrClient wraps the generic client for Sonarr-specific operations.
type SonarrClient struct {
	*Client
}

// NewSonarrClient creates a new Sonarr client.
func NewSonarrClient(baseURL, apiKey string) (*SonarrClient, error) {
	if baseURL == "" || apiKey == "" {
		return nil, fmt.Errorf("sonarr URL and API key are both required")
	}
	c := NewClient(baseURL, apiKey)
	c.name, c.env = "Sonarr", "SONARR"
	return &SonarrClient{Client: c}, nil
}

// SeasonStatistics summarises a season's or series' files.
type SeasonStatistics struct {
	SeasonCount       int     `json:"seasonCount"`
	EpisodeFileCount  int     `json:"episodeFileCount"`
	EpisodeCount      int     `json:"episodeCount"`
	TotalEpisodeCount int     `json:"totalEpisodeCount"`
	SizeOnDisk        int64   `json:"sizeOnDisk"`
	PercentOfEpisodes float64 `json:"percentOfEpisodes"`
	NextAiring        string  `json:"nextAiring,omitempty"`
}

// Season is one season of a series.
type Season struct {
	SeasonNumber int               `json:"seasonNumber"`
	Monitored    bool              `json:"monitored"`
	Statistics   *SeasonStatistics `json:"statistics,omitempty"`
}

// Series is a series from Sonarr's lookup or library API. ID is non-zero
// when the series is already in the library. Sonarr v3+ has no top-level
// seasonCount: it is in statistics, or countable from seasons.
type Series struct {
	ID               int               `json:"id"`
	Title            string            `json:"title"`
	Year             int               `json:"year"`
	TvdbID           int               `json:"tvdbId"`
	ImdbID           string            `json:"imdbId,omitempty"`
	Overview         string            `json:"overview"`
	Status           string            `json:"status"`
	Network          string            `json:"network,omitempty"`
	SeriesType       string            `json:"seriesType,omitempty"`
	Genres           []string          `json:"genres,omitempty"`
	Monitored        bool              `json:"monitored"`
	Seasons          []Season          `json:"seasons,omitempty"`
	Statistics       *SeasonStatistics `json:"statistics,omitempty"`
	NextAiring       string            `json:"nextAiring,omitempty"`
	FirstAired       string            `json:"firstAired,omitempty"`
	QualityProfileID int               `json:"qualityProfileId,omitempty"`
	OriginalLanguage *struct {
		Name string `json:"name"`
	} `json:"originalLanguage,omitempty"`
}

// SeriesLookupResult is kept as an alias for callers of the old name.
type SeriesLookupResult = Series

// SeasonCount counts regular seasons (specials, season 0, excluded).
func (s Series) SeasonCount() int {
	n := 0
	for _, season := range s.Seasons {
		if season.SeasonNumber > 0 {
			n++
		}
	}
	if n == 0 && s.Statistics != nil {
		return s.Statistics.SeasonCount
	}
	return n
}

// IsAnime reports whether Sonarr should treat the series as anime
// (absolute episode numbering, anime release naming).
func (s Series) IsAnime() bool {
	if s.SeriesType == "anime" {
		return true
	}
	for _, g := range s.Genres {
		if strings.EqualFold(g, "anime") {
			return true
		}
	}
	return false
}

// SearchSeries searches for series by term.
func (c *SonarrClient) SearchSeries(ctx context.Context, term string) ([]Series, error) {
	var results []Series
	return results, c.getJSON(ctx, "/api/v3/series/lookup", url.Values{"term": {term}}, &results, "series lookup")
}

// LookupSeries fetches one series by TVDB id, raw and parsed.
func (c *SonarrClient) LookupSeries(ctx context.Context, tvdbID int) (map[string]any, *Series, error) {
	body, err := c.doRequest(ctx, http.MethodGet, "/api/v3/series/lookup", url.Values{"term": {"tvdb:" + strconv.Itoa(tvdbID)}}, nil)
	if err != nil {
		return nil, nil, err
	}
	var raws []map[string]any
	var all []Series
	if err := c.decode(body, &raws, "series lookup"); err != nil {
		return nil, nil, err
	}
	if err := c.decode(body, &all, "series lookup"); err != nil {
		return nil, nil, err
	}
	for i := range all {
		if all[i].TvdbID == tvdbID {
			return raws[i], &all[i], nil
		}
	}
	return nil, nil, fmt.Errorf("no series with tvdb_id %d; use the tvdb_id from search_series", tvdbID)
}

// GetSeriesByTvdb returns the library entry for a TVDB id, or nil.
func (c *SonarrClient) GetSeriesByTvdb(ctx context.Context, tvdbID int) (*Series, error) {
	var results []Series
	if err := c.getJSON(ctx, "/api/v3/series", url.Values{"tvdbId": {strconv.Itoa(tvdbID)}}, &results, "series"); err != nil {
		return nil, err
	}
	for i := range results {
		if results[i].TvdbID == tvdbID {
			return &results[i], nil
		}
	}
	return nil, nil
}

// Library returns the slim library listing.
func (c *SonarrClient) Library(ctx context.Context) ([]LibraryItem, error) {
	return c.getLibrary(ctx, "/api/v3/series")
}

// AddSeriesRequest is the request body for adding a series.
type AddSeriesRequest struct {
	TvdbID           int              `json:"tvdbId"`
	Title            string           `json:"title"`
	QualityProfileID int              `json:"qualityProfileId"`
	RootFolderPath   string           `json:"rootFolderPath"`
	Monitored        bool             `json:"monitored"`
	SeasonFolder     bool             `json:"seasonFolder"`
	SeriesType       string           `json:"seriesType,omitempty"`
	AddOptions       AddSeriesOptions `json:"addOptions"`
}

// AddSeriesOptions contains options for adding a series.
type AddSeriesOptions struct {
	Monitor                  string `json:"monitor"`
	SearchForMissingEpisodes bool   `json:"searchForMissingEpisodes"`
}

// AddSeries adds a series to Sonarr over its lookup record (see AddMovie).
func (c *SonarrClient) AddSeries(ctx context.Context, req AddSeriesRequest, base map[string]any) (*Series, error) {
	payload := overlay(base, req)
	body, err := c.doRequest(ctx, http.MethodPost, "/api/v3/series", nil, payload)
	if err != nil {
		return nil, err
	}
	var result Series
	return &result, c.decode(body, &result, "add series")
}

// overlay lays req's fields over a copy of base. With a nil base it is req.
func overlay(base map[string]any, req any) any {
	if base == nil {
		return req
	}
	b, err := json.Marshal(req)
	if err != nil {
		return req
	}
	var fields map[string]any
	if json.Unmarshal(b, &fields) != nil {
		return req
	}
	out := make(map[string]any, len(base)+len(fields))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range fields {
		out[k] = v
	}
	// A lookup record of an item already in the library carries its id;
	// posting it back would be an update, not an add.
	delete(out, "id")
	return out
}

// --- Queue ---

// StatusMessage is one group of messages on a tracked download.
type StatusMessage struct {
	Title    string   `json:"title"`
	Messages []string `json:"messages"`
}

// QueueItem represents an item in the download queue. Sizes are numbers
// that newer versions serialise as doubles.
type QueueItem struct {
	ID                      int             `json:"id"`
	Title                   string          `json:"title"`
	Status                  string          `json:"status"`
	TrackedDownloadStatus   string          `json:"trackedDownloadStatus,omitempty"`
	TrackedDownloadState    string          `json:"trackedDownloadState,omitempty"`
	StatusMessages          []StatusMessage `json:"statusMessages,omitempty"`
	ErrorMessage            string          `json:"errorMessage,omitempty"`
	DownloadID              string          `json:"downloadId,omitempty"`
	Protocol                string          `json:"protocol"`
	DownloadClient          string          `json:"downloadClient,omitempty"`
	Indexer                 string          `json:"indexer,omitempty"`
	Size                    float64         `json:"size"`
	Sizeleft                float64         `json:"sizeleft"`
	Timeleft                string          `json:"timeleft,omitempty"`
	EstimatedCompletionTime *time.Time      `json:"estimatedCompletionTime,omitempty"`
	Added                   *time.Time      `json:"added,omitempty"`
	Quality                 *struct {
		Quality struct {
			Name string `json:"name"`
		} `json:"quality"`
	} `json:"quality,omitempty"`
	Movie *struct {
		Title string `json:"title"`
		Year  int    `json:"year"`
	} `json:"movie,omitempty"`
	Series *struct {
		Title string `json:"title"`
		Year  int    `json:"year"`
	} `json:"series,omitempty"`
	Episode *struct {
		SeasonNumber  int    `json:"seasonNumber"`
		EpisodeNumber int    `json:"episodeNumber"`
		Title         string `json:"title"`
	} `json:"episode,omitempty"`
	// Progress is the downloaded percentage, computed by GetQueue.
	Progress float64 `json:"-"`
}

// QueueResponse represents the paginated queue response.
type QueueResponse struct {
	Page         int         `json:"page"`
	PageSize     int         `json:"pageSize"`
	TotalRecords int         `json:"totalRecords"`
	Records      []QueueItem `json:"records"`
}

// GetQueue retrieves the download queue with the movie or series and
// episode attached, so items can be named by title rather than by release.
func (c *Client) GetQueue(ctx context.Context, pageSize int) (*QueueResponse, error) {
	if pageSize <= 0 {
		pageSize = 50
	}
	query := url.Values{
		"page":     {"1"},
		"pageSize": {strconv.Itoa(pageSize)},
		// Ignored by the service they do not apply to.
		"includeMovie":   {"true"},
		"includeSeries":  {"true"},
		"includeEpisode": {"true"},
	}

	var result QueueResponse
	if err := c.getJSON(ctx, "/api/v3/queue", query, &result, "queue"); err != nil {
		return nil, err
	}

	for i := range result.Records {
		item := &result.Records[i]
		if item.Size > 0 {
			item.Progress = (item.Size - item.Sizeleft) / item.Size * 100
		}
	}
	return &result, nil
}
