package hass

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestHandleIntentSendsNameAndData(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertAuth(t, r)
		if r.Method != "POST" || r.URL.Path != "/api/intent/handle" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		_, _ = w.Write([]byte(`{
			"speech": {"plain": {"speech": "Turned on the lights", "extra_data": null}},
			"response_type": "action_done",
			"data": {"targets": [], "success": [{"id": "light.kitchen", "name": "Kitchen", "type": "entity"}], "failed": []}
		}`))
	})

	result, err := c.HandleIntent(context.Background(), "HassTurnOn", map[string]any{"area": "kitchen"})
	if err != nil {
		t.Fatalf("HandleIntent: %v", err)
	}

	if got["name"] != "HassTurnOn" {
		t.Errorf("name = %v, want HassTurnOn", got["name"])
	}
	data, _ := got["data"].(map[string]any)
	if data["area"] != "kitchen" {
		t.Errorf("data.area = %v, want kitchen", data["area"])
	}
	if result.Speech != "Turned on the lights" {
		t.Errorf("Speech = %q", result.Speech)
	}
	if len(result.Success) != 1 || result.Success[0]["id"] != "light.kitchen" {
		t.Errorf("Success = %v", result.Success)
	}
}

func TestHandleIntentOmitsEmptyData(t *testing.T) {
	var got map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"speech": {"plain": {"speech": "ok"}}}`))
	})

	if _, err := c.HandleIntent(context.Background(), "HassTurnOn", nil); err != nil {
		t.Fatalf("HandleIntent: %v", err)
	}
	if _, ok := got["data"]; ok {
		t.Errorf("data should be omitted when there are no slots, got %v", got)
	}
}

// Home Assistant answers an unmatched intent with HTTP 200 and an error
// response, which must not read as success.
func TestHandleIntentErrorResponseIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"speech": {"plain": {"speech": "Sorry, I am not aware of any device called foo"}},
			"response_type": "error",
			"data": {"code": "no_valid_targets"}
		}`))
	})

	result, err := c.HandleIntent(context.Background(), "HassTurnOn", map[string]any{"name": "foo"})
	if err == nil {
		t.Fatal("expected an error for an error response")
	}
	if !contains(err.Error(), "no_valid_targets") {
		t.Errorf("error %q does not mention the failure code", err)
	}
	// The parsed response is still returned so callers can show the speech.
	if result == nil || result.Speech == "" {
		t.Error("expected the speech to survive alongside the error")
	}
}

// The intent name travels in the JSON body, never the URL, so a hostile name
// cannot escape the path. Assert that rather than relying on it by inspection.
func TestHandleIntentNameNeverReachesThePath(t *testing.T) {
	var gotPath, gotName string
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		gotName, _ = body["name"].(string)
		_, _ = w.Write([]byte(`{"speech": {"plain": {"speech": "ok"}}}`))
	})

	hostile := "../../config/core/check_config"
	if _, err := c.HandleIntent(context.Background(), hostile, nil); err != nil {
		t.Fatalf("HandleIntent: %v", err)
	}
	if gotPath != "/api/intent/handle" {
		t.Errorf("path = %q, want /api/intent/handle", gotPath)
	}
	if gotName != hostile {
		t.Errorf("name = %q, want it passed through in the body verbatim", gotName)
	}
}

func TestIntentInputSchemaTargeting(t *testing.T) {
	var def IntentDef
	for _, d := range IntentCatalog {
		if d.ToolName == "home_turn_on" {
			def = d
		}
	}
	if def.Intent != "HassTurnOn" {
		t.Fatal("home_turn_on missing from the catalog")
	}

	got := schemaJSON(t, def.InputSchema())
	if got["type"] != "object" {
		t.Fatalf("type = %v, want object", got["type"])
	}
	props, _ := got["properties"].(map[string]any)
	for _, name := range []string{"name", "area", "floor", "domain", "device_class"} {
		if _, ok := props[name]; !ok {
			t.Errorf("missing targeting property %q", name)
		}
	}

	// At least one of name/area/floor/domain must be supplied, so an
	// under-specified call cannot act on every entity in scope.
	anyOf, _ := got["anyOf"].([]any)
	if len(anyOf) != 4 {
		t.Fatalf("anyOf = %v, want 4 alternatives", got["anyOf"])
	}

	// preferred_area_id/preferred_floor_id are voice-satellite context we
	// cannot supply over REST, so they must not be advertised.
	for _, name := range []string{"preferred_area_id", "preferred_floor_id"} {
		if _, ok := props[name]; ok {
			t.Errorf("%q should not be exposed to REST callers", name)
		}
	}
}

func TestIntentInputSchemaRequiredSlots(t *testing.T) {
	var def IntentDef
	for _, d := range IntentCatalog {
		if d.ToolName == "home_set_position" {
			def = d
		}
	}
	got := schemaJSON(t, def.InputSchema())
	req, _ := got["required"].([]any)
	if len(req) != 1 || req[0] != "position" {
		t.Errorf("required = %v, want [position]", got["required"])
	}
	props, _ := got["properties"].(map[string]any)
	position, _ := props["position"].(map[string]any)
	if position["minimum"] != 0.0 || position["maximum"] != 100.0 {
		t.Errorf("position = %v, want 0..100", position)
	}
}

func TestIntentCatalogIsWellFormed(t *testing.T) {
	seenTool := map[string]bool{}
	seenIntent := map[string]bool{}
	for _, d := range IntentCatalog {
		if d.ToolName == "" || d.Intent == "" || d.Description == "" {
			t.Errorf("incomplete entry: %+v", d)
		}
		if seenTool[d.ToolName] {
			t.Errorf("duplicate tool name %q", d.ToolName)
		}
		seenTool[d.ToolName] = true
		if seenIntent[d.Intent] {
			t.Errorf("duplicate intent %q", d.Intent)
		}
		seenIntent[d.Intent] = true

		// Every schema must be a valid tool input schema.
		if s := d.InputSchema(); s.Type != "object" {
			t.Errorf("%s: input schema type = %q, want object", d.ToolName, s.Type)
		}
	}

	// Timer intents need a voice-satellite device_id we do not have.
	for _, d := range IntentCatalog {
		if contains(d.Intent, "Timer") {
			t.Errorf("%s: timer intents cannot be satisfied over REST", d.Intent)
		}
	}
}

func TestHasAnyDomain(t *testing.T) {
	present := map[string]bool{"light": true, "sensor": true}

	if !hasAnyDomain(present, nil) {
		t.Error("an empty domain list should always match")
	}
	if !hasAnyDomain(present, []string{"cover", "light"}) {
		t.Error("should match when one domain is present")
	}
	if hasAnyDomain(present, []string{"vacuum", "humidifier"}) {
		t.Error("should not match when no domain is present")
	}
}

// An action_done response with no successes and at least one failure means
// nothing happened, even though HA sets no error code.
func TestHandleIntentAllTargetsFailedIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"speech": {"plain": {"speech": "Kitchen light is unavailable"}},
			"response_type": "action_done",
			"data": {"success": [], "failed": [{"id": "light.kitchen", "name": "Kitchen"}], "targets": []}
		}`))
	})

	result, err := c.HandleIntent(context.Background(), "HassTurnOn", map[string]any{"name": "kitchen light"})
	if err == nil {
		t.Fatal("expected an error when every target failed")
	}
	if result == nil || len(result.Failed) != 1 {
		t.Errorf("failed targets should survive alongside the error: %+v", result)
	}
}

// A partial success is still a success: some targets acted.
func TestHandleIntentPartialSuccessIsNotAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"speech": {"plain": {"speech": "Done"}},
			"response_type": "action_done",
			"data": {"success": [{"id": "light.a"}], "failed": [{"id": "light.b"}]}
		}`))
	})
	if _, err := c.HandleIntent(context.Background(), "HassTurnOn", map[string]any{"area": "kitchen"}); err != nil {
		t.Errorf("partial success should not be an error: %v", err)
	}
}

// An intent that affects nothing at all (no targets matched the filter but HA
// reported action_done) must not be reported as a failure either, since there
// is no failed target to point at.
func TestHandleIntentNoTargetsIsNotAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"speech": {"plain": {"speech": "Done"}}, "response_type": "action_done", "data": {"success": [], "failed": []}}`))
	})
	if _, err := c.HandleIntent(context.Background(), "HassTurnOn", map[string]any{"area": "kitchen"}); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// Every intent must declare the services it can dispatch, or the deny policy
// silently fails to cover it.
func TestIntentCatalogDeclaresDispatchableServices(t *testing.T) {
	for _, d := range IntentCatalog {
		if len(d.Services) == 0 {
			t.Errorf("%s declares no services; HASS_DENY_SERVICES cannot cover it", d.ToolName)
		}
		for _, svc := range d.Services {
			if _, _, ok := stringsCut(svc, "."); !ok {
				t.Errorf("%s: service %q is not domain.service", d.ToolName, svc)
			}
		}
	}
}

func stringsCut(s, sep string) (string, string, bool) {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}
