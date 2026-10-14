package esphome

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// defaultVerifySeconds / maxVerifySeconds bound verify_esphome_device's
	// log capture. It ends early once it has what it needs; the window is
	// long enough to ride out a post-flash reboot.
	defaultVerifySeconds = 45
	maxVerifySeconds     = 120
	// displayBytes caps inline output in one result.
	displayBytes = 8000
	// failureTailLines / successTailLines are how many trailing output lines
	// a result shows for a failed / successful run.
	failureTailLines = 80
	successTailLines = 10
	// recentLines is how much live output a running job's status shows.
	recentLines = 15
	// jobLogTimeout bounds the follow_job log fetch for a terminal job.
	jobLogTimeout = 90 * time.Second
)

// Poll intervals suggested to a model, so it neither hammers the dashboard
// nor waits needlessly.
const (
	pollQueuedSeconds  = 10
	pollCompileSeconds = 20
	pollUploadSeconds  = 5
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
	t.registerCancelJob(server)
	t.registerDownload(server)
	t.registerLogs(server)
	t.registerVerify(server)
}

// errResult renders a client error with its next action.
func errResult(err error) *mcp.CallToolResult {
	return mcputil.Errorf("%s", explainErr(err))
}

func jsonResult(v any) *mcp.CallToolResult {
	res, _, _ := mcputil.JSONResult(v)
	return res
}

// --- list_esphome_devices ---

type listDevicesArgs struct {
	Full bool `json:"full,omitempty" jsonschema:"Return the raw dashboard records (loaded integrations, platforms, build sizes). Default returns a compact summary per device."`
}

func (t *Tools) registerListDevices(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_esphome_devices",
		Description: "List the devices configured in the ESPHome dashboard, with their configuration file, address, online state (online/offline/unknown), installed/available versions, and queued_update (an update waiting for an offline device to reconnect).",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args listDevicesArgs) (*mcp.CallToolResult, any, error) {
		devices, err := t.client.ListDevices(ctx)
		if err != nil {
			return errResult(err), nil, nil
		}
		if !args.Full {
			devices = slimDevices(devices)
		}
		return mcputil.JSONResult(map[string]any{
			"devices": devices,
			"count":   len(devices),
		})
	})
}

// deviceSummaryKeys are the devices/list fields kept by default. The raw
// record also carries every loaded integration and platform, which is a few
// kilobytes per device and only matters when reading the YAML anyway.
var deviceSummaryKeys = []string{
	"name", "friendly_name", "configuration", "address", "ip", "area", "comment",
	"target_platform", "board_id", "current_version", "update_available", "has_pending_changes",
}

// slimDevices projects dashboard device records to a summary, lifting the
// deployed version, online state and queued-update flag out of runtime_state.
func slimDevices(devices []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(devices))
	for _, d := range devices {
		rec := make(map[string]any, len(deviceSummaryKeys)+3)
		for _, k := range deviceSummaryKeys {
			v, ok := d[k]
			if !ok || v == nil || v == "" {
				continue
			}
			rec[k] = v
		}
		if rs, ok := d["runtime_state"].(map[string]any); ok {
			for _, k := range []string{"state", "deployed_version"} {
				if v, ok := rs[k]; ok && v != nil && v != "" {
					rec[k] = v
				}
			}
			if v, ok := rs["queued_update"].(bool); ok && v {
				rec["queued_update"] = true
			}
		}
		out = append(out, rec)
	}
	return out
}

func runtimeState(d map[string]any) map[string]any {
	rs, _ := d["runtime_state"].(map[string]any)
	return rs
}

func str(v any) string {
	s, _ := v.(string)
	return s
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
			return errResult(err), nil, nil
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
		Description: "Read a device's YAML configuration from the ESPHome dashboard. Read before writing: write_esphome_file refuses to overwrite a file that changed since it was read.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args readFileArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("file", args.File); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		content, err := t.client.ReadConfig(ctx, args.File)
		if err != nil {
			return errResult(err), nil, nil
		}
		return mcputil.TextResult(content), nil, nil
	})
}

// --- write_esphome_file ---

type writeFileArgs struct {
	File    string `json:"file" jsonschema:"The device configuration filename to write (e.g. pump.yaml). Created if it doesn't exist, overwritten if it does. The YAML must be self-contained — the dashboard writes a single device config, not separate include files."`
	Content string `json:"content" jsonschema:"Full device YAML to write. This overwrites the config."`
}

// secretRefRe finds `!secret name` references in YAML.
var secretRefRe = regexp.MustCompile(`!secret\s+['"]?([A-Za-z0-9_.-]+)`)

func (t *Tools) registerWriteFile(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "write_esphome_file",
		Description: "Create or overwrite a device's YAML configuration in the ESPHome dashboard. The config must be a single self-contained YAML (the dashboard has no separate-include-file write; bundle anything needed inline). Overwrites only succeed if the file is unchanged since this call read it; the result lists any !secret keys missing from secrets.yaml. Run validate_esphome next.",
		Annotations: mcputil.Additive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args writeFileArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("file", args.File); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		if strings.TrimSpace(args.Content) == "" {
			return mcputil.Errorf("content is required (the dashboard refuses to write an empty config)"), nil, nil
		}
		// Creating a file is additive; overwriting one is not. Only the
		// latter is worth interrupting the user for, so look first. A read
		// error is treated as "new file": the write is about to hit the same
		// dashboard and will surface any real problem itself.
		var expected *string
		if existing, err := t.client.ReadConfig(ctx, args.File); err == nil {
			if existing == args.Content {
				return jsonResult(map[string]any{
					"file": args.File, "changed": false,
					"next_action": "The file already has this content. Run validate_esphome, then compile_esphome if the device needs it.",
				}), nil, nil
			}
			if ok, res := mcputil.Confirm(req, fmt.Sprintf("Overwrite %s (%d bytes) with new content (%d bytes)", args.File, len(existing), len(args.Content))); !ok {
				return res, nil, nil
			}
			// Pass what was read as the precondition, so an edit made in the
			// dashboard between that read and this write is not clobbered.
			expected = &existing
		}
		created, err := t.client.WriteConfig(ctx, args.File, args.Content, expected)
		if err != nil {
			return errResult(err), nil, nil
		}
		out := map[string]any{
			"file":    args.File,
			"bytes":   len(args.Content),
			"created": created,
			"changed": true,
		}
		next := "Run validate_esphome on this file before compiling."
		if missing := t.missingSecrets(ctx, args.Content); len(missing) > 0 {
			out["missing_secrets"] = missing
			next = "The YAML references !secret keys that secrets.yaml does not define (missing_secrets); validation will fail until they exist. Use defined names (list_esphome_secrets) or ask the user to add them, then validate_esphome."
		}
		out["next_action"] = next
		return jsonResult(out), nil, nil
	})
}

// missingSecrets returns the !secret names content references that the
// dashboard's secrets.yaml lacks. Best effort: a lookup failure returns nil
// and validation reports the problem instead.
func (t *Tools) missingSecrets(ctx context.Context, content string) []string {
	refs := secretRefRe.FindAllStringSubmatch(content, -1)
	if len(refs) == 0 {
		return nil
	}
	keys, err := t.client.SecretKeys(ctx)
	if err != nil {
		return nil
	}
	have := make(map[string]bool, len(keys))
	for _, k := range keys {
		have[k] = true
	}
	var missing []string
	seen := map[string]bool{}
	for _, r := range refs {
		if name := r[1]; !have[name] && !seen[name] {
			seen[name] = true
			missing = append(missing, name)
		}
	}
	return missing
}

// --- validate_esphome ---

type configArgs struct {
	Config string `json:"config" jsonschema:"The device configuration filename (e.g. pump.yaml)"`
}

func (t *Tools) registerValidate(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "validate_esphome",
		Description: "Validate an ESPHome device configuration without building it. Fast way to catch YAML/schema errors before compiling. Returns valid (true/false, or null if the run did not finish), the errors, and the next action. Secret values are redacted.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args configArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("config", args.Config); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		res, err := t.client.Validate(ctx, args.Config)
		if err != nil && res == nil {
			return errResult(err), nil, nil
		}
		return jsonResult(validateView(args.Config, res, err)), nil, nil
	})
}

// validateView renders a validate run. err is a dashboard error that ended
// the stream after some output (e.g. "no output for 60s").
func validateView(cfg string, res *CommandResult, err error) map[string]any {
	out := map[string]any{"config": cfg}
	switch {
	case err != nil || !res.Final:
		out["valid"] = nil
		reason := res.Incomplete
		if err != nil {
			reason = explainErr(err)
		}
		out["incomplete"] = reason
		setOutput(out, res.Lines, failureTailLines)
		out["next_action"] = "Validation did not finish, so nothing is known about the YAML. Retry validate_esphome."
	case res.Succeeded():
		out["valid"] = true
		// A successful run prints the whole resolved config after the
		// warnings; that dump is noise. Keep the CLI's own log lines.
		var keep, warnings []string
		for _, l := range res.Lines {
			if isCLILogLine(l) {
				keep = append(keep, l)
				if strings.HasPrefix(l, "WARNING ") {
					warnings = append(warnings, l)
				}
			}
		}
		if len(warnings) > 0 {
			out["warnings"] = warnings
		}
		setOutput(out, keep, successTailLines)
		out["next_action"] = "The configuration is valid. Next: compile_esphome."
	default:
		out["valid"] = false
		out["exit_code"] = *res.ExitCode
		setErrors(out, res)
		setOutput(out, res.Lines, failureTailLines)
		d := diagnoseLines(scopeBuild, res.Errors, res.Lines)
		if d == nil {
			d = &Diagnosis{Kind: "config_invalid", Next: "The YAML does not validate; the output says why. Fix it with write_esphome_file and validate again before compiling."}
		}
		out["diagnosis"] = d.Kind
		out["next_action"] = d.Next
	}
	return out
}

func setErrors(out map[string]any, res *CommandResult) {
	if len(res.Errors) > 0 {
		out["errors"] = res.Errors
	}
	if res.MoreErrors > 0 {
		out["more_errors"] = res.MoreErrors
	}
}

// setOutput puts the last n lines (byte-capped) into out["output"].
func setOutput(out map[string]any, lines []string, n int) {
	shown, cut := tailLines(lines, n)
	shown, cut2 := capBytes(shown, displayBytes)
	if len(shown) == 0 {
		return
	}
	out["output"] = strings.Join(shown, "\n")
	if cut || cut2 {
		out["output_truncated"] = true
	}
}

// --- compile_esphome ---

func (t *Tools) registerCompile(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "compile_esphome",
		Description: "Start a firmware build for an ESPHome device (no flashing). Builds can run for minutes — longer than a tool call may stay open — so this is asynchronous: it queues the build and returns a job_id immediately. Poll get_esphome_job with that job_id until done is true. Run validate_esphome first. Refuses while the device is being flashed (a new job would cancel the flash).",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args configArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("config", args.Config); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		var replaced []string
		jobs, err := t.client.GetJobs(ctx, args.Config)
		if err != nil && errorCode(err) != "unknown_command" {
			return errResult(err), nil, nil
		}
		for _, j := range jobs {
			if !j.Active() {
				continue
			}
			if j.JobType == "upload" || j.JobType == "install" || j.JobType == "rename" {
				return mcputil.Errorf("job %s is flashing %s right now (%s). The dashboard cancels a device's in-flight jobs when a new one is submitted, so compiling now would abort that flash. Poll get_esphome_job %s until it is done, then compile.", j.JobID, args.Config, j.Status, j.JobID), nil, nil
			}
			replaced = append(replaced, j.JobID)
		}
		job, err := t.client.Compile(ctx, args.Config)
		if err != nil {
			return errResult(err), nil, nil
		}
		out := queuedView(job, "Build queued. Poll get_esphome_job with this job_id until done is true.")
		if len(replaced) > 0 {
			out["replaced_jobs"] = replaced
			out["note"] = "This replaces the earlier in-flight compile(s) listed in replaced_jobs; the dashboard cancels them. Poll only the new job_id."
		}
		return jsonResult(out), nil, nil
	})
}

func queuedView(job *Job, next string) map[string]any {
	out := map[string]any{
		"job_id":             job.JobID,
		"job_type":           job.JobType,
		"configuration":      job.Configuration,
		"status":             job.Status,
		"done":               job.Terminal(),
		"poll_after_seconds": pollAfter(job),
		"next_action":        next,
	}
	return out
}

func pollAfter(job *Job) int {
	switch {
	case job.Status == "queued":
		return pollQueuedSeconds
	case job.JobType == "upload":
		return pollUploadSeconds
	}
	return pollCompileSeconds
}

// --- upload_esphome ---

type uploadArgs struct {
	Config string `json:"config" jsonschema:"The device configuration filename (e.g. pump.yaml)"`
	Port   string `json:"port,omitempty" jsonschema:"Upload target: OTA (default, flashes over Wi-Fi to a device already running ESPHome) or a serial device path on the dashboard host. First-ever flash of a blank board needs USB and cannot be done over the network."`
}

func (t *Tools) registerUpload(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "upload_esphome",
		Description: "Flash a device's already-compiled binary over the air (queue the upload). Asynchronous: returns a job_id immediately; poll get_esphome_job until done, then confirm with verify_esphome_device. Compile first (compile_esphome) and wait for it to succeed: this refuses while a compile for the device is in flight or when its latest compile failed, because it would flash an older binary. OTA requires the device to already be running ESPHome on the network; a first flash of a blank board needs USB (download_esphome_binary / web.esphome.io).",
		Annotations: mcputil.Destructive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args uploadArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("config", args.Config); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		// Check before asking the user: no point confirming a flash that
		// would be refused or would flash the wrong binary.
		notes, refusal, err := t.uploadPrecheck(ctx, args.Config)
		if err != nil {
			return errResult(err), nil, nil
		}
		if refusal != "" {
			return mcputil.Errorf("%s", refusal), nil, nil
		}
		target := "over the air"
		if args.Port != "" && !strings.EqualFold(args.Port, "OTA") {
			target = "via " + args.Port
		}
		if ok, res := mcputil.Confirm(req, fmt.Sprintf("Flash %s %s with the latest build", args.Config, target)); !ok {
			return res, nil, nil
		}
		job, err := t.client.Upload(ctx, args.Config, args.Port)
		if err != nil {
			return errResult(err), nil, nil
		}
		out := queuedView(job, "Flash queued. Poll get_esphome_job with this job_id until done is true, then run verify_esphome_device.")
		if len(notes) > 0 {
			out["notes"] = notes
		}
		return jsonResult(out), nil, nil
	})
}

// uploadPrecheck decides whether an upload would flash the build the caller
// expects. The dashboard flashes whatever binary is on disk, and submitting
// any job for a device cancels that device's in-flight jobs, so an upload
// sent while a compile is running cancels the compile and flashes the
// previous binary.
func (t *Tools) uploadPrecheck(ctx context.Context, cfg string) (notes []string, refusal string, err error) {
	jobs, err := t.client.GetJobs(ctx, cfg)
	if err != nil {
		if errorCode(err) != "unknown_command" {
			return nil, "", err
		}
		notes = append(notes, "This dashboard cannot list jobs, so the upload was not checked against a running or failed compile.")
	}
	sort.SliceStable(jobs, func(i, k int) bool { return jobs[i].CreatedAt > jobs[k].CreatedAt })
	for _, j := range jobs {
		if !j.Active() {
			continue
		}
		switch j.JobType {
		case "upload", "install", "rename":
			return nil, fmt.Sprintf("an upload for %s is already %s (job %s). Poll get_esphome_job %s instead of starting another: a second submission would cancel it mid-flash.", cfg, j.Status, j.JobID, j.JobID), nil
		default:
			return nil, fmt.Sprintf("%s job %s for %s is still %s. Uploading now would make the dashboard cancel it (a new job replaces a device's in-flight jobs) and flash the previous binary. Poll get_esphome_job %s until it completes, then upload.", j.JobType, j.JobID, cfg, j.Status, j.JobID), nil
		}
	}
	var lastCompile *Job
	for i := range jobs {
		if jobs[i].JobType == "compile" || jobs[i].JobType == "install" {
			lastCompile = &jobs[i]
			break
		}
	}
	switch {
	case lastCompile == nil && err == nil:
		notes = append(notes, "No compile for this device is in the dashboard's job history, so this flashes whatever binary was last built.")
	case lastCompile != nil && lastCompile.Status != "completed":
		return nil, fmt.Sprintf("the latest compile for %s (job %s) ended %s, so the binary on disk is from an older build and would not include the latest YAML. Fix the cause (get_esphome_job %s shows it), compile again, then upload.", cfg, lastCompile.JobID, lastCompile.Status, lastCompile.JobID), nil
	}

	bins, err := t.client.Binaries(ctx, cfg)
	if err != nil {
		return nil, "", err
	}
	if len(bins) == 0 {
		return nil, fmt.Sprintf("there is no compiled firmware for %s (the build directory is empty or was cleaned). Run compile_esphome, wait for it to succeed, then upload.", cfg), nil
	}

	if dev, err := t.client.FindDevice(ctx, cfg); err == nil && dev != nil {
		if state := str(runtimeState(dev)["state"]); state == "offline" {
			notes = append(notes, "The dashboard reports this device offline. The OTA will most likely fail; the dashboard then queues the update and flashes it when the device reconnects.")
		}
	}
	return notes, "", nil
}

// --- get_esphome_job ---

type jobArgs struct {
	JobID string `json:"job_id" jsonschema:"The job_id returned by compile_esphome or upload_esphome"`
}

func (t *Tools) registerGetJob(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_esphome_job",
		Description: "Check a compile or upload job started by compile_esphome / upload_esphome. While running it returns progress, recent output and poll_after_seconds; once done is true it returns success, the errors, a diagnosis and next_action. Jobs survive a dashboard restart under the same job_id.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args jobArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("job_id", args.JobID); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		job, err := t.client.GetJob(ctx, args.JobID)
		if err != nil {
			return errResult(err), nil, nil
		}
		if !job.Terminal() {
			return jsonResult(activeJobView(job)), nil, nil
		}
		return jsonResult(t.terminalJobView(ctx, job)), nil, nil
	})
}

func jobBase(job *Job) map[string]any {
	out := map[string]any{
		"job_id":        job.JobID,
		"job_type":      job.JobType,
		"configuration": job.Configuration,
		"status":        job.Status,
		"done":          job.Terminal(),
	}
	if job.Progress != nil {
		out["progress"] = *job.Progress
	}
	return out
}

func activeJobView(job *Job) map[string]any {
	out := jobBase(job)
	poll := pollAfter(job)
	out["poll_after_seconds"] = poll
	next := fmt.Sprintf("Still %s; call get_esphome_job again in about %d seconds.", job.Status, poll)
	if job.Status == "queued" && job.DependsOn != "" {
		next = fmt.Sprintf("Waiting for job %s to finish first; call get_esphome_job again in about %d seconds.", job.DependsOn, poll)
	}
	if job.Restarted() {
		out["restarted"] = true
		next = "The dashboard restarted while this job ran and started it over under the same job_id; the log above the restart marker is from the abandoned run. " + next
	}
	out["next_action"] = next
	if len(job.Output) > 0 {
		var buf lineBuffer
		for _, l := range job.Output {
			buf.add(l)
		}
		lines, _ := buf.tail()
		shown, _ := tailLines(lines, recentLines)
		shown, _ = capBytes(shown, displayBytes)
		out["recent_output"] = strings.Join(shown, "\n")
	}
	return out
}

func (t *Tools) terminalJobView(ctx context.Context, job *Job) map[string]any {
	out := jobBase(job)
	out["success"] = job.Succeeded()
	if job.ExitCode != nil {
		out["exit_code"] = *job.ExitCode
	}
	if job.Error != "" {
		out["error"] = job.Error
	}
	// get_job omits output for terminal jobs; the log and the terminal frame
	// (which carries queued_update_armed) come from follow_job.
	logCtx, cancel := context.WithTimeout(ctx, jobLogTimeout)
	defer cancel()
	log, lerr := t.client.JobLog(logCtx, job.JobID)
	switch {
	case lerr != nil:
		out["output_unavailable"] = explainErr(lerr)
		log = nil
	case log.Incomplete != "":
		out["output_incomplete"] = log.Incomplete
	}
	if log != nil {
		if log.QueuedUpdateArmed {
			out["queued_update_armed"] = true
		}
		if job.Succeeded() {
			setOutput(out, log.Lines, successTailLines)
		} else {
			setErrors(out, log)
			setOutput(out, log.Lines, failureTailLines)
		}
	}
	var newer *Job
	if job.Status == "cancelled" {
		newer = t.newerJob(ctx, job)
	}
	if d := diagnoseJob(job, log, newer); d != nil {
		out["diagnosis"] = d.Kind
		out["next_action"] = d.Next
	}
	return out
}

// newerJob returns the newest job for the same configuration created after
// job, or nil. Best effort.
func (t *Tools) newerJob(ctx context.Context, job *Job) *Job {
	jobs, err := t.client.GetJobs(ctx, job.Configuration)
	if err != nil {
		return nil
	}
	var newest *Job
	for i := range jobs {
		j := &jobs[i]
		if j.JobID == job.JobID || j.CreatedAt <= job.CreatedAt {
			continue
		}
		if newest == nil || j.CreatedAt > newest.CreatedAt {
			newest = j
		}
	}
	return newest
}

// --- cancel_esphome_job ---

func (t *Tools) registerCancelJob(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "cancel_esphome_job",
		Description: "Cancel a queued or running compile/upload job. A queued job is cancelled at once; a running one is signalled and may still finish, so poll get_esphome_job for its final status. Cancelling an upload mid-transfer leaves the device on its previous firmware.",
		Annotations: mcputil.Destructive(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args jobArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("job_id", args.JobID); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		if err := t.client.CancelJob(ctx, args.JobID); err != nil {
			return errResult(err), nil, nil
		}
		out := map[string]any{"job_id": args.JobID, "cancel_requested": true}
		if job, err := t.client.GetJob(ctx, args.JobID); err == nil {
			out["status"] = job.Status
		}
		out["next_action"] = "Poll get_esphome_job until done is true to see the final status."
		return jsonResult(out), nil, nil
	})
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
		Description: "Capture live logs from a running ESPHome device for a bounded duration, then return them, with what they show: whether the device answered, the firmware version and build time it reports, failed components, and device errors/warnings. Secret values (SSID, keys) are redacted. To check a flash, prefer verify_esphome_device.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args logsArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("config", args.Config); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		secs := clampSeconds(args.TimeoutSeconds, defaultLogSeconds, maxLogSeconds)
		logCtx, cancel := context.WithTimeout(ctx, time.Duration(secs)*time.Second)
		defer cancel()
		facts := newLogFacts(nil)
		res, err := t.client.Logs(logCtx, args.Config, LogOptions{Port: args.Port, Observe: facts.observe})
		if err != nil && res == nil {
			return errResult(err), nil, nil
		}
		return jsonResult(logsView(res, err, facts)), nil, nil
	})
}

func clampSeconds(v, def, limit int) int {
	if v <= 0 {
		return def
	}
	if v > limit {
		return limit
	}
	return v
}

func logsView(res *CommandResult, err error, facts *logFacts) map[string]any {
	out := facts.summary()
	setOutput(out, res.Lines, 150)
	if res.TimedOut {
		out["timed_out"] = true
	}
	if res.Final && res.ExitCode != nil {
		out["exit_code"] = *res.ExitCode
	}
	switch {
	case err != nil:
		out["error"] = explainErr(err)
	case res.Incomplete != "":
		out["incomplete"] = res.Incomplete
	}
	if !facts.connected {
		d := diagnoseLines(scopeLogs, res.Errors, res.Lines)
		if d == nil {
			d = &Diagnosis{Kind: "device_not_answering", Next: "The device did not answer during the capture. It may be offline or rebooting; check list_esphome_devices and try again with a longer timeout_seconds."}
		}
		out["diagnosis"] = d.Kind
		out["next_action"] = d.Next
	}
	return out
}

// --- verify_esphome_device ---

type verifyArgs struct {
	Config         string   `json:"config" jsonschema:"The device configuration filename (e.g. pump.yaml)"`
	CompileJobID   string   `json:"compile_job_id,omitempty" jsonschema:"The job_id of the compile whose build should be on the device (an upload job_id also works: its preceding successful compile is used). Without it only the ESPHome version is checked, not that this particular build is running."`
	Expect         []string `json:"expect,omitempty" jsonschema:"Text that must appear in the device's startup dump to count as working, e.g. the name of a newly added sensor ('Porch Humidity'). Case-insensitive."`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" jsonschema:"How long to wait for the device to answer and print its startup dump (default 45, max 120). Returns early once everything is seen."`
}

func (t *Tools) registerVerify(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name: "verify_esphome_device",
		Description: "After a flash, confirm the device actually runs the new build: it connects to the device, reads the version and build time from its startup dump, checks it matches the compile job and the dashboard's ESPHome version, that every expected text (e.g. a new sensor's name) appears, and that no component failed setup. " +
			"The dashboard's own deployed_version is set optimistically when an upload finishes, so it is not proof. Returns verified, the individual checks, and next_action.",
		Annotations: mcputil.ReadOnly(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, args verifyArgs) (*mcp.CallToolResult, any, error) {
		if err := validate.Identifier("config", args.Config); err != nil {
			return mcputil.Errorf("%v", err), nil, nil
		}
		dev, err := t.client.FindDevice(ctx, args.Config)
		if err != nil {
			return errResult(err), nil, nil
		}
		if dev == nil {
			return mcputil.Errorf("the dashboard has no device with configuration %q. Call list_esphome_devices for the exact filenames.", args.Config), nil, nil
		}
		in := verifyInput{dashboardVersion: str(dev["current_version"])}
		rs := runtimeState(dev)
		in.queuedUpdate, _ = rs["queued_update"].(bool)

		var compileJob *Job
		if args.CompileJobID != "" {
			if err := validate.Identifier("compile_job_id", args.CompileJobID); err != nil {
				return mcputil.Errorf("%v", err), nil, nil
			}
			compileJob, err = t.resolveCompileJob(ctx, args.Config, args.CompileJobID)
			if err != nil {
				return errResult(err), nil, nil
			}
			if !compileJob.Succeeded() {
				return mcputil.Errorf("compile job %s is %s, not completed, so there is no build of it to verify. Poll get_esphome_job %s; verify after it succeeds and has been uploaded.", compileJob.JobID, compileJob.Status, compileJob.JobID), nil, nil
			}
			in.buildStart = parseJobTime(compileJob.StartedAt)
			in.buildEnd = parseJobTime(compileJob.CompletedAt)
		}

		secs := clampSeconds(args.TimeoutSeconds, defaultVerifySeconds, maxVerifySeconds)
		logCtx, cancel := context.WithTimeout(ctx, time.Duration(secs)*time.Second)
		defer cancel()
		facts := newLogFacts(args.Expect)
		res, err := t.captureForVerify(logCtx, args.Config, facts)
		if err != nil && res == nil {
			return errResult(err), nil, nil
		}
		in.facts = facts

		verified, checks, d := verdict(in)
		out := map[string]any{
			"config":      args.Config,
			"verified":    verified,
			"checks":      checks,
			"diagnosis":   d.Kind,
			"next_action": d.Next,
			"device":      facts.summary(),
			"dashboard": map[string]any{
				"state":            rs["state"],
				"esphome_version":  in.dashboardVersion,
				"deployed_version": rs["deployed_version"],
				"queued_update":    in.queuedUpdate,
				"note":             "deployed_version is recorded optimistically when an upload completes; the device fields above are what the device itself reported.",
			},
		}
		if compileJob != nil {
			out["compile_job_id"] = compileJob.JobID
		}
		if !verified {
			setOutput(out, res.Lines, 40)
		}
		if err != nil {
			out["error"] = explainErr(err)
		}
		return jsonResult(out), nil, nil
	})
}

// verifyGrace is how long verify keeps reading after it has seen everything
// it was looking for, so the rest of the device's dump_config (where failed
// components are reported) arrives too. A variable so tests can shrink it.
var verifyGrace = 3 * time.Second

// captureForVerify reads device logs until the window ends or, once the
// facts are complete, for verifyGrace longer.
func (t *Tools) captureForVerify(ctx context.Context, cfg string, facts *logFacts) (*CommandResult, error) {
	graceCtx, stop := context.WithCancel(ctx)
	defer stop()
	var (
		mu    sync.Mutex
		timer *time.Timer
		fired atomic.Bool
	)
	defer func() {
		mu.Lock()
		if timer != nil {
			timer.Stop()
		}
		mu.Unlock()
	}()
	until := func() bool {
		if facts.complete() {
			mu.Lock()
			if timer == nil {
				timer = time.AfterFunc(verifyGrace, func() { fired.Store(true); stop() })
			}
			mu.Unlock()
		}
		return false
	}
	res, err := t.client.Logs(graceCtx, cfg, LogOptions{Observe: facts.observe, Until: until})
	if err != nil && fired.Load() && errors.Is(err, context.Canceled) && ctx.Err() == nil {
		// Our own grace timer ended the capture: a normal stop.
		err = nil
		if res != nil {
			res.Stopped = true
		}
	}
	return res, err
}

// resolveCompileJob returns the compile job to verify against. Given an
// upload job, it picks the newest successful compile for the same
// configuration created before that upload.
func (t *Tools) resolveCompileJob(ctx context.Context, cfg, jobID string) (*Job, error) {
	job, err := t.client.GetJob(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if job.Configuration != "" && job.Configuration != cfg {
		return nil, fmt.Errorf("job %s belongs to %s, not %s", jobID, job.Configuration, cfg)
	}
	if job.JobType != "upload" {
		return job, nil
	}
	jobs, err := t.client.GetJobs(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var best *Job
	for i := range jobs {
		j := &jobs[i]
		if (j.JobType == "compile" || j.JobType == "install") && j.Succeeded() && j.CreatedAt <= job.CreatedAt {
			if best == nil || j.CreatedAt > best.CreatedAt {
				best = j
			}
		}
	}
	if best == nil {
		return nil, errors.New("no successful compile precedes upload job " + jobID + " in the dashboard's history; pass the compile job_id instead")
	}
	return best, nil
}

func parseJobTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
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
		data, bin, err := t.client.DownloadBinary(ctx, args.Config, args.Factory)
		if err != nil {
			return errResult(err), nil, nil
		}
		sum := sha256.Sum256(data)
		out := map[string]any{
			"config":     args.Config,
			"factory":    args.Factory,
			"file":       bin.File,
			"size_bytes": len(data),
			"sha256":     hex.EncodeToString(sum[:]),
			"note":       "Binary verified downloadable from the dashboard. For a first USB flash, use the dashboard download or https://web.esphome.io; OTA updates can use upload_esphome.",
		}
		if bin.Type != "" {
			out["type"] = bin.Type
		}
		return jsonResult(out), nil, nil
	})
}
