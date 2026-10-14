package hass

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeHA serves the endpoints RegisterGenerated queries at startup.
type fakeHA struct {
	states   string
	services string
	// calls records service calls so a test can assert what a generated tool did.
	lastServicePath string
	lastServiceBody map[string]any
	lastIntentBody  map[string]any
}

func (f *fakeHA) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/states":
			_, _ = w.Write([]byte(f.states))
		case r.URL.Path == "/api/services":
			_, _ = w.Write([]byte(f.services))
		case r.URL.Path == "/api/intent/handle":
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &f.lastIntentBody)
			_, _ = w.Write([]byte(`{"speech":{"plain":{"speech":"Done"}},"data":{"success":[{"id":"light.kitchen"}]}}`))
		case len(r.URL.Path) > len("/api/services/") && r.URL.Path[:len("/api/services/")] == "/api/services/":
			f.lastServicePath = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &f.lastServiceBody)
			_, _ = w.Write([]byte(`[]`))
		default:
			// The WebSocket dial for script aliases lands here and fails,
			// which is the best-effort path we want exercised.
			http.Error(w, "not found", http.StatusNotFound)
		}
	}
}

// connect registers generated tools against a server backed by fake and
// returns a connected MCP client session.
func connect(t *testing.T, fake *fakeHA) *mcp.ClientSession {
	t.Helper()
	return connectWithDeny(t, fake, nil)
}

// connectWithDeny is connect with HASS_DENY_SERVICES patterns applied.
func connectWithDeny(t *testing.T, fake *fakeHA, deny []string) *mcp.ClientSession {
	t.Helper()
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)

	tools, err := NewTools(srv.URL, "test-token", deny)
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	return connectTools(t, tools)
}

// connectTools registers generated tools from tools and returns a connected
// client session.
func connectTools(t *testing.T, tools *Tools) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tools.RegisterGenerated(context.Background(), server)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func toolNames(t *testing.T, session *mcp.ClientSession) map[string]*mcp.Tool {
	t.Helper()
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	out := make(map[string]*mcp.Tool, len(result.Tools))
	for _, tool := range result.Tools {
		out[tool.Name] = tool
	}
	return out
}

// A light-and-switch-only instance must not advertise vacuum or humidifier
// tools, and must still get the domain-independent ones.
func TestRegisterGeneratedSelectsByPresentDomains(t *testing.T) {
	fake := &fakeHA{
		states: `[
			{"entity_id":"light.kitchen","state":"on"},
			{"entity_id":"switch.porch","state":"off"},
			{"entity_id":"sensor.temp","state":"21"}
		]`,
		services: `[{"domain":"script","services":{"reload":{},"turn_on":{},"turn_off":{},"toggle":{}}}]`,
	}
	tools := toolNames(t, connect(t, fake))

	for _, want := range []string{"home_turn_on", "home_turn_off", "home_light_set"} {
		if _, ok := tools[want]; !ok {
			t.Errorf("missing %q for an instance with lights", want)
		}
	}
	for _, unwanted := range []string{
		"home_vacuum_start", "home_humidifier_mode", "home_media",
		"home_climate_set_temperature", "home_set_position",
	} {
		if _, ok := tools[unwanted]; ok {
			t.Errorf("registered %q for an instance with no such domain", unwanted)
		}
	}
}

func TestRegisterGeneratedRegistersAllIntentsWhenStatesUnavailable(t *testing.T) {
	fake := &fakeHA{states: `not json`, services: `[]`}
	tools := toolNames(t, connect(t, fake))

	// A momentarily unreachable instance must not silently shrink the tool
	// list, so every intent is registered.
	for _, want := range []string{"home_turn_on", "home_vacuum_start", "home_humidifier_mode"} {
		if _, ok := tools[want]; !ok {
			t.Errorf("missing %q when domain detection failed", want)
		}
	}
}

func TestRegisterGeneratedScriptTools(t *testing.T) {
	fake := &fakeHA{
		states: `[{"entity_id":"script.movie_night","state":"off"}]`,
		services: `[{"domain":"script","services":{
			"reload":{},"turn_on":{},"turn_off":{},"toggle":{},
			"movie_night":{"name":"Movie Night","description":"Dims lights and starts the projector.","fields":{
				"volume":{"required":true,"selector":{"number":{"min":0,"max":100}},"description":"Projector volume"}
			}},
			"good_morning":{"description":"Opens the blinds.","fields":{}}
		}}]`,
	}
	tools := toolNames(t, connect(t, fake))

	movie, ok := tools["home_script_movie_night"]
	if !ok {
		t.Fatal("missing home_script_movie_night")
	}
	if !contains(movie.Description, "Dims lights") {
		t.Errorf("description = %q", movie.Description)
	}
	if _, ok := tools["home_script_good_morning"]; !ok {
		t.Error("missing home_script_good_morning")
	}

	// The script domain's own services are not scripts.
	for _, unwanted := range []string{
		"home_script_reload", "home_script_turn_on", "home_script_turn_off", "home_script_toggle",
	} {
		if _, ok := tools[unwanted]; ok {
			t.Errorf("registered %q, which is not a script", unwanted)
		}
	}

	// The declared field must survive into the tool's input schema.
	schema := schemaJSON(t, movie.InputSchema)
	props, _ := schema["properties"].(map[string]any)
	volume, _ := props["volume"].(map[string]any)
	if volume["type"] != "number" || volume["maximum"] != 100.0 {
		t.Errorf("volume schema = %v", volume)
	}
	req, _ := schema["required"].([]any)
	if len(req) != 1 || req[0] != "volume" {
		t.Errorf("required = %v, want [volume]", schema["required"])
	}
}

func TestGeneratedScriptToolCallsService(t *testing.T) {
	fake := &fakeHA{
		states:   `[{"entity_id":"script.movie_night","state":"off"}]`,
		services: `[{"domain":"script","services":{"movie_night":{"description":"Movie night.","fields":{"volume":{"selector":{"number":{"min":0,"max":100}}}}}}}]`,
	}
	session := connect(t, fake)

	_, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "home_script_movie_night",
		Arguments: map[string]any{"volume": 40},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if fake.lastServicePath != "/api/services/script/movie_night" {
		t.Errorf("called %q, want /api/services/script/movie_night", fake.lastServicePath)
	}
	if fake.lastServiceBody["volume"] != 40.0 {
		t.Errorf("body = %v, want the argument forwarded", fake.lastServiceBody)
	}
}

func TestGeneratedIntentToolCallsIntentEndpoint(t *testing.T) {
	fake := &fakeHA{
		states:   `[{"entity_id":"light.kitchen","state":"on"}]`,
		services: `[]`,
	}
	session := connect(t, fake)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "home_turn_on",
		Arguments: map[string]any{"area": "kitchen", "domain": []any{"light"}},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned an error: %v", result.Content)
	}
	text, _ := result.Content[0].(*mcp.TextContent)
	if text == nil || !contains(text.Text, "Done") {
		t.Errorf("result = %v, want the intent speech", result.Content)
	}
	// Matching is limited to Assist-exposed entities, as upstream's LLM
	// API does; without it an area reaches config switches too.
	if fake.lastIntentBody["assistant"] != "conversation" {
		t.Errorf("assistant = %v, want conversation", fake.lastIntentBody["assistant"])
	}
}

// HassTurnOn/Off have no required domain, so an area alone would hit every
// exposed device there. It must carry a name, domain or device_class.
func TestOnOffAreaAloneIsRejected(t *testing.T) {
	fake := &fakeHA{
		states:   `[{"entity_id":"light.kitchen","state":"on"}]`,
		services: `[]`,
	}
	session := connect(t, fake)
	for _, args := range []map[string]any{
		{"area": "garage"},
		{"floor": "upstairs"},
		// HA treats name "all" as no name, so it must not unlock the rule.
		{"area": "garage", "name": "all"},
	} {
		fake.lastIntentBody = nil
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "home_turn_off", Arguments: args})
		if err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		if !result.IsError {
			t.Errorf("%v: accepted an area/floor-only target", args)
		}
		if fake.lastIntentBody != nil {
			t.Errorf("%v: reached Home Assistant: %v", args, fake.lastIntentBody)
		}
	}
}

// Older Home Assistant rejects the assistant field with a 400; the tool must
// fall back rather than fail every call.
func TestIntentFallsBackWhenAssistantUnsupported(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/states":
			_, _ = w.Write([]byte(`[{"entity_id":"light.kitchen","state":"on"}]`))
		case "/api/services":
			_, _ = w.Write([]byte(`[]`))
		case "/api/intent/handle":
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			bodies = append(bodies, body)
			if _, ok := body["assistant"]; ok {
				http.Error(w, "Message format incorrect: extra keys not allowed @ data['assistant']", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"speech":{"plain":{"speech":"Done"}},"data":{"success":[{"id":"light.kitchen"}]}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	tools, err := NewTools(srv.URL, "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	session := connectTools(t, tools)
	for i := 0; i < 2; i++ {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "home_light_set", Arguments: map[string]any{"name": "Kitchen", "brightness": 30},
		})
		if err != nil || result.IsError {
			t.Fatalf("call %d failed: %v %v", i, err, result)
		}
	}
	// First call tries with assistant and retries without; the second goes
	// straight to the legacy form.
	if len(bodies) != 3 {
		t.Fatalf("intent posts = %d (%v), want 3", len(bodies), bodies)
	}
	if _, ok := bodies[2]["assistant"]; ok {
		t.Error("kept sending assistant after Home Assistant rejected it")
	}
}

// Targeting is "at least one of name/area/floor"; a call with none must be
// rejected by schema validation rather than sent as a no-op.
func TestGeneratedIntentToolRequiresATarget(t *testing.T) {
	fake := &fakeHA{
		states:   `[{"entity_id":"light.kitchen","state":"on"}]`,
		services: `[]`,
	}
	session := connect(t, fake)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "home_turn_on",
		Arguments: map[string]any{},
	})
	if err == nil && (result == nil || !result.IsError) {
		t.Error("expected a call with no name/area/floor to be rejected")
	}
}

// HassTurnOff reaches lock.unlock inside Home Assistant, so denying
// lock.unlock must withhold home_turn_off rather than leaving an unchecked
// path around the policy.
func TestRegisterGeneratedHonoursServiceDenyPolicy(t *testing.T) {
	fake := &fakeHA{
		states: `[
			{"entity_id":"light.kitchen","state":"on"},
			{"entity_id":"lock.front_door","state":"locked"},
			{"entity_id":"vacuum.rosie","state":"docked"}
		]`,
		services: `[]`,
	}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)

	tools, err := NewTools(srv.URL, "test-token", []string{"lock.unlock"})
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tools.RegisterGenerated(context.Background(), server)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := context.Background()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	registered := toolNames(t, session)
	// home_turn_off stays, but can never reach a lock: the domain slot is
	// pinned to the domains whose services are all permitted.
	if _, ok := registered["home_turn_off"]; !ok {
		t.Fatal("home_turn_off withheld; a lock.unlock deny should only remove locks from it")
	}
	for _, args := range []map[string]any{
		{"name": "Front Door"},
		{"name": "Front Door", "domain": []any{"lock", "light"}},
	} {
		fake.lastIntentBody = nil
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "home_turn_off", Arguments: args}); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
		data, _ := fake.lastIntentBody["data"].(map[string]any)
		doms := stringSlice(data["domain"])
		if len(doms) == 0 {
			t.Fatalf("%v: no domain pin sent: %v", args, fake.lastIntentBody)
		}
		for _, d := range doms {
			if d == "lock" {
				t.Errorf("%v: domain pin %v still includes lock", args, doms)
			}
		}
	}
	// Asking only for a denied domain is refused locally.
	fake.lastIntentBody = nil
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "home_turn_off", Arguments: map[string]any{"domain": []any{"lock"}}})
	if err != nil || !res.IsError || fake.lastIntentBody != nil {
		t.Errorf("domain [lock] should be refused before reaching HA: %v %v", res, fake.lastIntentBody)
	}
	// Intents that cannot reach the denied service are unaffected.
	for _, want := range []string{"home_turn_on", "home_light_set", "home_vacuum_start"} {
		if _, ok := registered[want]; !ok {
			t.Errorf("%s should be unaffected by a lock.unlock deny", want)
		}
	}
}

// homeassistant.turn_off dispatches <domain>.turn_off, so a deny on
// switch.turn_off must keep home_turn_off away from switches. Checking only
// homeassistant.turn_off let it through.
func TestOnOffDenyFollowsTheDomainService(t *testing.T) {
	for _, tc := range []struct {
		deny      []string
		dir       string
		forbidden []string
		withheld  bool
	}{
		{deny: []string{"switch.turn_off"}, dir: "off", forbidden: []string{"switch"}},
		{deny: []string{"script.alert"}, dir: "on", forbidden: []string{"script"}},
		{deny: []string{"cover.open_cover"}, dir: "on", forbidden: []string{"cover"}},
		{deny: []string{"button.press"}, dir: "on", forbidden: []string{"button"}},
		{deny: []string{"homeassistant.turn_off"}, dir: "off", forbidden: []string{"light", "switch", "fan", "media_player"}},
		{deny: []string{"*.*"}, dir: "on", withheld: true},
	} {
		tools, err := NewTools("http://localhost", "t", tc.deny)
		if err != nil {
			t.Fatal(err)
		}
		allowed, narrowed := tools.onOffAllowed(tc.dir)
		if tc.withheld {
			if len(allowed) != 0 {
				t.Errorf("%v: allowed %v, want nothing", tc.deny, allowed)
			}
			continue
		}
		if !narrowed {
			t.Errorf("%v: not narrowed", tc.deny)
		}
		for _, f := range tc.forbidden {
			for _, a := range allowed {
				if a == f {
					t.Errorf("%v: %s still allowed for turn_%s", tc.deny, f, tc.dir)
				}
			}
		}
	}
}

func TestDeniedServicesReportsTheOffender(t *testing.T) {
	tools, err := NewTools("http://localhost", "t", []string{"cover.*"})
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	var setPosition, lightSet IntentDef
	for _, d := range IntentCatalog {
		switch d.ToolName {
		case "home_set_position":
			setPosition = d
		case "home_light_set":
			lightSet = d
		}
	}
	if got := tools.deniedServices(setPosition); got != "cover.set_cover_position" {
		t.Errorf("deniedServices = %q, want cover.set_cover_position", got)
	}
	if got := tools.deniedServices(lightSet); got != "" {
		t.Errorf("deniedServices = %q, want empty for an unaffected intent", got)
	}
}

// With no deny patterns configured the policy is nil and nothing is withheld.
func TestDeniedServicesWithNoPolicy(t *testing.T) {
	tools, err := NewTools("http://localhost", "t", nil)
	if err != nil {
		t.Fatalf("NewTools: %v", err)
	}
	for _, d := range IntentCatalog {
		if got := tools.deniedServices(d); got != "" {
			t.Errorf("%s: deniedServices = %q with no policy", d.ToolName, got)
		}
	}
}

// A deliberate broad command ("turn off all the lights") carries a domain but
// no name/area/floor, and must be allowed through.
func TestGeneratedIntentToolAcceptsDomainOnlyTarget(t *testing.T) {
	fake := &fakeHA{
		states:   `[{"entity_id":"light.kitchen","state":"on"}]`,
		services: `[]`,
	}
	session := connect(t, fake)

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "home_turn_off",
		Arguments: map[string]any{"domain": []any{"light"}},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if result.IsError {
		t.Errorf("a domain-only target should be accepted: %v", result.Content)
	}
}
