package esphome

import (
	"regexp"
	"strings"
	"time"
)

// What "verified working" means after a flash, and why the dashboard alone
// cannot say it: on a successful upload the dashboard optimistically copies
// the compiled config hash and version into the device's deployed_* fields
// (device-builder firmware_sync.sync_deployed_state_after_flash), so those
// match the instant the upload job completes, whether or not the device ever
// booted the new image. The evidence has to come from the device itself.
//
// esphome logs connects to the device's native API, which replays the
// device's dump_config on connect. That dump starts with
//
//	[I][app:151]: ESPHome version 2026.8.0 compiled on 2026-08-20 17:37:30 -0400
//
// (core/application.cpp), then one [C] block per component. So a device that
// answers, reports the dashboard's ESPHome version, was compiled during the
// compile job, and lists the expected components is running the new build.

var (
	versionRe   = regexp.MustCompile(`ESPHome version (\S+) compiled on (.+?)\s*$`)
	handshakeRe = regexp.MustCompile(`Successful handshake with|Successfully connected to`)
	// connectFailRe matches esphome's API connection failures.
	connectFailRe = regexp.MustCompile(`Can't connect to ESPHome API|Error resolving IP address|Timeout resolving IP address|Invalid encryption key|noise handshake|Connection requires encryption|Handshake failed`)
	// device log levels: [E] error, [W] warning, as rendered by esphome.
	deviceErrRe  = regexp.MustCompile(`\[E\]\[`)
	deviceWarnRe = regexp.MustCompile(`\[W\]\[`)
)

// compiledOnLayouts are the build-time formats ESPHome has printed: current
// releases print an ISO-ish local time with offset; older ones the C
// __DATE__ ", " __TIME__ pair with no zone.
var compiledOnLayouts = []struct {
	layout string
	zoned  bool
}{
	{"2006-01-02 15:04:05 -0700", true},
	{"Jan _2 2006, 15:04:05", false},
	{"Jan 2 2006, 15:04:05", false},
}

func parseCompiledOn(s string) (t time.Time, zoned bool, ok bool) {
	s = strings.TrimSpace(s)
	for _, l := range compiledOnLayouts {
		if t, err := time.Parse(l.layout, s); err == nil {
			return t, l.zoned, true
		}
	}
	return time.Time{}, false, false
}

// logFacts accumulates what a log capture says about the running firmware.
type logFacts struct {
	connected      bool
	connectError   string
	runningVersion string
	compiledOn     string
	deviceErrors   []string
	deviceWarnings []string
	failedComps    []string
	expect         []string
	found          map[string]bool
}

const maxDeviceLines = 15

func newLogFacts(expect []string) *logFacts {
	return &logFacts{expect: expect, found: map[string]bool{}}
}

func (f *logFacts) observe(line string) {
	if handshakeRe.MatchString(line) {
		f.connected = true
		f.connectError = ""
	}
	if !f.connected && connectFailRe.MatchString(line) {
		f.connectError = line
	}
	if m := versionRe.FindStringSubmatch(line); m != nil {
		f.connected = true
		f.runningVersion = m[1]
		f.compiledOn = m[2]
	}
	if strings.Contains(line, "marked as failed") || strings.Contains(line, "is marked FAILED") {
		f.failedComps = appendCapped(f.failedComps, line)
	}
	if deviceErrRe.MatchString(line) {
		f.deviceErrors = appendCapped(f.deviceErrors, line)
	} else if deviceWarnRe.MatchString(line) {
		f.deviceWarnings = appendCapped(f.deviceWarnings, line)
	}
	lower := strings.ToLower(line)
	for _, e := range f.expect {
		if !f.found[e] && strings.Contains(lower, strings.ToLower(e)) {
			f.found[e] = true
		}
	}
}

func appendCapped(list []string, line string) []string {
	if len(list) >= maxDeviceLines {
		return list
	}
	return append(list, line)
}

// complete reports whether the capture has everything it was looking for:
// the version banner and every expected string. Used to end a capture early.
func (f *logFacts) complete() bool {
	if f.runningVersion == "" {
		return false
	}
	for _, e := range f.expect {
		if !f.found[e] {
			return false
		}
	}
	return true
}

func (f *logFacts) missing() []string {
	var out []string
	for _, e := range f.expect {
		if !f.found[e] {
			out = append(out, e)
		}
	}
	return out
}

// logSummary renders the facts for a tool result.
func (f *logFacts) summary() map[string]any {
	out := map[string]any{"connected": f.connected}
	if f.connectError != "" {
		out["connect_error"] = f.connectError
	}
	if f.runningVersion != "" {
		out["running_version"] = f.runningVersion
		out["compiled_on"] = f.compiledOn
	}
	if len(f.failedComps) > 0 {
		out["failed_components"] = f.failedComps
	}
	if len(f.deviceErrors) > 0 {
		out["device_errors"] = f.deviceErrors
	}
	if len(f.deviceWarnings) > 0 {
		out["device_warnings"] = f.deviceWarnings
	}
	return out
}

// verifyInput is everything a verdict is computed from.
type verifyInput struct {
	facts *logFacts
	// dashboardVersion is the ESPHome version the dashboard compiles with
	// (the device record's current_version).
	dashboardVersion string
	// buildStart/buildEnd bound when the expected build ran (the compile
	// job's started/completed times); zero when no job was given.
	buildStart, buildEnd time.Time
	queuedUpdate         bool
}

// buildSlack absorbs clock skew between the job timestamps (dashboard
// process) and the build time baked into the firmware (same host, but
// stamped at code generation inside the job).
const buildSlack = 2 * time.Minute

type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// verdict decides whether the device runs the expected build, and what to do
// if not.
func verdict(in verifyInput) (verified bool, checks []check, d *Diagnosis) {
	f := in.facts
	add := func(name string, ok bool, detail string) {
		checks = append(checks, check{Name: name, OK: ok, Detail: detail})
	}

	if !f.connected {
		add("device_answers", false, f.connectError)
		next := "The device did not answer within the capture window. If it was just flashed it may still be rebooting: wait 30-60 seconds and verify again. If it stays silent it is offline, or crash-looping on the new firmware (after repeated failed boots ESPHome enters safe mode, which still answers OTA); check power and Wi-Fi, and consider re-flashing the previous config."
		if in.queuedUpdate {
			next = "The device is offline and the dashboard has an update queued for it (it flashes when the device reconnects). Verify again after the device comes back online."
		}
		return false, checks, &Diagnosis{Kind: "device_not_answering", Next: next}
	}
	add("device_answers", true, "")

	if f.runningVersion == "" {
		add("version_reported", false, "connected, but the device's startup banner was not seen in the capture window")
		return false, checks, &Diagnosis{Kind: "inconclusive", Next: "The device answered but did not print its version banner before the capture ended. Verify again with a longer timeout_seconds."}
	}
	add("version_reported", true, f.runningVersion+" compiled on "+f.compiledOn)

	verified = true
	var problems []*Diagnosis
	if in.dashboardVersion != "" {
		ok := f.runningVersion == in.dashboardVersion
		detail := "device runs " + f.runningVersion + ", dashboard builds " + in.dashboardVersion
		add("version_matches_dashboard", ok, detail)
		if !ok {
			verified = false
			problems = append(problems, &Diagnosis{Kind: "old_firmware", Next: "The device runs firmware built by a different ESPHome version than the dashboard's, so the latest build is not on it. Check the upload job's result (get_esphome_job); if it failed or was never run, upload_esphome again."})
		}
	}

	if !in.buildStart.IsZero() {
		built, zoned, ok := parseCompiledOn(f.compiledOn)
		switch {
		case !ok || !zoned:
			add("built_by_compile_job", false, "cannot compare: the device's build time has no time zone ("+f.compiledOn+")")
		case built.Before(in.buildStart.Add(-buildSlack)):
			add("built_by_compile_job", false, "device firmware was built "+built.UTC().Format(time.RFC3339)+", before the compile job started at "+in.buildStart.UTC().Format(time.RFC3339))
			verified = false
			problems = append(problems, &Diagnosis{Kind: "old_firmware", Next: "The device still runs a build from before this compile job, so the flash did not take effect. Check the upload job with get_esphome_job; if it failed, follow its next_action; if it succeeded, the device may have rolled back to its previous image on a failed boot (check device_errors), so look for a crash cause before uploading again."})
		case !in.buildEnd.IsZero() && built.After(in.buildEnd.Add(buildSlack)):
			add("built_by_compile_job", false, "device firmware was built "+built.UTC().Format(time.RFC3339)+", after the compile job finished; a newer build is on the device")
		default:
			add("built_by_compile_job", true, "")
		}
	}

	if missing := f.missing(); len(missing) > 0 {
		add("expected_text_present", false, "not seen: "+strings.Join(missing, ", "))
		verified = false
		problems = append(problems, &Diagnosis{Kind: "component_missing", Next: "The device runs firmware, but its startup dump did not mention: " + strings.Join(missing, ", ") + ". Check the names match the YAML (the dump prints component names, e.g. a sensor's name:), or verify again with a longer timeout_seconds if the dump was cut short."})
	} else if len(f.expect) > 0 {
		add("expected_text_present", true, "")
	}

	if len(f.failedComps) > 0 {
		add("components_healthy", false, strings.Join(f.failedComps, " | "))
		verified = false
		problems = append(problems, &Diagnosis{Kind: "component_failed", Next: "The new firmware boots, but a component failed setup (failed_components). Typical causes: wrong pin, wrong I2C address, sensor not wired or not powered. Fix the YAML or wiring, then compile and upload again."})
	} else {
		add("components_healthy", true, "")
	}

	if verified {
		return true, checks, &Diagnosis{Kind: "verified", Next: "The device is running the expected build. If device_warnings or device_errors are listed, review them; otherwise the change is live."}
	}
	return false, checks, problems[0]
}
