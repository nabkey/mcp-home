package hass

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nabkey/mcp-home/internal/mcputil"
)

// Generated tools are registered at startup against the live instance, so the
// tool list reflects what this particular Home Assistant actually has. Two
// sources, both mirroring what HA's own MCP server does:
//
//   - Intents (see intents.go), registered when the instance has entities in
//     the intent's domains.
//   - Scripts, one tool per script, with parameters taken from the script's
//     declared fields. This is the one place upstream does generate tools from
//     services, because scripts are the one place service descriptions are
//     reliably populated.

// generateTimeout bounds the startup queries that decide what to register.
const generateTimeout = 15 * time.Second

// scriptDomainServices are the script domain's own services, which are not
// individual scripts and so get no generated tool. manage_scripts and
// call_home_service already cover them.
var scriptDomainServices = map[string]bool{
	"reload": true, "turn_on": true, "turn_off": true, "toggle": true,
}

// RegisterGenerated adds the intent and per-script tools. It queries the
// instance to decide what to register; if that fails the intent catalog is
// registered in full so a momentarily unreachable Home Assistant does not
// silently shrink the tool list.
func (t *Tools) RegisterGenerated(ctx context.Context, server *mcp.Server) {
	// Startup must not hang on an unreachable or slow instance; the server
	// still starts, just without the generated tools.
	ctx, cancel := context.WithTimeout(ctx, generateTimeout)
	defer cancel()

	domains, err := t.presentDomains(ctx)
	if err != nil {
		slog.Warn("hass: could not read states to select generated tools; registering all intents", "error", err)
		domains = nil
	}

	registered := 0
	for _, def := range IntentCatalog {
		if domains != nil && !hasAnyDomain(domains, def.Domains) {
			continue
		}
		// The intent endpoint takes an intent name, not a service, so
		// CallService's deny check never runs for it. Enforce the policy here
		// instead, against every service the intent could dispatch.
		if denied := t.deniedServices(def); denied != "" {
			slog.Info("hass: intent tool withheld by service deny policy",
				"tool", def.ToolName, "denied", denied)
			continue
		}
		t.registerIntentTool(server, def)
		registered++
	}
	slog.Info("hass: registered intent tools", "count", registered)

	if n, err := t.registerScriptTools(ctx, server); err != nil {
		slog.Warn("hass: could not register script tools", "error", err)
	} else {
		slog.Info("hass: registered script tools", "count", n)
	}
}

// deniedServices returns the first service an intent could dispatch that the
// deny policy refuses, or "" when the intent is fully permitted.
//
// This is deliberately conservative: HassTurnOff can reach lock.unlock, so a
// deny on lock.unlock withholds the whole tool rather than allowing a call
// that might resolve to a lock. The narrower alternative — checking only once
// Home Assistant has resolved a target — is not available to us, because
// resolution happens inside the intent handler.
func (t *Tools) deniedServices(def IntentDef) string {
	policy := t.client.policy
	if policy == nil {
		return ""
	}
	for _, svc := range def.Services {
		domain, service, ok := strings.Cut(svc, ".")
		if !ok {
			continue
		}
		if err := policy.Check(domain, service); err != nil {
			return svc
		}
	}
	return ""
}

// presentDomains returns the set of entity domains the instance currently has.
func (t *Tools) presentDomains(ctx context.Context) (map[string]bool, error) {
	states, err := t.client.GetStates(ctx)
	if err != nil {
		return nil, err
	}
	domains := make(map[string]bool)
	for _, s := range states {
		if domain, _, ok := strings.Cut(s.EntityID, "."); ok {
			domains[domain] = true
		}
	}
	return domains, nil
}

// hasAnyDomain reports whether the instance has entities in any of want. An
// empty want means the intent is domain-independent and always applies.
func hasAnyDomain(present map[string]bool, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for _, d := range want {
		if present[d] {
			return true
		}
	}
	return false
}

// registerIntentTool adds one intent as an MCP tool. Arguments pass through to
// Home Assistant's intent handler, which resolves name/area/floor itself.
func (t *Tools) registerIntentTool(server *mcp.Server, def IntentDef) {
	annotations := mcputil.Additive()
	if def.Destructive {
		annotations = mcputil.Destructive()
	}

	schema := def.InputSchema()
	resolved := resolveSchema(def.ToolName, schema)

	server.AddTool(&mcp.Tool{
		Name:        def.ToolName,
		Description: def.Description + " (Home Assistant intent " + def.Intent + ".)",
		InputSchema: schema,
		Annotations: annotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slots, err := decodeArgs(req, resolved)
		if err != nil {
			return mcputil.Errorf("%v", err), nil
		}
		result, err := t.client.HandleIntent(ctx, def.Intent, slots)
		if err != nil {
			return mcputil.Errorf("%v", err), nil
		}
		res, _, _ := mcputil.JSONResult(result)
		return res, nil
	})
}

// registerScriptTools adds one tool per script, with parameters from the
// script's declared fields.
func (t *Tools) registerScriptTools(ctx context.Context, server *mcp.Server) (int, error) {
	services, err := t.client.GetServices(ctx)
	if err != nil {
		return 0, err
	}

	var scripts map[string]any
	for _, entry := range services {
		if domain, _ := entry["domain"].(string); domain == "script" {
			scripts, _ = entry["services"].(map[string]any)
			break
		}
	}
	if len(scripts) == 0 {
		return 0, nil
	}

	aliases := t.scriptAliases(ctx)

	names := make([]string, 0, len(scripts))
	for name := range scripts {
		names = append(names, name)
	}
	sort.Strings(names)

	count := 0
	for _, objectID := range names {
		if scriptDomainServices[objectID] {
			continue
		}
		desc, _ := scripts[objectID].(map[string]any)
		if desc == nil {
			desc = map[string]any{}
		}

		schema := &jsonschema.Schema{Type: "object"}
		if fields, ok := desc["fields"].(map[string]any); ok && len(fields) > 0 {
			schema = ServiceFieldsToSchema(fields)
		}

		description := scriptDescription(objectID, desc, aliases["script."+objectID])
		toolName := "home_script_" + objectID
		resolved := resolveSchema(toolName, schema)

		server.AddTool(&mcp.Tool{
			Name:        toolName,
			Description: description,
			InputSchema: schema,
			// A script can do anything, so do not claim it is non-destructive.
			Annotations: mcputil.Destructive(),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args, err := decodeArgs(req, resolved)
			if err != nil {
				return mcputil.Errorf("%v", err), nil
			}
			states, err := t.client.CallService(ctx, "script", objectID, args)
			if err != nil {
				return mcputil.Errorf("%v", err), nil
			}
			res, _, _ := mcputil.JSONResult(map[string]any{
				"script":         "script." + objectID,
				"changed_states": states,
			})
			return res, nil
		})
		count++
	}
	return count, nil
}

// scriptDescription builds a script tool's description, appending the entity's
// registry aliases the way upstream's ScriptTool does so spoken or informal
// names still match.
func scriptDescription(objectID string, desc map[string]any, aliases []string) string {
	text, _ := desc["description"].(string)
	if text == "" {
		text, _ = desc["name"].(string)
	}
	if text == "" {
		text = "Runs the Home Assistant script " + objectID + "."
	}
	if len(aliases) > 0 {
		sort.Strings(aliases)
		text += " Aliases: " + joinComma(aliases) + "."
	}
	return text
}

// scriptAliases reads entity-registry aliases for script entities. Best effort:
// aliases are a nicety, and a WebSocket failure must not cost us the tools.
func (t *Tools) scriptAliases(ctx context.Context) map[string][]string {
	ws := t.client.NewWebsocketClient()
	if err := ws.Dial(ctx); err != nil {
		slog.Debug("hass: skipping script aliases", "error", err)
		return nil
	}
	defer func() { _ = ws.Close() }()

	entries, err := ws.ListEntityRegistry()
	if err != nil {
		slog.Debug("hass: skipping script aliases", "error", err)
		return nil
	}

	out := make(map[string][]string)
	for _, e := range entries {
		entityID, _ := e["entity_id"].(string)
		if !strings.HasPrefix(entityID, "script.") {
			continue
		}
		if names := stringList(e["aliases"]); len(names) > 0 {
			out[entityID] = names
		}
	}
	return out
}

// resolveSchema prepares a runtime-built schema for input validation. The
// go-sdk only validates arguments for tools added through the generic
// mcp.AddTool, which resolves the schema it derives from the Go type. Tools
// added through the raw Server.AddTool advertise their schema but get no
// validation, so generated tools resolve it here and check arguments
// themselves. A schema that cannot be resolved yields nil, which skips
// validation rather than losing the tool.
func resolveSchema(name string, schema *jsonschema.Schema) *jsonschema.Resolved {
	resolved, err := schema.Resolve(nil)
	if err != nil {
		slog.Warn("hass: tool schema will not be validated", "tool", name, "error", err)
		return nil
	}
	return resolved
}

// decodeArgs unmarshals a raw tool call's arguments and validates them against
// resolved, when the schema could be resolved. The generic mcp.AddTool does
// this from a struct type; generated tools have no compile-time type, so they
// decode into a map.
func decodeArgs(req *mcp.CallToolRequest, resolved *jsonschema.Resolved) (map[string]any, error) {
	args := map[string]any{}
	if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return nil, fmt.Errorf("decoding arguments: %w", err)
		}
		if args == nil {
			args = map[string]any{}
		}
	}
	if resolved != nil {
		if err := resolved.Validate(args); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
	}
	return args, nil
}
