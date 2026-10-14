package esphome

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Every failure a tool reports carries a short machine-readable kind and a
// next action written for a model that has nothing else to go on: what
// happened, what state the device is in, and which tool to call next.

// Diagnosis explains a failed or unexpected outcome.
type Diagnosis struct {
	Kind string `json:"kind"`
	Next string `json:"next_action"`
}

// explainErr renders a client error as "<what happened>. <next action>".
func explainErr(err error) string {
	return err.Error() + ". " + errorNext(err)
}

func errorNext(err error) string {
	switch {
	case errors.Is(err, ErrJobNotFound):
		return "The dashboard has no job with that id: it was cleared from the job history, pruned (only the most recent jobs are kept), or the id is mistyped. Use the job_id exactly as compile_esphome/upload_esphome returned it; if the work is still needed, start it again."
	case errors.Is(err, context.DeadlineExceeded):
		return "The call ran out of time. Retry it; for long work use the async compile/upload tools and poll get_esphome_job."
	case errors.Is(err, context.Canceled):
		return "The request was cancelled before the dashboard answered."
	}
	var te *transportError
	if errors.As(err, &te) {
		switch te.kind {
		case kindUnreachable:
			return "The ESPHome dashboard is not answering; the add-on may be restarting (it restarts on updates) or stopped. Wait about 30 seconds and retry. Queued and running jobs survive a dashboard restart under the same job_id."
		case kindStalled:
			return "The dashboard accepted the request but stopped responding (overloaded host or a hung add-on). Retry once; if it keeps happening the ESPHome add-on needs a restart."
		case kindLost:
			return "The connection to the dashboard dropped, most likely because it restarted. Retry in about 30 seconds; queued and running jobs survive a restart under the same job_id."
		case kindAuth:
			return "The dashboard rejected the credentials. ESPHOME_PASSWORD on this server must match the dashboard password; this cannot be fixed from a tool call."
		case kindProtocol:
			return "The dashboard answered with something this client does not understand. Check that ESPHOME_URL points at the ESPHome Device Builder and that the add-on is up to date."
		}
	}
	switch errorCode(err) {
	case "not_found":
		return "The dashboard does not know that name. Call list_esphome_devices for the exact configuration filenames."
	case "invalid_args":
		return "The dashboard rejected the arguments; the details above say which one. Correct it and retry."
	case "precondition_failed":
		return "The file changed on the dashboard after it was read (someone edited it). Read it again with read_esphome_file, reapply the change to the new text, and write again."
	case "unavailable", "rate_limited":
		return "The dashboard is busy (too many concurrent runs, or a run went silent and was stopped). Retry in a few seconds."
	case "not_authenticated":
		return "The dashboard session is not authenticated; ESPHOME_PASSWORD on this server must match the dashboard password."
	case "unknown_command":
		return "This dashboard version does not support that operation; the ESPHome add-on needs updating."
	case "internal_error":
		return "The dashboard hit an internal error. Retry once; if it repeats, the ESPHome add-on log has the cause."
	}
	return "Retry once; if it fails the same way, report the error to the user."
}

// linePattern maps substrings of esphome CLI / build / OTA output to a
// diagnosis. Order matters: the first matching pattern wins.
type linePattern struct {
	kind string
	// scopes lists the stages the pattern applies to (scopeBuild,
	// scopeUpload, scopeLogs); empty means all. "timed out" means an
	// unreachable device in an upload but a failed package download in a
	// build, so the stage decides.
	scopes []string
	match  []string // lower-cased substrings; any one matches
	// re, when set, is used instead of match (for messages a substring
	// cannot pin down without also hitting the config dump).
	re   *regexp.Regexp
	next string
}

// Stages a diagnosis is made for.
const (
	scopeBuild  = "build" // validate and compile
	scopeUpload = "upload"
	scopeLogs   = "logs"
)

var (
	inBuild       = []string{scopeBuild}
	inUpload      = []string{scopeUpload}
	inUploadLogs  = []string{scopeUpload, scopeLogs}
	inBuildUpload = []string{scopeBuild, scopeUpload}
)

var linePatterns = []linePattern{
	{
		// espota2: a network failure after the device received the whole
		// image. It may have committed and be rebooting into the new build.
		kind:   "ota_outcome_unknown",
		scopes: inUpload,
		match:  []string{"may have already committed the update"},
		next:   "The connection dropped after the device received the full image, so it may already be running the new firmware. Do NOT upload again yet: wait about 30 seconds, then call verify_esphome_device with the compile job id. Only upload again if it still reports the old build.",
	},
	{
		kind:  "config_not_found",
		match: []string{"no such file or directory: '/config/esphome", "error reading file"},
		next:  "There is no configuration file by that name, or a file it !includes is missing. Call list_esphome_devices for the exact filenames.",
	},
	{
		kind: "missing_secret",
		// esphome yaml_util: "Secret 'name' not defined". A bare "secret '"
		// would also hit `ssid: !secret 'wifi_ssid'` in a failed-config dump.
		re:   regexp.MustCompile(`(?i)secret '[^']+' not defined`),
		next: "The YAML references a !secret key that secrets.yaml does not define. Call list_esphome_secrets for the defined names and use one of them, or ask the user to add the secret (secret values never pass through this tool).",
	},
	{
		kind:   "firmware_too_large",
		scopes: inBuildUpload,
		match:  []string{"not enough space", "partition on the esp is too small", "couldn't prepare flash memory", "is greater than maximum allowed", "will not fit in region", "overflowed by"},
		next:   "The firmware does not fit this board's flash/partition. Remove or slim components (web_server, captive_portal, bluetooth_proxy, logger level) and compile again. Changing the partition layout requires a USB flash. The device keeps running its previous firmware.",
	},
	{
		kind:   "ota_auth_failed",
		scopes: inUploadLogs,
		match:  []string{"authentication invalid", "requests password", "invalid ota encryption key", "noise handshake", "encryption is required", "encryption not offered"},
		next:   "The device rejected the upload's credentials: the ota:/api: password or encryption key in the YAML differs from what the device runs now. Restore the previous key/password in the YAML (validate, compile, upload again), or flash over USB. The device keeps its previous firmware.",
	},
	{
		kind:   "ota_transfer_failed",
		scopes: inUpload,
		match:  []string{"md5 mismatch", "writing ota data to flash memory failed", "finishing update failed", "device closed connection without responding", "unexpected response from esp"},
		next:   "The transfer broke off or was corrupted; the device rejects a partial image and keeps its previous firmware. Retry upload_esphome once. If it fails again, check the device's Wi-Fi signal (get_esphome_logs) or flash over USB.",
	},
	{
		kind:   "device_reset_required",
		scopes: inUpload,
		match:  []string{"manual reset is required", "press the reset button"},
		next:   "The device needs a physical reset after its first USB flash before it accepts OTA. Ask the user to press reset or power-cycle it, then upload again.",
	},
	{
		kind:   "device_unreachable",
		scopes: inUploadLogs,
		match:  []string{"error resolving ip address", "timeout resolving ip address", "no addresses to connect to", "can't connect to esphome api", "upload failed after", "host is unreachable", "connection refused", "network is unreachable", "errno 113", "timed out"},
		next:   "The device did not answer on the network: powered off, off Wi-Fi, or rebooting. It keeps running its previous firmware. Check its state with list_esphome_devices, then retry once it is online.",
	},
	{
		kind:   "no_firmware",
		scopes: inUpload,
		match:  []string{"compile the configuration first", "firmware.bin does not exist", "no such file or directory: '/config/esphome/.esphome/build"},
		next:   "There is no compiled firmware to flash. Run compile_esphome, wait for get_esphome_job to report success, then upload.",
	},
	{
		kind:   "build_network",
		scopes: inBuild,
		match:  []string{"httpclienterror", "could not install package", "failed to download", "connectionerror", "name or service not known", "temporary failure in name resolution", "timed out"},
		next:   "The Home Assistant host could not download toolchain or library packages (no internet, or a registry is down). Nothing changed on the device. Retry the compile in a few minutes.",
	},
	{
		kind:   "build_host_resources",
		scopes: inBuild,
		match:  []string{"no space left on device", "out of memory", "cannot allocate memory", "killed signal terminated program"},
		next:   "The Home Assistant host ran out of disk or memory during the build; nothing changed on the device. Retry the compile once; if it repeats, the user has to free resources on the host.",
	},
	{
		kind:  "config_invalid",
		match: []string{"failed config", "error while reading config", "invalid yaml", "mapping values are not allowed", "while parsing", "is not a valid", "[source "},
		next:  "The YAML does not validate. Fix the reported keys with write_esphome_file, then run validate_esphome until it succeeds before compiling.",
	},
	{
		kind:   "compile_error",
		scopes: inBuild,
		match:  []string{"error:", "*** [", "[failed]", "undefined reference to", "collect2"},
		next:   "The C++ build failed; the first error is in errors. It usually comes from a lambda, an include or an external component in the YAML. Fix the YAML, validate_esphome, then compile_esphome again. Nothing changed on the device.",
	},
}

// diagnoseLines classifies output by the first pattern any line matches.
// errLines are checked first because they are the most specific.
func diagnoseLines(scope string, errLines, lines []string) *Diagnosis {
	for _, p := range linePatterns {
		if !inScope(p.scopes, scope) {
			continue
		}
		if p.re != nil {
			if matchRe(errLines, p.re) || matchRe(lines, p.re) {
				return &Diagnosis{Kind: p.kind, Next: p.next}
			}
			continue
		}
		if matchAny(errLines, p.match) || matchAny(lines, p.match) {
			return &Diagnosis{Kind: p.kind, Next: p.next}
		}
	}
	return nil
}

func inScope(scopes []string, scope string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, s := range scopes {
		if s == scope {
			return true
		}
	}
	return false
}

func matchRe(lines []string, re *regexp.Regexp) bool {
	for _, l := range lines {
		if re.MatchString(l) {
			return true
		}
	}
	return false
}

func matchAny(lines []string, needles []string) bool {
	for _, l := range lines {
		lower := strings.ToLower(l)
		for _, n := range needles {
			if strings.Contains(lower, n) {
				return true
			}
		}
	}
	return false
}

// diagnoseJob explains a terminal job. log may be nil when the log could
// not be fetched. newer is the newest other job for the same configuration
// created after this one, if any (used to recognise supersession).
func diagnoseJob(job *Job, log *CommandResult, newer *Job) *Diagnosis {
	switch job.Status {
	case "completed":
		if job.JobType == "upload" {
			return &Diagnosis{Kind: "flashed", Next: "The dashboard reports the flash finished. Confirm the device actually rebooted into the new build: call verify_esphome_device with this config and the compile job's id."}
		}
		return &Diagnosis{Kind: "built", Next: "Build succeeded. Next: upload_esphome to flash it, then verify_esphome_device."}
	case "cancelled":
		errText := job.Error
		if log != nil && log.Error != "" {
			errText = log.Error
		}
		switch {
		case strings.Contains(errText, "offline"):
			return &Diagnosis{Kind: "deferred_offline", Next: "The device was offline, so the dashboard queued the update instead of flashing now; it flashes automatically when the device reconnects (list_esphome_devices shows queued_update). Verify later with verify_esphome_device."}
		case strings.Contains(errText, "prerequisite"):
			return &Diagnosis{Kind: "prerequisite_failed", Next: "This job waited on an earlier job that failed or was cancelled, so it never ran. Look at that job, fix it, and start again."}
		case newer != nil:
			return &Diagnosis{Kind: "superseded", Next: fmt.Sprintf("A newer %s job (%s) was submitted for the same device, and the dashboard cancels older in-flight jobs when that happens. Poll that job instead.", newer.JobType, newer.JobID)}
		}
		return &Diagnosis{Kind: "cancelled", Next: "The job was cancelled (from the dashboard UI, cancel_esphome_job, or a build-environment reset). Start it again if it is still needed."}
	case "failed":
		if log != nil && log.QueuedUpdateArmed {
			return &Diagnosis{Kind: "deferred_offline", Next: "The device was offline, so the dashboard queued the update: it flashes automatically when the device reconnects (list_esphome_devices shows queued_update). Nothing to retry now; verify later with verify_esphome_device."}
		}
		if job.FailureReason == "provision" {
			return &Diagnosis{Kind: "remote_build_failed", Next: "A paired remote build server could not prepare its toolchain. Compile again; the dashboard falls back to building locally."}
		}
		var errs, lines []string
		if log != nil {
			errs, lines = log.Errors, log.Lines
		}
		if job.Error != "" {
			errs = append([]string{job.Error}, errs...)
		}
		scope := scopeBuild
		if job.JobType == "upload" {
			scope = scopeUpload
		}
		if d := diagnoseLines(scope, errs, lines); d != nil {
			return d
		}
		if log == nil {
			return &Diagnosis{Kind: "failed", Next: "The job failed and its log could not be fetched. Call get_esphome_job again to retry fetching the log."}
		}
		return &Diagnosis{Kind: "failed", Next: "The job failed for a reason this tool does not recognise; read the output tail. If it was an upload, the device keeps its previous firmware."}
	}
	return nil
}
