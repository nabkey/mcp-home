package hass

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync/atomic"
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
//
// Intent tools are matched against Assist-exposed entities only, and carry
// the names those entities, their areas and floors answer to (see vocab.go),
// so a model can aim the first call correctly without a lookup.

// generateTimeout bounds the startup queries that decide what to register.
const generateTimeout = 15 * time.Second

// scriptDomainServices are the script domain's own services, which are not
// individual scripts and so get no generated tool. manage_scripts and
// call_home_service already cover them.
var scriptDomainServices = map[string]bool{
	"reload": true, "turn_on": true, "turn_off": true, "toggle": true,
}

// generated holds what the generated tool handlers share at call time.
type generated struct {
	client *Client
	vocab  *houseVocab
	// legacyIntentAPI is set once Home Assistant rejects the assistant
	// field (releases before 2026.4); later calls go without it.
	legacyIntentAPI atomic.Bool
}

// handleIntent fires an intent restricted to Assist-exposed entities,
// falling back to an unrestricted call on releases that cannot restrict.
func (g *generated) handleIntent(ctx context.Context, intent string, slots map[string]any) (*IntentResult, error) {
	if !g.legacyIntentAPI.Load() {
		res, err := g.client.HandleIntentAs(ctx, intent, slots, assistantID)
		if !errors.Is(err, errAssistantUnsupported) {
			return res, err
		}
		g.legacyIntentAPI.Store(true)
		slog.Warn("hass: this Home Assistant predates 2026.4 and cannot limit /api/intent/handle to Assist-exposed entities; generated intent tools can reach unexposed entities")
	}
	return g.client.HandleIntent(ctx, intent, slots)
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

	g := &generated{client: t.client}
	var domains map[string]bool
	states, err := t.client.GetStates(ctx)
	if err != nil {
		slog.Warn("hass: could not read states to select generated tools; registering all intents", "error", err)
	} else {
		domains = presentDomainSet(states)
		g.vocab = t.loadVocab(ctx, states)
	}

	registered := 0
	for _, def := range IntentCatalog {
		if def.Group != "" {
			continue // registered below as part of its group
		}
		if domains != nil && !hasAnyDomain(domains, def.Domains) {
			continue
		}
		var allowed []string
		if def.OnOff != "" {
			// HassTurnOn/Off dispatch per entity domain, so the deny policy
			// narrows the domains they may reach instead of withholding
			// the whole tool.
			var narrowed bool
			allowed, narrowed = t.onOffAllowed(def.OnOff)
			if len(allowed) == 0 {
				slog.Info("hass: intent tool withheld by service deny policy", "tool", def.ToolName)
				continue
			}
			if narrowed {
				slog.Info("hass: intent tool limited by service deny policy", "tool", def.ToolName, "domains", allowed)
			} else {
				allowed = nil
			}
		} else if denied := t.deniedServices(def); denied != "" {
			// The intent endpoint takes an intent name, not a service, so
			// CallService's deny check never runs for it. Enforce the
			// policy here instead, against every service it could dispatch.
			slog.Info("hass: intent tool withheld by service deny policy",
				"tool", def.ToolName, "denied", denied)
			continue
		}
		g.registerIntentTool(server, def, allowed)
		registered++
	}
	groups := make([]string, 0, len(IntentGroups))
	for name := range IntentGroups {
		groups = append(groups, name)
	}
	sort.Strings(groups)
	for _, name := range groups {
		var members []IntentDef
		for _, def := range groupMembers(name) {
			if domains != nil && !hasAnyDomain(domains, def.Domains) {
				continue
			}
			// A denied action drops out of the enum; the rest of the
			// group stays available.
			if denied := t.deniedServices(def); denied != "" {
				slog.Info("hass: intent action withheld by service deny policy",
					"tool", name, "action", def.Action, "denied", denied)
				continue
			}
			members = append(members, def)
		}
		if len(members) == 0 {
			continue
		}
		g.registerIntentGroup(server, name, members)
		registered++
	}
	slog.Info("hass: registered intent tools", "count", registered, "vocabulary", g.vocab != nil)

	if n, err := t.registerScriptTools(ctx, server, g.vocab); err != nil {
		slog.Warn("hass: could not register script tools", "error", err)
	} else {
		slog.Info("hass: registered script tools", "count", n)
	}
}

// deniedServices returns the first service an intent could dispatch that the
// deny policy refuses, or "" when the intent is fully permitted.
//
// This is deliberately conservative: resolution happens inside the intent
// handler, so there is no later point at which to check a single target.
// HassTurnOn/Off are the exception (see onOffAllowed), because their
// services depend only on the matched entity's domain, which the domain slot
// can pin down in advance.
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

// onOffAllowed returns the domains HassTurnOn (dir "on") or HassTurnOff
// ("off") may act on under the deny policy, and whether any were removed.
//
// Checking only homeassistant.turn_off, as an earlier version did, left a
// gap: OnOffIntentHandler's fallback calls homeassistant.turn_off, which
// dispatches switch.turn_off, so a deny on switch.turn_off (say, to protect
// a freezer plug) was not enforced. Every domain's real services are checked
// now, and a domain with a denied one is excluded via the domain slot on
// every call. Scripts are excluded on any script.* deny, because
// script.turn_on runs whichever script was matched, including one denied
// by name.
func (t *Tools) onOffAllowed(dir string) ([]string, bool) {
	all := onOffDomains(dir)
	policy := t.client.policy
	if policy == nil {
		return all, false
	}
	var allowed []string
	for _, d := range all {
		ok := true
		for _, svc := range onOffServices(dir, d) {
			domain, service, _ := strings.Cut(svc, ".")
			if policy.Check(domain, service) != nil {
				ok = false
				break
			}
		}
		if ok && d == "script" && policyTouchesDomain(policy, "script") {
			ok = false
		}
		if ok {
			allowed = append(allowed, d)
		}
	}
	return allowed, len(allowed) != len(all)
}

// policyTouchesDomain reports whether any deny pattern names domain.
func policyTouchesDomain(p *ServicePolicy, domain string) bool {
	if p == nil {
		return false
	}
	for _, pat := range p.deny {
		if pat.domain == domain || pat.domain == "*" {
			return true
		}
	}
	return false
}

// presentDomainSet returns the set of entity domains in states.
func presentDomainSet(states []State) map[string]bool {
	domains := make(map[string]bool)
	for _, s := range states {
		if domain, _, ok := strings.Cut(s.EntityID, "."); ok {
			domains[domain] = true
		}
	}
	return domains
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

// applyVocab appends the house's names to a tool schema's targeting
// properties. domains selects which entities are listed.
func (g *generated) applyVocab(schema *jsonschema.Schema, domains []string, withDomain bool) {
	if g.vocab == nil {
		return
	}
	names, areas, floors := g.vocab.targetDescriptions(domains, withDomain)
	appendDesc(schema.Properties["name"], names)
	appendDesc(schema.Properties["area"], areas)
	appendDesc(schema.Properties["floor"], floors)
	if withDomain {
		present := map[string]bool{}
		for _, e := range g.vocab.entitiesFor(domains) {
			present[e.Domain] = true
		}
		var list []string
		for _, d := range domains {
			if present[d] {
				list = append(list, d)
			}
		}
		if len(list) > 0 {
			appendDesc(schema.Properties["domain"], "Types in this home: "+strings.Join(list, ", ")+".")
		}
	}
}

func appendDesc(s *jsonschema.Schema, extra string) {
	if s == nil || extra == "" {
		return
	}
	if s.Description == "" {
		s.Description = extra
		return
	}
	s.Description += " " + extra
}

// targetKeys are the slots Home Assistant resolves to entities.
var targetKeys = []string{"name", "area", "floor", "domain"}

// prepareTargets canonicalises the targeting slots against the house
// vocabulary and applies the per-intent targeting rules. allowed, when
// non-nil, is the set of domains the deny policy permits; the domain slot is
// always pinned inside it.
func (g *generated) prepareTargets(def IntentDef, args map[string]any, allowed []string) (map[string]any, error) {
	slots := map[string]any{}
	for _, key := range targetKeys {
		if v, ok := args[key]; ok {
			slots[key] = v
		}
	}
	if def.Group == "" {
		for _, slot := range def.Slots {
			if v, ok := args[slot.Name]; ok {
				slots[slot.Name] = v
			}
		}
	}

	vocabDomains := def.actsOn()
	if allowed != nil {
		vocabDomains = allowed
	}
	// A list of names is kept as []string for runIntent to fan out.
	if list, ok := slots["name"].([]any); ok {
		names := stringSlice(list)
		switch len(names) {
		case 0:
			delete(slots, "name")
		case 1:
			slots["name"] = names[0]
		default:
			slots["name"] = names
		}
	}
	if g.vocab != nil {
		if s, ok := slots["name"].(string); ok {
			slots["name"] = g.vocab.canonicalName(s, vocabDomains)
		}
		if names, ok := slots["name"].([]string); ok {
			for i, n := range names {
				names[i] = g.vocab.canonicalName(n, vocabDomains)
			}
		}
		if s, ok := slots["area"].(string); ok {
			slots["area"] = g.vocab.canonicalArea(s)
		}
		if s, ok := slots["floor"].(string); ok {
			slots["floor"] = g.vocab.canonicalFloor(s)
		}
	}

	if def.OnOff != "" {
		// Home Assistant treats the name "all" as no name at all, which
		// would turn {name: "all", area: "garage"} back into the
		// area-only call the schema forbids.
		if s, ok := slots["name"].(string); ok && isAllName(s) {
			delete(slots, "name")
		}
		if names, ok := slots["name"].([]string); ok {
			for _, n := range names {
				if isAllName(n) {
					return nil, errors.New("invalid arguments: \"all\" is not a device name; list the devices, or use domain to mean every device of a type")
				}
			}
		}
		_, hasName := slots["name"]
		_, hasDomain := slots["domain"]
		_, hasClass := slots["device_class"]
		if !hasName && !hasDomain && !hasClass {
			return nil, errors.New("invalid arguments: an area or floor alone would switch every device in it; add domain (e.g. [\"light\"]), device_class, or the device's name")
		}
	}

	requested := stringSlice(slots["domain"])
	switch {
	case allowed != nil && len(requested) > 0:
		kept := intersect(requested, allowed)
		if len(kept) == 0 {
			return nil, fmt.Errorf("invalid arguments: domain %s is not permitted by HASS_DENY_SERVICES for this tool", strings.Join(requested, ", "))
		}
		slots["domain"] = kept
	case allowed != nil:
		slots["domain"] = allowed
	case len(requested) == 0 && len(def.DefaultDomains) > 0:
		slots["domain"] = def.DefaultDomains
	}
	return slots, nil
}

// isAllName reports whether name is Home Assistant's "all" (which it treats
// as no name) or an obvious synonym.
func isAllName(name string) bool {
	switch normalizeName(name) {
	case "all", "everything", "all devices":
		return true
	}
	return false
}

// runIntent fires def with slots, once per name when name is a list.
func (g *generated) runIntent(ctx context.Context, def IntentDef, slots map[string]any, domains []string) *mcp.CallToolResult {
	names, multi := slots["name"].([]string)
	if !multi {
		result, err := g.handleIntent(ctx, def.Intent, slots)
		if err != nil {
			return mcputil.Errorf("%v", g.explain(err, slots, domains))
		}
		res, _, _ := mcputil.JSONResult(result)
		return res
	}
	type outcome struct {
		Name   string        `json:"name"`
		Result *IntentResult `json:"result,omitempty"`
		Error  string        `json:"error,omitempty"`
	}
	outs := make([]outcome, 0, len(names))
	failed := 0
	for _, n := range names {
		one := make(map[string]any, len(slots))
		for k, v := range slots {
			one[k] = v
		}
		one["name"] = n
		r, err := g.handleIntent(ctx, def.Intent, one)
		if err != nil {
			failed++
			outs = append(outs, outcome{Name: n, Error: g.explain(err, one, domains).Error()})
			continue
		}
		outs = append(outs, outcome{Name: n, Result: r})
	}
	res, _, _ := mcputil.JSONResult(map[string]any{"names": outs})
	// Any miss is an error so it is not read as done; the per-name results
	// show which ones did take effect.
	res.IsError = failed > 0
	return res
}

func stringSlice(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		return []string{x}
	}
	return nil
}

func intersect(a, b []string) []string {
	in := map[string]bool{}
	for _, s := range b {
		in[s] = true
	}
	var out []string
	for _, s := range a {
		if in[s] {
			out = append(out, s)
		}
	}
	return out
}

// explain adds names worth trying to a failed intent's error, so a miss
// costs at most one more call rather than a registry lookup.
func (g *generated) explain(err error, slots map[string]any, domains []string) error {
	if g.vocab == nil {
		return err
	}
	name, _ := slots["name"].(string)
	if name == "" {
		return err
	}
	if s := g.vocab.suggestNames(name, domains, 5); len(s) > 0 {
		return fmt.Errorf("%w (exposed names close to %q: %s)", err, name, strings.Join(s, "; "))
	}
	return err
}

// registerIntentTool adds one intent as an MCP tool. Arguments pass through to
// Home Assistant's intent handler, which resolves name/area/floor itself.
func (g *generated) registerIntentTool(server *mcp.Server, def IntentDef, allowed []string) {
	annotations := mcputil.Additive()
	if def.Destructive {
		annotations = mcputil.Destructive()
	}

	schema := def.InputSchema()
	domains := def.actsOn()
	if allowed != nil {
		domains = allowed
		appendDesc(schema.Properties["domain"], "Limited by policy to: "+strings.Join(allowed, ", ")+".")
	}
	g.applyVocab(schema, domains, def.OnOff != "")
	resolved := resolveSchema(def.ToolName, schema)

	server.AddTool(&mcp.Tool{
		Name:        def.ToolName,
		Description: def.Description + " (Home Assistant intent " + def.Intent + ".)",
		InputSchema: schema,
		Annotations: annotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs(req, resolved)
		if err != nil {
			return mcputil.Errorf("%v", err), nil
		}
		slots, err := g.prepareTargets(def, args, allowed)
		if err != nil {
			return mcputil.Errorf("%v", err), nil
		}
		return g.runIntent(ctx, def, slots, domains), nil
	})
}

// stateScopedActions are media actions that, when aimed at no particular
// player, should only act on players in the given state. Upstream already
// does this for pause/next/previous (required_states) and relative volume.
// Without it, "set the volume to 20" sent as domain ["media_player"] sets
// every speaker in the house to 20%, and "resume the music" with nothing
// paused by voice earlier sends media_play to every player, starting
// whatever each one last had queued.
var stateScopedActions = map[string]string{
	"set_volume": "playing", "mute": "playing", "unmute": "playing",
	"unpause": "paused",
}

// registerIntentGroup adds one tool for several intents, selected by an
// action argument. The merged schema validates types, the target
// requirement and per-action required slots; only the chosen action's slots
// are forwarded so Home Assistant never sees a stray one.
func (g *generated) registerIntentGroup(server *mcp.Server, name string, members []IntentDef) {
	annotations := mcputil.Additive()
	byAction := make(map[string]IntentDef, len(members))
	var domains []string
	seen := map[string]bool{}
	for _, m := range members {
		byAction[m.Action] = m
		if m.Destructive {
			annotations = mcputil.Destructive()
		}
		for _, d := range m.actsOn() {
			if !seen[d] {
				seen[d] = true
				domains = append(domains, d)
			}
		}
	}

	schema := groupInputSchema(members)
	g.applyVocab(schema, domains, false)
	resolved := resolveSchema(name, schema)

	server.AddTool(&mcp.Tool{
		Name:        name,
		Description: IntentGroups[name].Description,
		InputSchema: schema,
		Annotations: annotations,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		args, err := decodeArgs(req, resolved)
		if err != nil {
			return mcputil.Errorf("%v", err), nil
		}
		action, _ := args["action"].(string)
		def, ok := byAction[action]
		if !ok {
			return mcputil.Errorf("unknown action %q", action), nil
		}
		slots, err := g.prepareTargets(def, args, nil)
		if err != nil {
			return mcputil.Errorf("%v", err), nil
		}
		for _, slot := range def.Slots {
			v, ok := args[slot.Name]
			if !ok && slot.Required {
				return mcputil.Errorf("invalid arguments: action %q requires %q", action, slot.Name), nil
			}
			if ok {
				slots[slot.Name] = v
			}
		}

		if state, ok := stateScopedActions[action]; ok && untargeted(slots) {
			return g.actOnState(ctx, def, slots, state), nil
		}
		return g.runIntent(ctx, def, slots, domains), nil
	})
}

// untargeted reports whether slots carry no name, area or floor.
func untargeted(slots map[string]any) bool {
	for _, k := range []string{"name", "area", "floor"} {
		if _, ok := slots[k]; ok {
			return false
		}
	}
	return true
}

// actOnState runs def once per media player currently in state, naming each
// by entity id (which the intent matcher accepts as a name). Home Assistant
// still applies the exposure filter to every call.
func (g *generated) actOnState(ctx context.Context, def IntentDef, slots map[string]any, state string) *mcp.CallToolResult {
	states, err := g.client.GetStates(ctx)
	if err != nil {
		return mcputil.Errorf("%v", err)
	}
	var matched []string
	for _, s := range states {
		if strings.HasPrefix(s.EntityID, "media_player.") && s.State == state {
			matched = append(matched, s.EntityID)
		}
	}
	if len(matched) == 0 {
		return mcputil.Errorf("no media player is %s; name the player (or area) to %s", state, def.Action)
	}
	var results []*IntentResult
	var failures []string
	for _, id := range matched {
		one := map[string]any{"name": id}
		for k, v := range slots {
			if k != "domain" {
				one[k] = v
			}
		}
		res, err := g.handleIntent(ctx, def.Intent, one)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		results = append(results, res)
	}
	if len(results) == 0 {
		return mcputil.Errorf("%s", strings.Join(failures, "; "))
	}
	res, _, _ := mcputil.JSONResult(map[string]any{state: matched, "results": results, "failed": failures})
	return res
}

// registerScriptTools adds one tool per script, with parameters from the
// script's declared fields.
func (t *Tools) registerScriptTools(ctx context.Context, server *mcp.Server, vocab *houseVocab) (int, error) {
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
		// CallService would refuse a denied script on every call; do not
		// advertise a tool that can only fail.
		if err := t.client.policy.Check("script", objectID); err != nil {
			slog.Info("hass: script tool withheld by service deny policy", "script", objectID)
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

		aliases, area := scriptVocab(vocab, "script."+objectID, desc)
		description := scriptDescription(objectID, desc, aliases)
		if area != "" {
			description += " Area: " + area + "."
		}
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

// scriptVocab returns a script entity's aliases (its match names other than
// the script's own name) and area, from the vocabulary.
func scriptVocab(vocab *houseVocab, entityID string, desc map[string]any) ([]string, string) {
	if vocab == nil {
		return nil, ""
	}
	own, _ := desc["name"].(string)
	for _, e := range vocab.entities {
		if e.ID != entityID {
			continue
		}
		var aliases []string
		for _, n := range e.Names {
			if !strings.EqualFold(n, own) {
				aliases = append(aliases, n)
			}
		}
		return aliases, e.Area
	}
	return nil, ""
}

// scriptDescription builds a script tool's description: the script's name,
// then its description, then its registry aliases the way upstream's
// ScriptTool appends them, so spoken or informal names still match. The name
// matters on its own: "Mood Lights" may be described only as "Sets bedroom
// mode to Lamps Only: Red", and the tool name (from the object id, e.g.
// home_script_new_script) need not resemble either.
func scriptDescription(objectID string, desc map[string]any, aliases []string) string {
	text, _ := desc["description"].(string)
	name, _ := desc["name"].(string)
	switch {
	case text != "" && name != "" && !strings.Contains(strings.ToLower(text), strings.ToLower(name)):
		text = name + ": " + text
	case text == "":
		text = name
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
