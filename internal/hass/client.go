// Package hass provides a client for the Home Assistant REST and WebSocket APIs.
package hass

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nabkey/mcp-home/internal/validate"
	"github.com/gorilla/websocket"
)

const (
	// maxJSONResponseSize is the maximum size of a JSON API response (5 MB).
	maxJSONResponseSize = 5 * 1024 * 1024
	// wsReadTimeout is the maximum time to wait for a WebSocket response.
	wsReadTimeout = 30 * time.Second
)

// Client is a Home Assistant REST API client.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
}

// NewClient creates a new Home Assistant client.
func NewClient(baseURL, token string) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("home assistant URL is required")
	}
	if token == "" {
		return nil, fmt.Errorf("home assistant token is required")
	}

	baseURL = strings.TrimSuffix(baseURL, "/")

	return &Client{
		baseURL: baseURL,
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

// State represents a Home Assistant entity state.
type State struct {
	EntityID    string         `json:"entity_id"`
	State       string         `json:"state"`
	Attributes  map[string]any `json:"attributes"`
	LastChanged time.Time      `json:"last_changed"`
	LastUpdated time.Time      `json:"last_updated"`
}

// LogbookEntry represents an entry in the Home Assistant logbook.
type LogbookEntry struct {
	When      time.Time `json:"when"`
	Name      string    `json:"name"`
	Message   string    `json:"message,omitempty"`
	EntityID  string    `json:"entity_id,omitempty"`
	State     string    `json:"state,omitempty"`
	Domain    string    `json:"domain,omitempty"`
	ContextID string    `json:"context_id,omitempty"`
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

	req.Header.Set("Authorization", "Bearer "+c.token)
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

// GetStates retrieves all entity states from Home Assistant.
func (c *Client) GetStates(ctx context.Context) ([]State, error) {
	body, err := c.doRequest(ctx, "GET", "/api/states", nil, nil)
	if err != nil {
		return nil, err
	}

	var states []State
	if err := json.Unmarshal(body, &states); err != nil {
		return nil, fmt.Errorf("failed to parse states: %w", err)
	}

	return states, nil
}

// GetLogbook retrieves logbook entries from Home Assistant.
func (c *Client) GetLogbook(ctx context.Context, since time.Time, entityID string) ([]LogbookEntry, error) {
	path := fmt.Sprintf("/api/logbook/%s", since.Format(time.RFC3339))

	query := url.Values{}
	if entityID != "" {
		query.Set("entity", entityID)
	}

	body, err := c.doRequest(ctx, "GET", path, query, nil)
	if err != nil {
		return nil, err
	}

	var entries []LogbookEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("failed to parse logbook: %w", err)
	}

	return entries, nil
}

// CallService calls a Home Assistant service.
func (c *Client) CallService(ctx context.Context, domain, service string, data map[string]any) ([]State, error) {
	if err := validate.Identifier("domain", domain); err != nil {
		return nil, err
	}
	if err := validate.Identifier("service", service); err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/api/services/%s/%s", domain, service)
	body, err := c.doRequest(ctx, "POST", path, nil, data)
	if err != nil {
		return nil, err
	}

	var states []State
	if len(body) > 0 {
		if err := json.Unmarshal(body, &states); err != nil {
			return nil, nil
		}
	}
	return states, nil
}

// GetTodoLists returns all entities in the todo domain.
func (c *Client) GetTodoLists(ctx context.Context) ([]State, error) {
	states, err := c.GetStates(ctx)
	if err != nil {
		return nil, err
	}
	var lists []State
	for _, s := range states {
		if strings.HasPrefix(s.EntityID, "todo.") {
			lists = append(lists, s)
		}
	}
	return lists, nil
}

// GetTodoItems retrieves items for a specific to-do list entity.
func (c *Client) GetTodoItems(ctx context.Context, entityID string) ([]map[string]any, error) {
	data := map[string]any{
		"entity_id": entityID,
	}
	query := url.Values{}
	query.Set("return_response", "")
	body, err := c.doRequest(ctx, "POST", "/api/services/todo/get_items", query, data)
	if err != nil {
		return nil, err
	}

	var resp map[string]map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse todo items response: %w", err)
	}

	result, ok := resp[entityID]
	if !ok {
		return nil, fmt.Errorf("entity %s not found in response", entityID)
	}

	items, ok := result["items"].([]any)
	if !ok {
		return []map[string]any{}, nil
	}

	typedItems := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if m, ok := item.(map[string]any); ok {
			typedItems = append(typedItems, m)
		}
	}

	return typedItems, nil
}

// generateID returns a random hex string suitable for automation IDs.
func generateID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp if crypto/rand fails (shouldn't happen).
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// CreateAutomation creates a new automation.
func (c *Client) CreateAutomation(ctx context.Context, config map[string]any) (map[string]any, error) {
	id, ok := config["id"].(string)
	if !ok || id == "" {
		id = generateID()
		config["id"] = id
	}
	if err := validate.Identifier("automation id", id); err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/api/config/automation/config/%s", id)
	body, err := c.doRequest(ctx, "POST", path, nil, config)
	if err != nil {
		return nil, err
	}

	var response map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &response); err != nil {
			return map[string]any{"status": "ok", "id": id}, nil
		}
	} else {
		return map[string]any{"status": "ok", "id": id}, nil
	}

	return response, nil
}

// UpdateAutomation updates an existing automation.
func (c *Client) UpdateAutomation(ctx context.Context, id string, config map[string]any) (map[string]any, error) {
	if err := validate.Identifier("automation id", id); err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/api/config/automation/config/%s", id)
	body, err := c.doRequest(ctx, "POST", path, nil, config)
	if err != nil {
		return nil, err
	}

	var response map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &response); err != nil {
			return map[string]any{"status": "ok", "id": id}, nil
		}
	} else {
		return map[string]any{"status": "ok", "id": id}, nil
	}

	return response, nil
}

// DeleteAutomation deletes an automation.
func (c *Client) DeleteAutomation(ctx context.Context, id string) error {
	if err := validate.Identifier("automation id", id); err != nil {
		return err
	}
	_, err := c.doRequest(ctx, "DELETE", fmt.Sprintf("/api/config/automation/config/%s", id), nil, nil)
	return err
}

// BaseURL returns the client's base URL.
func (c *Client) BaseURL() string { return c.baseURL }

// NewWebsocketClient creates a new WebSocket client using this client's credentials.
func (c *Client) NewWebsocketClient() *WebsocketClient {
	return &WebsocketClient{
		baseURL: c.baseURL,
		token:   c.token,
	}
}

// WebsocketClient handles Home Assistant WebSocket API interactions.
type WebsocketClient struct {
	baseURL string
	token   string
	conn    *websocket.Conn
	idSeq   int64
}

// Dial connects and authenticates with the Home Assistant WebSocket API.
func (c *WebsocketClient) Dial() error {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return fmt.Errorf("invalid base URL: %w", err)
	}

	scheme := "ws"
	if u.Scheme == "https" {
		scheme = "wss"
	}
	u.Scheme = scheme
	u.Path = "/api/websocket"

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}
	c.conn = conn

	var authReq map[string]any
	if err := c.conn.ReadJSON(&authReq); err != nil {
		return fmt.Errorf("failed to read auth_required: %w", err)
	}
	if authReq["type"] != "auth_required" {
		return fmt.Errorf("unexpected message type: %v", authReq["type"])
	}

	authMsg := map[string]string{
		"type":         "auth",
		"access_token": c.token,
	}
	if err := c.conn.WriteJSON(authMsg); err != nil {
		return fmt.Errorf("failed to send auth: %w", err)
	}

	var authResp map[string]any
	if err := c.conn.ReadJSON(&authResp); err != nil {
		return fmt.Errorf("failed to read auth response: %w", err)
	}
	if authResp["type"] != "auth_ok" {
		return fmt.Errorf("authentication failed: %v", authResp["message"])
	}

	return nil
}

// Close closes the WebSocket connection.
func (c *WebsocketClient) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func (c *WebsocketClient) nextID() int {
	return int(atomic.AddInt64(&c.idSeq, 1))
}

// readResponse reads WebSocket messages until it finds one matching the given request ID.
// It enforces a read deadline to prevent hanging forever.
func (c *WebsocketClient) readResponse(id int) (map[string]any, error) {
	deadline := time.Now().Add(wsReadTimeout)
	for {
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return nil, fmt.Errorf("failed to set read deadline: %w", err)
		}

		var resp map[string]any
		if err := c.conn.ReadJSON(&resp); err != nil {
			return nil, fmt.Errorf("failed to read response: %w", err)
		}

		respID, ok := resp["id"].(float64)
		if ok && int(respID) == id {
			if success, ok := resp["success"].(bool); ok && !success {
				return nil, fmt.Errorf("request error: %v", resp["error"])
			}
			return resp, nil
		}
	}
}

// GetAutomationConfig retrieves the raw configuration for a specific automation via WebSocket.
func (c *WebsocketClient) GetAutomationConfig(entityID string) (map[string]any, error) {
	id := c.nextID()
	req := map[string]any{
		"id":        id,
		"type":      "automation/config",
		"entity_id": entityID,
	}

	if err := c.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to send automation/config request: %w", err)
	}

	resp, err := c.readResponse(id)
	if err != nil {
		return nil, fmt.Errorf("automation/config: %w", err)
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected automation/config result format")
	}
	return result, nil
}

// TraceList returns a list of traces for an automation.
func (c *WebsocketClient) TraceList(domain, itemID string) ([]map[string]any, error) {
	id := c.nextID()
	req := map[string]any{
		"id":      id,
		"type":    "trace/list",
		"domain":  domain,
		"item_id": itemID,
	}

	if err := c.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to send trace/list request: %w", err)
	}

	resp, err := c.readResponse(id)
	if err != nil {
		return nil, fmt.Errorf("trace/list: %w", err)
	}

	result, ok := resp["result"].([]any)
	if !ok {
		return []map[string]any{}, nil
	}

	traces := make([]map[string]any, 0, len(result))
	for _, r := range result {
		if m, ok := r.(map[string]any); ok {
			traces = append(traces, m)
		}
	}
	return traces, nil
}

// TraceGet returns full details for a specific trace.
func (c *WebsocketClient) TraceGet(domain, itemID, runID string) (map[string]any, error) {
	id := c.nextID()
	req := map[string]any{
		"id":      id,
		"type":    "trace/get",
		"domain":  domain,
		"item_id": itemID,
		"run_id":  runID,
	}

	if err := c.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to send trace/get request: %w", err)
	}

	resp, err := c.readResponse(id)
	if err != nil {
		return nil, fmt.Errorf("trace/get: %w", err)
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected trace/get result format")
	}
	return result, nil
}
