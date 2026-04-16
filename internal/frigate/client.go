package frigate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/nabkey/mcp-home/internal/validate"
)

const (
	// maxJSONResponseSize is the maximum size of a JSON API response (5 MB).
	maxJSONResponseSize = 5 * 1024 * 1024
	// maxImageResponseSize is the maximum size of a JPEG snapshot (10 MB).
	maxImageResponseSize = 10 * 1024 * 1024
)

// Client is a Frigate NVR API client.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new Frigate client.
func NewClient(baseURL string) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("frigate URL is required")
	}

	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// Event represents a Frigate detection event.
type Event struct {
	ID                 string   `json:"id"`
	Camera             string   `json:"camera"`
	Label              string   `json:"label"`
	SubLabel           string   `json:"sub_label,omitempty"`
	TopScore           float64  `json:"top_score"`
	StartTime          float64  `json:"start_time"`
	EndTime            *float64 `json:"end_time"`
	HasSnapshot        bool     `json:"has_snapshot"`
	HasClip            bool     `json:"has_clip"`
	Zones              []string `json:"zones"`
	CurrentZones       []string `json:"current_zones"`
	EnteredZones       []string `json:"entered_zones"`
	RetainIndefinitely bool     `json:"retain_indefinitely"`
}

// CameraConfig represents a camera's configuration.
type CameraConfig struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

// Config represents Frigate's configuration.
type Config struct {
	Cameras map[string]CameraConfig `json:"cameras"`
}

func (c *Client) doRequest(ctx context.Context, method, path string, query url.Values, maxSize int64) ([]byte, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	u.Path = path
	if query != nil {
		u.RawQuery = query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSize))
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// GetConfig retrieves Frigate's configuration including camera list.
func (c *Client) GetConfig(ctx context.Context) (*Config, error) {
	body, err := c.doRequest(ctx, "GET", "/api/config", nil, maxJSONResponseSize)
	if err != nil {
		return nil, err
	}

	var config Config
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &config, nil
}

// GetEvents retrieves recent events from Frigate.
func (c *Client) GetEvents(ctx context.Context, cameras []string, labels []string, limit int, after *time.Time) ([]Event, error) {
	query := url.Values{}
	if len(cameras) > 0 {
		for _, cam := range cameras {
			query.Add("cameras", cam)
		}
	}
	if len(labels) > 0 {
		for _, label := range labels {
			query.Add("labels", label)
		}
	}
	if limit > 0 {
		query.Set("limit", fmt.Sprintf("%d", limit))
	}
	if after != nil {
		query.Set("after", fmt.Sprintf("%d", after.Unix()))
	}

	body, err := c.doRequest(ctx, "GET", "/api/events", query, maxJSONResponseSize)
	if err != nil {
		return nil, err
	}

	var events []Event
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, fmt.Errorf("failed to parse events: %w", err)
	}

	return events, nil
}

// GetLatestFrame retrieves the latest frame from a camera as JPEG bytes.
func (c *Client) GetLatestFrame(ctx context.Context, camera string) ([]byte, error) {
	if err := validate.Identifier("camera", camera); err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/api/%s/latest.jpg", camera)
	return c.doRequest(ctx, "GET", path, nil, maxImageResponseSize)
}

// GetEventSnapshot retrieves the snapshot for a specific event.
func (c *Client) GetEventSnapshot(ctx context.Context, eventID string) ([]byte, error) {
	if err := validate.Identifier("event_id", eventID); err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/api/events/%s/snapshot.jpg", eventID)
	return c.doRequest(ctx, "GET", path, nil, maxImageResponseSize)
}
