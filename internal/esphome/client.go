// Package esphome provides MCP tools for driving an ESPHome dashboard (the
// "ESPHome Device Builder", typically the Home Assistant add-on): listing
// devices, reading and writing device configs, managing secrets, validating,
// compiling, uploading firmware, and streaming logs.
//
// The modern dashboard exposes a single multiplexed WebSocket at /ws using a
// {command, message_id, args} -> {result} / streaming-event protocol. This
// client speaks that protocol for everything except compile/upload, which use
// the dashboard's still-supported legacy spawn-protocol WebSockets (/compile,
// /upload) — those long-running build streams are simplest over the legacy
// channel.
package esphome

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// maxBinarySize caps a downloaded firmware image (16 MB covers ESP32 flash).
	maxBinarySize = 16 * 1024 * 1024
	// maxCommandOutput caps captured stdout from a streamed command.
	maxCommandOutput = 512 * 1024
	// wsIdleTimeout bounds how long we wait between messages before giving up.
	// Compiles can be quiet for a while as toolchains churn, so it is generous.
	wsIdleTimeout = 10 * time.Minute
)

// Client talks to an ESPHome Device Builder dashboard.
type Client struct {
	baseURL  string
	password string
}

// NewClient creates a dashboard client. baseURL points at the dashboard root
// (e.g. http://homeassistant.local:6052). password is optional and only used
// if the dashboard reports requires_auth.
func NewClient(baseURL, password string) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("esphome URL is required")
	}
	return &Client{
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		password: password,
	}, nil
}

// --- WebSocket command session (/ws) ---

// session is a single /ws connection scoped to one logical operation: dial,
// (optionally) authenticate, issue one or more commands, then close.
type session struct {
	conn  *websocket.Conn
	ctx   context.Context
	idSeq int
}

// wsMessage is the union of every server->client message shape on /ws. The
// field that is set distinguishes the kind: server info (server_version, no
// message_id), result (message_id + no event/error), event (message_id +
// event), or error (message_id + error_code).
type wsMessage struct {
	MessageID     string          `json:"message_id"`
	Result        json.RawMessage `json:"result"`
	Event         string          `json:"event"`
	Data          json.RawMessage `json:"data"`
	ErrorCode     string          `json:"error_code"`
	Details       string          `json:"details"`
	ServerVersion string          `json:"server_version"`
	RequiresAuth  bool            `json:"requires_auth"`
}

func (c *Client) wsURL(path string) (string, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid base URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	return u.String(), nil
}

// dial opens a /ws session, reads the ServerInfoMessage, and authenticates if
// the dashboard requires it.
func (c *Client) dial(ctx context.Context) (*session, error) {
	wsURL, err := c.wsURL("/ws")
	if err != nil {
		return nil, err
	}
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dialing /ws (status %d): %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("dialing /ws: %w", err)
	}
	s := &session{conn: conn, ctx: ctx}
	// Unblock pending reads when the caller's context is cancelled.
	context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })

	info, err := s.read()
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("reading server info: %w", err)
	}
	if info.ServerVersion == "" {
		_ = conn.Close()
		return nil, fmt.Errorf("unexpected first message (no server info)")
	}
	if info.RequiresAuth {
		if c.password == "" {
			_ = conn.Close()
			return nil, fmt.Errorf("dashboard requires authentication but no ESPHOME_PASSWORD is set")
		}
		if _, err := s.command("auth/login", map[string]any{
			"username": "admin",
			"password": c.password,
		}); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("authentication failed: %w", err)
		}
	}
	return s, nil
}

func (s *session) close() { _ = s.conn.Close() }

func (s *session) read() (*wsMessage, error) {
	// Bound each read by the idle timeout. Context cancellation is handled
	// separately by the AfterFunc set in dial(), which forces a read deadline
	// only once ctx is done — so a read interrupted by cancellation always
	// observes a non-nil ctx.Err() below, never a bare i/o timeout.
	if err := s.conn.SetReadDeadline(time.Now().Add(wsIdleTimeout)); err != nil {
		return nil, err
	}
	var msg wsMessage
	if err := s.conn.ReadJSON(&msg); err != nil {
		if s.ctx.Err() != nil {
			return nil, s.ctx.Err()
		}
		return nil, err
	}
	return &msg, nil
}

func (s *session) nextID() string {
	s.idSeq++
	return fmt.Sprintf("%d", s.idSeq)
}

// command sends a request and returns its result payload, ignoring any
// unrelated push messages. Use for non-streaming commands.
func (s *session) command(cmd string, args map[string]any) (json.RawMessage, error) {
	id := s.nextID()
	if err := s.conn.WriteJSON(map[string]any{
		"command":    cmd,
		"message_id": id,
		"args":       args,
	}); err != nil {
		return nil, fmt.Errorf("sending %s: %w", cmd, err)
	}
	for {
		msg, err := s.read()
		if err != nil {
			return nil, err
		}
		if msg.MessageID != id {
			continue // unrelated push/event or another command's reply
		}
		if msg.ErrorCode != "" {
			return nil, &commandError{code: msg.ErrorCode, details: msg.Details}
		}
		if msg.Event == "" {
			return msg.Result, nil
		}
		// Streaming output for a command we treat as non-streaming; skip until
		// the terminal result event.
		if msg.Event == "result" {
			return msg.Data, nil
		}
	}
}

// stream sends a streaming command (validate, logs) and collects its output
// lines until the terminal result event, the context expires, or the socket
// closes.
func (s *session) stream(cmd string, args map[string]any) (*CommandResult, error) {
	id := s.nextID()
	if err := s.conn.WriteJSON(map[string]any{
		"command":    cmd,
		"message_id": id,
		"args":       args,
	}); err != nil {
		return nil, fmt.Errorf("sending %s: %w", cmd, err)
	}

	var b strings.Builder
	result := &CommandResult{}
	for {
		msg, err := s.read()
		if err != nil {
			if s.ctx.Err() != nil {
				result.TimedOut = true
				result.Output = b.String()
				return result, nil
			}
			// Socket closed or read error ends the stream; return what we have.
			result.Output = b.String()
			return result, nil
		}
		if msg.MessageID != id {
			continue
		}
		if msg.ErrorCode != "" {
			return nil, &commandError{code: msg.ErrorCode, details: msg.Details}
		}
		switch msg.Event {
		case "output":
			var line string
			if json.Unmarshal(msg.Data, &line) == nil {
				if b.Len() < maxCommandOutput {
					b.WriteString(line)
				} else {
					result.Truncated = true
				}
			}
		case "result", "":
			// Terminal result for a streaming command: {success, code}.
			var term struct {
				Success bool `json:"success"`
				Code    int  `json:"code"`
			}
			payload := msg.Data
			if msg.Event == "" {
				payload = msg.Result
			}
			_ = json.Unmarshal(payload, &term)
			code := term.Code
			if msg.Event == "" && len(payload) == 0 {
				// A bare result with no body: treat as success.
				term.Success = true
			}
			result.ExitCode = &code
			result.Output = b.String()
			return result, nil
		}
	}
}

// commandError carries the dashboard's structured error code.
type commandError struct {
	code    string
	details string
}

func (e *commandError) Error() string {
	if e.details != "" {
		return fmt.Sprintf("%s: %s", e.code, e.details)
	}
	return e.code
}

func isNotFound(err error) bool {
	ce, ok := err.(*commandError)
	return ok && ce.code == "not_found"
}

// --- High-level operations ---

// ListDevices returns the dashboard's configured devices as raw objects.
func (c *Client) ListDevices(ctx context.Context) ([]map[string]any, error) {
	s, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	raw, err := s.command("devices/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var payload struct {
		Configured []map[string]any `json:"configured"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("parsing devices/list: %w", err)
	}
	return payload.Configured, nil
}

// SecretKeys returns the names (not values) of keys in the shared secrets.yaml.
func (c *Client) SecretKeys(ctx context.Context) ([]string, error) {
	s, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	raw, err := s.command("config/get_secrets", map[string]any{})
	if err != nil {
		return nil, err
	}
	var keys []string
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil, fmt.Errorf("parsing config/get_secrets: %w", err)
	}
	return keys, nil
}

// ReadConfig returns a device's YAML configuration.
func (c *Client) ReadConfig(ctx context.Context, configuration string) (string, error) {
	s, err := c.dial(ctx)
	if err != nil {
		return "", err
	}
	defer s.close()
	raw, err := s.command("devices/get_config", map[string]any{"configuration": configuration})
	if err != nil {
		return "", err
	}
	var content string
	if err := json.Unmarshal(raw, &content); err != nil {
		return "", fmt.Errorf("parsing devices/get_config: %w", err)
	}
	return content, nil
}

// WriteConfig writes a device's YAML configuration, creating the device if it
// does not yet exist. Returns true if a new device was created.
func (c *Client) WriteConfig(ctx context.Context, configuration, content string) (created bool, err error) {
	s, err := c.dial(ctx)
	if err != nil {
		return false, err
	}
	defer s.close()

	_, err = s.command("devices/update_config", map[string]any{
		"configuration": configuration,
		"content":       content,
	})
	if err == nil {
		return false, nil
	}
	if !isNotFound(err) {
		return false, err
	}
	// Device doesn't exist yet — create it. The name only sets the filename;
	// the YAML is written as-is, so esphome.name from the content is preserved.
	name := strings.TrimSuffix(configuration, ".yaml")
	_, err = s.command("devices/create", map[string]any{
		"name":         name,
		"file_content": content,
		"overwrite":    true,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// Validate runs config validation for a device (streaming).
func (c *Client) Validate(ctx context.Context, configuration string) (*CommandResult, error) {
	s, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	return s.stream("devices/validate", map[string]any{"configuration": configuration})
}

// Logs streams device logs until ctx is cancelled (the caller should pass a
// bounded context) and returns what was captured. port selects the source
// ("OTA" for a network device, default).
func (c *Client) Logs(ctx context.Context, configuration, port string) (*CommandResult, error) {
	if port == "" {
		port = "OTA"
	}
	s, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	return s.stream("devices/logs", map[string]any{
		"configuration": configuration,
		"port":          port,
	})
}

// Binary describes a downloadable firmware artifact.
type Binary struct {
	Title string `json:"title"`
	File  string `json:"file"`
	Type  string `json:"type"`
}

// DownloadBinary fetches a compiled firmware image. When factory is true it
// selects the full-flash image (for a first USB flash); otherwise the OTA
// image. Returns the bytes and the artifact's filename.
func (c *Client) DownloadBinary(ctx context.Context, configuration string, factory bool) ([]byte, string, error) {
	s, err := c.dial(ctx)
	if err != nil {
		return nil, "", err
	}
	defer s.close()

	raw, err := s.command("firmware/get_binaries", map[string]any{"configuration": configuration})
	if err != nil {
		return nil, "", err
	}
	var binaries []Binary
	if err := json.Unmarshal(raw, &binaries); err != nil {
		return nil, "", fmt.Errorf("parsing firmware/get_binaries: %w", err)
	}
	if len(binaries) == 0 {
		return nil, "", fmt.Errorf("no firmware artifacts found — compile first")
	}
	want := "ota"
	if factory {
		want = "factory"
	}
	chosen := pickBinary(binaries, want)
	if chosen == nil {
		return nil, "", fmt.Errorf("no %q firmware artifact available (have: %s)", want, binaryTypes(binaries))
	}

	tokRaw, err := s.command("firmware/download_token", map[string]any{
		"configuration": configuration,
		"file":          chosen.File,
	})
	if err != nil {
		return nil, "", err
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil || tok.Token == "" {
		return nil, "", fmt.Errorf("parsing firmware/download_token: %w", err)
	}

	data, err := c.httpDownload(ctx, tok.Token)
	if err != nil {
		return nil, "", err
	}
	return data, chosen.File, nil
}

func pickBinary(binaries []Binary, want string) *Binary {
	for i := range binaries {
		if binaries[i].Type == want {
			return &binaries[i]
		}
	}
	// Fall back to the first non-elf artifact if the exact type is absent.
	for i := range binaries {
		if binaries[i].Type != "elf" {
			return &binaries[i]
		}
	}
	return nil
}

func binaryTypes(binaries []Binary) string {
	var parts []string
	for _, b := range binaries {
		t := b.Type
		if t == "" {
			t = b.File
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, ", ")
}

// httpDownload fetches a firmware artifact over HTTP using a single-use token.
func (c *Client) httpDownload(ctx context.Context, token string) ([]byte, error) {
	u := c.baseURL + "/api/firmware/download?" + url.Values{"token": {token}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("download failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBinarySize))
	if err != nil {
		return nil, fmt.Errorf("reading firmware: %w", err)
	}
	return data, nil
}

// --- Legacy spawn-protocol commands (compile, upload) ---

// CommandResult captures the outcome of a streamed command.
type CommandResult struct {
	// Output is the captured stdout (possibly tail-truncated).
	Output string
	// ExitCode is the process exit code; nil if the stream ended without one.
	ExitCode *int
	// Truncated is true if Output was capped at maxCommandOutput.
	Truncated bool
	// TimedOut is true if we stopped before the process exited.
	TimedOut bool
}

// Compile builds the firmware for a device without flashing.
func (c *Client) Compile(ctx context.Context, configuration string) (*CommandResult, error) {
	return c.spawn(ctx, "/compile", configuration, nil)
}

// Upload compiles and flashes a device. port is the upload target: "OTA"
// (default) for an over-the-air flash, or a serial path on the dashboard host.
func (c *Client) Upload(ctx context.Context, configuration, port string) (*CommandResult, error) {
	if port == "" {
		port = "OTA"
	}
	return c.spawn(ctx, "/upload", configuration, map[string]any{"port": port})
}

// spawnEvent is one message from a legacy spawn-protocol WebSocket: per-line
// {"event":"line","data":"..."} then {"event":"exit","code":N}.
type spawnEvent struct {
	Event string `json:"event"`
	Data  string `json:"data"`
	Code  int    `json:"code"`
}

func (c *Client) spawn(ctx context.Context, path, configuration string, extra map[string]any) (*CommandResult, error) {
	wsURL, err := c.wsURL(path)
	if err != nil {
		return nil, err
	}
	header := http.Header{}
	if c.password != "" {
		// Legacy REST/WS endpoints accept HTTP Basic auth.
		req, _ := http.NewRequest(http.MethodGet, c.baseURL, nil)
		req.SetBasicAuth("admin", c.password)
		header.Set("Authorization", req.Header.Get("Authorization"))
	}

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL, header)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dialing %s (status %d): %w", path, resp.StatusCode, err)
		}
		return nil, fmt.Errorf("dialing %s: %w", path, err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stop()

	spawn := map[string]any{"type": "spawn", "configuration": configuration}
	for k, v := range extra {
		spawn[k] = v
	}
	if err := conn.WriteJSON(spawn); err != nil {
		return nil, fmt.Errorf("sending spawn command: %w", err)
	}

	var b strings.Builder
	result := &CommandResult{}
	for {
		if ctx.Err() != nil {
			result.TimedOut = true
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(wsIdleTimeout))
		var ev spawnEvent
		if err := conn.ReadJSON(&ev); err != nil {
			if ctx.Err() != nil {
				result.TimedOut = true
			}
			break
		}
		switch ev.Event {
		case "line":
			if b.Len() < maxCommandOutput {
				b.WriteString(ev.Data)
			} else {
				result.Truncated = true
			}
		case "exit":
			code := ev.Code
			result.ExitCode = &code
			result.Output = b.String()
			return result, nil
		}
	}
	result.Output = b.String()
	return result, nil
}
