package esphome

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// End-to-end tests of the edit -> validate -> compile -> upload -> verify
// pipeline through the MCP tool layer, against a fake dashboard whose state
// (files, jobs, binaries, device) each test arranges. They check what a model
// would see: whether each result names the problem and the next action.

type fakeJob struct {
	Job
	log      []string
	terminal map[string]any // follow_job result frame
}

type fakeDash struct {
	mu       sync.Mutex
	files    map[string]string
	secrets  []string
	jobs     map[string]*fakeJob
	binaries []Binary
	device   map[string]any
	// validate is the output and exit code devices/validate streams.
	validateLines []string
	validateCode  int
	// logLines is what devices/logs streams (then it idles).
	logLines []string
	// calls records commands received, in order.
	calls []string
	// handlers override commands.
	handlers map[string]func(sc *srvConn, c command)
	nextID   int
}

func newFakeDash() *fakeDash {
	return &fakeDash{
		files:    map[string]string{},
		jobs:     map[string]*fakeJob{},
		handlers: map[string]func(*srvConn, command){},
		device: map[string]any{
			"name": "porch", "configuration": "porch.yaml", "current_version": "2026.9.1",
			"runtime_state": map[string]any{"state": "online", "deployed_version": "2026.9.1"},
		},
	}
}

func (f *fakeDash) addJob(j Job, log []string, terminal map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[j.JobID] = &fakeJob{Job: j, log: log, terminal: terminal}
}

func (f *fakeDash) called(cmd string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == cmd {
			return true
		}
	}
	return false
}

func (f *fakeDash) serve(t *testing.T) *httptest.Server {
	up := websocket.Upgrader{}
	var wg sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Add before the upgrade: until the connection is hijacked,
		// srv.Close waits for this handler, so Add cannot race Wait.
		wg.Add(1)
		defer wg.Done()
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		sc := &srvConn{t: t, conn: conn}
		sc.sendInfo(false)
		for {
			var c command
			if err := conn.ReadJSON(&c); err != nil {
				return
			}
			f.mu.Lock()
			f.calls = append(f.calls, c.Command)
			h := f.handlers[c.Command]
			f.mu.Unlock()
			if h != nil {
				h(sc, c)
				continue
			}
			f.dispatch(sc, c)
		}
	}))
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		wg.Wait()
	})
	return srv
}

func (f *fakeDash) dispatch(sc *srvConn, c command) {
	f.mu.Lock()
	defer f.mu.Unlock()
	arg := func(k string) string { s, _ := c.Args[k].(string); return s }
	switch c.Command {
	case "devices/list":
		sc.result(c.MessageID, map[string]any{"configured": []any{f.device}})
	case "config/get_secrets":
		sc.result(c.MessageID, f.secrets)
	case "devices/get_config":
		content, ok := f.files[arg("configuration")]
		if !ok {
			sc.errorMsg(c.MessageID, "not_found", "File not found: "+arg("configuration"))
			return
		}
		sc.result(c.MessageID, content)
	case "devices/update_config":
		cfg := arg("configuration")
		if exp, ok := c.Args["expected"].(string); ok && f.files[cfg] != exp {
			sc.errorMsg(c.MessageID, "precondition_failed", cfg+" changed since it was read")
			return
		}
		f.files[cfg] = arg("content")
		sc.result(c.MessageID, nil)
	case "devices/validate":
		for _, l := range f.validateLines {
			sc.output(c.MessageID, l)
		}
		sc.streamResult(c.MessageID, f.validateCode == 0, f.validateCode)
		sc.result(c.MessageID, nil)
	case "devices/logs":
		for _, l := range f.logLines {
			sc.output(c.MessageID, l)
		}
	case "firmware/get_binaries":
		sc.result(c.MessageID, f.binaries)
	case "firmware/compile", "firmware/upload":
		f.nextID++
		typ := strings.TrimPrefix(c.Command, "firmware/")
		j := &fakeJob{Job: Job{
			JobID: fmt.Sprintf("job%d", f.nextID), Configuration: arg("configuration"), JobType: typ,
			Status: "queued", CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), Port: arg("port"),
		}}
		f.jobs[j.JobID] = j
		sc.result(c.MessageID, j.Job)
	case "firmware/get_job":
		j, ok := f.jobs[arg("job_id")]
		if !ok {
			sc.result(c.MessageID, nil) // what upstream does for an unknown id
			return
		}
		out := j.Job
		if out.Terminal() {
			out.Output = nil
		} else {
			out.Output = j.log
		}
		sc.result(c.MessageID, out)
	case "firmware/get_jobs":
		var list []Job
		for _, j := range f.jobs {
			if j.Configuration == arg("configuration") {
				list = append(list, j.Job)
			}
		}
		sc.result(c.MessageID, list)
	case "firmware/follow_job":
		j, ok := f.jobs[arg("job_id")]
		if !ok {
			sc.errorMsg(c.MessageID, "internal_error", "Command failed: firmware/follow_job")
			return
		}
		for _, l := range j.log {
			sc.output(c.MessageID, l)
		}
		term := j.terminal
		if term == nil {
			term = map[string]any{"status": j.Status, "exit_code": j.ExitCode, "error": nil, "queued_update_armed": false}
		}
		_ = sc.conn.WriteJSON(map[string]any{"message_id": c.MessageID, "event": "result", "data": term})
		sc.result(c.MessageID, nil)
	case "firmware/cancel":
		j, ok := f.jobs[arg("job_id")]
		if !ok {
			sc.errorMsg(c.MessageID, "not_found", "Job not found")
			return
		}
		j.Status = "cancelled"
		sc.result(c.MessageID, nil)
	default:
		sc.errorMsg(c.MessageID, "unknown_command", "Unknown command: "+c.Command)
	}
}

// session connects an MCP client (no elicitation, so Confirm proceeds) to
// the ESPHome tools backed by the fake dashboard.
func (f *fakeDash) session(t *testing.T) *mcp.ClientSession {
	t.Helper()
	shrink(t, &verifyGrace, 300*time.Millisecond)
	srv := f.serve(t)
	tools, err := NewTools(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	tools.Register(server)
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close(); _ = ss.Wait() })
	return cs
}

// call invokes a tool and returns its text and whether it is an error.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

// callJSON invokes a tool that must succeed and decodes its JSON result.
func callJSON(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	text, isErr := call(t, cs, name, args)
	if isErr {
		t.Fatalf("%s returned an error: %s", name, text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s: not JSON: %s", name, text)
	}
	return out
}

func intp(i int) *int { return &i }

// --- edit ---

func TestWriteRefusesConcurrentEdit(t *testing.T) {
	f := newFakeDash()
	f.files["porch.yaml"] = "esphome:\n  name: porch\n"
	// Someone edits the file in the dashboard between our read and write.
	f.handlers["devices/get_config"] = func(sc *srvConn, c command) {
		sc.result(c.MessageID, "esphome:\n  name: porch\n")
		f.mu.Lock()
		f.files["porch.yaml"] = "esphome:\n  name: porch\n# user edit\n"
		f.mu.Unlock()
	}
	cs := f.session(t)
	text, isErr := call(t, cs, "write_esphome_file", map[string]any{"file": "porch.yaml", "content": "esphome:\n  name: porch2\n"})
	if !isErr || !strings.Contains(text, "precondition_failed") || !strings.Contains(text, "read_esphome_file") {
		t.Fatalf("got %v %s", isErr, text)
	}
	if strings.Contains(f.files["porch.yaml"], "porch2") {
		t.Error("the concurrent edit was overwritten")
	}
}

func TestWriteReportsMissingSecrets(t *testing.T) {
	f := newFakeDash()
	f.secrets = []string{"wifi_ssid", "wifi_password"}
	cs := f.session(t)
	out := callJSON(t, cs, "write_esphome_file", map[string]any{
		"file":    "porch.yaml",
		"content": "wifi:\n  ssid: !secret wifi_ssid\n  password: !secret wifi_password\napi:\n  encryption:\n    key: !secret porch_api_key\n",
	})
	missing, _ := out["missing_secrets"].([]any)
	if len(missing) != 1 || missing[0] != "porch_api_key" {
		t.Errorf("missing_secrets = %v", out["missing_secrets"])
	}
	if !strings.Contains(fmt.Sprint(out["next_action"]), "list_esphome_secrets") {
		t.Errorf("next_action = %v", out["next_action"])
	}
}

// --- validate ---

func TestValidateFailureNamesErrorAndNextStep(t *testing.T) {
	f := newFakeDash()
	f.validateLines = []string{
		`\033[32mINFO ESPHome 2026.9.1\033[0m`,
		`\033[32mINFO Reading configuration /config/esphome/porch.yaml...\033[0m`,
		`Failed config`,
		``,
		`sensor.dht: [source /config/esphome/porch.yaml:21]`,
		`  platform: dht`,
		`  pin: GPIO99`,
		`  \033[1;31m[pin] GPIO99 is not a valid pin for this board.\033[0m`,
	}
	f.validateCode = 2
	cs := f.session(t)
	out := callJSON(t, cs, "validate_esphome", map[string]any{"config": "porch.yaml"})
	if out["valid"] != false || out["diagnosis"] != "config_invalid" {
		t.Fatalf("out = %v", out)
	}
	output := fmt.Sprint(out["output"])
	if !strings.Contains(output, "sensor.dht: [source") || !strings.Contains(output, "\n  pin: GPIO99") {
		t.Errorf("output lost its lines: %q", output)
	}
	if strings.Contains(output, `\033`) {
		t.Errorf("ANSI escapes left in output: %q", output)
	}
	if !strings.Contains(fmt.Sprint(out["next_action"]), "write_esphome_file") {
		t.Errorf("next_action = %v", out["next_action"])
	}
}

func TestValidateSuccessDropsConfigDump(t *testing.T) {
	f := newFakeDash()
	f.validateLines = []string{
		`\033[32mINFO ESPHome 2026.9.1\033[0m`,
		`\033[33mWARNING 'ota' password wastes significant flash\033[0m`,
		`esphome:`,
		`  name: porch`,
		`api:`,
		`  encryption:`,
		`    key: <removed>`,
		`\033[32mINFO Configuration is valid!\033[0m`,
	}
	cs := f.session(t)
	out := callJSON(t, cs, "validate_esphome", map[string]any{"config": "porch.yaml"})
	if out["valid"] != true {
		t.Fatalf("out = %v", out)
	}
	output := fmt.Sprint(out["output"])
	if strings.Contains(output, "name: porch") {
		t.Errorf("config dump kept on success: %q", output)
	}
	if w, _ := out["warnings"].([]any); len(w) != 1 {
		t.Errorf("warnings = %v", out["warnings"])
	}
	if !strings.Contains(fmt.Sprint(out["next_action"]), "compile_esphome") {
		t.Errorf("next_action = %v", out["next_action"])
	}
}

func TestValidateMissingSecretDiagnosed(t *testing.T) {
	f := newFakeDash()
	f.validateLines = []string{`\033[31mERROR Error while reading config: Invalid YAML syntax:\n\nSecret 'porch_api_key' not defined\033[0m`}
	f.validateCode = 2
	cs := f.session(t)
	out := callJSON(t, cs, "validate_esphome", map[string]any{"config": "porch.yaml"})
	if out["diagnosis"] != "missing_secret" {
		t.Fatalf("diagnosis = %v (%v)", out["diagnosis"], out)
	}
}

// --- compile / upload guards ---

func TestUploadRefusedWhileCompileRuns(t *testing.T) {
	f := newFakeDash()
	f.binaries = []Binary{{File: "firmware.ota.bin", Type: "ota"}}
	f.addJob(Job{JobID: "c1", Configuration: "porch.yaml", JobType: "compile", Status: "running", CreatedAt: "2026-10-14T10:00:00Z"}, nil, nil)
	cs := f.session(t)
	text, isErr := call(t, cs, "upload_esphome", map[string]any{"config": "porch.yaml"})
	if !isErr || !strings.Contains(text, "c1") || !strings.Contains(text, "get_esphome_job") {
		t.Fatalf("got %v %s", isErr, text)
	}
	if f.called("firmware/upload") {
		t.Error("upload was submitted anyway")
	}
}

func TestUploadRefusedAfterFailedCompile(t *testing.T) {
	f := newFakeDash()
	f.binaries = []Binary{{File: "firmware.ota.bin", Type: "ota"}}
	f.addJob(Job{JobID: "c0", Configuration: "porch.yaml", JobType: "compile", Status: "completed", ExitCode: intp(0), CreatedAt: "2026-10-14T09:00:00Z"}, nil, nil)
	f.addJob(Job{JobID: "c1", Configuration: "porch.yaml", JobType: "compile", Status: "failed", ExitCode: intp(1), CreatedAt: "2026-10-14T10:00:00Z"}, nil, nil)
	cs := f.session(t)
	text, isErr := call(t, cs, "upload_esphome", map[string]any{"config": "porch.yaml"})
	if !isErr || !strings.Contains(text, "c1") || !strings.Contains(text, "older build") {
		t.Fatalf("got %v %s", isErr, text)
	}
}

func TestUploadRefusedWithoutBinary(t *testing.T) {
	f := newFakeDash()
	cs := f.session(t)
	text, isErr := call(t, cs, "upload_esphome", map[string]any{"config": "porch.yaml"})
	if !isErr || !strings.Contains(text, "compile_esphome") {
		t.Fatalf("got %v %s", isErr, text)
	}
}

func TestUploadAfterGoodCompile(t *testing.T) {
	f := newFakeDash()
	f.binaries = []Binary{{File: "firmware.ota.bin", Type: "ota"}}
	f.device["runtime_state"] = map[string]any{"state": "offline"}
	f.addJob(Job{JobID: "c1", Configuration: "porch.yaml", JobType: "compile", Status: "completed", ExitCode: intp(0), CreatedAt: "2026-10-14T10:00:00Z"}, nil, nil)
	cs := f.session(t)
	out := callJSON(t, cs, "upload_esphome", map[string]any{"config": "porch.yaml"})
	if out["job_type"] != "upload" || out["done"] != false || out["poll_after_seconds"] == nil {
		t.Fatalf("out = %v", out)
	}
	if !strings.Contains(fmt.Sprint(out["notes"]), "offline") {
		t.Errorf("offline device not flagged: %v", out["notes"])
	}
}

func TestCompileRefusedWhileFlashing(t *testing.T) {
	f := newFakeDash()
	f.addJob(Job{JobID: "u1", Configuration: "porch.yaml", JobType: "upload", Status: "running", CreatedAt: "2026-10-14T10:00:00Z"}, nil, nil)
	cs := f.session(t)
	text, isErr := call(t, cs, "compile_esphome", map[string]any{"config": "porch.yaml"})
	if !isErr || !strings.Contains(text, "u1") || f.called("firmware/compile") {
		t.Fatalf("got %v %s", isErr, text)
	}
}

func TestCompileReportsReplacedJob(t *testing.T) {
	f := newFakeDash()
	f.addJob(Job{JobID: "c1", Configuration: "porch.yaml", JobType: "compile", Status: "queued", CreatedAt: "2026-10-14T10:00:00Z"}, nil, nil)
	cs := f.session(t)
	out := callJSON(t, cs, "compile_esphome", map[string]any{"config": "porch.yaml"})
	if r, _ := out["replaced_jobs"].([]any); len(r) != 1 || r[0] != "c1" {
		t.Errorf("replaced_jobs = %v", out["replaced_jobs"])
	}
}

// --- polling ---

func TestGetJobUnknownIsAnError(t *testing.T) {
	f := newFakeDash()
	cs := f.session(t)
	text, isErr := call(t, cs, "get_esphome_job", map[string]any{"job_id": "deadbeef"})
	if !isErr || !strings.Contains(text, "no job with that id") {
		t.Fatalf("got %v %s", isErr, text)
	}
}

func TestGetJobRunningAfterRestart(t *testing.T) {
	f := newFakeDash()
	f.addJob(Job{JobID: "c1", Configuration: "porch.yaml", JobType: "compile", Status: "running", Progress: intp(40)},
		[]string{"Compiling...\n", "... [dashboard restarted mid-build; the previous run's log is above, the rebuild begins below] ...\n", "Linking .pioenvs/porch/firmware.elf\n"}, nil)
	cs := f.session(t)
	out := callJSON(t, cs, "get_esphome_job", map[string]any{"job_id": "c1"})
	if out["done"] != false || out["restarted"] != true || out["poll_after_seconds"] == nil {
		t.Fatalf("out = %v", out)
	}
	if !strings.Contains(fmt.Sprint(out["recent_output"]), "Linking") {
		t.Errorf("recent_output = %v", out["recent_output"])
	}
}

func TestGetJobCompileError(t *testing.T) {
	f := newFakeDash()
	f.addJob(Job{JobID: "c1", Configuration: "porch.yaml", JobType: "compile", Status: "failed", ExitCode: intp(1)},
		[]string{
			"INFO Reading configuration /config/esphome/porch.yaml...\n",
			"Compiling .pioenvs/porch/src/main.cpp.o\n",
			"/config/esphome/porch.yaml: In lambda function:\n",
			"/config/esphome/porch.yaml:40:14: error: 'hum' was not declared in this scope\n",
			"*** [.pioenvs/porch/src/main.cpp.o] Error 1\n",
			"========================= [FAILED] Took 41.2 seconds =========================\n",
		}, nil)
	cs := f.session(t)
	out := callJSON(t, cs, "get_esphome_job", map[string]any{"job_id": "c1"})
	if out["success"] != false || out["diagnosis"] != "compile_error" {
		t.Fatalf("out = %v", out)
	}
	errs, _ := out["errors"].([]any)
	if len(errs) == 0 || !strings.Contains(fmt.Sprint(errs[0]), "'hum' was not declared") {
		t.Errorf("errors = %v", out["errors"])
	}
}

func TestGetJobUploadOfflineDeferred(t *testing.T) {
	f := newFakeDash()
	f.addJob(Job{JobID: "u1", Configuration: "porch.yaml", JobType: "upload", Status: "failed", ExitCode: intp(1)},
		[]string{"ERROR Error resolving IP address of porch.local. Is it connected to WiFi?\n"},
		map[string]any{"status": "failed", "exit_code": 1, "error": nil, "queued_update_armed": true})
	cs := f.session(t)
	out := callJSON(t, cs, "get_esphome_job", map[string]any{"job_id": "u1"})
	if out["diagnosis"] != "deferred_offline" || out["queued_update_armed"] != true {
		t.Fatalf("out = %v", out)
	}
}

func TestGetJobUploadUnreachable(t *testing.T) {
	f := newFakeDash()
	f.addJob(Job{JobID: "u1", Configuration: "porch.yaml", JobType: "upload", Status: "failed", ExitCode: intp(1)},
		[]string{
			"INFO Resolving IP address of porch.local\n",
			"WARNING Connecting to 192.168.1.50 port 3232 failed: [Errno 113] No route to host\n",
			"ERROR Upload failed after 3 attempts: Connecting to 192.168.1.50 port 3232 failed\n",
		}, nil)
	cs := f.session(t)
	out := callJSON(t, cs, "get_esphome_job", map[string]any{"job_id": "u1"})
	if out["diagnosis"] != "device_unreachable" {
		t.Fatalf("out = %v", out)
	}
}

func TestGetJobUploadInterruptedAfterTransfer(t *testing.T) {
	f := newFakeDash()
	f.addJob(Job{JobID: "u1", Configuration: "porch.yaml", JobType: "upload", Status: "failed", ExitCode: intp(1)},
		[]string{
			"Uploading: [============================================================] 100% Done...\n",
			"ERROR receiving result response: timed out (the device may have already committed the update and be rebooting; check whether it comes back with the new firmware before uploading again)\n",
		}, nil)
	cs := f.session(t)
	out := callJSON(t, cs, "get_esphome_job", map[string]any{"job_id": "u1"})
	if out["diagnosis"] != "ota_outcome_unknown" || !strings.Contains(fmt.Sprint(out["next_action"]), "verify_esphome_device") {
		t.Fatalf("out = %v", out)
	}
}

func TestGetJobProgressCollapsed(t *testing.T) {
	f := newFakeDash()
	f.addJob(Job{JobID: "u1", Configuration: "porch.yaml", JobType: "upload", Status: "completed", ExitCode: intp(0)},
		[]string{"Uploading: [=     ] 10%\r", "Uploading: [===   ] 50%\r", "Uploading: [======] 100% Done...\n", "INFO OTA successful\n"}, nil)
	cs := f.session(t)
	out := callJSON(t, cs, "get_esphome_job", map[string]any{"job_id": "u1"})
	output := fmt.Sprint(out["output"])
	if strings.Contains(output, "10%") || !strings.Contains(output, "100% Done") {
		t.Errorf("progress redraws not collapsed: %q", output)
	}
	if out["diagnosis"] != "flashed" || !strings.Contains(fmt.Sprint(out["next_action"]), "verify_esphome_device") {
		t.Errorf("out = %v", out)
	}
}

func TestGetJobSuperseded(t *testing.T) {
	f := newFakeDash()
	f.addJob(Job{JobID: "c1", Configuration: "porch.yaml", JobType: "compile", Status: "cancelled", CreatedAt: "2026-10-14T10:00:00Z"}, nil, nil)
	f.addJob(Job{JobID: "u2", Configuration: "porch.yaml", JobType: "upload", Status: "running", CreatedAt: "2026-10-14T10:01:00Z"}, nil, nil)
	cs := f.session(t)
	out := callJSON(t, cs, "get_esphome_job", map[string]any{"job_id": "c1"})
	if out["diagnosis"] != "superseded" || !strings.Contains(fmt.Sprint(out["next_action"]), "u2") {
		t.Fatalf("out = %v", out)
	}
}

func TestGetJobLogUnavailable(t *testing.T) {
	f := newFakeDash()
	f.addJob(Job{JobID: "c1", Configuration: "porch.yaml", JobType: "compile", Status: "failed", ExitCode: intp(1)}, nil, nil)
	f.handlers["firmware/follow_job"] = func(sc *srvConn, c command) {
		_ = sc.conn.Close() // dashboard restarts while replaying
	}
	cs := f.session(t)
	out := callJSON(t, cs, "get_esphome_job", map[string]any{"job_id": "c1"})
	if out["done"] != true || out["success"] != false {
		t.Fatalf("out = %v", out)
	}
	if out["output_incomplete"] == nil && out["output_unavailable"] == nil {
		t.Errorf("missing log not reported: %v", out)
	}
}

// --- verify ---

const verifyCompileStart = "2026-10-14T10:00:00Z"

func verifyFake(logLines []string) *fakeDash {
	f := newFakeDash()
	f.addJob(Job{JobID: "c1", Configuration: "porch.yaml", JobType: "compile", Status: "completed", ExitCode: intp(0),
		CreatedAt: "2026-10-14T09:59:59Z", StartedAt: verifyCompileStart, CompletedAt: "2026-10-14T10:03:00Z"}, nil, nil)
	f.addJob(Job{JobID: "u1", Configuration: "porch.yaml", JobType: "upload", Status: "completed", ExitCode: intp(0),
		CreatedAt: "2026-10-14T10:04:00Z"}, nil, nil)
	f.logLines = logLines
	return f
}

func deviceBoot(compiledOn string, extra ...string) []string {
	return append([]string{
		`\033[32mINFO Starting log output from 192.168.1.50 using esphome API\033[0m`,
		`\033[32mINFO Successful handshake with porch @ 192.168.1.50 in 0.4s\033[0m`,
		`[10:05:01.100]\033[0;32m[I][app:151]: ESPHome version 2026.9.1 compiled on ` + compiledOn + `\033[0m`,
		`[10:05:01.101]\033[0;35m[C][dht:123]: DHT:\033[0m`,
		`[10:05:01.102]\033[0;35m[C][dht:124]:   Temperature 'Porch Temperature'\033[0m`,
		`[10:05:01.103]\033[0;35m[C][dht:125]:   Humidity 'Porch Humidity'\033[0m`,
	}, extra...)
}

func TestVerifyNewBuild(t *testing.T) {
	f := verifyFake(deviceBoot("2026-10-14 06:01:30 -0400"))
	cs := f.session(t)
	out := callJSON(t, cs, "verify_esphome_device", map[string]any{
		"config": "porch.yaml", "compile_job_id": "u1", "expect": []string{"porch humidity"}, "timeout_seconds": 5,
	})
	if out["verified"] != true || out["compile_job_id"] != "c1" {
		t.Fatalf("out = %v", out)
	}
}

func TestVerifyStopsEarly(t *testing.T) {
	// The fake's log stream never ends; verify must return once it has seen
	// what it needs rather than wait out the window.
	f := verifyFake(deviceBoot("2026-10-14 06:01:30 -0400"))
	cs := f.session(t)
	start := time.Now()
	callJSON(t, cs, "verify_esphome_device", map[string]any{"config": "porch.yaml", "timeout_seconds": 30})
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("verify took %s; it should stop once the banner is seen", el)
	}
}

func TestVerifyOldBuild(t *testing.T) {
	f := verifyFake(deviceBoot("2026-10-01 06:01:30 -0400"))
	cs := f.session(t)
	out := callJSON(t, cs, "verify_esphome_device", map[string]any{"config": "porch.yaml", "compile_job_id": "c1", "timeout_seconds": 5})
	if out["verified"] != false || out["diagnosis"] != "old_firmware" {
		t.Fatalf("out = %v", out)
	}
}

func TestVerifyMissingComponent(t *testing.T) {
	f := verifyFake(deviceBoot("2026-10-14 06:01:30 -0400"))
	cs := f.session(t)
	out := callJSON(t, cs, "verify_esphome_device", map[string]any{"config": "porch.yaml", "expect": []string{"Porch Pressure"}, "timeout_seconds": 2})
	if out["verified"] != false || out["diagnosis"] != "component_missing" {
		t.Fatalf("out = %v", out)
	}
}

func TestVerifyFailedComponent(t *testing.T) {
	f := verifyFake(deviceBoot("2026-10-14 06:01:30 -0400",
		`[10:05:01.200]\033[1;31m[E][component:204]: dht.sensor was marked as failed\033[0m`))
	cs := f.session(t)
	out := callJSON(t, cs, "verify_esphome_device", map[string]any{"config": "porch.yaml", "expect": []string{"marked as failed"}, "timeout_seconds": 3})
	if out["verified"] != false || out["diagnosis"] != "component_failed" {
		t.Fatalf("out = %v", out)
	}
}

func TestVerifyDeviceSilent(t *testing.T) {
	f := verifyFake([]string{
		`\033[32mINFO Starting log output from 192.168.1.50 using esphome API\033[0m`,
		`\033[33mWARNING Can't connect to ESPHome API for porch @ 192.168.1.50: [Errno 113] Connect call failed\033[0m`,
		`\033[32mINFO Trying to connect to porch @ 192.168.1.50 in the background\033[0m`,
	})
	cs := f.session(t)
	out := callJSON(t, cs, "verify_esphome_device", map[string]any{"config": "porch.yaml", "timeout_seconds": 1})
	if out["verified"] != false || out["diagnosis"] != "device_not_answering" {
		t.Fatalf("out = %v", out)
	}
	if !strings.Contains(fmt.Sprint(out["device"]), "Can't connect") {
		t.Errorf("connect error not surfaced: %v", out["device"])
	}
}

func TestVerifyRefusesUnfinishedCompile(t *testing.T) {
	f := newFakeDash()
	f.addJob(Job{JobID: "c1", Configuration: "porch.yaml", JobType: "compile", Status: "running"}, nil, nil)
	cs := f.session(t)
	text, isErr := call(t, cs, "verify_esphome_device", map[string]any{"config": "porch.yaml", "compile_job_id": "c1"})
	if !isErr || !strings.Contains(text, "running") {
		t.Fatalf("got %v %s", isErr, text)
	}
}

func TestLogsOfflineDevice(t *testing.T) {
	f := newFakeDash()
	f.logLines = []string{
		`\033[32mINFO Starting log output from 192.168.1.219 using esphome API\033[0m`,
		`\033[33mWARNING Can't connect to ESPHome API for pool-pump @ 192.168.1.219: [Errno 113] Connect call failed ('192.168.1.219', 6053) (SocketAPIError)\033[0m`,
	}
	cs := f.session(t)
	out := callJSON(t, cs, "get_esphome_logs", map[string]any{"config": "porch.yaml", "timeout_seconds": 1})
	if out["connected"] != false || out["diagnosis"] != "device_unreachable" || out["connect_error"] == nil {
		t.Fatalf("out = %v", out)
	}
}

// --- the whole path ---

func TestPipelineHappyPath(t *testing.T) {
	// A model taking a device from "add humidity" to verified, using only
	// what each tool returns to decide the next call.
	f := newFakeDash()
	f.files["porch.yaml"] = "esphome:\n  name: porch\n"
	f.secrets = []string{"wifi_ssid", "wifi_password"}
	f.validateLines = []string{`\033[32mINFO Configuration is valid!\033[0m`}
	f.binaries = []Binary{{File: "firmware.ota.bin", Type: "ota"}}
	cs := f.session(t)

	w := callJSON(t, cs, "write_esphome_file", map[string]any{"file": "porch.yaml", "content": "esphome:\n  name: porch\nsensor:\n  - platform: dht\n    humidity:\n      name: Porch Humidity\n"})
	if !strings.Contains(fmt.Sprint(w["next_action"]), "validate_esphome") {
		t.Fatalf("write: %v", w)
	}
	v := callJSON(t, cs, "validate_esphome", map[string]any{"config": "porch.yaml"})
	if v["valid"] != true {
		t.Fatalf("validate: %v", v)
	}
	c := callJSON(t, cs, "compile_esphome", map[string]any{"config": "porch.yaml"})
	compileID := c["job_id"].(string)

	// The build runs and finishes.
	now := time.Now().UTC()
	f.mu.Lock()
	j := f.jobs[compileID]
	j.Status, j.ExitCode = "completed", intp(0)
	j.StartedAt = now.Add(-time.Minute).Format(time.RFC3339Nano)
	j.CompletedAt = now.Format(time.RFC3339Nano)
	j.log = []string{"INFO Successfully compiled program.\n"}
	f.mu.Unlock()
	g := callJSON(t, cs, "get_esphome_job", map[string]any{"job_id": compileID})
	if g["success"] != true || !strings.Contains(fmt.Sprint(g["next_action"]), "upload_esphome") {
		t.Fatalf("get compile: %v", g)
	}

	u := callJSON(t, cs, "upload_esphome", map[string]any{"config": "porch.yaml"})
	uploadID := u["job_id"].(string)
	f.mu.Lock()
	f.jobs[uploadID].Status, f.jobs[uploadID].ExitCode = "completed", intp(0)
	f.jobs[uploadID].log = []string{"INFO OTA successful\n"}
	f.logLines = deviceBoot(now.Add(-30 * time.Second).Format("2006-01-02 15:04:05 -0700"))
	f.mu.Unlock()
	g = callJSON(t, cs, "get_esphome_job", map[string]any{"job_id": uploadID})
	if !strings.Contains(fmt.Sprint(g["next_action"]), "verify_esphome_device") {
		t.Fatalf("get upload: %v", g)
	}
	vr := callJSON(t, cs, "verify_esphome_device", map[string]any{"config": "porch.yaml", "compile_job_id": uploadID, "expect": []string{"Porch Humidity"}, "timeout_seconds": 5})
	if vr["verified"] != true {
		t.Fatalf("verify: %v", vr)
	}
}
