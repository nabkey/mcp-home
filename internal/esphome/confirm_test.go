package esphome

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func callToolReq(caps *mcp.ClientCapabilities, responses mcp.InputResponseMap) *mcp.CallToolRequest {
	params := &mcp.CallToolParamsRaw{Name: "upload_esphome", InputResponses: responses}
	if caps != nil {
		params.SetMeta(map[string]any{mcp.MetaKeyClientCapabilities: caps})
	}
	return &mcp.ServerRequest[*mcp.CallToolParamsRaw]{Params: params}
}

func formCaps() *mcp.ClientCapabilities {
	return &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{Form: &mcp.FormElicitationCapabilities{}}}
}

func TestDecideFlashAsksWhenClientCanElicit(t *testing.T) {
	if got := decideFlash(callToolReq(formCaps(), nil)); got != flashAsk {
		t.Errorf("decideFlash = %v, want flashAsk", got)
	}
}

func TestDecideFlashProceedsWhenClientCannotElicit(t *testing.T) {
	cases := map[string]*mcp.ClientCapabilities{
		"no capabilities":      nil,
		"no elicitation":       {},
		"url elicitation only": {Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}},
	}
	for name, caps := range cases {
		t.Run(name, func(t *testing.T) {
			if got := decideFlash(callToolReq(caps, nil)); got != flashProceed {
				t.Errorf("decideFlash = %v, want flashProceed", got)
			}
		})
	}
}

func TestDecideFlashUnnamedElicitationModeCountsAsForm(t *testing.T) {
	caps := &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{}}
	if got := decideFlash(callToolReq(caps, nil)); got != flashAsk {
		t.Errorf("decideFlash = %v, want flashAsk", got)
	}
}

func TestDecideFlashReadsAnswer(t *testing.T) {
	cases := []struct {
		name string
		resp mcp.InputResponse
		want flashDecision
	}{
		{"accepted", &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, flashProceed},
		{"accepted but false", &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": false}}, flashDeclined},
		{"accepted without content", &mcp.ElicitResult{Action: "accept"}, flashDeclined},
		{"declined", &mcp.ElicitResult{Action: "decline"}, flashDeclined},
		{"cancelled", &mcp.ElicitResult{Action: "cancel"}, flashDeclined},
		{"unreadable response", nil, flashDeclined},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := callToolReq(formCaps(), mcp.InputResponseMap{confirmFlashKey: tc.resp})
			if got := decideFlash(req); got != tc.want {
				t.Errorf("decideFlash = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDecideFlashDeclinesUnrelatedAnswer(t *testing.T) {
	req := callToolReq(formCaps(), mcp.InputResponseMap{
		"something_else": &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}},
	})
	if got := decideFlash(req); got != flashDeclined {
		t.Errorf("decideFlash = %v, want flashDeclined", got)
	}
}

func TestFlashConfirmationRequestNamesTheDevice(t *testing.T) {
	res := flashConfirmationRequest(uploadArgs{Config: "pump.yaml", Port: "/dev/ttyUSB0"})
	if res.RequestState != "flash:pump.yaml" {
		t.Errorf("RequestState = %q", res.RequestState)
	}
	if len(res.Content) != 0 {
		t.Errorf("an input request must carry no content, got %d blocks", len(res.Content))
	}
	elicit, ok := res.InputRequests[confirmFlashKey].(*mcp.ElicitParams)
	if !ok {
		t.Fatalf("InputRequests[%s] = %T", confirmFlashKey, res.InputRequests[confirmFlashKey])
	}
	for _, want := range []string{"pump.yaml", "/dev/ttyUSB0"} {
		if !strings.Contains(elicit.Message, want) {
			t.Errorf("message %q missing %q", elicit.Message, want)
		}
	}
}

// --- end-to-end through the SDK's multi-round-trip machinery ---

// uploadThroughMCP registers the ESPHome tools on a real server, connects a
// client whose elicitation handler answers with confirm, and calls
// upload_esphome. It reports the tool's text output and whether the dashboard
// was actually asked to flash.
func uploadThroughMCP(t *testing.T, confirm bool) (string, bool) {
	t.Helper()

	var flashed atomic.Bool
	dash := wsDashboard(t, false, func(sc *srvConn, c command) {
		if c.Command != "firmware/upload" {
			t.Errorf("unexpected command %q", c.Command)
			return
		}
		flashed.Store(true)
		sc.result(c.MessageID, map[string]any{"job_id": "job-1", "job_type": "upload", "status": "queued"})
	})

	tools, err := NewTools(dash.URL, "")
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "v0"}, nil)
	tools.Register(srv)

	var asked atomic.Int32
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			asked.Add(1)
			if !confirm {
				return &mcp.ElicitResult{Action: "decline"}, nil
			}
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
		},
	})

	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "upload_esphome",
		Arguments: map[string]any{"config": "pump.yaml"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := asked.Load(); got != 1 {
		t.Errorf("elicitation handler called %d times, want 1", got)
	}
	if len(res.Content) == 0 {
		t.Fatalf("no content in result")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %T", res.Content[0])
	}
	return text.Text, flashed.Load()
}

func TestUploadFlashesAfterConfirmation(t *testing.T) {
	out, flashed := uploadThroughMCP(t, true)
	if !flashed {
		t.Error("dashboard was never asked to flash")
	}
	if !strings.Contains(out, "job-1") {
		t.Errorf("result %q missing job id", out)
	}
}

func TestUploadDoesNotFlashWhenDeclined(t *testing.T) {
	out, flashed := uploadThroughMCP(t, false)
	if flashed {
		t.Error("dashboard was asked to flash despite the user declining")
	}
	if !strings.Contains(out, "cancelled") {
		t.Errorf("result %q does not report the cancellation", out)
	}
}
