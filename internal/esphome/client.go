// Package esphome provides MCP tools for driving an ESPHome dashboard (the
// "ESPHome Device Builder", typically the Home Assistant add-on): listing
// devices, reading and writing device configs, managing secrets, validating,
// compiling, uploading firmware, streaming logs, and verifying a flash.
//
// The dashboard exposes a single multiplexed WebSocket at /ws using a
// {command, message_id, args} -> ResultMessage / EventMessage / ErrorMessage
// protocol (esphome/device-builder, api/ws.py). Every operation here opens one
// /ws session, issues its command(s) and closes it.
package esphome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// maxBinarySize caps a downloaded firmware image (16 MB covers ESP32 flash).
	maxBinarySize = 16 * 1024 * 1024
	// downloadTimeout bounds the HTTP firmware fetch.
	downloadTimeout = 60 * time.Second
)

// Protocol limits. Variables only so tests can shrink them.
var (
	// maxMessageBytes caps one WebSocket frame. The largest legitimate frame
	// is a running job's in-RAM output (up to ~4000 lines) in get_job /
	// get_jobs; anything far beyond that is a misbehaving peer.
	maxMessageBytes int64 = 32 * 1024 * 1024
	// handshakeTimeout bounds the HTTP upgrade, helloTimeout the server-info
	// frame that must follow it.
	handshakeTimeout = 15 * time.Second
	helloTimeout     = 15 * time.Second
	// commandTimeout bounds a request/response command. These are all quick
	// on the dashboard side (file reads, queue inserts, registry lookups).
	commandTimeout = 45 * time.Second
	// writeTimeout bounds sending one frame.
	writeTimeout = 15 * time.Second
	// validateIdle bounds silence during a validate stream. The dashboard
	// kills a validate that is silent for 60 s and reports it; this is the
	// backstop if that report never arrives.
	validateIdle = 2 * time.Minute
	// followIdle bounds silence while replaying a finished job's log.
	followIdle = 30 * time.Second
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
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("invalid esphome URL: %w", err)
	}
	return &Client{
		baseURL:  strings.TrimSuffix(baseURL, "/"),
		password: password,
	}, nil
}

// --- errors ---

// Transport failure kinds. A tool turns each into a next action.
const (
	kindUnreachable = "dashboard_unreachable"
	kindStalled     = "dashboard_stalled"
	kindLost        = "connection_lost"
	kindProtocol    = "protocol_error"
	kindAuth        = "auth_failed"
)

// transportError is a failure talking to the dashboard, as opposed to the
// dashboard answering with an error (commandError).
type transportError struct {
	kind string
	msg  string
	err  error
}

func (e *transportError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("%s: %v", e.msg, e.err)
	}
	return e.msg
}

func (e *transportError) Unwrap() error { return e.err }

// commandError carries the dashboard's structured error code (api.ErrorCode
// upstream: invalid_message, unknown_command, invalid_args, not_found,
// already_exists, internal_error, not_authenticated, rate_limited,
// unavailable, precondition_failed, ...).
type commandError struct {
	command string
	code    string
	details string
}

func (e *commandError) Error() string {
	prefix := e.code
	if e.command != "" {
		prefix = e.command + ": " + e.code
	}
	if e.details != "" {
		return fmt.Sprintf("%s: %s", prefix, e.details)
	}
	return prefix
}

func errorCode(err error) string {
	var ce *commandError
	if errors.As(err, &ce) {
		return ce.code
	}
	return ""
}

func isNotFound(err error) bool { return errorCode(err) == "not_found" }

// ErrJobNotFound is returned when the dashboard has no record of a job id.
// firmware/get_job answers an unknown id with a null result, not an error.
var ErrJobNotFound = errors.New("job not found")

// --- WebSocket command session (/ws) ---

// session is a single /ws connection scoped to one logical operation: dial,
// (optionally) authenticate, issue one or more commands, then close.
type session struct {
	conn  *websocket.Conn
	ctx   context.Context
	stop  func() bool
	idSeq int
	info  wsMessage
}

// wsMessage is the union of every server->client message shape on /ws. The
// field that is set distinguishes the kind: server info (server_version, no
// message_id), result (message_id + no event/error), event (message_id +
// event), or error (error_code; message_id is empty when the dashboard could
// not parse the command at all).
type wsMessage struct {
	MessageID      string          `json:"message_id"`
	Result         json.RawMessage `json:"result"`
	Event          string          `json:"event"`
	Data           json.RawMessage `json:"data"`
	ErrorCode      string          `json:"error_code"`
	Details        string          `json:"details"`
	ServerVersion  string          `json:"server_version"`
	ESPHomeVersion string          `json:"esphome_version"`
	RequiresAuth   bool            `json:"requires_auth"`
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
	dialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: handshakeTimeout,
	}
	conn, resp, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		msg := "cannot reach the ESPHome dashboard at " + c.baseURL
		if resp != nil {
			msg = fmt.Sprintf("%s (HTTP %d on the /ws upgrade)", msg, resp.StatusCode)
		}
		return nil, &transportError{kind: kindUnreachable, msg: msg, err: err}
	}
	conn.SetReadLimit(maxMessageBytes)
	s := &session{conn: conn, ctx: ctx}
	// Closing the socket is what unblocks a pending read or write when the
	// caller gives up. A read deadline would not do: read() re-arms the
	// deadline on every call and could overwrite one set at cancellation.
	s.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })

	info, err := s.read(helloTimeout)
	if err != nil {
		s.close()
		return nil, fmt.Errorf("waiting for the dashboard's server info: %w", err)
	}
	if info.ServerVersion == "" {
		s.close()
		return nil, &transportError{kind: kindProtocol, msg: "the dashboard's first message was not server info; is ESPHOME_URL pointing at the ESPHome Device Builder?"}
	}
	s.info = *info
	if info.RequiresAuth {
		if c.password == "" {
			s.close()
			return nil, &transportError{kind: kindAuth, msg: "dashboard requires authentication but no ESPHOME_PASSWORD is set"}
		}
		if _, err := s.command("auth/login", map[string]any{
			"username": "admin",
			"password": c.password,
		}); err != nil {
			s.close()
			return nil, &transportError{kind: kindAuth, msg: "dashboard authentication failed (check ESPHOME_PASSWORD)", err: err}
		}
	}
	return s, nil
}

func (s *session) close() {
	s.stop()
	_ = s.conn.Close()
}

// read returns the next message, waiting at most idle (0 = no limit beyond
// the context).
func (s *session) read(idle time.Duration) (*wsMessage, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	var deadline time.Time
	if idle > 0 {
		deadline = time.Now().Add(idle)
	}
	if err := s.conn.SetReadDeadline(deadline); err != nil {
		return nil, s.readErr(err, idle)
	}
	_, data, err := s.conn.ReadMessage()
	if err != nil {
		return nil, s.readErr(err, idle)
	}
	var msg wsMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, &transportError{kind: kindProtocol, msg: "the dashboard sent a malformed message", err: err}
	}
	return &msg, nil
}

func (s *session) readErr(err error, idle time.Duration) error {
	if cerr := s.ctx.Err(); cerr != nil {
		return cerr
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &transportError{kind: kindStalled, msg: fmt.Sprintf("the dashboard sent nothing for %s", idle)}
	}
	if errors.Is(err, websocket.ErrReadLimit) {
		return &transportError{kind: kindProtocol, msg: fmt.Sprintf("the dashboard sent a message larger than %d bytes", maxMessageBytes), err: err}
	}
	return &transportError{kind: kindLost, msg: "the connection to the dashboard dropped", err: err}
}

func (s *session) send(cmd string, args map[string]any) (string, error) {
	s.idSeq++
	id := fmt.Sprintf("%d", s.idSeq)
	if args == nil {
		args = map[string]any{}
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := s.conn.WriteJSON(map[string]any{
		"command":    cmd,
		"message_id": id,
		"args":       args,
	}); err != nil {
		if cerr := s.ctx.Err(); cerr != nil {
			return "", cerr
		}
		return "", &transportError{kind: kindLost, msg: "sending " + cmd, err: err}
	}
	return id, nil
}

// unparsedError reports an error frame the dashboard sends with an empty
// message_id: it could not parse a command. Only this session talks on the
// socket, so it is ours, and waiting for a reply would hang.
func unparsedError(cmd string, msg *wsMessage) error {
	return &commandError{command: cmd, code: msg.ErrorCode, details: msg.Details}
}

// command sends a request and returns its result payload, ignoring unrelated
// messages. Use for non-streaming commands.
func (s *session) command(cmd string, args map[string]any) (json.RawMessage, error) {
	id, err := s.send(cmd, args)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(commandTimeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, &transportError{kind: kindStalled, msg: fmt.Sprintf("the dashboard did not answer %s within %s", cmd, commandTimeout)}
		}
		msg, err := s.read(remaining)
		if err != nil {
			var te *transportError
			if errors.As(err, &te) && te.kind == kindStalled {
				return nil, &transportError{kind: kindStalled, msg: fmt.Sprintf("the dashboard did not answer %s within %s", cmd, commandTimeout)}
			}
			return nil, err
		}
		if msg.MessageID == "" && msg.ErrorCode != "" {
			return nil, unparsedError(cmd, msg)
		}
		if msg.MessageID != id {
			continue // unrelated push/event or another command's reply
		}
		if msg.ErrorCode != "" {
			return nil, &commandError{command: cmd, code: msg.ErrorCode, details: msg.Details}
		}
		switch msg.Event {
		case "":
			return msg.Result, nil
		case "result":
			return msg.Data, nil
		}
		// Streaming output for a command treated as non-streaming; skip
		// until its result.
	}
}

// CommandResult captures the outcome of a streamed command (validate, logs,
// follow_job).
type CommandResult struct {
	// Lines are the cleaned output lines (ANSI stripped, concealed values
	// redacted, progress redraws collapsed), the last maxLines of them.
	Lines []string
	// Dropped counts earlier lines that were not retained.
	Dropped int
	// Errors are the first error-looking lines over the whole stream.
	Errors []string
	// MoreErrors counts error lines beyond those kept in Errors.
	MoreErrors int
	// Final is true when the dashboard sent the stream's terminal result.
	Final bool
	// ExitCode is the process exit code from the terminal result, if any.
	ExitCode *int
	// Status, Error and QueuedUpdateArmed come from follow_job's terminal
	// frame ({status, exit_code, error, queued_update_armed}).
	Status            string
	Error             string
	QueuedUpdateArmed bool
	// TimedOut is true when the caller's deadline ended the stream (the
	// expected way a bounded log capture ends).
	TimedOut bool
	// Stopped is true when the caller's stop condition ended the stream.
	Stopped bool
	// Incomplete, when non-empty, says why the stream ended without a
	// terminal result for a reason other than TimedOut/Stopped.
	Incomplete string
}

// Output returns the retained lines joined by newlines.
func (r *CommandResult) Output() string { return strings.Join(r.Lines, "\n") }

// Succeeded reports whether the stream reached a terminal result with exit 0.
func (r *CommandResult) Succeeded() bool {
	return r.Final && r.ExitCode != nil && *r.ExitCode == 0
}

// streamOpts tunes one streamed command.
type streamOpts struct {
	// idle bounds silence between messages (0 = only the context bounds it).
	idle time.Duration
	// observe sees each settled cleaned line.
	observe func(string)
	// until is checked after each output event; true ends the stream early.
	until func() bool
}

// stream sends a streaming command and collects its output until the
// terminal result, the context ends, the stop condition holds, or the
// connection fails. It returns the partial result alongside a dashboard error
// so callers can show what ran before the failure.
func (s *session) stream(cmd string, args map[string]any, o streamOpts) (*CommandResult, error) {
	id, err := s.send(cmd, args)
	if err != nil {
		return nil, err
	}
	res := &CommandResult{}
	errs := &errorCollector{}
	buf := &lineBuffer{observe: func(l string) {
		errs.observe(l)
		if o.observe != nil {
			o.observe(l)
		}
	}}
	finish := func() *CommandResult {
		res.Lines, res.Dropped = buf.tail()
		res.Errors, res.MoreErrors = errs.lines, errs.more
		return res
	}
	for {
		msg, err := s.read(o.idle)
		if err != nil {
			cerr := s.ctx.Err()
			switch {
			case errors.Is(cerr, context.DeadlineExceeded):
				res.TimedOut = true
				return finish(), nil
			case cerr != nil:
				return finish(), cerr
			}
			var te *transportError
			if errors.As(err, &te) && te.kind == kindStalled {
				res.Incomplete = fmt.Sprintf("the dashboard sent nothing for %s and the stream was abandoned", o.idle)
			} else {
				res.Incomplete = err.Error()
			}
			return finish(), nil
		}
		if msg.MessageID == "" && msg.ErrorCode != "" {
			return finish(), unparsedError(cmd, msg)
		}
		if msg.MessageID != id {
			continue
		}
		if msg.ErrorCode != "" {
			return finish(), &commandError{command: cmd, code: msg.ErrorCode, details: msg.Details}
		}
		switch msg.Event {
		case "output":
			var line string
			if json.Unmarshal(msg.Data, &line) != nil {
				// Not a string: keep the raw payload rather than drop it.
				line = string(msg.Data)
			}
			buf.add(line)
			if o.until != nil && o.until() {
				res.Stopped = true
				return finish(), nil
			}
		case "result":
			if err := res.applyTerminal(msg.Data); err != nil {
				res.Incomplete = "the dashboard's final result was unreadable: " + err.Error()
			}
			return finish(), nil
		case "":
			// The handler returned without a terminal frame. Upstream does
			// this when a run is cancelled on the dashboard side (stop_stream
			// or shutdown); the generic dispatcher then sends a bare result.
			// It is not a success.
			res.Incomplete = "the dashboard ended the stream without a final result (the run was stopped on the dashboard side)"
			return finish(), nil
		default:
			// Unknown frame type (snapshot, or a newer protocol's events).
		}
	}
}

func (r *CommandResult) applyTerminal(data json.RawMessage) error {
	var term struct {
		Success           *bool   `json:"success"`
		Code              *int    `json:"code"`
		Status            string  `json:"status"`
		ExitCode          *int    `json:"exit_code"`
		Error             *string `json:"error"`
		QueuedUpdateArmed bool    `json:"queued_update_armed"`
	}
	if len(data) > 0 && string(data) != "null" {
		if err := json.Unmarshal(data, &term); err != nil {
			return err
		}
	}
	r.Final = true
	code := term.Code
	if code == nil {
		code = term.ExitCode
	}
	if code == nil && term.Success != nil {
		c := 0
		if !*term.Success {
			c = 1
		}
		code = &c
	}
	r.ExitCode = code
	r.Status = term.Status
	if term.Error != nil {
		r.Error = *term.Error
	}
	r.QueuedUpdateArmed = term.QueuedUpdateArmed
	return nil
}

// --- High-level operations ---

// ListDevices returns the dashboard's configured devices as raw objects.
func (c *Client) ListDevices(ctx context.Context) ([]map[string]any, error) {
	s, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	raw, err := s.command("devices/list", nil)
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

// FindDevice returns the device record whose configuration file is
// configuration, or nil if the dashboard has none.
func (c *Client) FindDevice(ctx context.Context, configuration string) (map[string]any, error) {
	devices, err := c.ListDevices(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range devices {
		if d["configuration"] == configuration {
			return d, nil
		}
	}
	return nil, nil
}

// SecretKeys returns the names (not values) of keys in the shared secrets.yaml.
func (c *Client) SecretKeys(ctx context.Context) ([]string, error) {
	s, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	raw, err := s.command("config/get_secrets", nil)
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
//
// expected, when non-nil, is the text the caller last read: the dashboard
// refuses the write with precondition_failed if the file changed since, so a
// concurrent edit (the user in the dashboard editor) is never overwritten
// unseen.
func (c *Client) WriteConfig(ctx context.Context, configuration, content string, expected *string) (created bool, err error) {
	s, err := c.dial(ctx)
	if err != nil {
		return false, err
	}
	defer s.close()

	args := map[string]any{
		"configuration": configuration,
		"content":       content,
	}
	if expected != nil {
		args["expected"] = *expected
	}
	_, err = s.command("devices/update_config", args)
	if err == nil {
		return false, nil
	}
	if !isNotFound(err) {
		return false, err
	}
	// Device doesn't exist (any more) — create it. The name only sets the
	// filename; the YAML is written as-is, so esphome.name from the content
	// is preserved.
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
	// show_secrets defaults to false upstream; pinned so a default change
	// there cannot start streaming resolved secrets into a model's context.
	return s.stream("devices/validate", map[string]any{
		"configuration": configuration,
		"show_secrets":  false,
	}, streamOpts{idle: validateIdle})
}

// LogOptions tunes a log capture.
type LogOptions struct {
	// Port selects the source: "OTA" (network, default) or a serial path.
	Port string
	// Observe sees each settled cleaned line.
	Observe func(string)
	// Until ends the capture early when it returns true.
	Until func() bool
}

// Logs streams device logs until ctx ends (the caller should pass a bounded
// context) or opts.Until holds, and returns what was captured.
func (c *Client) Logs(ctx context.Context, configuration string, opts LogOptions) (*CommandResult, error) {
	port := opts.Port
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
	}, streamOpts{observe: opts.Observe, until: opts.Until})
}

// Binary describes a downloadable firmware artifact.
type Binary struct {
	Title string `json:"title"`
	File  string `json:"file"`
	Type  string `json:"type"`
}

// Binaries lists the firmware artifacts on disk for a device. Empty means
// nothing has been built (or the build directory was cleaned).
func (c *Client) Binaries(ctx context.Context, configuration string) ([]Binary, error) {
	s, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	return getBinaries(s, configuration)
}

func getBinaries(s *session, configuration string) ([]Binary, error) {
	raw, err := s.command("firmware/get_binaries", map[string]any{"configuration": configuration})
	if err != nil {
		return nil, err
	}
	var binaries []Binary
	if err := json.Unmarshal(raw, &binaries); err != nil {
		return nil, fmt.Errorf("parsing firmware/get_binaries: %w", err)
	}
	return binaries, nil
}

// DownloadBinary fetches a compiled firmware image. When factory is true it
// selects the full-flash image (for a first USB flash); otherwise the OTA
// image. Returns the bytes and the chosen artifact.
func (c *Client) DownloadBinary(ctx context.Context, configuration string, factory bool) ([]byte, *Binary, error) {
	s, err := c.dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer s.close()

	binaries, err := getBinaries(s, configuration)
	if err != nil {
		return nil, nil, err
	}
	if len(binaries) == 0 {
		return nil, nil, fmt.Errorf("no firmware artifacts found for %s — compile it first (compile_esphome)", configuration)
	}
	want := "ota"
	if factory {
		want = "factory"
	}
	chosen := pickBinary(binaries, want)
	if chosen == nil {
		return nil, nil, fmt.Errorf("no %q firmware artifact available (have: %s)", want, binaryTypes(binaries))
	}

	tokRaw, err := s.command("firmware/download_token", map[string]any{
		"configuration": configuration,
		"file":          chosen.File,
	})
	if err != nil {
		return nil, nil, err
	}
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(tokRaw, &tok); err != nil {
		return nil, nil, fmt.Errorf("parsing firmware/download_token: %w", err)
	}
	if tok.Token == "" {
		return nil, nil, fmt.Errorf("firmware/download_token returned no token")
	}

	data, err := c.httpDownload(ctx, tok.Token)
	if err != nil {
		return nil, nil, err
	}
	return data, chosen, nil
}

func pickBinary(binaries []Binary, want string) *Binary {
	for i := range binaries {
		if binaries[i].Type == want {
			return &binaries[i]
		}
	}
	// Fall back to the first flashable artifact if the exact type is absent
	// (an ESP8266 build has a plain firmware.bin and no separate OTA image).
	for i := range binaries {
		if binaries[i].Type != "elf" && !strings.HasSuffix(binaries[i].File, ".elf") {
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
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBinarySize+1))
	if err != nil {
		return nil, fmt.Errorf("reading firmware: %w", err)
	}
	if len(data) > maxBinarySize {
		return nil, fmt.Errorf("firmware image is larger than %d bytes; refusing to hash a truncated copy", maxBinarySize)
	}
	return data, nil
}

// --- Async firmware jobs (compile, upload) ---
//
// Compile and upload can each run for minutes — far longer than an MCP request
// may stay open. They use the dashboard's job queue: enqueue returns a job
// immediately, and the caller polls GetJob until the job reaches a terminal
// state. Jobs are persisted by the dashboard: a job queued or running when it
// restarts is re-queued under the same id (a running one starts over, with a
// marker line in its log).
//
// Submitting any firmware job for a device cancels ("supersedes") every other
// queued or running job for the same configuration. The tools guard against
// the harmful cases of that before submitting.

// Job is the subset of the dashboard's FirmwareJob we surface.
type Job struct {
	JobID             string   `json:"job_id"`
	Configuration     string   `json:"configuration"`
	JobType           string   `json:"job_type"`
	Status            string   `json:"status"`
	CreatedAt         string   `json:"created_at"`
	StartedAt         string   `json:"started_at"`
	CompletedAt       string   `json:"completed_at"`
	ExitCode          *int     `json:"exit_code"`
	Output            []string `json:"output"`
	Error             string   `json:"error"`
	Port              string   `json:"port"`
	Progress          *int     `json:"progress"`
	DependsOn         string   `json:"depends_on"`
	FailureReason     string   `json:"failure_reason"`
	IsDeferredInstall bool     `json:"is_deferred_install"`
}

// Terminal reports whether the job has finished (success, failure, or cancel).
func (j *Job) Terminal() bool {
	switch j.Status {
	case "completed", "failed", "cancelled":
		return true
	}
	return false
}

// Active reports whether the job is queued or running.
func (j *Job) Active() bool { return j.Status == "queued" || j.Status == "running" }

// Succeeded reports whether the job finished cleanly.
func (j *Job) Succeeded() bool {
	return j.Status == "completed" && (j.ExitCode == nil || *j.ExitCode == 0)
}

// recoveryMarker is the line the dashboard appends to a running job's log
// when it restarts mid-job and re-runs it (FirmwareJob._RECOVERY_NOTICE).
const recoveryMarker = "dashboard restarted mid-build"

// Restarted reports whether the job's in-memory log shows the dashboard
// restarted while it ran. Only meaningful for active jobs: get_job omits
// output for terminal ones.
func (j *Job) Restarted() bool {
	for _, l := range j.Output {
		if strings.Contains(l, recoveryMarker) {
			return true
		}
	}
	return false
}

// Compile queues a build for a device and returns the queued job immediately.
// Poll GetJob with the returned JobID until it is Terminal.
func (c *Client) Compile(ctx context.Context, configuration string) (*Job, error) {
	return c.enqueue(ctx, "firmware/compile", map[string]any{"configuration": configuration})
}

// Upload queues a flash of a device's already-compiled binary and returns the
// queued job immediately. port is the target: "OTA" (default) or a serial path.
// Poll GetJob with the returned JobID until it is Terminal.
func (c *Client) Upload(ctx context.Context, configuration, port string) (*Job, error) {
	if port == "" {
		port = "OTA"
	}
	return c.enqueue(ctx, "firmware/upload", map[string]any{
		"configuration": configuration,
		"port":          port,
	})
}

func (c *Client) enqueue(ctx context.Context, cmd string, args map[string]any) (*Job, error) {
	s, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	raw, err := s.command(cmd, args)
	if err != nil {
		return nil, err
	}
	job, err := parseJob(raw, cmd)
	if err != nil {
		return nil, err
	}
	if job == nil || job.JobID == "" {
		return nil, &transportError{kind: kindProtocol, msg: cmd + " returned no job"}
	}
	return job, nil
}

// GetJob returns the current state of a firmware job by ID, or ErrJobNotFound.
// For terminal jobs the dashboard returns an empty Output here — the full
// build/flash log lives in a sidecar and is retrieved via JobLog.
func (c *Client) GetJob(ctx context.Context, jobID string) (*Job, error) {
	s, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	raw, err := s.command("firmware/get_job", map[string]any{"job_id": jobID})
	if err != nil {
		return nil, err
	}
	job, err := parseJob(raw, "firmware/get_job")
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, ErrJobNotFound
	}
	return job, nil
}

// GetJobs lists the dashboard's retained jobs for one configuration, newest
// first.
func (c *Client) GetJobs(ctx context.Context, configuration string) ([]Job, error) {
	s, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	raw, err := s.command("firmware/get_jobs", map[string]any{"configuration": configuration})
	if err != nil {
		return nil, err
	}
	var jobs []Job
	if err := json.Unmarshal(raw, &jobs); err != nil {
		return nil, fmt.Errorf("parsing firmware/get_jobs: %w", err)
	}
	return jobs, nil
}

// CancelJob cancels a queued or running job. A queued job is cancelled at
// once; a running one is signalled and may still finish.
func (c *Client) CancelJob(ctx context.Context, jobID string) error {
	s, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer s.close()
	_, err = s.command("firmware/cancel", map[string]any{"job_id": jobID})
	return err
}

// JobLog fetches a job's output log and terminal result. The dashboard omits
// output from get_job for terminal jobs (it's flushed to a per-job sidecar),
// so the log is retrieved by following the job: firmware/follow_job replays
// the log as `output` events then a terminal `result` event carrying
// {status, exit_code, error, queued_update_armed}. Call it for a terminal
// job; for a running one the stream tails live until the job ends.
func (c *Client) JobLog(ctx context.Context, jobID string) (*CommandResult, error) {
	s, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer s.close()
	return s.stream("firmware/follow_job", map[string]any{"job_id": jobID}, streamOpts{idle: followIdle})
}

func parseJob(raw json.RawMessage, cmd string) (*Job, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var job Job
	if err := json.Unmarshal(raw, &job); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", cmd, err)
	}
	return &job, nil
}
