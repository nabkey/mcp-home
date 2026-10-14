package esphome

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// Fixtures below are shaped exactly like output captured from a live
// dashboard (ESPHome 2026.9.1): literal `\033` escapes, no line terminators
// on validate/log events, concealed values. Real SSIDs/keys are replaced.

func TestCleanLine(t *testing.T) {
	cases := map[string]string{
		`\033[32mINFO ESPHome 2026.9.1\033[0m`:                    "INFO ESPHome 2026.9.1",
		"\x1b[0;32m[I][app:151]: up\x1b[0m\r\n":                   "[I][app:151]: up",
		`[C][wifi:1279]:   SSID: \033[8m'HomeNet'\033[28m\033[0m`: "[C][wifi:1279]:   SSID: <removed>",
		`key: \033[8mabc`: "key: <removed>",
		`Signal strength: -74 dB \033[0;33m▂▄\033[0;37m▆█\033[0m`:                   "Signal strength: -74 dB ▂▄▆█",
		`\033[1;31m[pin] GPIO99 is not a valid pin for this board.\033[0m trailing`: "[pin] GPIO99 is not a valid pin for this board. trailing",
	}
	for in, want := range cases {
		if got := cleanLine(in); got != want {
			t.Errorf("cleanLine(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("x", maxLineBytes*3)
	if got := cleanLine(long); len(got) > maxLineBytes+32 {
		t.Errorf("long line kept %d bytes", len(got))
	}
}

func TestLineBufferTerminators(t *testing.T) {
	var b lineBuffer
	b.add("validate line without terminator")
	b.add("two\nlines\n")
	b.add("crlf line\r\n")
	b.add("Writing at 0x1000 (10 %)\r")
	b.add("Writing at 0x8000 (90 %)\r")
	b.add("Wrote 460080 bytes\n")
	b.add("spinner |\rspinner /\rdone\n")
	got, _ := b.tail()
	want := []string{"validate line without terminator", "two", "lines", "crlf line", "Wrote 460080 bytes", "done"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestLineBufferObservesSettledLinesOnly(t *testing.T) {
	var seen []string
	b := lineBuffer{observe: func(l string) { seen = append(seen, l) }}
	b.add("a\n")
	b.add("50%\r")
	b.add("100%\n")
	b.add("\n")
	if !reflect.DeepEqual(seen, []string{"a", "100%"}) {
		t.Errorf("seen = %q", seen)
	}
}

func TestErrorLines(t *testing.T) {
	yes := []string{
		"ERROR Error while reading config: Invalid YAML syntax",
		"/config/esphome/porch.yaml:40:14: error: 'hum' was not declared in this scope",
		"*** [.pioenvs/porch/src/main.cpp.o] Error 1",
		"========== [FAILED] Took 41.2 seconds ==========",
		"Failed config",
		"region `iram0_0_seg' overflowed by 1234 bytes",
	}
	no := []string{
		"INFO Reading configuration /config/esphome/porch.yaml...",
		"[22:26:34.879][C][wifi:1279]:   Signal strength: -74 dB",
		"WARNING 'ota' password wastes significant flash",
		"  error_count: 3", // a YAML key in the config dump
	}
	for _, l := range yes {
		if !isErrorLine(l) {
			t.Errorf("isErrorLine(%q) = false", l)
		}
	}
	for _, l := range no {
		if isErrorLine(l) {
			t.Errorf("isErrorLine(%q) = true", l)
		}
	}
}

func TestLogFactsFromLiveCapture(t *testing.T) {
	live := []string{
		`\033[32mINFO ESPHome 2026.9.1\033[0m`,
		`\033[32mINFO Reading configuration /config/esphome/doorbell.yaml...\033[0m`,
		`\033[32mINFO Starting log output from 192.168.1.48 using esphome API\033[0m`,
		`\033[32mINFO Successfully resolved doorbell @ 192.168.1.48 in 0.001s\033[0m`,
		`\033[32mINFO Successfully connected to doorbell @ 192.168.1.48 in 0.005s\033[0m`,
		`\033[32mINFO Successful handshake with doorbell @ 192.168.1.48 in 0.456s\033[0m`,
		`[22:26:34.761]\033[0;32m[I][app:151]: ESPHome version 2026.8.0 compiled on 2026-08-20 17:37:30 -0400\033[0m`,
		`[22:26:34.807]\033[0;35m[C][gpio.binary_sensor:016]: GPIO Binary Sensor 'Button'\033[0m`,
	}
	facts := newLogFacts([]string{"binary sensor 'button'"})
	b := lineBuffer{observe: facts.observe}
	for _, l := range live {
		b.add(l)
	}
	if !facts.connected || facts.runningVersion != "2026.8.0" || !facts.complete() {
		t.Fatalf("facts = %+v", facts)
	}
	built, zoned, ok := parseCompiledOn(facts.compiledOn)
	if !ok || !zoned || built.UTC().Format(time.RFC3339) != "2026-08-20T21:37:30Z" {
		t.Errorf("compiled_on %q parsed as %v zoned=%v ok=%v", facts.compiledOn, built, zoned, ok)
	}

	// The dashboard builds with 2026.9.1, so this live device (deployed
	// 2026.8.0) is correctly reported as not running the latest build.
	ok2, _, d := verdict(verifyInput{facts: facts, dashboardVersion: "2026.9.1"})
	if ok2 || d.Kind != "old_firmware" {
		t.Errorf("verdict = %v %v", ok2, d)
	}
}

func TestLogFactsOfflineLiveCapture(t *testing.T) {
	live := []string{
		`\033[32mINFO Starting log output from 192.168.1.219 using esphome API\033[0m`,
		`\033[32mINFO Successfully resolved pool-pump @ 192.168.1.219 in 0.001s\033[0m`,
		`\033[33mWARNING Can't connect to ESPHome API for pool-pump @ 192.168.1.219: Error connecting to [AddrInfo(family=<AddressFamily.AF_INET: 2>, type=<SocketKind.SOCK_STREAM: 1>, proto=6, sockaddr=IPv4Sockaddr(address='192.168.1.219', port=6053))]: [Errno 113] Connect call failed ('192.168.1.219', 6053) (SocketAPIError)\033[0m`,
		`\033[32mINFO Trying to connect to pool-pump @ 192.168.1.219 in the background\033[0m`,
	}
	facts := newLogFacts(nil)
	b := lineBuffer{observe: facts.observe}
	for _, l := range live {
		b.add(l)
	}
	if facts.connected || !strings.Contains(facts.connectError, "Can't connect") {
		t.Fatalf("facts = %+v", facts)
	}
	lines, _ := b.tail()
	if d := diagnoseLines(scopeLogs, nil, lines); d == nil || d.Kind != "device_unreachable" {
		t.Errorf("diagnosis = %v", d)
	}
}

func TestParseCompiledOnLegacy(t *testing.T) {
	if _, zoned, ok := parseCompiledOn("Aug 20 2026, 17:37:30"); !ok || zoned {
		t.Errorf("legacy format: ok=%v zoned=%v", ok, zoned)
	}
	if _, _, ok := parseCompiledOn("garbage"); ok {
		t.Error("garbage parsed")
	}
}

func TestDiagnoseScopes(t *testing.T) {
	// "timed out" in a build is a package download, not an offline device.
	lines := []string{"HTTPClientError: Read timed out"}
	if d := diagnoseLines(scopeBuild, nil, lines); d == nil || d.Kind != "build_network" {
		t.Errorf("build: %v", d)
	}
	if d := diagnoseLines(scopeUpload, nil, []string{"ERROR Error resolving IP address of porch.local. Is it connected to WiFi?"}); d == nil || d.Kind != "device_unreachable" {
		t.Errorf("upload: %v", d)
	}
	// The live "no such file" validate error.
	nf := []string{"ERROR Error while reading config: Invalid YAML syntax:Error reading file /config/esphome/no-such-device.yaml: [Errno 2] No such file or directory: '/config/esphome/no-such-device.yaml'"}
	if d := diagnoseLines(scopeBuild, nf, nf); d == nil || d.Kind != "config_not_found" {
		t.Errorf("not found: %v", d)
	}
}

func TestDiagnoseFailedConfigWithSecretRefs(t *testing.T) {
	// A failed-config dump quotes !secret references; that is not a
	// missing secret.
	lines := []string{
		"Failed config",
		"wifi: [source /config/esphome/porch.yaml:10]",
		"  ssid: !secret 'wifi_ssid'",
		"  power_save_mode: TURBO",
		"  [power_save_mode] Unknown value 'TURBO', valid options are 'NONE', 'LIGHT', 'HIGH'.",
	}
	if d := diagnoseLines(scopeBuild, nil, lines); d == nil || d.Kind != "config_invalid" {
		t.Errorf("diagnosis = %v", d)
	}
}
