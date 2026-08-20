package esphome

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
	"github.com/nabkey/mcp-home/internal/validate"
)

const (
	// defaultLogSeconds bounds a logs stream when the caller omits a duration.
	defaultLogSeconds = 30
	// maxLogSeconds caps how long a logs stream may run.
	maxLogSeconds = 120
	// displayTail is how much trailing command output to surface inline.
	displayTail = 8000
	// jobLogTimeout bounds the follow_job log fetch for a terminal job.
	jobLogTimeout = 90 * time.Second
)

// Tools holds ESPHome dashboard tools.
type Tools struct {
	client *Client
}

// NewTools creates ESPHome tools targeting the dashboard at url. password is
// optional (only needed if the dashboard has auth enabled).
func NewTools(url, password string) (*Tools, error) {
	client, err := NewClient(url, password)
	if err != nil {
		return nil, err
	}
	return &Tools{client: client}, nil
}

// Register adds all ESPHome tools to the given MCP server.
func (t *Tools) Register(server *mcp.Server) {
	t.registerListDevices(server)
	t.registerListSecrets(server)
	t.registerReadFile(server)
	t.registerWriteFile(server)
	t.registerValidate(server)
	t.registerCompile(server)
	t.registerUpload(server)
	t.registerGetJob(server)
	t.registerDownload(server)
	t.registerLogs(server)
}

// --- list_esphome_devices ---

func (t *Tools) registerListDevices(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_esphome_devices",
		Description: "List the devices configured in the ESPHome dashboard, with their configuration file, address, online status, and installed/available versions.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		devices, err := t.client.ListDevices(ctx)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		return mcputil.JSONResult(map[string]any{
			"devices": devices,
			"count":   len(devices),
		})
	})
}

// --- list_esphome_secrets ---

func (t *Tools) registerListSecrets(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_esphome_secrets",
		Description: "List the key NAMES (never the values) defined in the ESPHome dashboard's shared secrets.yaml. Use this to check whether the keys a config references (e.g. wifi_ssid, wifi_password) already exist before pushing the config.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		keys, err := t.client.SecretKeys(ctx)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		sort.Strings(keys)
		return mcputil.JSONResult(map[string]any{
			"keys":  keys,
			"count": len(keys),
		})
	})
}

// --- read_esphome_file ---

type readFileArgs struct {
	File string `json:"file" jsonschema:"The device configuration filename to read (e.g. pump.yaml)"`
}

func (t *Tools) registerReadFile(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_esphome_file",
		Description: "Read a device's YAML configuration from the ESPHome dashboard.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args readFileArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("file", args.File); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		content, err := t.client.ReadConfig(ctx, args.File)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		return mcputil.TextResult(content), nil, nil
	})
}

// --- write_esphome_file ---

type writeFileArgs struct {
	File    string `json:"file" jsonschema:"The device configuration filename to write (e.g. pump.yaml). Created if it doesn't exist, overwritten if it does. The YAML must be self-contained — the dashboard writes a single device config, not separate include files."`
	Content string `json:"content" jsonschema:"Full device YAML to write. This overwrites the config."`
}

func (t *Tools) registerWriteFile(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "write_esphome_file",
		Description: "Create or overwrite a device's YAML configuration in the ESPHome dashboard. The config must be a single self-contained YAML (the dashboard has no separate-include-file write; bundle anything needed inline).",
		Annotations: mcputil.Additive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args writeFileArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("file", args.File); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		if args.Content == "" {
			return mcputil.Errorf("content is required"), nil, nil
		}
		created, err := t.client.WriteConfig(ctx, args.File, args.Content)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		verb := "Updated"
		if created {
			verb = "Created"
		}
		return mcputil.TextResult(fmt.Sprintf("%s %s (%d bytes)", verb, args.File, len(args.Content))), nil, nil
	})
}

// --- validate_esphome ---

type configArgs struct {
	Config string `json:"config" jsonschema:"The device configuration filename (e.g. pump.yaml)"`
}

func (t *Tools) registerValidate(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "validate_esphome",
		Description: "Validate an ESPHome device configuration without building it. Fast way to catch YAML/schema errors before compiling.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args configArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("config", args.Config); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		res, err := t.client.Validate(ctx, args.Config)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		return commandResult(res), nil, nil
	})
}

// --- compile_esphome ---

func (t *Tools) registerCompile(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "compile_esphome",
		Description: "Start a firmware build for an ESPHome device (no flashing). Builds can run for minutes — longer than a tool call may stay open — so this is asynchronous: it queues the build and returns a job_id immediately. Poll get_esphome_job with that job_id until status is terminal (completed/failed/cancelled).",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args configArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("config", args.Config); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		job, err := t.client.Compile(ctx, args.Config)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		return jobResult(job, "Build queued. Poll get_esphome_job with this job_id until done is true.", ""), nil, nil
	})
}

// --- upload_esphome ---

type uploadArgs struct {
	Config string `json:"config" jsonschema:"The device configuration filename (e.g. pump.yaml)"`
	Port   string `json:"port,omitempty" jsonschema:"Upload target: OTA (default, flashes over Wi-Fi to a device already running ESPHome) or a serial device path on the dashboard host. First-ever flash of a blank board needs USB and cannot be done over the network."`
}

func (t *Tools) registerUpload(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "upload_esphome",
		Description: "Flash a device's already-compiled binary over the air (queue the upload). Asynchronous: returns a job_id immediately; poll get_esphome_job until done. Compile first (compile_esphome) — this flashes the latest build. OTA requires the device to already be running ESPHome on the network; a first flash of a blank board needs USB (download_esphome_binary / web.esphome.io).",
		Annotations: mcputil.Destructive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args uploadArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("config", args.Config); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		switch decideFlash(req) {
		case flashAsk:
			return flashConfirmationRequest(args), nil, nil
		case flashDeclined:
			return flashDeclinedResult(args), nil, nil
		}
		job, err := t.client.Upload(ctx, args.Config, args.Port)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		return jobResult(job, "Flash queued. Poll get_esphome_job with this job_id until done is true.", ""), nil, nil
	})
}

// --- get_esphome_job ---

type jobArgs struct {
	JobID string `json:"job_id" jsonschema:"The job_id returned by compile_esphome or upload_esphome"`
}

func (t *Tools) registerGetJob(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_esphome_job",
		Description: "Check the status of a compile or upload job started by compile_esphome / upload_esphome. Poll this until done is true; success reports whether it finished cleanly, and output carries the build/flash log (full on failure).",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args jobArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("job_id", args.JobID); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		job, err := t.client.GetJob(ctx, args.JobID)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		// get_job omits output for terminal jobs; fetch the log via follow_job
		// once the job is done so the caller sees build/flash errors.
		log := ""
		if job.Terminal() {
			logCtx, cancel := context.WithTimeout(ctx, jobLogTimeout)
			defer cancel()
			if l, lerr := t.client.JobLog(logCtx, args.JobID); lerr == nil {
				log = l
			}
		}
		return jobResult(job, "", log), nil, nil
	})
}

// jobResult renders a firmware job as a JSON result. log, when non-empty, is
// the build/flash output fetched separately (tail-trimmed for display).
func jobResult(job *Job, note, log string) *mcp.CallToolResult {
	out := map[string]any{
		"job_id":   job.JobID,
		"job_type": job.JobType,
		"status":   job.Status,
		"done":     job.Terminal(),
		"success":  job.Succeeded(),
	}
	if note != "" {
		out["note"] = note
	}
	if job.Progress != nil {
		out["progress"] = *job.Progress
	}
	if job.ExitCode != nil {
		out["exit_code"] = *job.ExitCode
	}
	if job.Error != "" {
		out["error"] = job.Error
	}
	if log != "" {
		out["output"] = tail(log, displayTail)
	}
	result, _, _ := mcputil.JSONResult(out)
	return result
}

// --- get_esphome_logs ---

type logsArgs struct {
	Config         string `json:"config" jsonschema:"The device configuration filename (e.g. pump.yaml)"`
	Port           string `json:"port,omitempty" jsonschema:"Log source: OTA (default, over the network) or a serial device path on the dashboard host"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"How long to capture logs before returning (default 30, max 120). Device logs stream continuously, so this bounds the call."`
}

func (t *Tools) registerLogs(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_esphome_logs",
		Description: "Capture live logs from a running ESPHome device for a bounded duration, then return them. Use to verify behavior after flashing (e.g. confirm the pump RS-485 traffic).",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args logsArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("config", args.Config); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		secs := args.TimeoutSeconds
		if secs <= 0 {
			secs = defaultLogSeconds
		}
		if secs > maxLogSeconds {
			secs = maxLogSeconds
		}
		logCtx, cancel := context.WithTimeout(ctx, time.Duration(secs)*time.Second)
		defer cancel()
		res, err := t.client.Logs(logCtx, args.Config, args.Port)
		if err != nil && res == nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		return commandResult(res), nil, nil
	})
}

// commandResult renders a streamed command outcome as a JSON result, trimming
// the captured output to its tail for inline display.
func commandResult(res *CommandResult) *mcp.CallToolResult {
	out := map[string]any{
		"output":    tail(res.Output, displayTail),
		"truncated": res.Truncated || len(res.Output) > displayTail,
	}
	if res.ExitCode != nil {
		out["exit_code"] = *res.ExitCode
		out["success"] = *res.ExitCode == 0
	}
	if res.TimedOut {
		out["timed_out"] = true
	}
	result, _, _ := mcputil.JSONResult(out)
	return result
}

// tail returns the last n bytes of s, prefixed with an elision marker if cut.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...(earlier output truncated)...\n" + s[len(s)-n:]
}

// --- download_esphome_binary ---

type downloadArgs struct {
	Config  string `json:"config" jsonschema:"The device configuration filename (e.g. pump.yaml)"`
	Factory bool   `json:"factory,omitempty" jsonschema:"Request the factory image (full flash, for a first USB flash) instead of the OTA image. Default false."`
}

func (t *Tools) registerDownload(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "download_esphome_binary",
		Description: "Confirm a compiled firmware image exists and report its size and SHA-256. The image itself is not flashable through MCP; for a first USB flash, open the ESPHome dashboard's download or web.esphome.io. Set factory=true for the full-flash image used on a blank board.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args downloadArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("config", args.Config); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		data, file, err := t.client.DownloadBinary(ctx, args.Config, args.Factory)
		if err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		sum := sha256.Sum256(data)
		return mcputil.JSONResult(map[string]any{
			"config":     args.Config,
			"factory":    args.Factory,
			"file":       file,
			"size_bytes": len(data),
			"sha256":     hex.EncodeToString(sum[:]),
			"note":       "Binary verified downloadable from the dashboard. For a first USB flash, use the dashboard download or https://web.esphome.io; OTA updates can use upload_esphome.",
		})
	})
}
