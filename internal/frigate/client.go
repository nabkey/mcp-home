package frigate

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
	"sync"
	"time"

	"github.com/nabkey/mcp-home/internal/validate"
)

const (
	// maxJSONResponseSize is the maximum size of a JSON API response (5 MB).
	maxJSONResponseSize = 5 * 1024 * 1024
	// maxImageResponseSize is the maximum size of a JPEG snapshot (10 MB).
	maxImageResponseSize = 10 * 1024 * 1024
	// configTTL bounds how long the camera/zone/timezone config is cached.
	// It changes only when someone edits Frigate's config and restarts it.
	configTTL = 5 * time.Minute
	// requestTimeout bounds a single Frigate request.
	requestTimeout = 30 * time.Second
)

// Client is a Frigate NVR API client.
type Client struct {
	baseURL    string
	httpClient *http.Client

	mu        sync.Mutex
	config    *Config
	configAge time.Time
	now       func() time.Time
}

// NewClient creates a new Frigate client.
func NewClient(baseURL string) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("frigate URL is required")
	}

	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		httpClient: &http.Client{
			Timeout: requestTimeout,
		},
		now: time.Now,
	}, nil
}

// Event represents a Frigate detection event as returned by /api/events.
//
// Since Frigate 0.14 the scores live under data: the top-level top_score
// column is kept for compatibility but is written as 0, which is what the
// live instance returns for every event. Score() prefers data.top_score.
type Event struct {
	ID          string          `json:"id"`
	Camera      string          `json:"camera"`
	Label       string          `json:"label"`
	RawSubLabel json.RawMessage `json:"sub_label,omitempty"`
	TopScore    float64         `json:"top_score"`
	StartTime   float64         `json:"start_time"`
	EndTime     *float64        `json:"end_time"`
	HasSnapshot bool            `json:"has_snapshot"`
	HasClip     bool            `json:"has_clip"`
	Zones       []string        `json:"zones"`
	// EnteredZones is the pre-0.14 name for Zones.
	EnteredZones       []string  `json:"entered_zones"`
	RetainIndefinitely bool      `json:"retain_indefinitely"`
	FalsePositive      *bool     `json:"false_positive"`
	Data               EventData `json:"data"`
}

// EventData is the free-form data column of an event.
type EventData struct {
	TopScore float64 `json:"top_score"`
	Score    float64 `json:"score"`
	Type     string  `json:"type,omitempty"`
}

// Score returns the event's best detection score, from wherever this
// Frigate version keeps it.
func (e Event) Score() float64 {
	if e.Data.TopScore > 0 {
		return e.Data.TopScore
	}
	if e.TopScore > 0 {
		return e.TopScore
	}
	return e.Data.Score
}

// EventZones returns the zones the object entered, across Frigate versions.
func (e Event) EventZones() []string {
	if len(e.Zones) > 0 {
		return e.Zones
	}
	return e.EnteredZones
}

// SubLabel returns the sub label (a recognised face, licence plate or
// delivery company) and its score, if any. Frigate has stored it both as a
// plain string and as a [name, score] pair.
func (e Event) SubLabel() (string, float64) {
	raw := bytes.TrimSpace(e.RawSubLabel)
	if len(raw) == 0 || string(raw) == "null" {
		return "", 0
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, 0
	}
	var pair []json.RawMessage
	if json.Unmarshal(raw, &pair) == nil && len(pair) > 0 {
		_ = json.Unmarshal(pair[0], &s)
		var score float64
		if len(pair) > 1 {
			_ = json.Unmarshal(pair[1], &score)
		}
		return s, score
	}
	return "", 0
}

// CameraConfig represents the parts of a camera's configuration a model
// needs to phrase a query: whether it is on, its zones, and what it tracks.
type CameraConfig struct {
	Enabled bool                       `json:"enabled"`
	Zones   map[string]json.RawMessage `json:"zones"`
	Objects struct {
		Track []string `json:"track"`
	} `json:"objects"`
}

// Config represents Frigate's configuration.
type Config struct {
	Cameras map[string]CameraConfig `json:"cameras"`
	UI      struct {
		Timezone string `json:"timezone"`
	} `json:"ui"`
}

// APIError is a non-2xx response from Frigate.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	switch e.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("Frigate rejected the request (HTTP %d). FRIGATE_URL must point at Frigate's unauthenticated API port (5000) or a proxy that authenticates for us; the authenticated port (8971) needs a login token this server does not send", e.StatusCode)
	case http.StatusNotFound:
		return fmt.Sprintf("Frigate returned 404 (not found): %s", e.Body)
	}
	if e.StatusCode >= 500 {
		return fmt.Sprintf("Frigate server error (HTTP %d): %s. Frigate may be restarting; retry in a minute", e.StatusCode, e.Body)
	}
	return fmt.Sprintf("Frigate API error (HTTP %d): %s", e.StatusCode, e.Body)
}

// IsNotFound reports whether err is a Frigate 404.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// response is a successful Frigate response.
type response struct {
	body   []byte
	header http.Header
}

// doRequest performs a GET request against the Frigate API (all Frigate
// endpoints used here are read-only).
func (c *Client) doRequest(ctx context.Context, path string, query url.Values, maxSize int64) (*response, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid FRIGATE_URL: %w", err)
	}
	// Append to any path prefix in the base URL (e.g. https://host/frigate).
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Read one byte past the cap so a truncated body is detected rather
	// than handed on as a corrupt image or unparsable JSON.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read Frigate response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{StatusCode: resp.StatusCode, Body: snippet(body)}
	}
	if int64(len(body)) > maxSize {
		return nil, fmt.Errorf("frigate response exceeds %d bytes; request a smaller image (lower height) or narrower filter", maxSize)
	}

	return &response{body: body, header: resp.Header}, nil
}

// transportError turns a client-side failure into a message that says what
// happened and what to try.
func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("request cancelled: %w", ctx.Err())
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return fmt.Errorf("frigate did not respond within %s; it may be overloaded or restarting, retry shortly", requestTimeout)
	}
	return fmt.Errorf("cannot reach Frigate (%v); check that Frigate is running and FRIGATE_URL is correct", err)
}

// snippet trims an error body to something worth showing a model.
func snippet(body []byte) string {
	s := strings.TrimSpace(string(body))
	if strings.HasPrefix(strings.ToLower(s), "<!doctype") || strings.HasPrefix(s, "<html") {
		return "(HTML page, not an API response: FRIGATE_URL may point at a login page or proxy)"
	}
	const maxLen = 300
	if len(s) > maxLen {
		s = s[:maxLen] + "..."
	}
	return s
}

// decodeJSON parses a JSON body, explaining the likely cause when it is not
// JSON at all.
func decodeJSON(body []byte, v any, what string) error {
	if err := json.Unmarshal(body, v); err != nil {
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) > 0 && trimmed[0] == '<' {
			return fmt.Errorf("frigate returned HTML instead of %s JSON; FRIGATE_URL probably points at the web UI behind a login page or proxy rather than the API", what)
		}
		return fmt.Errorf("failed to parse Frigate %s: %w", what, err)
	}
	return nil
}

// GetConfig retrieves Frigate's configuration including camera list. The
// result is cached for configTTL.
func (c *Client) GetConfig(ctx context.Context) (*Config, error) {
	c.mu.Lock()
	if c.config != nil && c.now().Sub(c.configAge) < configTTL {
		cfg := c.config
		c.mu.Unlock()
		return cfg, nil
	}
	c.mu.Unlock()

	resp, err := c.doRequest(ctx, "/api/config", nil, maxJSONResponseSize)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := decodeJSON(resp.body, &config, "config"); err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.config, c.configAge = &config, c.now()
	c.mu.Unlock()
	return &config, nil
}

// EventsQuery filters /api/events. Frigate (0.14+) takes each list as a
// single comma-joined value; repeating the parameter keeps only the last.
type EventsQuery struct {
	Cameras   []string
	Labels    []string
	Zones     []string
	SubLabels []string
	After     time.Time
	Before    time.Time
	Limit     int
	MinScore  float64
	// HasSnapshot restricts to events with a stored snapshot.
	HasSnapshot bool
}

func (q EventsQuery) values() url.Values {
	v := url.Values{}
	if len(q.Cameras) > 0 {
		v.Set("cameras", strings.Join(q.Cameras, ","))
	}
	if len(q.Labels) > 0 {
		v.Set("labels", strings.Join(q.Labels, ","))
	}
	if len(q.Zones) > 0 {
		v.Set("zones", strings.Join(q.Zones, ","))
	}
	if len(q.SubLabels) > 0 {
		v.Set("sub_labels", strings.Join(q.SubLabels, ","))
	}
	if !q.After.IsZero() {
		v.Set("after", strconv.FormatInt(q.After.Unix(), 10))
	}
	if !q.Before.IsZero() {
		v.Set("before", strconv.FormatInt(q.Before.Unix(), 10))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.MinScore > 0 {
		v.Set("min_score", strconv.FormatFloat(q.MinScore, 'f', -1, 64))
	}
	if q.HasSnapshot {
		v.Set("has_snapshot", "1")
	}
	// Thumbnails are fetched separately and only on request; ask older
	// Frigate versions not to inline base64 thumbnails in the list.
	v.Set("include_thumbnails", "0")
	return v
}

// GetEvents retrieves events from Frigate, newest first.
func (c *Client) GetEvents(ctx context.Context, q EventsQuery) ([]Event, error) {
	resp, err := c.doRequest(ctx, "/api/events", q.values(), maxJSONResponseSize)
	if err != nil {
		return nil, err
	}

	var events []Event
	if err := decodeJSON(resp.body, &events, "events"); err != nil {
		return nil, err
	}
	return events, nil
}

// GetEvent retrieves a single event by id.
func (c *Client) GetEvent(ctx context.Context, eventID string) (*Event, error) {
	if err := validate.Identifier("event_id", eventID); err != nil {
		return nil, err
	}
	resp, err := c.doRequest(ctx, "/api/events/"+eventID, nil, maxJSONResponseSize)
	if err != nil {
		return nil, err
	}
	var event Event
	if err := decodeJSON(resp.body, &event, "event"); err != nil {
		return nil, err
	}
	return &event, nil
}

// ImageOptions shapes an image Frigate renders. Zero values leave Frigate's
// own defaults in place.
type ImageOptions struct {
	// Height resizes the image, preserving aspect ratio.
	Height int
	// Quality is the JPEG quality (1-100).
	Quality int
	// Crop crops an event snapshot to the detected object's region.
	Crop bool
	// BBox draws the bounding box on an event snapshot.
	BBox *bool
}

func (o ImageOptions) values(event bool) url.Values {
	v := url.Values{}
	if o.Height > 0 {
		v.Set("height", strconv.Itoa(o.Height))
	}
	if o.Quality > 0 {
		v.Set("quality", strconv.Itoa(o.Quality))
	}
	if event && o.Crop {
		v.Set("crop", "1")
	}
	if o.BBox != nil {
		if *o.BBox {
			v.Set("bbox", "1")
		} else {
			v.Set("bbox", "0")
		}
	}
	return v
}

// Image is a JPEG returned by Frigate.
type Image struct {
	Data []byte
	// Offline is set when Frigate served its last preview frame because the
	// camera is not producing frames.
	Offline bool
}

func (c *Client) getImage(ctx context.Context, path string, q url.Values) (*Image, error) {
	resp, err := c.doRequest(ctx, path, q, maxImageResponseSize)
	if err != nil {
		return nil, err
	}
	if ct := resp.header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "image/") {
		return nil, fmt.Errorf("frigate returned %q instead of an image: %s", ct, snippet(resp.body))
	}
	return &Image{
		Data:    resp.body,
		Offline: resp.header.Get("X-Frigate-Offline") == "true",
	}, nil
}

// GetLatestFrame retrieves the latest frame from a camera as JPEG bytes.
func (c *Client) GetLatestFrame(ctx context.Context, camera string, opts ImageOptions) (*Image, error) {
	if err := validate.Identifier("camera", camera); err != nil {
		return nil, err
	}
	return c.getImage(ctx, fmt.Sprintf("/api/%s/latest.jpg", camera), opts.values(false))
}

// GetEventSnapshot retrieves the snapshot for a specific event.
func (c *Client) GetEventSnapshot(ctx context.Context, eventID string, opts ImageOptions) (*Image, error) {
	if err := validate.Identifier("event_id", eventID); err != nil {
		return nil, err
	}
	return c.getImage(ctx, fmt.Sprintf("/api/events/%s/snapshot.jpg", eventID), opts.values(true))
}

// GetEventThumbnail retrieves the small object thumbnail Frigate stores for
// every event (around 175px tall, a few KB).
func (c *Client) GetEventThumbnail(ctx context.Context, eventID string) (*Image, error) {
	if err := validate.Identifier("event_id", eventID); err != nil {
		return nil, err
	}
	return c.getImage(ctx, fmt.Sprintf("/api/events/%s/thumbnail.jpg", eventID), nil)
}
