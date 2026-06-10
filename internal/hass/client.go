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

	"github.com/gorilla/websocket"
	"github.com/nabkey/mcp-home/internal/validate"
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
	policy     *ServicePolicy
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

// SetServicePolicy installs a deny policy enforced by CallService and
// execute_script sequences. A nil policy allows everything.
func (c *Client) SetServicePolicy(p *ServicePolicy) { c.policy = p }

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
	// Append to any path prefix in the base URL (e.g. https://host/homeassistant).
	u.Path = strings.TrimSuffix(u.Path, "/") + path
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
	if err := c.policy.Check(domain, service); err != nil {
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
			return nil, fmt.Errorf("failed to parse service response: %w", err)
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

// upsertConfig POSTs a config to /api/config/<kind>/config/<id> and returns
// the decoded response, or a {"status": "ok", "id": id} placeholder when Home
// Assistant returns an empty or non-object body.
func (c *Client) upsertConfig(ctx context.Context, kind, id string, config map[string]any) (map[string]any, error) {
	if err := validate.Identifier(kind+" id", id); err != nil {
		return nil, err
	}

	path := fmt.Sprintf("/api/config/%s/config/%s", kind, id)
	body, err := c.doRequest(ctx, "POST", path, nil, config)
	if err != nil {
		return nil, err
	}

	if len(body) > 0 {
		var response map[string]any
		if err := json.Unmarshal(body, &response); err == nil {
			return response, nil
		}
	}
	return map[string]any{"status": "ok", "id": id}, nil
}

// deleteConfig deletes the config stored at /api/config/<kind>/config/<id>.
func (c *Client) deleteConfig(ctx context.Context, kind, id string) error {
	if err := validate.Identifier(kind+" id", id); err != nil {
		return err
	}
	_, err := c.doRequest(ctx, "DELETE", fmt.Sprintf("/api/config/%s/config/%s", kind, id), nil, nil)
	return err
}

// CreateAutomation creates a new automation.
func (c *Client) CreateAutomation(ctx context.Context, config map[string]any) (map[string]any, error) {
	id, ok := config["id"].(string)
	if !ok || id == "" {
		id = generateID()
		config["id"] = id
	}
	return c.upsertConfig(ctx, "automation", id, config)
}

// UpdateAutomation updates an existing automation.
func (c *Client) UpdateAutomation(ctx context.Context, id string, config map[string]any) (map[string]any, error) {
	return c.upsertConfig(ctx, "automation", id, config)
}

// DeleteAutomation deletes an automation.
func (c *Client) DeleteAutomation(ctx context.Context, id string) error {
	return c.deleteConfig(ctx, "automation", id)
}

// CreateScript creates a new script with the given object_id.
func (c *Client) CreateScript(ctx context.Context, objectID string, config map[string]any) (map[string]any, error) {
	return c.upsertConfig(ctx, "script", objectID, config)
}

// UpdateScript updates an existing script identified by its object_id.
func (c *Client) UpdateScript(ctx context.Context, objectID string, config map[string]any) (map[string]any, error) {
	return c.upsertConfig(ctx, "script", objectID, config)
}

// DeleteScript deletes a script identified by its object_id.
func (c *Client) DeleteScript(ctx context.Context, objectID string) error {
	return c.deleteConfig(ctx, "script", objectID)
}

// CreateScene creates a new scene.
func (c *Client) CreateScene(ctx context.Context, objectID string, config map[string]any) (map[string]any, error) {
	return c.upsertConfig(ctx, "scene", objectID, config)
}

// UpdateScene updates an existing scene identified by its object_id.
func (c *Client) UpdateScene(ctx context.Context, objectID string, config map[string]any) (map[string]any, error) {
	return c.upsertConfig(ctx, "scene", objectID, config)
}

// DeleteScene deletes a scene identified by its object_id.
func (c *Client) DeleteScene(ctx context.Context, objectID string) error {
	return c.deleteConfig(ctx, "scene", objectID)
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
	Summary      string         `json:"summary"`
	Start        map[string]any `json:"start,omitempty"`
	End          map[string]any `json:"end,omitempty"`
	Description  string         `json:"description,omitempty"`
	Location     string         `json:"location,omitempty"`
	UID          string         `json:"uid,omitempty"`
	RecurrenceID string         `json:"recurrence_id,omitempty"`
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

// WebsocketClient handles Home Assistant WebSocket API interactions. It is
// scoped to a single request: Dial with the request context, issue commands,
// then Close.
type WebsocketClient struct {
	baseURL string
	token   string
	conn    *websocket.Conn
	idSeq   int64

	// ctx is the context passed to Dial; pending reads are unblocked when it
	// is cancelled.
	ctx          context.Context
	stopCtxWatch func() bool
}

// Dial connects and authenticates with the Home Assistant WebSocket API.
// Cancelling ctx aborts the dial and unblocks any in-flight reads on the
// connection.
func (c *WebsocketClient) Dial(ctx context.Context) error {
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

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to dial: %w", err)
	}
	c.conn = conn
	c.ctx = ctx
	// gorilla/websocket reads cannot take a context, so force any pending
	// read to fail with an expired deadline when the request is cancelled.
	c.stopCtxWatch = context.AfterFunc(ctx, func() {
		_ = conn.SetReadDeadline(time.Now())
	})

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
	if c.stopCtxWatch != nil {
		c.stopCtxWatch()
	}
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

func (c *WebsocketClient) nextID() int {
	return int(atomic.AddInt64(&c.idSeq, 1))
}

// readResponse reads WebSocket messages until it finds one matching the given request ID.
// It enforces a read deadline to prevent hanging forever and honors cancellation
// of the context passed to Dial.
func (c *WebsocketClient) readResponse(id int) (map[string]any, error) {
	deadline := time.Now().Add(wsReadTimeout)
	if c.ctx != nil {
		if d, ok := c.ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
	}
	for {
		if c.ctx != nil && c.ctx.Err() != nil {
			return nil, c.ctx.Err()
		}
		if err := c.conn.SetReadDeadline(deadline); err != nil {
			return nil, fmt.Errorf("failed to set read deadline: %w", err)
		}

		var resp map[string]any
		if err := c.conn.ReadJSON(&resp); err != nil {
			if c.ctx != nil && c.ctx.Err() != nil {
				return nil, c.ctx.Err()
			}
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

// getEntityConfig retrieves the raw configuration for an automation, script,
// or scene entity via the "<domain>/config" WebSocket command.
func (c *WebsocketClient) getEntityConfig(domain, entityID string) (map[string]any, error) {
	resp, err := c.wsCommand(domain+"/config", map[string]any{"entity_id": entityID})
	if err != nil {
		return nil, err
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected %s/config result format", domain)
	}
	return result, nil
}

// GetAutomationConfig retrieves the raw configuration for a specific automation via WebSocket.
func (c *WebsocketClient) GetAutomationConfig(entityID string) (map[string]any, error) {
	return c.getEntityConfig("automation", entityID)
}

// GetScriptConfig retrieves the raw configuration for a specific script via WebSocket.
func (c *WebsocketClient) GetScriptConfig(entityID string) (map[string]any, error) {
	return c.getEntityConfig("script", entityID)
}

// TraceList returns a list of traces for an automation.
func (c *WebsocketClient) TraceList(domain, itemID string) ([]map[string]any, error) {
	resp, err := c.wsCommand("trace/list", map[string]any{
		"domain":  domain,
		"item_id": itemID,
	})
	if err != nil {
		return nil, err
	}
	return resultList(resp), nil
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
	return c.listResultMaps(helperType + "/list")
}

// CreateHelper creates a new helper of the given type. The config map should
// contain helper-specific fields (e.g. "name", "icon", and any type-specific
// settings); "type" is set automatically.
func (c *WebsocketClient) CreateHelper(helperType string, config map[string]any) (map[string]any, error) {
	if !IsHelperType(helperType) {
		return nil, fmt.Errorf("unsupported helper type: %s", helperType)
	}
	resp, err := c.wsCommand(helperType+"/create", config)
	if err != nil {
		return nil, err
	}
	return resultMap(resp), nil
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

	idField := helperType + "_id"
	fields := map[string]any{idField: helperID}
	for k, v := range config {
		if k == idField {
			continue
		}
		fields[k] = v
	}
	resp, err := c.wsCommand(helperType+"/update", fields)
	if err != nil {
		return nil, err
	}
	return resultMap(resp), nil
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
	_, err := c.wsCommand(helperType+"/delete", map[string]any{helperType + "_id": helperID})
	return err
}

// listResultMaps sends a WebSocket command that returns an array of objects and decodes the result.
func (c *WebsocketClient) listResultMaps(commandType string) ([]map[string]any, error) {
	resp, err := c.wsCommand(commandType, nil)
	if err != nil {
		return nil, err
	}
	return resultList(resp), nil
}

// wsCommand sends a WebSocket command of the given type with the supplied
// extra fields and returns the full response. The "id" and "type" fields are
// set automatically and cannot be overridden via fields.
func (c *WebsocketClient) wsCommand(commandType string, fields map[string]any) (map[string]any, error) {
	id := c.nextID()
	req := map[string]any{"id": id, "type": commandType}
	for k, v := range fields {
		if k == "id" || k == "type" {
			continue
		}
		req[k] = v
	}
	if err := c.conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("failed to send %s request: %w", commandType, err)
	}
	resp, err := c.readResponse(id)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", commandType, err)
	}
	return resp, nil
}

// resultMap extracts the "result" object from a WebSocket response, returning
// an empty map if it is absent or not an object.
func resultMap(resp map[string]any) map[string]any {
	if m, ok := resp["result"].(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// resultList extracts the "result" array from a WebSocket response as a slice
// of objects, skipping any non-object elements.
func resultList(resp map[string]any) []map[string]any {
	result, ok := resp["result"].([]any)
	if !ok {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(result))
	for _, r := range result {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// nullable returns nil for an empty string so it marshals to JSON null,
// otherwise the string itself. Used for optional WebSocket fields like
// Lovelace url_path where null selects the default dashboard.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
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
	return c.getEntityConfig("scene", entityID)
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
	fields := map[string]any{
		"start_time":    start.UTC().Format(time.RFC3339),
		"statistic_ids": statisticIDs,
		"period":        period,
	}
	if !end.IsZero() {
		fields["end_time"] = end.UTC().Format(time.RFC3339)
	}
	resp, err := c.wsCommand("recorder/statistics_during_period", fields)
	if err != nil {
		return nil, err
	}
	return resultMap(resp), nil
}

// TraceGet returns full details for a specific trace.
func (c *WebsocketClient) TraceGet(domain, itemID, runID string) (map[string]any, error) {
	resp, err := c.wsCommand("trace/get", map[string]any{
		"domain":  domain,
		"item_id": itemID,
		"run_id":  runID,
	})
	if err != nil {
		return nil, err
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected trace/get result format")
	}
	return result, nil
}
