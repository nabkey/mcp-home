// Package media provides MCP tools for interacting with Sonarr and Radarr.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// maxJSONResponseSize is the maximum size of a JSON API response (5 MB).
	maxJSONResponseSize = 5 * 1024 * 1024
)

// Client is a generic *arr API client.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient creates a new *arr API client.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		apiKey:  apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

func (c *Client) doRequest(ctx context.Context, method, path string, query url.Values, payload any) ([]byte, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	u.Path = path
	if query != nil {
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
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONResponseSize))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// GetRootFolders returns the configured root folders.
func (c *Client) GetRootFolders(ctx context.Context) ([]map[string]any, error) {
	body, err := c.doRequest(ctx, "GET", "/api/v3/rootfolder", nil, nil)
	if err != nil {
		return nil, err
	}

	var results []map[string]any
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("failed to parse root folders response: %w", err)
	}

	return results, nil
}

// GetQualityProfiles returns the configured quality profiles.
func (c *Client) GetQualityProfiles(ctx context.Context) ([]map[string]any, error) {
	body, err := c.doRequest(ctx, "GET", "/api/v3/qualityprofile", nil, nil)
	if err != nil {
		return nil, err
	}

	var results []map[string]any
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("failed to parse quality profiles response: %w", err)
	}

	return results, nil
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
	return &RadarrClient{Client: NewClient(baseURL, apiKey)}, nil
}

// MovieLookupResult represents a movie from the Radarr lookup API.
type MovieLookupResult struct {
	Title       string `json:"title"`
	Year        int    `json:"year"`
	TmdbID      int    `json:"tmdbId"`
	ImdbID      string `json:"imdbId,omitempty"`
	Overview    string `json:"overview"`
	Status      string `json:"status"`
	Studio      string `json:"studio,omitempty"`
	Runtime     int    `json:"runtime"`
	IsAvailable bool   `json:"isAvailable"`
	HasFile     bool   `json:"hasFile"`
}

// SearchMovies searches for movies by term.
func (c *RadarrClient) SearchMovies(ctx context.Context, term string) ([]MovieLookupResult, error) {
	query := url.Values{}
	query.Set("term", term)

	body, err := c.doRequest(ctx, "GET", "/api/v3/movie/lookup", query, nil)
	if err != nil {
		return nil, err
	}

	var results []MovieLookupResult
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("failed to parse movie lookup response: %w", err)
	}

	return results, nil
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

// AddMovie adds a movie to Radarr.
func (c *RadarrClient) AddMovie(ctx context.Context, req AddMovieRequest) (map[string]any, error) {
	body, err := c.doRequest(ctx, "POST", "/api/v3/movie", nil, req)
	if err != nil {
		return nil, err
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse add movie response: %w", err)
	}

	return result, nil
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
	return &SonarrClient{Client: NewClient(baseURL, apiKey)}, nil
}

// SeriesLookupResult represents a series from the Sonarr lookup API.
type SeriesLookupResult struct {
	Title    string `json:"title"`
	Year     int    `json:"year"`
	TvdbID   int    `json:"tvdbId"`
	ImdbID   string `json:"imdbId,omitempty"`
	Overview string `json:"overview"`
	Status   string `json:"status"`
	Network  string `json:"network,omitempty"`
	Seasons  int    `json:"seasonCount"`
}

// SearchSeries searches for series by term.
func (c *SonarrClient) SearchSeries(ctx context.Context, term string) ([]SeriesLookupResult, error) {
	query := url.Values{}
	query.Set("term", term)

	body, err := c.doRequest(ctx, "GET", "/api/v3/series/lookup", query, nil)
	if err != nil {
		return nil, err
	}

	var results []SeriesLookupResult
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("failed to parse series lookup response: %w", err)
	}

	return results, nil
}

// AddSeriesRequest is the request body for adding a series.
type AddSeriesRequest struct {
	TvdbID           int              `json:"tvdbId"`
	Title            string           `json:"title"`
	QualityProfileID int              `json:"qualityProfileId"`
	RootFolderPath   string           `json:"rootFolderPath"`
	Monitored        bool             `json:"monitored"`
	SeasonFolder     bool             `json:"seasonFolder"`
	AddOptions       AddSeriesOptions `json:"addOptions"`
}

// AddSeriesOptions contains options for adding a series.
type AddSeriesOptions struct {
	Monitor                  string `json:"monitor"`
	SearchForMissingEpisodes bool   `json:"searchForMissingEpisodes"`
}

// AddSeries adds a series to Sonarr.
func (c *SonarrClient) AddSeries(ctx context.Context, req AddSeriesRequest) (map[string]any, error) {
	body, err := c.doRequest(ctx, "POST", "/api/v3/series", nil, req)
	if err != nil {
		return nil, err
	}

	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse add series response: %w", err)
	}

	return result, nil
}

// QueueItem represents an item in the download queue.
type QueueItem struct {
	ID                      int     `json:"id"`
	Title                   string  `json:"title"`
	Status                  string  `json:"status"`
	TrackedDownloadStatus   string  `json:"trackedDownloadStatus,omitempty"`
	TrackedDownloadState    string  `json:"trackedDownloadState,omitempty"`
	StatusMessages          []any   `json:"statusMessages,omitempty"`
	ErrorMessage            string  `json:"errorMessage,omitempty"`
	DownloadID              string  `json:"downloadId,omitempty"`
	Protocol                string  `json:"protocol"`
	DownloadClient          string  `json:"downloadClient,omitempty"`
	Size                    int64   `json:"size"`
	Sizeleft                int64   `json:"sizeleft"`
	EstimatedCompletionTime string  `json:"estimatedCompletionTime,omitempty"`
	Progress                float64 `json:"-"`
}

// QueueResponse represents the paginated queue response.
type QueueResponse struct {
	Page         int         `json:"page"`
	PageSize     int         `json:"pageSize"`
	TotalRecords int         `json:"totalRecords"`
	Records      []QueueItem `json:"records"`
}

// GetQueue retrieves the download queue.
func (c *Client) GetQueue(ctx context.Context, pageSize int) (*QueueResponse, error) {
	query := url.Values{}
	query.Set("page", "1")
	if pageSize <= 0 {
		pageSize = 50
	}
	query.Set("pageSize", fmt.Sprintf("%d", pageSize))

	body, err := c.doRequest(ctx, "GET", "/api/v3/queue", query, nil)
	if err != nil {
		return nil, err
	}

	var result QueueResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse queue response: %w", err)
	}

	for i := range result.Records {
		item := &result.Records[i]
		if item.Size > 0 {
			item.Progress = float64(item.Size-item.Sizeleft) / float64(item.Size) * 100
		}
	}

	return &result, nil
}
