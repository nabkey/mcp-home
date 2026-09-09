package hass

import (
	"sort"

	"github.com/google/jsonschema-go/jsonschema"
)

// This file ports Home Assistant's selector -> JSON Schema conversion, which
// upstream lives in homeassistant/helpers/llm.py as selector_serializer(). Our
// input differs: upstream works on constructed Selector objects, while we get
// the raw selector JSON from GET /api/services, which is a single-key object
// mapping the selector type to its config (e.g. {"number": {"min": 0}}).
//
// Keeping the mapping in one place lets both the generated intent/script tools
// and any hand-written tool describe HA parameters the same way.

func ptrFloat(f float64) *float64 { return &f }
func ptrInt(i int) *int           { return &i }

// numFromConfig reads a numeric config value, tolerating the int/float
// ambiguity of decoded JSON.
func numFromConfig(cfg map[string]any, key string) (float64, bool) {
	v, ok := cfg[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func boolFromConfig(cfg map[string]any, key string) bool {
	b, _ := cfg[key].(bool)
	return b
}

// stringList extracts a []string from a config value that decoded as []any.
// Select options may be plain strings or {"value": ..., "label": ...} objects.
func stringList(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		switch x := item.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			if s, ok := x["value"].(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func enumOf(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}

// wrapMultiple wraps a schema in an array when the selector config sets
// "multiple", matching upstream's handling.
func wrapMultiple(cfg map[string]any, s *jsonschema.Schema) *jsonschema.Schema {
	if !boolFromConfig(cfg, "multiple") {
		return s
	}
	return &jsonschema.Schema{Type: "array", Items: s}
}

// SelectorToSchema converts a Home Assistant selector definition into a JSON
// Schema. sel is the raw selector object from GET /api/services: a single-key
// map of selector type to config. An unrecognised selector degrades to a
// string (or array of strings when it declares "multiple"), which is what
// upstream does for anything its explicit cases miss.
func SelectorToSchema(sel map[string]any) *jsonschema.Schema {
	if len(sel) == 0 {
		return &jsonschema.Schema{Type: "string"}
	}

	// Selector objects carry exactly one key; sort for deterministic behaviour
	// if HA ever emits more than one.
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kind := keys[0]

	// A selector's config is usually an object but may be null (e.g.
	// "color_rgb": {} or "text": null).
	cfg, _ := sel[kind].(map[string]any)
	if cfg == nil {
		cfg = map[string]any{}
	}

	switch kind {
	case "boolean":
		return &jsonschema.Schema{Type: "boolean"}

	case "backup_location":
		return &jsonschema.Schema{Type: "string", Pattern: `^(?:\/backup|\w+)$`}

	case "color_rgb":
		return &jsonschema.Schema{
			Type:     "array",
			Items:    &jsonschema.Schema{Type: "number"},
			MinItems: ptrInt(3),
			MaxItems: ptrInt(3),
			Format:   "RGB",
		}

	case "color_temp":
		s := &jsonschema.Schema{Type: "number"}
		// HA still emits the legacy mired bounds on some integrations.
		if v, ok := numFromConfig(cfg, "min"); ok {
			s.Minimum = ptrFloat(v)
		} else if v, ok := numFromConfig(cfg, "min_mireds"); ok {
			s.Minimum = ptrFloat(v)
		}
		if v, ok := numFromConfig(cfg, "max"); ok {
			s.Maximum = ptrFloat(v)
		} else if v, ok := numFromConfig(cfg, "max_mireds"); ok {
			s.Maximum = ptrFloat(v)
		}
		return s

	case "constant":
		if v, ok := cfg["value"]; ok {
			c := v
			return &jsonschema.Schema{Const: &c}
		}
		return &jsonschema.Schema{Type: "string"}

	case "country":
		if opts := stringList(cfg["countries"]); len(opts) > 0 {
			return &jsonschema.Schema{Type: "string", Enum: enumOf(opts)}
		}
		return &jsonschema.Schema{Type: "string", Format: "ISO 3166-1 alpha-2"}

	case "date":
		return &jsonschema.Schema{Type: "string", Format: "date"}

	case "datetime":
		return &jsonschema.Schema{Type: "string", Format: "date-time"}

	case "time":
		return &jsonschema.Schema{Type: "string", Format: "time"}

	case "duration":
		return &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"hours":        {Type: "number"},
				"minutes":      {Type: "number"},
				"seconds":      {Type: "number"},
				"milliseconds": {Type: "number"},
				"days":         {Type: "number"},
			},
		}

	case "entity":
		return wrapMultiple(cfg, &jsonschema.Schema{Type: "string", Format: "entity_id"})

	case "language":
		if opts := stringList(cfg["languages"]); len(opts) > 0 {
			return &jsonschema.Schema{Type: "string", Enum: enumOf(opts)}
		}
		return &jsonschema.Schema{Type: "string", Format: "RFC 5646"}

	case "location":
		return &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"latitude":  {Type: "number"},
				"longitude": {Type: "number"},
				"radius":    {Type: "number"},
			},
			Required: []string{"latitude", "longitude"},
		}

	case "media":
		return wrapMultiple(cfg, &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"entity_id":          {Type: "string", Format: "entity_id"},
				"media_content_id":   {Type: "string"},
				"media_content_type": {Type: "string"},
			},
		})

	case "number":
		s := &jsonschema.Schema{Type: "number"}
		if v, ok := numFromConfig(cfg, "min"); ok {
			s.Minimum = ptrFloat(v)
		}
		if v, ok := numFromConfig(cfg, "max"); ok {
			s.Maximum = ptrFloat(v)
		}
		return s

	case "object":
		s := &jsonschema.Schema{Type: "object"}
		if fields, ok := cfg["fields"].(map[string]any); ok && len(fields) > 0 {
			props := map[string]*jsonschema.Schema{}
			var required []string
			names := make([]string, 0, len(fields))
			for name := range fields {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				fc, _ := fields[name].(map[string]any)
				if fc == nil {
					continue
				}
				inner, _ := fc["selector"].(map[string]any)
				props[name] = SelectorToSchema(inner)
				if boolFromConfig(fc, "required") {
					required = append(required, name)
				}
			}
			s.Properties = props
			s.Required = required
		} else {
			s.AdditionalProperties = &jsonschema.Schema{}
		}
		return wrapMultiple(cfg, s)

	case "select":
		// An empty-but-non-nil Enum validates nothing while marshalling away
		// under omitempty, so the advertised schema would look like a plain
		// string while rejecting every value. A select whose options we cannot
		// read (custom_value with no list, or an unfamiliar option shape)
		// degrades to an unconstrained string instead.
		opts := stringList(cfg["options"])
		item := &jsonschema.Schema{Type: "string"}
		if len(opts) > 0 {
			item.Enum = enumOf(opts)
		}
		if boolFromConfig(cfg, "multiple") {
			return &jsonschema.Schema{
				Type:        "array",
				Items:       item,
				UniqueItems: true,
			}
		}
		return item

	case "target":
		return &jsonschema.Schema{
			Type: "object",
			Properties: map[string]*jsonschema.Schema{
				"entity_id": {Type: "array", Items: &jsonschema.Schema{Type: "string", Format: "entity_id"}},
				"device_id": {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
				"area_id":   {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
				"floor_id":  {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
				"label_id":  {Type: "array", Items: &jsonschema.Schema{Type: "string"}},
			},
		}

	case "template":
		return &jsonschema.Schema{Type: "string", Format: "jinja2"}

	case "trigger":
		return &jsonschema.Schema{Type: "array", Items: &jsonschema.Schema{Type: "string"}}

	case "text":
		return wrapMultiple(cfg, &jsonschema.Schema{Type: "string"})
	}

	return wrapMultiple(cfg, &jsonschema.Schema{Type: "string"})
}

// fieldDescription builds a parameter description from a service field's
// metadata. GET /api/services strips the translated "description" for most
// integrations (it lives in frontend translations), so fall back to "name" and
// finally to the field's own key.
func fieldDescription(name string, cfg map[string]any) string {
	desc, _ := cfg["description"].(string)
	if desc == "" {
		desc, _ = cfg["name"].(string)
	}
	if desc == "" {
		desc = name
	}
	// A field's "filter" says it only applies to entities with certain
	// supported_features or attributes. JSON Schema cannot express a
	// constraint on a *different* argument's runtime state, so surface it as
	// prose rather than dropping it the way upstream does.
	if filter, ok := cfg["filter"].(map[string]any); ok && len(filter) > 0 {
		if attrs, ok := filter["attribute"].(map[string]any); ok {
			for attr, allowed := range attrs {
				if opts := stringList(allowed); len(opts) > 0 {
					desc += " (only for entities whose " + attr + " includes one of: " + joinComma(opts) + ")"
					break
				}
			}
		} else if _, ok := filter["supported_features"]; ok {
			desc += " (only for entities that support this feature)"
		}
	}
	return desc
}

func joinComma(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// ServiceFieldsToSchema converts the "fields" map of a service description from
// GET /api/services into an object JSON Schema.
//
// HA nests rarely-used parameters under a collapsed "additional_fields" group;
// those are flattened in so the schema stays a flat object, which is what tool
// callers expect.
func ServiceFieldsToSchema(fields map[string]any) *jsonschema.Schema {
	props := map[string]*jsonschema.Schema{}
	var required []string
	collectServiceFields(fields, props, &required)
	sort.Strings(required)
	return &jsonschema.Schema{Type: "object", Properties: props, Required: required}
}

func collectServiceFields(fields map[string]any, props map[string]*jsonschema.Schema, required *[]string) {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		cfg, _ := fields[name].(map[string]any)
		if cfg == nil {
			continue
		}
		// A collapsed group ("additional_fields") has nested fields and no
		// selector of its own; flatten its members up into this level.
		if nested, ok := cfg["fields"].(map[string]any); ok {
			if _, hasSelector := cfg["selector"]; !hasSelector {
				collectServiceFields(nested, props, required)
				continue
			}
		}

		sel, _ := cfg["selector"].(map[string]any)
		schema := SelectorToSchema(sel)
		schema.Description = fieldDescription(name, cfg)
		if ex, ok := cfg["example"]; ok {
			schema.Examples = []any{ex}
		}
		props[name] = schema
		if boolFromConfig(cfg, "required") {
			*required = append(*required, name)
		}
	}
}
