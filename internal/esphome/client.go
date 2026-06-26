// Package esphome provides MCP tools for driving an ESPHome dashboard
// (typically the Home Assistant ESPHome add-on): listing devices, reading and
// writing configuration files, and running validate/compile/upload/logs
// commands over the dashboard's WebSocket command channels.
package esphome

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// maxHTTPResponseSize caps plain HTTP responses (device list, file reads).
	maxHTTPResponseSize = 8 * 1024 * 1024
	// maxBinarySize caps a downloaded firmware image (16 MB covers ESP32 flash).
	maxBinarySize = 16 * 1024 * 1024
	// maxCommandOutput caps captured stdout from a streamed command.
	maxCommandOutput = 512 * 1024
	// wsIdleTimeout bounds how long we wait between lines before giving up.
	// Compiles can be quiet for a while as toolchains churn, so it is generous.
	wsIdleTimeout = 10 * time.Minute
)

// Client talks to an ESPHome dashboard over HTTP and WebSocket.
type Client struct {
	baseURL    string
	password   string
	httpClient *http.Client

	loginOnce sync.Once
	loginErr  error
}

// NewClient creates an ESPHome dashboard client. baseURL points at the
// dashboard root (e.g. http://homeassistant.local:6052). password is optional
// and only used if the dashboard has authentication enabled.
func NewClient(baseURL, password string) (*Client, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("esphome URL is required")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("creating cookie jar: %w", err)
	}
	return &Client{
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		password: password,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
			Jar:     jar,
		},
	}, nil
}

// ensureAuth logs in once if a password is configured. The resulting session
// cookie is stored in the client's jar and reused for HTTP and WebSocket calls.
func (c *Client) ensureAuth(ctx context.Context) error {
	if c.password == "" {
		return nil
	}
	c.loginOnce.Do(func() {
		form := url.Values{}
		form.Set("username", "admin")
		form.Set("password", c.password)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.baseURL+"/login", strings.NewReader(form.Encode()))
		if err != nil {
			c.loginErr = fmt.Errorf("building login request: %w", err)
			return
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := c.httpClient.Do(req)
		if err != nil {
			c.loginErr = fmt.Errorf("login request failed: %w", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxHTTPResponseSize))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			c.loginErr = fmt.Errorf("login failed (status %d)", resp.StatusCode)
		}
	})
	return c.loginErr
}

func (c *Client) doRequest(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	if err := c.ensureAuth(ctx); err != nil {
		return nil, err
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	return resp, nil
}

// ListDevices returns the dashboard's configured devices. Each entry is the
// raw object the dashboard reports (name, configuration, address, online,
// current/deployed version, target platform, ...), passed through as-is so
// fields stay available across dashboard versions.
func (c *Client) ListDevices(ctx context.Context) ([]map[string]any, error) {
	resp, err := c.doRequest(ctx, http.MethodGet, "/devices", nil, nil, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Configured []map[string]any `json:"configured"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parsing devices response: %w", err)
	}
	return payload.Configured, nil
}

// SecretKeys returns the names (not values) of keys defined in the dashboard's
// shared secrets.yaml.
func (c *Client) SecretKeys(ctx context.Context) ([]string, error) {
	resp, err := c.doRequest(ctx, http.MethodGet, "/secret_keys", nil, nil, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	var keys []string
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, fmt.Errorf("parsing secret keys response: %w", err)
	}
	return keys, nil
}

// ReadFile returns the contents of a file in the dashboard config directory
// (e.g. "pump.yaml", "pentair.h", "secrets.yaml").
func (c *Client) ReadFile(ctx context.Context, name string) (string, error) {
	q := url.Values{}
	q.Set("configuration", name)
	resp, err := c.doRequest(ctx, http.MethodGet, "/edit", q, nil, "")
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := readBody(resp)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// WriteFile writes content to a file in the dashboard config directory,
// creating or overwriting it.
func (c *Client) WriteFile(ctx context.Context, name, content string) error {
	q := url.Values{}
	q.Set("configuration", name)
	resp, err := c.doRequest(ctx, http.MethodPost, "/edit", q,
		strings.NewReader(content), "application/x-www-form-urlencoded")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := readBody(resp); err != nil {
		return err
	}
	return nil
}

// DownloadBinary fetches a compiled firmware image. fileType selects the image
// variant ("firmware.bin" for OTA, "firmware-factory.bin" for a first USB
// flash); empty means the dashboard default.
func (c *Client) DownloadBinary(ctx context.Context, name, fileType string) ([]byte, error) {
	q := url.Values{}
	q.Set("configuration", name)
	if fileType != "" {
		q.Set("file", fileType)
	}
	resp, err := c.doRequest(ctx, http.MethodGet, "/download.bin", q, nil, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxHTTPResponseSize))
		return nil, fmt.Errorf("download failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBinarySize))
	if err != nil {
		return nil, fmt.Errorf("reading binary: %w", err)
	}
	return data, nil
}

// CommandResult captures the outcome of a streamed dashboard command.
type CommandResult struct {
	// Output is the captured stdout (possibly tail-truncated).
	Output string
	// ExitCode is the process exit code; nil if the stream ended without one
	// (e.g. a logs stream we stopped on timeout).
	ExitCode *int
	// Truncated is true if Output was capped at maxCommandOutput.
	Truncated bool
	// TimedOut is true if we stopped before the process exited.
	TimedOut bool
}

// Validate runs `esphome config` (config validation) for the named config.
func (c *Client) Validate(ctx context.Context, config string) (*CommandResult, error) {
	return c.runCommand(ctx, "validate", config, nil)
}

// Compile builds the firmware for the named config without flashing.
func (c *Client) Compile(ctx context.Context, config string) (*CommandResult, error) {
	return c.runCommand(ctx, "compile", config, nil)
}

// Upload compiles and flashes the named config. port is the upload target:
// "OTA" for an over-the-air flash (default), or a serial device path if the
// dashboard host has the board attached.
func (c *Client) Upload(ctx context.Context, config, port string) (*CommandResult, error) {
	if port == "" {
		port = "OTA"
	}
	return c.runCommand(ctx, "upload", config, map[string]string{"port": port})
}

// Logs streams device logs for the named config until ctx is cancelled (the
// caller should pass a bounded context) and returns what was captured. port
// selects the source ("OTA" for a network device, default).
func (c *Client) Logs(ctx context.Context, config, port string) (*CommandResult, error) {
	if port == "" {
		port = "OTA"
	}
	return c.runCommand(ctx, "logs", config, map[string]string{"port": port})
}

// dashboardEvent is one message from a command WebSocket. The dashboard emits
// {"event":"line","data":"..."} per output line and {"event":"exit","code":N}
// when the process exits.
type dashboardEvent struct {
	Event string `json:"event"`
	Data  string `json:"data"`
	Code  int    `json:"code"`
}

func (c *Client) runCommand(ctx context.Context, command, config string, extra map[string]string) (*CommandResult, error) {
	if err := c.ensureAuth(ctx); err != nil {
		return nil, err
	}

	wsURL, err := c.wsURL(command)
	if err != nil {
		return nil, err
	}

	header := http.Header{}
	if u, err := url.Parse(c.baseURL); err == nil && c.httpClient.Jar != nil {
		if cookies := c.httpClient.Jar.Cookies(u); len(cookies) > 0 {
			var parts []string
			for _, ck := range cookies {
				parts = append(parts, ck.Name+"="+ck.Value)
			}
			header.Set("Cookie", strings.Join(parts, "; "))
		}
	}

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, wsURL, header)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dialing %s command (status %d): %w", command, resp.StatusCode, err)
		}
		return nil, fmt.Errorf("dialing %s command: %w", command, err)
	}
	defer func() { _ = conn.Close() }()

	// Unblock a pending read when the caller's context is cancelled.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stop()

	spawn := map[string]any{"type": "spawn", "configuration": config}
	for k, v := range extra {
		spawn[k] = v
	}
	if err := conn.WriteJSON(spawn); err != nil {
		return nil, fmt.Errorf("sending spawn command: %w", err)
	}

	var buf bytes.Buffer
	result := &CommandResult{}
	for {
		if ctx.Err() != nil {
			result.TimedOut = true
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(wsIdleTimeout))
		var ev dashboardEvent
		if err := conn.ReadJSON(&ev); err != nil {
			if ctx.Err() != nil {
				result.TimedOut = true
				break
			}
			// A normal close, or any read error, ends the stream; return what
			// we captured so far rather than discarding a useful partial log.
			break
		}
		switch ev.Event {
		case "line":
			if buf.Len() < maxCommandOutput {
				buf.WriteString(ev.Data)
			} else {
				result.Truncated = true
			}
		case "exit":
			code := ev.Code
			result.ExitCode = &code
			result.Output = buf.String()
			return result, nil
		}
	}

	result.Output = buf.String()
	return result, nil
}

func (c *Client) wsURL(command string) (string, error) {
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
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + command
	return u.String(), nil
}

func readBody(resp *http.Response) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPResponseSize))
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("dashboard error (status %d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}
