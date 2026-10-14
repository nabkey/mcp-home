# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Go MCP (Model Context Protocol) server for smart home and media management, served over HTTPS via Cloudflare Tunnel and/or an embedded Tailscale node. Built with the [go-sdk](https://github.com/modelcontextprotocol/go-sdk) MCP library. Integrates Home Assistant, Sonarr/Radarr, and Frigate NVR.

## Build & Run

```bash
# Build
go build ./...

# Run (creates/reuses Cloudflare Tunnel, starts HTTP server, runs cloudflared)
go run ./cmd/mcp-server

# Run tests
go test ./...

# Run a single test
go test ./internal/hass -run TestFunctionName

# Vet
go vet ./...

# Lint
golangci-lint run ./...

# Everything CI runs (format check, build, vet, test, lint)
mise run check
```

Go 1.27.1 and golangci-lint are managed via mise (see `mise.toml`, which also defines `build`/`test`/`vet`/`lint`/`fmt`/`check` tasks). CI (`.github/workflows/ci.yml`) runs gofmt/vet/build/test (with `-race`) and golangci-lint on every PR; the publish workflow is gated on tests.

### Releases & container

Versioning is [SemVer](https://semver.org), automated via [release-please](https://github.com/googleapis/release-please) — push [conventional commits](https://www.conventionalcommits.org) (`feat:`, `fix:`, `feat!:`) to `main` and a release PR appears. Merging it tags `vX.Y.Z`, which triggers `.github/workflows/publish.yml` to build and push a multi-arch image to `ghcr.io/nabkey/mcp-home`. The version is injected into the binary via `-ldflags "-X main.version=..."` and surfaced by the `--version` (`-V`) flag. `cloudflared` is bundled in the image.

### Configuration

All config is via environment variables (or CLI flags). Uses [Kong](https://github.com/alecthomas/kong) with `envprefix` tags — run `go run ./cmd/mcp-server --help` to see all flags with their env var names.

**Front doors (at least one required; both may run at once):**

- **Cloudflare:** `CF_API_TOKEN`, `CF_ACCOUNT_ID`, `CF_ZONE_ID`, `CF_HOSTNAME` (all-or-nothing), `CF_TUNNEL_NAME` (default: `mcp-server`). Public HTTPS via Cloudflare Tunnel, authenticated by Cloudflare Access OAuth. `--insecure` disables JWT validation on this listener only.
- **Tailscale:** `TS_AUTHKEY` enables an embedded tsnet node; `TS_HOSTNAME` (default `mcp-home`), `TS_STATE_DIR` (default `/home/nonroot/tsstate`, persist it), `TS_ALLOWED_TAGS` (default `tag:voice-agent`), `TS_ALLOWED_LOGINS`. At least one allowlist must name someone: entries are trimmed, blank ones (`TS_ALLOWED_TAGS=","`) do not count, tags must have the `tag:` prefix, and a tag in the login list is rejected. Callers are identified by WireGuard peer via `WhoIs`; there is no token and `--insecure` does not apply.

The root `CLI.Validate` rejects a configuration with neither, so a server nobody can reach never starts.

**Tool integrations (optional groups):** Each group is all-or-nothing — partially setting a group (e.g., `HASS_URL` without `HASS_TOKEN`) produces a clear error at startup.

- `HASS_URL`, `HASS_TOKEN` — Home Assistant; optional `HASS_DENY_SERVICES` (comma-separated `domain.service` patterns, `*` wildcards) refuses listed services in `call_home_service` and `execute_script`
- `SONARR_URL`, `SONARR_API_KEY` — Sonarr (TV)
- `RADARR_URL`, `RADARR_API_KEY` — Radarr (Movies)
- `FRIGATE_URL` — Frigate NVR
- `ESPHOME_URL` — ESPHome dashboard (the HA ESPHome add-on); optional `ESPHOME_PASSWORD` if the dashboard has auth enabled

`LOG_LEVEL` (debug/info/warn/error) controls slog verbosity; `LOG_FORMAT` (text/json, default text) picks the slog handler. Every tool call is audit-logged via an MCP receiving middleware (`internal/server/audit.go`) with the user (CF Access email, `service:<id>` for an Access service token, or the tailnet WhoIs identity), the `front_door` (`cloudflare`, `tailnet`, `cloudflare-insecure`), the arguments with credentials redacted (by key, and by pattern for JWTs, bearer tokens, `token=` query params and YAML `password:`/`key:` lines; `!secret` references are kept), the outcome and the tool's error text, and whether a confirmation was `accepted`/`declined`/`accepted_unverified`. Redaction is best-effort pattern matching, so keep new tools from echoing credentials into arguments or error text. All tools carry MCP annotations (`mcputil.ReadOnly/Destructive/Additive`); error results set `IsError`.

Results are budgeted for a model's context, not a screen: `mcputil.JSONResult` emits compact JSON, and the list-shaped tools (`get_home_states`, `get_home_registry`, `list_home_services`, `list_esphome_devices`) return a slim projection by default (`internal/hass/slim.go`, `states.go`) with `attributes=true` / `full=true` / a `domain` for the raw record. Keep new list tools on the same pattern.

The HA read tools go further and are held to a budget: a common household question ("did the garage close after we left?", "what used the most energy this week?") is answered in at most two calls and ~5k tokens of tool output. `TestHouseholdQuestionBudget` (`internal/hass/budget_test.go`) holds a synthetic home shaped like a real one to that; `TestReplayLiveCaptures` reruns the question set (`questions_test.go`) against captures of a real instance when `HASS_REPLAY_DIR` points at them (captures are personal data and are never committed). Measured on the live instance (903 states, 1,562 registry entities, ~2,000 logbook entries a day): the slim unfiltered state dump was ~34k tokens and the slim registry ~74k (raw: ~71k and ~321k); unfiltered, both now return counts per domain/area (~200 and ~500 tokens). The 14 household questions went from ~114k tokens of output, three of them over budget (a day of logbook at ~74k, overnight power history at ~19k, energy-by-circuit at ~5.5k) and one dead end (a calendar 404), to ~7.9k with none over. How:

- **Home context** (`context.go`): the time zone and the entity/device/area registries are cached per client for 5 minutes. Every timestamp is the home's local time with its offset, states carry their `area`, `search` matches area names, and `start`/`end` accept local times (`2026-10-13 22:00`).
- **Diagnostic/config/hidden entities are left out by default** (~45% of states live: signal strength, firmware, restart buttons, network device trackers), reported as `omitted_diagnostic`, with `all=true` to include them; an explicit `entity_id` always includes them.
- **History** (`history.go`): numeric series become time-weighted min/mean/max, bucketed to ~24 rows when long; state series become transitions plus `time_in_state`. `raw=true` returns the samples.
- **Logbook** (`logbook.go`): array rows with each name listed once; in a whole-house view an entity with ≥6 entries folds into `frequent`; a filtered view folds only to fit the 100-row cap; every omission is counted.
- **Statistics** (`statistics.go`): a window `total_change` (or mean/min/max) per statistic, local period starts instead of epoch ms, and `search=` ranks matching sensors by total, so "what used the most" is one call.
- An empty or unfiltered result carries a `note` saying what to try next, and REST errors (`APIError`) say what the status means (bad token, integration not loaded, proxy down, login page instead of the API).

Config struct definitions are in `internal/config/config.go`. Each optional group has `Enabled() bool` and `Validate() error` methods. Kong invokes `Validate()` on each embedded group during `Parse`, so a group enforces its all-or-nothing rule just by defining the method.

### Prerequisites

`cloudflared` is auto-downloaded to `~/.cache/mcp-server/` if not on PATH.

## Architecture

Single entrypoint (`cmd/mcp-server/`) that builds one MCP handler and mounts it behind whichever front doors are configured. `runDoors` brings the doors up concurrently; if one fails to come up the server refuses to start, after stopping whatever the other already started (no orphaned cloudflared or tsnet node). Once up, they run in an `errgroup` so a fatal failure (or SIGINT/SIGTERM) shuts everything down. On shutdown each listener stops accepting and drains in-flight requests for `drainTimeout` (8s, equal to cloudflared's grace period, inside Docker's default 10s stop timeout); the tsnet node is closed only after its listener has drained. The mux for each door is built by `cloudflareMux` / `tailnetMux` so the wiring is testable (`cmd/mcp-server/frontdoor_test.go`).

- **Cloudflare** (`serveCloudflare`):
  1. Finds (or downloads) `cloudflared` first, so a host that cannot run it fails at boot
  2. Auto-discovers Cloudflare Access team domain + application AUD from the API (an app scoped to `host/mcp` is preferred over one for the whole host; transient API errors are retried)
  3. Uses the Cloudflare API (`cloudflare-go/v4`) to create/reuse a named tunnel, configure ingress, and ensure a DNS CNAME record (transient errors retried; permanent ones name the variable or token permission to fix, via `internal/cfapi`)
  4. Starts an HTTP server on a random localhost port (serves `/mcp`, `/mcp/sse`, `/health`, `/.well-known/oauth-protected-resource`)
  5. Supervises `cloudflared tunnel run` (token via `TUNNEL_TOKEN` env var). An unexpected exit is logged and cloudflared restarted with backoff, after re-running setup — which picks up a rotated token and recreates a tunnel deleted out from under us — so a cloudflared crash costs the Cloudflare path seconds and never takes the tailnet door down. On shutdown it gets SIGTERM with `TUNNEL_GRACE_PERIOD`, not SIGKILL. Its output is re-logged through slog, so `LOG_FORMAT=json` stays one object per line.
- **Tailscale** (`serveTailnet`): brings up a tsnet node from `TS_STATE_DIR` (checked writable first), listens on `:443` on the tailnet with a Tailscale-issued cert, and serves `/mcp`, `/mcp/sse`, `/health` behind `tsauth.Middleware`. No ports are published on the host; tsnet dials out like cloudflared does.

```
Claude.ai / Claude Code CLI
  → HTTPS → Cloudflare Edge (CF_HOSTNAME)
    → Cloudflare Access (self_hosted app, OAuth enabled)
      → cloudflared tunnel (subprocess)
        → http://127.0.0.1:<random>/mcp
          → [Cf-Access-Jwt-Assertion → Bearer bridge]
            → auth.RequireBearerToken → StreamableHTTPHandler → mcp.Server
```

Clients connect directly to `https://CF_HOSTNAME/mcp`. Cloudflare Access acts as both the edge gateway and OAuth 2.1 authorization server. CF Access injects a signed JWT via `Cf-Access-Jwt-Assertion`; a bridge middleware copies it to `Authorization: Bearer` for the go-sdk's `auth.RequireBearerToken` to validate. See `SECURITY.md` for the full security model.

```
Tailnet peer (e.g. voice agent)
  → WireGuard → tsnet node (TS_HOSTNAME.<tailnet>.ts.net:443, Tailscale cert)
    → tsauth.Middleware: WhoIs(remote addr) → allowlist (TS_ALLOWED_TAGS / TS_ALLOWED_LOGINS)
      → auth.RequireBearerToken (placeholder token; records the WhoIs identity as TokenInfo)
        → StreamableHTTPHandler → mcp.Server
```

On the tailnet path the identity is the WireGuard peer, not a token. The middleware still passes through `auth.RequireBearerToken` with a placeholder bearer and a verifier that reads the WhoIs identity from the context: the go-sdk's `TokenInfo` context key is unexported, and both the transport's session-user check and `auditMiddleware` read the user from there. Without that step every tailnet tool call would be audit-logged as `anonymous`. Any `Authorization` header a tailnet client sends is discarded before this point.

### Transport

The streamable HTTP transport runs **stateless** (`internal/server/http.go`), which is what lets it serve protocol `2026-07-28` — the SDK accepts that revision only when sessions are off. Consequences worth knowing:

- No `Mcp-Session-Id` is issued and no client is pinned to a process, so recreating the container on release does not invalidate what a connected client holds (`TestRedeployIsInvisibleToClients` calls one instance and then another, on both protocols). Every POST carries its own peer info in `params._meta` and is answered on the spot. What a redeploy *is* visible as: requests that arrive while the new container is starting fail (cloudflared/tsnet are down for those seconds), and a call still running after the 8s drain is cut.
- Older clients are unaffected; they negotiate down to `2025-11-25` and go without a session id.
- `GET` returns `405`, so the standalone SSE stream is gone and `/mcp/sse` serves POST only. Nothing here needs it: the server issues no server→client requests (sampling, roots, logging), configures no `EventStore`, and pushes no unsolicited notifications — all of which stateless mode forbids or drops. The one interaction that runs the other way, confirmation prompts, is embedded in the tool result instead (next bullet).
- **Destructive tools ask before acting** through multi round-trip requests (SEP-2322). `mcputil.Confirm` returns an input-required result carrying a yes/no form; the client shows it and retries the same call with the answer in `inputResponses`, so nothing is held open and the stateless transport is unaffected. The question carries a `requestState` that is an HMAC over the tool name, the canonicalized arguments and a 15-minute expiry; a "yes" counts only with a valid state for the same call, so a client cannot pre-answer without asking or carry one call's "yes" onto a retry with different arguments (a "no" is always honoured). The key is derived from the configured secrets (`server.New` → `mcputil.SetConfirmSecret`), so an answer survives a redeploy. It is gated on the client having negotiated `2026-07-28` *and* advertising form elicitation, because an older client can only be asked via `elicitation/create`, which stateless mode cannot deliver; such a client gets the pre-confirmation behaviour, the tool just runs. Today it guards `upload_esphome`, `write_esphome_file` when the file already exists, `manage_dashboards` `save_config`/`delete_config`/`delete`, and `manage_automations` `delete`. The audit log records each call's `protocol` and `elicitation` so the deployed server shows which clients are actually being asked; Claude Code on its v2 runtime is, and claude.ai's connector was unverified when this shipped.
- `PropagateRequestCancellation` ties tool handlers to the originating HTTP request, so a client that hangs up stops long-running work (ESPHome compiles and log captures, Frigate snapshots, HA websocket round trips) instead of leaving it running against the home network. **This applies only to clients on `2026-07-28`**, where the POST is the whole request lifecycle; for an older client the option is a no-op and an abandoned request still runs to completion.
- Clients on `2026-07-28` must mirror the JSON-RPC method into an `Mcp-Method` header (SEP-2243); a body/header mismatch is rejected with `-32020`. Request bodies are capped at the SDK's `DefaultMaxRequestBodyBytes`, over which the server returns `413`.
- `tools/list` and `server/discover` carry a 5-minute freshness hint (`ttlMs`, SEP-2549) from the `ServerOptions.SetCacheable` policy in `internal/server/cache.go`. The list is fixed for the process lifetime — static tools and generated ones are both registered at startup — so the TTL exists to bound how long a client keeps calling tools a redeploy has removed, not to track in-process change. Scope is `private`, not the SDK's `public` default: the list describes this specific home (script names, areas, device names) and no intermediary should be serving it to anyone else. That hint only binds MCP-aware clients (the go-sdk client does not even read it), and the SDK's own HTTP header was `Cache-Control: no-cache`, which lets a shared cache store a response; so `middleware.NoStore` sets `private, no-store, no-transform` on every MCP response, and that is what keeps HTTP intermediaries from sharing it. A request with a cursor is left unhinted, since a page is only meaningful next to its cursor; the hook cannot see the result, so it relies on `PageSize` staying unset (pinned by `TestToolListIsSinglePage`).

### Key packages

- `cmd/mcp-server/` — Entrypoint. Parses config via Kong, starts HTTP, sets up tunnel, runs cloudflared.
- `internal/config/` — Kong CLI struct with `envprefix` tags and `Enabled()`/`Validate()` methods.
- `internal/server/` — Server factory. Creates `mcp.Server` and conditionally registers tool sets based on `config.CLI`. `http.go` builds the stateless streamable HTTP handler; `audit.go` is the tool-call audit middleware.
- `internal/tunnel/` — Cloudflare Tunnel lifecycle: create/reuse tunnel via API, configure ingress rules, ensure DNS CNAME, get token, supervise cloudflared (restart + re-setup, graceful SIGTERM). Auto-downloads cloudflared if not on PATH. Tests run against a fake Cloudflare API and a fake `cloudflared` script.
- `internal/cfapi/` — Classifies Cloudflare API errors (transient vs. permanent), explains permanent ones in operator terms, and retries transient ones.
- `internal/tsauth/` — Embedded Tailscale node (tsnet): start/up, TLS listener, and the `WhoIs` allowlist middleware that records the caller as `auth.TokenInfo`. Allowlist entries are trimmed and matched exactly; an empty allowlist refuses everyone.
- `internal/cfaccess/` — Cloudflare Access JWT validation and auto-discovery. `Discover()` finds the team domain and application AUD from the API. `TokenVerifier()` adapts JWT validation to the go-sdk's `auth.RequireBearerToken` interface.
- `internal/middleware/` — HTTP request logging middleware.
- `internal/mcputil/` — Shared MCP result helpers (`TextResult`, `JSONResult`), annotations, and `Confirm`.
- `internal/validate/` — Input validation for path injection prevention.
- `internal/hass/` — Home Assistant REST + WebSocket client and MCP tools. Also holds the generated tool layer: `selector.go` (HA selector → JSON Schema), `intents.go` (the intent catalog + `/api/intent/handle`), `vocab.go` (exposed names/areas/floors for tool descriptions), and `generated_tools.go` (startup registration of intent and per-script tools).
- `internal/lists/` — To-do list management tools. Depends on the HA client.
- `internal/media/` — Sonarr/Radarr client and MCP tools.
- `internal/frigate/` — Frigate NVR client and MCP tools.
- `internal/esphome/` — ESPHome dashboard client (`client.go`, `/ws` command channel + HTTP firmware download), output cleaning (`lines.go`), failure diagnosis (`diagnose.go`), post-flash verification (`verify.go`) and MCP tools.

### MCP Tool Registration Pattern

Tools use the go-sdk generic `mcp.AddTool[In, Out]` pattern:
- Define an args struct with `json` and `jsonschema:"..."` tags (tag value is the description directly, no `description=` prefix)
- Handler signature: `func(ctx context.Context, req *mcp.CallToolRequest, args T) (*mcp.CallToolResult, any, error)`
- Return content via `*mcp.CallToolResult` with `TextContent`; the second return value (`any`) is unused

### Generated tool layer

Most tools are hand-written. Two families are generated at startup against the live instance (`hass.Tools.RegisterGenerated`, called from `internal/server`), so the tool list reflects what this particular Home Assistant actually has.

This mirrors Home Assistant's own MCP server (`homeassistant/components/mcp_server`), which is a thin adapter over the "assist" LLM API in `homeassistant/helpers/llm.py` plus a per-integration `llm.py` platform. Two decisions were taken from upstream:

- **Group by intent, not by domain or service.** A typical instance has 700+ services; one tool each is unusable. HA's answer is a curated set of cross-domain verbs — `HassTurnOn` covers light, switch, fan, lock and media_player at once — that each integration opts into. HA's intent handlers also resolve friendly names, areas and floors server-side, so callers say `area: "kitchen"` instead of looking up an `area_id` first.
- **Do not generate tools from arbitrary services.** Upstream has a generic `ActionTool` but wired it to scripts only; its own comment says the parameter cache "only works for services which add their description directly to the service description cache. This is not the case for most services, but it is for scripts." So scripts are the one place we generate per-service tools too.

Pieces:

- `selector.go` — `SelectorToSchema` ports upstream's `selector_serializer` (~25 selector types) to Go, and `ServiceFieldsToSchema` turns a service's `fields` into an object schema. It flattens HA's collapsed `additional_fields` group and, because JSON Schema cannot express "applies only to entities whose `supported_color_modes` includes X", renders a field's `filter` as description prose instead of dropping it. Where upstream is lossy it does better, because generated tools validate against the schema: a `select` with `custom_value` gets its options as suggestions rather than an enum (an enum would refuse the values the field exists for), option labels, number units and entity domain filters go into the description, `action` is a list of objects like `condition`, `area`/`device`/`floor`/`label` carry an `<kind>_id` format, and a field's `default` is kept.
- `intents.go` — `IntentCatalog` is the ported intent set, each with its slots and the domains it needs. `Client.HandleIntentAs` posts to `/api/intent/handle` with `assistant: "conversation"`; an unmatched intent returns HTTP 200 with an error response body, which is converted to a Go error so a no-op is not read as success.
- `vocab.go` — the house vocabulary, read once at startup: Assist-exposed entities with the names HA matches them by (computed name and registry aliases, which only `config/entity_registry/get_entries` returns — the `list` command omits them), their areas (entity, else device), and floors. Each intent tool's `name`/`area`/`floor` descriptions list the relevant slice ("Garage Workbench Switch (switch, Garage)"; names shared by several entities flagged; areas with nothing the tool controls left out; floors with the areas on them). HA matches names exactly after casefolding, so a model that had to guess "the left garage door" for "Garage Door Controller Left Garage Door" missed and spent a round trip on lookups; upstream puts the same list in its LLM prompt, which an MCP server cannot rely on. At call time the same data maps near-misses HA does not fold (curly vs straight apostrophe, a leading "the") to the exact spelling when the mapping is unique. It is guidance, not a gate: names absent from it still go to HA. Capped at `maxVocabEntries` per tool.
- `generated_tools.go` — registration. Intent tools register only when the instance has entities in the intent's domains (a vacuum-less home gets no vacuum tools); if the instance is unreachable the full catalog is registered rather than silently shrinking the tool list. Script tools come from the `script` domain in `GET /api/services`; the description leads with the script's name (the object id need not resemble it: `home_script_new_script` is "Mood Lights"), then its description, area and registry aliases. Startup queries are bounded by `generateTimeout`.

`utterance_test.go` is the outside judge for all of this: a fixture shaped like a real registry, a Go port of HA's `async_match_targets` and the intent handlers standing in for the instance, and a table of plain-language requests with the call a model would make from the advertised schemas. Each row asserts exactly which entities HA would act on, and that every name the call uses is on offer in the tool's own schema (no lookup needed). Extend the table when changing targeting.

Deliberate departures from upstream:

- **Timer intents are excluded.** Upstream gates them on `llm_context.device_id` being a voice satellite that supports timers; a REST caller has no such device, so those intents could never match.
- **Only Assist-exposed entities are matched.** Upstream's LLM API passes the assistant to the intent, so HA filters by exposure; `/api/intent/handle` without `assistant` matches *every* entity. Measured on a real registry, `home_turn_off {area: "driveway"}` then also switched off Frigate's driveway detection, and `{area: "garage"}` reached Frigate recording and config switches. The field is accepted from HA 2026.4; on a 400 that names it, the tools drop it for the process lifetime and log a warning (older HA cannot filter).
- **A target is required, and an area is not enough for on/off.** `vol.Any("name", "area", "floor")` in upstream's `DynamicServiceIntentHandler` is a key *matcher*, not a requirement — unmarked voluptuous keys are optional — so HA accepts an untargeted intent and matches every entity in scope. The generated schemas require at least one of `name`/`area`/`floor`/`domain`; `domain` counts, so "turn off all the lights" still works. `HassTurnOn`/`HassTurnOff` have no required domain upstream, so for those an area or floor only narrows and the call must also carry `name`, `domain` or `device_class`: `{area: "garage"}` for "close the garage" would also switch off the freezer plug. A `name` of "all" (HA's "no name") does not count. `HassSetPosition`/`HassStopMoving` likewise have no required domain and abort the whole call on the first matched non-cover, so they default `domain` to `[cover, valve]`.
- **Intent tools are gated on `HASS_DENY_SERVICES`.** `/api/intent/handle` takes an intent name rather than a service, so `CallService`'s policy check never runs. Each `IntentDef` therefore declares the services it can dispatch, and an intent whose services include a denied one is not registered at all. `HassTurnOn`/`HassTurnOff` are handled per domain instead (`onOffServices`): their fallback is `homeassistant.turn_off`, which dispatches `<domain>.turn_off`, so checking only the former let a deny on `switch.turn_off` through. Every domain whose services include a denied one is removed, and the `domain` slot is pinned to what remains on every call; any `script.*` deny removes `script` (`script.turn_on` runs whichever script matched). `HassSetVolumeRelative` declares `volume_up`/`volume_down` as well as `volume_set`, and search-and-play declares `search_media`. Generated script tools for a denied script are not registered.
- **Same-domain intents fold into one tool.** An `IntentDef` with `Group`/`Action` instead of `ToolName` becomes one value of an `action` enum on the grouped tool named in `IntentGroups` (today only `home_media`, holding the nine media_player intents). Every targeted intent repeats the same targeting schema, so nine near-identical tools cost the model far more than one with an enum. The merged schema validates types, the target requirement, and per-action required slots (as `allOf` if/then, re-checked at call time); only the chosen action's slots are forwarded to HA, and a denied service drops just that action from the enum. With no player named (`domain: ["media_player"]`), `set_volume`/`mute`/`unmute` act only on players that are playing and `unpause` only on paused ones, run once per player by entity id; upstream already scopes pause/next/previous/relative volume that way, and without it "set the volume to 20" set every speaker in the house.
- **`name` takes a list.** HA's intent takes one name, so "the porch light and the driveway lights" would be three tool calls; a list runs the intent once per name and reports each.
- **Generated tools validate their own input.** The go-sdk only validates arguments for tools added via the generic `mcp.AddTool`, which derives and resolves a schema from a Go type. Runtime-schema tools go through `Server.AddTool`, which advertises the schema but does not enforce it, so `decodeArgs` resolves and validates against it explicitly. Without this, `home_turn_on` with no `name`/`area`/`floor` would reach Home Assistant instead of being rejected.

### Service Clients

Each integration has its own client that handles HTTP/WebSocket communication:

- **Home Assistant** (`HASS_URL`, `HASS_TOKEN`) — REST API with Bearer token auth + WebSocket for automation/script/scene config, traces, registries (areas/devices/entities/labels/floors), and long-term statistics. Two-step WS auth handshake (`auth_required` → `auth` → `auth_ok`).
- **Sonarr** (`SONARR_URL`, `SONARR_API_KEY`) — `/api/v3/` endpoints, `X-Api-Key` header auth.
- **Radarr** (`RADARR_URL`, `RADARR_API_KEY`) — Same *arr API pattern as Sonarr.
- **Frigate** (`FRIGATE_URL`) — No auth (point it at the API port 5000, not the authenticated 8971), REST API for config, events, thumbnails and JPEG snapshots. Since 0.14 list filters are single comma-joined params and the score lives in `data.top_score` (top-level `top_score` is 0). Frigate's clock, and the timestamp burned into its images, is typically UTC; event times are reported in the house zone, taken from the tool's `timezone` argument, then Frigate's `ui.timezone`, then `TZ`, then the server zone (a UTC fallback is flagged in the result).
- **Sonarr/Radarr** lookups mark library items with a non-zero `id`; adds post the lookup record with the chosen profile/folder laid over it (as the *arr UIs do), minus that `id`.
- **ESPHome** (`ESPHOME_URL`, optional `ESPHOME_PASSWORD`) — ESPHome Device Builder dashboard (source: `esphome/device-builder`, not `esphome/esphome`; the protocol facts below were checked against it). All operations go over the single multiplexed `/ws` command socket (`{command, message_id, args}` → `ResultMessage` / streaming `EventMessage{event:"output"|"result"}` / `ErrorMessage`), one short-lived session per operation: `devices/list`, `config/get_secrets`, `devices/get_config`, `devices/update_config` (passes `expected` when overwriting so a concurrent dashboard edit fails with `precondition_failed` instead of being clobbered; falls back to `devices/create` on `not_found`), `devices/validate` (`show_secrets:false`) and `devices/logs` (streaming), `firmware/get_binaries` + `firmware/download_token` (binary then fetched over HTTP `/api/firmware/download?token=`), `firmware/get_jobs`, `firmware/cancel`. Compile and upload can run for minutes, so they use the dashboard's **async job queue**: `firmware/compile` / `firmware/upload` return a `FirmwareJob` immediately and the caller polls `firmware/get_job` until terminal (`completed`/`failed`/`cancelled`). This is the same poll pattern the MCP Tasks extension would model at the protocol layer, done at the application layer because the go-sdk in use doesn't implement Tasks yet. Auth: a dashboard reporting `requires_auth` gets an `auth/login` handshake on `/ws`; an add-on on its exposed port reports `requires_auth: false`.

  Upstream behaviours the client and tools depend on (verified in device-builder source and against the live dashboard):
  - `get_job` answers an **unknown or pruned id with a `null` result**, not an error; `Client.GetJob` turns that into `ErrJobNotFound` (otherwise a poller sees `status:""` forever). Terminal jobs come back with empty `output` (flushed to a per-job sidecar); running jobs carry their live buffer. `follow_job` replays the log then sends a terminal `result` frame `{status, exit_code, error, queued_update_armed}`.
  - **Supersede**: submitting any firmware job cancels every other queued/running job for the same configuration, with no error text on the cancelled job. So `upload_esphome` refuses while a compile is in flight (it would cancel the build and flash the old binary) or when the latest compile failed, `compile_esphome` refuses while a flash is in flight, and `get_esphome_job` reports a cancelled job with a newer sibling as `superseded`.
  - Jobs are **persisted across dashboard restarts**: queued/running jobs are re-queued under the same id, a running one starts over with a `dashboard restarted mid-build` marker in its log (surfaced as `restarted:true`).
  - **Offline OTA**: a failed OTA to a device the dashboard knows is offline arms `runtime_state.queued_update` and is flashed automatically on reconnect (`queued_update_armed` in the follow_job result → diagnosis `deferred_offline`).
  - **Output framing**: validate/logs events have their line terminator stripped, follow_job events keep theirs (bare `\r` = progress redraw). `esphome --dashboard` writes ANSI escapes as the literal text `\033`, and wraps secret-ish values (SSID, BSSID, keys) in the conceal SGR `\033[8m…\033[28m`; the dashboard redacts those only for validate, so `lines.go` redacts every stream, strips escapes, splits lines, collapses redraws and keeps the **tail** (2000 lines) while collecting error lines across the whole stream.
  - A streaming handler that returns without a terminal frame (server-side cancel) is followed by a bare `ResultMessage{result:null}`; that is reported as incomplete, never as success. An error frame with an empty `message_id` (unparseable command) fails the call immediately.
  - After a successful upload the dashboard **optimistically** sets `deployed_config_hash`/`deployed_version`, so they are not proof the device booted the build. `verify_esphome_device` instead reads the device's own dump_config over `devices/logs` (`ESPHome version X compiled on <time>` plus component lines), checks the version against the dashboard's, the build time against the compile job's window, the `expect` strings, and `was marked as failed` lines.

  Client hardening: socket closed on context cancel (a read deadline could be overwritten), 32 MB frame cap, handshake/hello/command/stream-idle timeouts, transport failures classified (`dashboard_unreachable`, `dashboard_stalled`, `connection_lost`, `protocol_error`, `auth_failed`). Every tool error and every terminal job result carries a `diagnosis` kind and a `next_action` (`diagnose.go`); output patterns are scoped by stage so e.g. "timed out" means a package download in a build but an unreachable device in an upload. The adversarial fake dashboard in `protocol_test.go` (drops, stalls, interleaved ids, unknown events, oversize frames, giant logs, cancellation, goroutine-leak check) and the pipeline tests in `pipeline_test.go` pin all of this.

All tool sets are optional — the server registers only what's configured and starts even with zero tools.

### Cloudflare Tunnel Management

The `internal/tunnel/` package manages the full tunnel lifecycle via the Cloudflare API (`cloudflare-go/v4`):
- Finds an existing tunnel by name or creates a new one (with `config_src: cloudflare`)
- Updates ingress configuration to route the hostname to the local HTTP server
- Creates or updates a proxied CNAME DNS record pointing to `<tunnel-id>.cfargotunnel.com`
- Retrieves the tunnel token and runs `cloudflared tunnel run` (token via env var)

Authentication: the server auto-discovers the CF Access team domain and application AUD at startup, validates Bearer tokens (RS256 JWTs signed by CF Access), and serves `/.well-known/oauth-protected-resource` for OAuth discovery. A bridge middleware copies `Cf-Access-Jwt-Assertion` to `Authorization: Bearer` since CF Access injects the JWT in its own header. Pass `--insecure` to disable auth for local development. All of this section applies only when the Cloudflare group is configured; a Tailscale-only deployment skips it entirely.

## MCP Tools Provided

**Home Assistant** (requires `HASS_URL`, `HASS_TOKEN`):

| Tool | Description |
|------|-------------|
| `get_home_states` | Current states with area and local `last_changed`; `search` (id/name/area), `domain`, `entity_id`, `area`, `device_class`, `state` combine; no filter returns counts per domain/area; diagnostic/config entities need `all=true`; `attributes=true` for raw |
| `get_home_events` | Logbook (state changes, automation triggers) over `hours` or `start`/`end`, filterable by `entity_id`/`domain`/`area`/`search`; chatty entities fold into `frequent` |
| `call_home_service` | Call HA services (turn_on, turn_off, set_temperature, etc.) |
| `get_todo_items` | Retrieve items from a todo list entity |
| `manage_automations` | CRUD operations on automations (`delete` asks for confirmation on capable clients) |
| `get_automation_traces` | Debug automation execution history via WebSocket |
| `manage_helpers` | CRUD operations on helpers (input_boolean, input_number, input_text, input_select, input_datetime, input_button, counter, timer, schedule) |
| `manage_scripts` | CRUD operations on scripts |
| `manage_scenes` | CRUD plus `activate` for scenes |
| `get_home_registry` | Topology (areas, devices, entities, labels, floors); unfiltered `kind=all` gives per-area counts; `area_id` (id or name)/`domain` for records, entities inheriting their device's area; disabled and diagnostic entries need `all=true`; `full=true` for raw records |
| `manage_registry` | Create/update/delete area/entity/device/label/floor registry entries (assign areas, rename, label) |
| `execute_script` | Run an ad-hoc action sequence (HA script syntax) without storing a script |
| `get_diagnostics` | Health/error diagnostics: error log (the in-memory system log when there is no log file, as on HA OS), system health, open repair issues, persistent notifications (`kind=all` by default) |
| `list_home_services` | Discover services before calling them: names-only index of all domains, or full field schemas for one `domain` (and optionally one `service`) |
| `get_state_history` | How entities changed over `hours` or `start`/`end`: numeric series as time-weighted min/mean/max (bucketed when long), state series as transitions plus time in each state; `raw=true` for samples |
| `render_template` | Evaluate a Jinja2 template against current state for compound queries |
| `get_long_term_statistics` | Long-term statistics by `statistic_ids` (per-period rows) or `search` (window totals ranked); period defaults to fit the window |
| `get_calendar_events` | Upcoming events from every calendar (or the given `entity_id`s), merged and sorted, local times |
| `manage_dashboards` | List/read/save/delete Lovelace dashboard configs and create/update/delete storage dashboards (save_config overwrites the whole config; save_config/delete_config/delete ask for confirmation on capable clients) |
| `manage_dashboard_resources` | CRUD for Lovelace dashboard resources (custom JS/CSS modules) |
| `manage_entity_state` | Write or delete an entity's state directly in the state machine (virtual entities; does not control devices) |
| `fire_home_event` | Fire an event on the event bus (triggers automations listening for it) |
| `get_home_config` | Core config (version, unit system, time zone, location; the component list only with `kind=components`), and `configuration.yaml` validation (`kind=check`) |

**Generated tools** (requires HA; registered at startup from the live instance):

These are not hand-written. At startup the server queries the instance and generates two families of tools, mirroring what Home Assistant's own MCP server does. See "Generated tool layer" below.

| Tool | Description |
|------|-------------|
| `home_turn_on` / `home_turn_off` | Turn on/open/start or turn off/close/stop anything exposed to Assist, by entity name (or list of names); area/floor narrow and need a `domain` or `device_class` alongside |
| `home_set_position` / `home_stop_moving` | Position or stop a cover or valve |
| `home_light_set` | Set a light's brightness percentage, color name, or color temperature |
| `home_climate_set_temperature` | Set a thermostat's target temperature |
| `home_fan_set_speed` | Set a fan's speed percentage |
| `home_media` | One tool for the nine media_player intents, selected by `action`: pause, unpause, next, previous, set_volume, set_volume_relative, mute, unmute, search_and_play; with no player named it acts only on what is playing (unpause: paused) |
| `home_vacuum_start` / `_return_to_base` / `_clean_area` | Vacuum control |
| `home_humidifier_setpoint` / `home_humidifier_mode` | Humidifier target humidity and mode |
| `home_list_add_item` / `_complete_item` / `_remove_item` | To-do and shopping list items |
| `home_script_<object_id>` | One tool per script, with parameters from the script's declared `fields` |

**Lists** (requires HA):

| Tool | Description |
|------|-------------|
| `get_lists` | Retrieve all available to-do lists |
| `get_list_items` | Get items from a specific to-do list (by entity id or list name) |
| `modify_list_item` | Add, remove, complete, or uncomplete items (list by entity id or name, e.g. "Shopping list") |

**Media** (requires `SONARR_URL`/`SONARR_API_KEY` and/or `RADARR_URL`/`RADARR_API_KEY`):

| Tool | Description |
|------|-------------|
| `search_movies` | Search Radarr; compact results (year, tmdb_id, studio, in_library/downloaded, `kind: short` for extras), optional `year` filter, and an `ambiguous` group when results share a title |
| `add_movie` | Add a movie; requires `tmdb_id`+`title`+`year`, re-checked against TMDB (a mismatch is refused, which catches remakes). Already in the library returns its state instead of adding. Quality profile/root folder default to what the library uses (never the stock `Any`); searches by default; says when an unreleased film can be grabbed |
| `search_series` | Search Sonarr; compact results with year, network, `type` (anime/standard/daily), season count, library state, and `ambiguous` groups (UK/US versions, reboots, anime vs live action) |
| `add_series` | Add a series with the same `tvdb_id`+`title`+`year` check; sets `seriesType` anime from the lookup and prefers an anime root folder. Already in the library returns per-season monitored/downloaded state (answers "get me the new season") |
| `get_download_queue` | Queue named by title/episode, season packs collapsed, one `state` per item (downloading, stalled, import_blocked, failed, delayed...), an ETA only when credible (moving and under 3 days), and per-service `errors` so a failed service never reads as an empty queue |

**Frigate NVR** (requires `FRIGATE_URL`):

| Tool | Description |
|------|-------------|
| `list_frigate_cameras` | Enabled cameras with their zones and tracked labels, plus the house time zone and current local time |
| `get_camera_snapshot` | Live frame as JPEG, 360px/q70 by default (~25-35 KB vs ~120-135 KB full size), with capture time and an offline flag |
| `get_frigate_events` | Events by camera/zone/label/sub_label/min_score and `after`/`before` in house-local time (bare `HH:MM` resolves against now); times returned with their offset; `thumbnails=true` attaches up to 6 small thumbnails (64 KB cap) so a door/driveway question takes 2 calls |
| `get_event_snapshot` | One event's snapshot, 360px/q70 by default, `crop=true` zooms to the object; returned with camera, label, score and local time |

**ESPHome** (requires `ESPHOME_URL`):

| Tool | Description |
|------|-------------|
| `list_esphome_devices` | List dashboard devices with config file, address, online state, installed/deployed versions, `queued_update` (`full=true` for the raw records) |
| `list_esphome_secrets` | List shared secrets.yaml key names (never values) |
| `read_esphome_file` | Read a device YAML |
| `write_esphome_file` | Create/overwrite a device YAML; overwrite is conditional on the text read (no clobbering a concurrent edit) and asks for confirmation on capable clients; reports `missing_secrets` |
| `validate_esphome` | Validate a config without building: `valid` true/false/null, errors, diagnosis, next_action; success output drops the resolved-config dump |
| `compile_esphome` | Queue a firmware build; returns a `job_id` and `poll_after_seconds` (async). Refuses while the device is being flashed |
| `upload_esphome` | Queue an OTA flash of the latest build (async, destructive, asks for confirmation on capable clients). Refuses while a compile is in flight, after a failed compile, or with no binary |
| `get_esphome_job` | Poll a compile/upload job: progress, recent output, `restarted`; once done, success, errors, `diagnosis`, `next_action` |
| `cancel_esphome_job` | Cancel a queued/running job |
| `download_esphome_binary` | Confirm a built image exists; report size + SHA-256 |
| `get_esphome_logs` | Capture live device logs for a bounded duration (redacted), with connected / running_version / compiled_on / failed_components |
| `verify_esphome_device` | Confirm a flash took: device answers, runs the dashboard's ESPHome version, was built by the given compile job, shows the `expect` strings, no failed components |
