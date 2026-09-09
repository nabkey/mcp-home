package hass

import (
	"encoding/json"
	"testing"
)

// schemaJSON renders a schema the way it goes over the wire, which is what a
// client actually sees.
func schemaJSON(t *testing.T, v any) map[string]any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return out
}

func selectorJSON(t *testing.T, raw string) map[string]any {
	t.Helper()
	var sel map[string]any
	if err := json.Unmarshal([]byte(raw), &sel); err != nil {
		t.Fatalf("bad selector fixture: %v", err)
	}
	return schemaJSON(t, SelectorToSchema(sel))
}

func TestSelectorToSchemaScalars(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		want     map[string]any
	}{
		{
			name:     "boolean",
			selector: `{"boolean": {}}`,
			want:     map[string]any{"type": "boolean"},
		},
		{
			// light.turn_on's transition field.
			name:     "number with bounds",
			selector: `{"number": {"min": 0, "max": 300, "step": 1, "mode": "slider"}}`,
			want:     map[string]any{"type": "number", "minimum": 0.0, "maximum": 300.0},
		},
		{
			name:     "entity",
			selector: `{"entity": {}}`,
			want:     map[string]any{"type": "string", "format": "entity_id"},
		},
		{
			name:     "entity multiple",
			selector: `{"entity": {"multiple": true}}`,
			want: map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string", "format": "entity_id"},
			},
		},
		{
			name:     "template",
			selector: `{"template": null}`,
			want:     map[string]any{"type": "string", "format": "jinja2"},
		},
		{
			name:     "date",
			selector: `{"date": {}}`,
			want:     map[string]any{"type": "string", "format": "date"},
		},
		{
			// An unknown selector must not become an untyped schema.
			name:     "unknown falls back to string",
			selector: `{"some_future_selector": {}}`,
			want:     map[string]any{"type": "string"},
		},
		{
			name:     "unknown multiple falls back to string array",
			selector: `{"some_future_selector": {"multiple": true}}`,
			want: map[string]any{
				"type":  "array",
				"items": map[string]any{"type": "string"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := selectorJSON(t, tc.selector)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, want := range tc.want {
				if !jsonEqual(got[k], want) {
					t.Errorf("field %q = %v, want %v", k, got[k], want)
				}
			}
		})
	}
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func TestSelectorToSchemaSelect(t *testing.T) {
	got := selectorJSON(t, `{"select": {"options": ["long", "short"], "multiple": false}}`)
	if got["type"] != "string" {
		t.Errorf("type = %v, want string", got["type"])
	}
	enum, ok := got["enum"].([]any)
	if !ok || len(enum) != 2 || enum[0] != "long" {
		t.Errorf("enum = %v, want [long short]", got["enum"])
	}

	// HA also emits options as {value,label} objects.
	got = selectorJSON(t, `{"select": {"options": [{"value": "a", "label": "A"}], "multiple": true}}`)
	if got["type"] != "array" {
		t.Fatalf("type = %v, want array", got["type"])
	}
	if got["uniqueItems"] != true {
		t.Errorf("uniqueItems = %v, want true", got["uniqueItems"])
	}
	items, _ := got["items"].(map[string]any)
	enum, _ = items["enum"].([]any)
	if len(enum) != 1 || enum[0] != "a" {
		t.Errorf("items.enum = %v, want [a]", items["enum"])
	}
}

func TestSelectorToSchemaColorRGB(t *testing.T) {
	got := selectorJSON(t, `{"color_rgb": {}}`)
	if got["type"] != "array" || got["minItems"] != 3.0 || got["maxItems"] != 3.0 {
		t.Errorf("got %v, want a 3-item array", got)
	}
	if got["format"] != "RGB" {
		t.Errorf("format = %v, want RGB", got["format"])
	}
}

func TestSelectorToSchemaColorTempPrefersKelvinOverMireds(t *testing.T) {
	// Modern integrations send min/max; older ones only min_mireds/max_mireds.
	got := selectorJSON(t, `{"color_temp": {"min": 2000, "max": 6500, "unit": "kelvin"}}`)
	if got["minimum"] != 2000.0 || got["maximum"] != 6500.0 {
		t.Errorf("got %v, want 2000..6500", got)
	}

	got = selectorJSON(t, `{"color_temp": {"min_mireds": 153, "max_mireds": 500}}`)
	if got["minimum"] != 153.0 || got["maximum"] != 500.0 {
		t.Errorf("legacy mireds: got %v, want 153..500", got)
	}
}

func TestSelectorToSchemaObject(t *testing.T) {
	// No declared fields: a free-form object.
	got := selectorJSON(t, `{"object": {"multiple": false}}`)
	if got["type"] != "object" {
		t.Fatalf("type = %v, want object", got["type"])
	}
	if _, ok := got["additionalProperties"]; !ok {
		t.Error("expected additionalProperties for a field-less object selector")
	}

	// Declared fields become properties, with required carried through.
	got = selectorJSON(t, `{"object": {"fields": {
		"qos": {"selector": {"number": {"min": 0, "max": 2}}, "required": true},
		"topic": {"selector": {"text": null}}
	}}}`)
	props, _ := got["properties"].(map[string]any)
	if len(props) != 2 {
		t.Fatalf("properties = %v, want 2 entries", props)
	}
	qos, _ := props["qos"].(map[string]any)
	if qos["type"] != "number" || qos["maximum"] != 2.0 {
		t.Errorf("qos = %v, want number 0..2", qos)
	}
	req, _ := got["required"].([]any)
	if len(req) != 1 || req[0] != "qos" {
		t.Errorf("required = %v, want [qos]", got["required"])
	}
}

func TestSelectorToSchemaEmpty(t *testing.T) {
	// A field with no selector at all still needs a usable schema.
	got := schemaJSON(t, SelectorToSchema(nil))
	if got["type"] != "string" {
		t.Errorf("type = %v, want string", got["type"])
	}
}

// lightTurnOnFields is a trimmed copy of the real light.turn_on service
// description from GET /api/services, including the collapsed
// additional_fields group and a filtered field.
const lightTurnOnFields = `{
  "brightness_pct": {
    "filter": {"attribute": {"supported_color_modes": ["brightness", "color_temp"]}},
    "selector": {"number": {"min": 0, "max": 100, "unit_of_measurement": "%"}}
  },
  "transition": {
    "filter": {"supported_features": [32]},
    "selector": {"number": {"min": 0, "max": 300, "unit_of_measurement": "seconds"}}
  },
  "rgb_color": {
    "example": "[255, 100, 100]",
    "selector": {"color_rgb": {}}
  },
  "additional_fields": {
    "collapsed": true,
    "fields": {
      "profile": {"example": "relax", "selector": {"text": {"multiline": false}}},
      "flash": {"selector": {"select": {"options": ["long", "short"]}}}
    }
  }
}`

func TestServiceFieldsToSchemaFlattensCollapsedGroup(t *testing.T) {
	var fields map[string]any
	if err := json.Unmarshal([]byte(lightTurnOnFields), &fields); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}

	got := schemaJSON(t, ServiceFieldsToSchema(fields))
	if got["type"] != "object" {
		t.Fatalf("type = %v, want object", got["type"])
	}
	props, _ := got["properties"].(map[string]any)

	// additional_fields itself must not survive as a property; its members
	// must be lifted to the top level.
	if _, ok := props["additional_fields"]; ok {
		t.Error("additional_fields leaked into properties")
	}
	for _, name := range []string{"brightness_pct", "transition", "rgb_color", "profile", "flash"} {
		if _, ok := props[name]; !ok {
			t.Errorf("missing property %q", name)
		}
	}

	transition, _ := props["transition"].(map[string]any)
	if transition["type"] != "number" || transition["maximum"] != 300.0 {
		t.Errorf("transition = %v, want number 0..300", transition)
	}

	rgb, _ := props["rgb_color"].(map[string]any)
	examples, _ := rgb["examples"].([]any)
	if len(examples) != 1 || examples[0] != "[255, 100, 100]" {
		t.Errorf("rgb_color examples = %v", rgb["examples"])
	}
}

func TestServiceFieldsToSchemaSurfacesFilterAsProse(t *testing.T) {
	var fields map[string]any
	if err := json.Unmarshal([]byte(lightTurnOnFields), &fields); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	got := schemaJSON(t, ServiceFieldsToSchema(fields))
	props, _ := got["properties"].(map[string]any)

	// JSON Schema cannot express "applies only to entities whose
	// supported_color_modes includes brightness", so it must appear in the
	// description rather than vanish.
	brightness, _ := props["brightness_pct"].(map[string]any)
	desc, _ := brightness["description"].(string)
	if desc == "" {
		t.Fatal("brightness_pct has no description")
	}
	if !contains(desc, "supported_color_modes") {
		t.Errorf("description %q does not mention the attribute filter", desc)
	}

	transition, _ := props["transition"].(map[string]any)
	tdesc, _ := transition["description"].(string)
	if !contains(tdesc, "support this feature") {
		t.Errorf("transition description %q does not mention the feature filter", tdesc)
	}
}

func TestServiceFieldsToSchemaRequired(t *testing.T) {
	var fields map[string]any
	if err := json.Unmarshal([]byte(`{
		"entity_id": {"required": true, "selector": {"entity": {}}},
		"message": {"selector": {"text": null}}
	}`), &fields); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	got := schemaJSON(t, ServiceFieldsToSchema(fields))
	req, _ := got["required"].([]any)
	if len(req) != 1 || req[0] != "entity_id" {
		t.Errorf("required = %v, want [entity_id]", got["required"])
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// A select whose options cannot be read must degrade to an unconstrained
// string. An empty-but-non-nil Enum marshals away under omitempty, so the
// advertised schema would claim to accept any string while the resolved
// schema rejected every value.
func TestSelectorToSchemaSelectWithoutUsableOptions(t *testing.T) {
	for _, raw := range []string{
		`{"select": {"custom_value": true}}`,
		`{"select": {"options": []}}`,
		`{"select": {"options": [17, 42]}}`,
	} {
		sel := map[string]any{}
		if err := json.Unmarshal([]byte(raw), &sel); err != nil {
			t.Fatalf("bad fixture: %v", err)
		}
		schema := SelectorToSchema(sel)

		if got := schemaJSON(t, schema); got["type"] != "string" {
			t.Errorf("%s: type = %v, want string", raw, got["type"])
		}
		resolved, err := schema.Resolve(nil)
		if err != nil {
			t.Fatalf("%s: resolve: %v", raw, err)
		}
		if err := resolved.Validate("anything"); err != nil {
			t.Errorf("%s: rejects an arbitrary string: %v", raw, err)
		}
	}
}

// The normal case must still constrain to the declared options.
func TestSelectorToSchemaSelectStillEnforcesRealOptions(t *testing.T) {
	sel := map[string]any{}
	_ = json.Unmarshal([]byte(`{"select": {"options": ["long", "short"]}}`), &sel)
	resolved, err := SelectorToSchema(sel).Resolve(nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := resolved.Validate("long"); err != nil {
		t.Errorf("rejected a valid option: %v", err)
	}
	if err := resolved.Validate("sideways"); err == nil {
		t.Error("accepted a value outside the declared options")
	}
}

// Upstream maps ConditionSelector to cv.CONDITIONS_SCHEMA — a list of
// condition objects. The string default would advertise a shape that rejects
// the only value the field accepts.
func TestSelectorToSchemaCondition(t *testing.T) {
	schema := SelectorToSchema(map[string]any{"condition": nil})
	got := schemaJSON(t, schema)
	if got["type"] != "array" {
		t.Fatalf("type = %v, want array", got["type"])
	}
	items, _ := got["items"].(map[string]any)
	if items["type"] != "object" {
		t.Errorf("items.type = %v, want object", items["type"])
	}

	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	conditions := []any{map[string]any{"condition": "state", "entity_id": "light.a", "state": "on"}}
	if err := resolved.Validate(conditions); err != nil {
		t.Errorf("rejected a valid condition list: %v", err)
	}
}

// A collapsed group must not shadow a same-named field at the outer level,
// whichever way the group's name happens to sort.
func TestServiceFieldsToSchemaCollapsedGroupDoesNotShadow(t *testing.T) {
	for _, groupName := range []string{"additional_fields", "zz_later_group"} {
		raw := `{
			"mode": {"selector": {"select": {"options": ["outer"]}}, "required": true},
			"` + groupName + `": {"collapsed": true, "fields": {
				"mode": {"selector": {"number": {"min": 0, "max": 1}}},
				"only_nested": {"selector": {"text": null}}
			}}
		}`
		var fields map[string]any
		if err := json.Unmarshal([]byte(raw), &fields); err != nil {
			t.Fatalf("bad fixture: %v", err)
		}
		got := schemaJSON(t, ServiceFieldsToSchema(fields))
		props, _ := got["properties"].(map[string]any)

		mode, _ := props["mode"].(map[string]any)
		if mode["type"] != "string" {
			t.Errorf("group %q: outer field was shadowed by the nested one (type = %v)", groupName, mode["type"])
		}
		if _, ok := props["only_nested"]; !ok {
			t.Errorf("group %q: non-colliding nested field was dropped", groupName)
		}
		req, _ := got["required"].([]any)
		if len(req) != 1 || req[0] != "mode" {
			t.Errorf("group %q: required = %v, want [mode]", groupName, got["required"])
		}
	}
}
