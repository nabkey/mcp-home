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

// CreateScript creates a new script with the given object_id.
func (c *Client) CreateScript(ctx context.Context, objectID string, config map[string]any) (map[string]any, error) {
	if err := validate.Identifier("script id", objectID); err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/api/config/script/config/%s", objectID)
	body, err := c.doRequest(ctx, "POST", path, nil, config)
	if err != nil {
		return nil, err
	}

	var response map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &response); err != nil {
			return map[string]any{"status": "ok", "id": objectID}, nil
		}
	} else {
		return map[string]any{"status": "ok", "id": objectID}, nil
	}

	return response, nil
}

// UpdateScript updates an existing script identified by its object_id.
func (c *Client) UpdateScript(ctx context.Context, objectID string, config map[string]any) (map[string]any, error) {
	if err := validate.Identifier("script id", objectID); err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/api/config/script/config/%s", objectID)
	body, err := c.doRequest(ctx, "POST", path, nil, config)
	if err != nil {
		return nil, err
	}

	var response map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &response); err != nil {
			return map[string]any{"status": "ok", "id": objectID}, nil
		}
	} else {
		return map[string]any{"status": "ok", "id": objectID}, nil
	}

	return response, nil
}

// DeleteScript deletes a script identified by its object_id.
func (c *Client) DeleteScript(ctx context.Context, objectID string) error {
	if err := validate.Identifier("script id", objectID); err != nil {
		return err
	}
	_, err := c.doRequest(ctx, "DELETE", fmt.Sprintf("/api/config/script/config/%s", objectID), nil, nil)
	return err
}

// CreateScene creates a new scene.
func (c *Client) CreateScene(ctx context.Context, objectID string, config map[string]any) (map[string]any, error) {
	if err := validate.Identifier("scene id", objectID); err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/api/config/scene/config/%s", objectID)
	body, err := c.doRequest(ctx, "POST", path, nil, config)
	if err != nil {
		return nil, err
	}

	var response map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &response); err != nil {
			return map[string]any{"status": "ok", "id": objectID}, nil
		}
	} else {
		return map[string]any{"status": "ok", "id": objectID}, nil
	}
	return response, nil
}

// UpdateScene updates an existing scene identified by its object_id.
func (c *Client) UpdateScene(ctx context.Context, objectID string, config map[string]any) (map[string]any, error) {
	if err := validate.Identifier("scene id", objectID); err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/api/config/scene/config/%s", objectID)
	body, err := c.doRequest(ctx, "POST", path, nil, config)
	if err != nil {
		return nil, err
	}

	var response map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &response); err != nil {
			return map[string]any{"status": "ok", "id": objectID}, nil
		}
	} else {
		return map[string]any{"status": "ok", "id": objectID}, nil
	}
	return response, nil
}

// DeleteScene deletes a scene identified by its object_id.
func (c *Client) DeleteScene(ctx context.Context, objectID string) error {
	if err := validate.Identifier("scene id", objectID); err != nil {
		return err
	}
	_, err := c.doRequest(ctx, "DELETE", fmt.Sprintf("/api/config/scene/config/%s", objectID), nil, nil)
	return err
}

// GetServices retrieves the list of available services per domain.
// The response is an array of {domain, services} objects; this method returns it as []map[string]any.
func (c *Client) GetServices(ctx context.Context) ([]map[string]any, error) {
	body, err := c.doRequest(ctx, "GET", "/api/services", nil, nil)
	if err != nil {
		return nil, err
	}

	var services []map[string]any
	if err := json.Unmarshal(body, &services); err != nil {
		return nil, fmt.Errorf("failed to parse services: %w", err)
	}
	return services, nil
}

// GetHistory retrieves state history for the given entity IDs over the given window.
// Returns an array of arrays — one inner array of state points per entity.
func (c *Client) GetHistory(ctx context.Context, since, end time.Time, entityIDs []string, minimalResponse, significantOnly bool) ([][]map[string]any, error) {
	path := fmt.Sprintf("/api/history/period/%s", since.UTC().Format(time.RFC3339))

	query := url.Values{}
	if !end.IsZero() {
		query.Set("end_time", end.UTC().Format(time.RFC3339))
	}
	if len(entityIDs) > 0 {
		query.Set("filter_entity_id", strings.Join(entityIDs, ","))
	}
	if minimalResponse {
		query.Set("minimal_response", "")
	}
	if significantOnly {
		query.Set("significant_changes_only", "")
	}

	body, err := c.doRequest(ctx, "GET", path, query, nil)
	if err != nil {
		return nil, err
	}

	var history [][]map[string]any
	if err := json.Unmarshal(body, &history); err != nil {
		return nil, fmt.Errorf("failed to parse history: %w", err)
	}
	return history, nil
}

// RenderTemplate evaluates a Jinja2 template against Home Assistant state.
// Optional variables map is passed through if non-nil.
func (c *Client) RenderTemplate(ctx context.Context, template string, variables map[string]any) (string, error) {
	if template == "" {
		return "", fmt.Errorf("template is required")
	}
	payload := map[string]any{"template": template}
	if len(variables) > 0 {
		payload["variables"] = variables
	}
	body, err := c.doRequest(ctx, "POST", "/api/template", nil, payload)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// CalendarEntity is the metadata returned by GET /api/calendars.
type CalendarEntity struct {
	EntityID string `json:"entity_id"`
	Name     string `json:"name,omitempty"`
}

// CalendarEvent represents a single event from a calendar entity.
type CalendarEvent struct {
	Summary     string         `json:"summary"`
	Start       map[string]any `json:"start,omitempty"`
	End         map[string]any `json:"end,omitempty"`
	Description string         `json:"description,omitempty"`
	Location    string         `json:"location,omitempty"`
	UID         string         `json:"uid,omitempty"`
	RecurrenceID string        `json:"recurrence_id,omitempty"`
}

// GetCalendars returns the list of calendar entities.
func (c *Client) GetCalendars(ctx context.Context) ([]CalendarEntity, error) {
	body, err := c.doRequest(ctx, "GET", "/api/calendars", nil, nil)
	if err != nil {
		return nil, err
	}
	var calendars []CalendarEntity
	if err := json.Unmarshal(body, &calendars); err != nil {
		return nil, fmt.Errorf("failed to parse calendars: %w", err)
	}
	return calendars, nil
}

// GetCalendarEvents returns events for a calendar entity in [start, end].
func (c *Client) GetCalendarEvents(ctx context.Context, entityID string, start, end time.Time) ([]CalendarEvent, error) {
	if err := validate.Identifier("calendar entity_id", entityID); err != nil {
		return nil, err
	}
	if start.IsZero() || end.IsZero() {
		return nil, fmt.Errorf("start and end are required")
	}

	query := url.Values{}
	query.Set("start", start.UTC().Format(time.RFC3339))
	query.Set("end", end.UTC().Format(time.RFC3339))

	body, err := c.doRequest(ctx, "GET", fmt.Sprintf("/api/calendars/%s", entityID), query, nil)
	if err != nil {
		return nil, err
	}
	var events []CalendarEvent
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, fmt.Errorf("failed to parse calendar events: %w", err)
	}
	return events, nil
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

// GetScriptConfig retrieves the raw configuration for a specific script via WebSocket.
func (c *WebsocketClient) GetScriptConfig(entityID string) (map[string]any, error) {
	id := c.nextID()
	req := map[string]any{
		"id":        id,
		"type":      "script/config",
		"entity_id": entityID,
	}

	if err := c.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to send script/config request: %w", err)
	}

	resp, err := c.readResponse(id)
	if err != nil {
		return nil, fmt.Errorf("script/config: %w", err)
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected script/config result format")
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

// HelperTypes lists the Home Assistant helper domains supported by the
// storage-collection WebSocket API.
var HelperTypes = []string{
	"input_boolean",
	"input_button",
	"input_datetime",
	"input_number",
	"input_select",
	"input_text",
	"counter",
	"timer",
	"schedule",
}

// IsHelperType reports whether t is a supported helper domain.
func IsHelperType(t string) bool {
	for _, h := range HelperTypes {
		if h == t {
			return true
		}
	}
	return false
}

// ListHelpers returns all configured helpers of the given type.
func (c *WebsocketClient) ListHelpers(helperType string) ([]map[string]any, error) {
	if !IsHelperType(helperType) {
		return nil, fmt.Errorf("unsupported helper type: %s", helperType)
	}

	id := c.nextID()
	req := map[string]any{
		"id":   id,
		"type": helperType + "/list",
	}

	if err := c.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to send %s/list request: %w", helperType, err)
	}

	resp, err := c.readResponse(id)
	if err != nil {
		return nil, fmt.Errorf("%s/list: %w", helperType, err)
	}

	result, ok := resp["result"].([]any)
	if !ok {
		return []map[string]any{}, nil
	}

	helpers := make([]map[string]any, 0, len(result))
	for _, r := range result {
		if m, ok := r.(map[string]any); ok {
			helpers = append(helpers, m)
		}
	}
	return helpers, nil
}

// CreateHelper creates a new helper of the given type. The config map should
// contain helper-specific fields (e.g. "name", "icon", and any type-specific
// settings); "type" is set automatically.
func (c *WebsocketClient) CreateHelper(helperType string, config map[string]any) (map[string]any, error) {
	if !IsHelperType(helperType) {
		return nil, fmt.Errorf("unsupported helper type: %s", helperType)
	}

	id := c.nextID()
	req := map[string]any{
		"id":   id,
		"type": helperType + "/create",
	}
	for k, v := range config {
		if k == "id" || k == "type" {
			continue
		}
		req[k] = v
	}

	if err := c.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to send %s/create request: %w", helperType, err)
	}

	resp, err := c.readResponse(id)
	if err != nil {
		return nil, fmt.Errorf("%s/create: %w", helperType, err)
	}

	if result, ok := resp["result"].(map[string]any); ok {
		return result, nil
	}
	return map[string]any{}, nil
}

// UpdateHelper updates an existing helper. helperID is the helper's storage ID
// (without the domain prefix, e.g. "my_toggle" not "input_boolean.my_toggle").
func (c *WebsocketClient) UpdateHelper(helperType, helperID string, config map[string]any) (map[string]any, error) {
	if !IsHelperType(helperType) {
		return nil, fmt.Errorf("unsupported helper type: %s", helperType)
	}
	if err := validate.Identifier("helper id", helperID); err != nil {
		return nil, err
	}

	id := c.nextID()
	idField := helperType + "_id"
	req := map[string]any{
		"id":    id,
		"type":  helperType + "/update",
		idField: helperID,
	}
	for k, v := range config {
		if k == "id" || k == "type" || k == idField {
			continue
		}
		req[k] = v
	}

	if err := c.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to send %s/update request: %w", helperType, err)
	}

	resp, err := c.readResponse(id)
	if err != nil {
		return nil, fmt.Errorf("%s/update: %w", helperType, err)
	}

	if result, ok := resp["result"].(map[string]any); ok {
		return result, nil
	}
	return map[string]any{}, nil
}

// DeleteHelper deletes a helper. helperID is the helper's storage ID
// (without the domain prefix).
func (c *WebsocketClient) DeleteHelper(helperType, helperID string) error {
	if !IsHelperType(helperType) {
		return fmt.Errorf("unsupported helper type: %s", helperType)
	}
	if err := validate.Identifier("helper id", helperID); err != nil {
		return err
	}

	id := c.nextID()
	req := map[string]any{
		"id":               id,
		"type":             helperType + "/delete",
		helperType + "_id": helperID,
	}

	if err := c.conn.WriteJSON(req); err != nil {
		return fmt.Errorf("failed to send %s/delete request: %w", helperType, err)
	}

	if _, err := c.readResponse(id); err != nil {
		return fmt.Errorf("%s/delete: %w", helperType, err)
	}
	return nil
}

// listResultMaps sends a WebSocket command that returns an array of objects and decodes the result.
func (c *WebsocketClient) listResultMaps(commandType string) ([]map[string]any, error) {
	id := c.nextID()
	if err := c.conn.WriteJSON(map[string]any{"id": id, "type": commandType}); err != nil {
		return nil, fmt.Errorf("failed to send %s request: %w", commandType, err)
	}
	resp, err := c.readResponse(id)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", commandType, err)
	}
	result, ok := resp["result"].([]any)
	if !ok {
		return []map[string]any{}, nil
	}
	out := make([]map[string]any, 0, len(result))
	for _, r := range result {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// ListAreas returns all configured areas from the area registry.
func (c *WebsocketClient) ListAreas() ([]map[string]any, error) {
	return c.listResultMaps("config/area_registry/list")
}

// ListDevices returns all configured devices from the device registry.
func (c *WebsocketClient) ListDevices() ([]map[string]any, error) {
	return c.listResultMaps("config/device_registry/list")
}

// ListEntityRegistry returns all entity registry entries (with name, area_id, device_id, labels, etc).
func (c *WebsocketClient) ListEntityRegistry() ([]map[string]any, error) {
	return c.listResultMaps("config/entity_registry/list")
}

// ListLabels returns all labels from the label registry.
func (c *WebsocketClient) ListLabels() ([]map[string]any, error) {
	return c.listResultMaps("config/label_registry/list")
}

// ListFloors returns all floors from the floor registry.
func (c *WebsocketClient) ListFloors() ([]map[string]any, error) {
	return c.listResultMaps("config/floor_registry/list")
}

// GetSceneConfig retrieves the raw configuration for a specific scene via WebSocket.
func (c *WebsocketClient) GetSceneConfig(entityID string) (map[string]any, error) {
	id := c.nextID()
	req := map[string]any{
		"id":        id,
		"type":      "scene/config",
		"entity_id": entityID,
	}
	if err := c.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to send scene/config request: %w", err)
	}
	resp, err := c.readResponse(id)
	if err != nil {
		return nil, fmt.Errorf("scene/config: %w", err)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected scene/config result format")
	}
	return result, nil
}

// StatisticsDuringPeriod returns long-term statistics for the given statistic_ids in [start, end].
// period must be one of "5minute", "hour", "day", "week", "month".
// Result is keyed by statistic_id.
func (c *WebsocketClient) StatisticsDuringPeriod(start, end time.Time, statisticIDs []string, period string) (map[string]any, error) {
	if len(statisticIDs) == 0 {
		return nil, fmt.Errorf("statistic_ids are required")
	}
	if period == "" {
		period = "hour"
	}
	id := c.nextID()
	req := map[string]any{
		"id":            id,
		"type":          "recorder/statistics_during_period",
		"start_time":    start.UTC().Format(time.RFC3339),
		"statistic_ids": statisticIDs,
		"period":        period,
	}
	if !end.IsZero() {
		req["end_time"] = end.UTC().Format(time.RFC3339)
	}
	if err := c.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to send recorder/statistics_during_period request: %w", err)
	}
	resp, err := c.readResponse(id)
	if err != nil {
		return nil, fmt.Errorf("recorder/statistics_during_period: %w", err)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	return result, nil
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
