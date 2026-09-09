# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Go MCP (Model Context Protocol) server for smart home and media management, served over HTTPS via Cloudflare Tunnel. Built with the [go-sdk](https://github.com/modelcontextprotocol/go-sdk) MCP library. Integrates Home Assistant, Sonarr/Radarr, and Frigate NVR.

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

**Cloudflare (required):** `CF_API_TOKEN`, `CF_ACCOUNT_ID`, `CF_ZONE_ID`, `CF_HOSTNAME`, `CF_TUNNEL_NAME` (default: `mcp-server`)

**Tool integrations (optional groups):** Each group is all-or-nothing — partially setting a group (e.g., `HASS_URL` without `HASS_TOKEN`) produces a clear error at startup.

- `HASS_URL`, `HASS_TOKEN` — Home Assistant; optional `HASS_DENY_SERVICES` (comma-separated `domain.service` patterns, `*` wildcards) refuses listed services in `call_home_service` and `execute_script`
- `SONARR_URL`, `SONARR_API_KEY` — Sonarr (TV)
- `RADARR_URL`, `RADARR_API_KEY` — Radarr (Movies)
- `FRIGATE_URL` — Frigate NVR
- `ESPHOME_URL` — ESPHome dashboard (the HA ESPHome add-on); optional `ESPHOME_PASSWORD` if the dashboard has auth enabled

`LOG_LEVEL` (debug/info/warn/error) controls slog verbosity. Every tool call is audit-logged with the CF Access user via an MCP receiving middleware (`internal/server/audit.go`). All tools carry MCP annotations (`mcputil.ReadOnly/Destructive/Additive`); error results set `IsError`.

Config struct definitions are in `internal/config/config.go`. Each optional group has `Enabled() bool` and `Validate() error` methods. Kong invokes `Validate()` on each embedded group during `Parse`, so a group enforces its all-or-nothing rule just by defining the method.

### Prerequisites

`cloudflared` is auto-downloaded to `~/.cache/mcp-server/` if not on PATH.

## Architecture

Single entrypoint (`cmd/mcp-server/`) that:
1. Auto-discovers Cloudflare Access team domain + application AUD from the API
2. Starts an HTTP server on a random localhost port (serves `/mcp`, `/mcp/sse`, `/health`, `/.well-known/oauth-protected-resource`)
3. Uses the Cloudflare API (`cloudflare-go/v4`) to create/reuse a named tunnel, configure ingress, and ensure a DNS CNAME record
4. Runs `cloudflared tunnel run` as a subprocess (token via `TUNNEL_TOKEN` env var)

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

### Transport

The streamable HTTP transport runs **stateless** (`internal/server/http.go`), which is what lets it serve protocol `2026-07-28` — the SDK accepts that revision only when sessions are off. Consequences worth knowing:

- No `Mcp-Session-Id` is issued and no client is pinned to a process, so recreating the container on release does not invalidate what a connected client holds. Every POST carries its own peer info in `params._meta` and is answered on the spot.
- Older clients are unaffected; they negotiate down to `2025-11-25` and go without a session id.
- `GET` returns `405`, so the standalone SSE stream is gone and `/mcp/sse` serves POST only. Nothing here needs it: the server issues no server→client requests (elicitation, sampling, roots, logging), configures no `EventStore`, and pushes no unsolicited notifications — all of which stateless mode forbids or drops.
- `PropagateRequestCancellation` ties tool handlers to the originating HTTP request, so a client that hangs up stops long-running work (ESPHome compiles and log captures, Frigate snapshots, HA websocket round trips) instead of leaving it running against the home network. **This applies only to clients on `2026-07-28`**, where the POST is the whole request lifecycle; for an older client the option is a no-op and an abandoned request still runs to completion.
- Clients on `2026-07-28` must mirror the JSON-RPC method into an `Mcp-Method` header (SEP-2243); a body/header mismatch is rejected with `-32020`. Request bodies are capped at the SDK's `DefaultMaxRequestBodyBytes`, over which the server returns `413`.
- `tools/list` carries a 5-minute freshness hint (`ttlMs`, SEP-2549) from `cacheHintMiddleware` in `internal/server/cache.go`. The list is fixed for the process lifetime — static tools and generated ones are both registered at startup — so the TTL exists to bound how long a client keeps calling tools a redeploy has removed, not to track in-process change. Scope is `private`, not the SDK's `public` default: the list describes this specific home (script names, areas, device names) and no intermediary should be serving it to anyone else. Paginated pages are left unhinted, since a page is only meaningful next to its cursor.

### Key packages

- `cmd/mcp-server/` — Entrypoint. Parses config via Kong, starts HTTP, sets up tunnel, runs cloudflared.
- `internal/config/` — Kong CLI struct with `envprefix` tags and `Enabled()`/`Validate()` methods.
- `internal/server/` — Server factory. Creates `mcp.Server` and conditionally registers tool sets based on `config.CLI`. `http.go` builds the stateless streamable HTTP handler; `audit.go` is the tool-call audit middleware.
- `internal/tunnel/` — Cloudflare Tunnel lifecycle: create/reuse tunnel via API, configure ingress rules, ensure DNS CNAME, get token, exec cloudflared. Auto-downloads cloudflared if not on PATH.
- `internal/cfaccess/` — Cloudflare Access JWT validation and auto-discovery. `Discover()` finds the team domain and application AUD from the API. `TokenVerifier()` adapts JWT validation to the go-sdk's `auth.RequireBearerToken` interface.
- `internal/middleware/` — HTTP request logging middleware.
- `internal/mcputil/` — Shared MCP result helpers (`TextResult`, `JSONResult`).
- `internal/validate/` — Input validation for path injection prevention.
- `internal/hass/` — Home Assistant REST + WebSocket client and MCP tools. Also holds the generated tool layer: `selector.go` (HA selector → JSON Schema), `intents.go` (the intent catalog + `/api/intent/handle`), and `generated_tools.go` (startup registration of intent and per-script tools).
- `internal/lists/` — To-do list management tools. Depends on the HA client.
- `internal/media/` — Sonarr/Radarr client and MCP tools.
- `internal/frigate/` — Frigate NVR client and MCP tools.
- `internal/esphome/` — ESPHome dashboard client (HTTP + WebSocket command channels) and MCP tools.

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

- `selector.go` — `SelectorToSchema` ports upstream's `selector_serializer` (~25 selector types) to Go, and `ServiceFieldsToSchema` turns a service's `fields` into an object schema. It flattens HA's collapsed `additional_fields` group and, because JSON Schema cannot express "applies only to entities whose `supported_color_modes` includes X", renders a field's `filter` as description prose instead of dropping it.
- `intents.go` — `IntentCatalog` is the ported intent set, each with its slots and the domains it needs. `Client.HandleIntent` posts to `/api/intent/handle`; an unmatched intent returns HTTP 200 with an error response body, which is converted to a Go error so a no-op is not read as success.
- `generated_tools.go` — registration. Intent tools register only when the instance has entities in the intent's domains (a vacuum-less home gets no vacuum tools); if the instance is unreachable the full catalog is registered rather than silently shrinking the tool list. Script tools come from the `script` domain in `GET /api/services`, with entity-registry aliases appended to the description as upstream does. Startup queries are bounded by `generateTimeout`.

Three deliberate departures from upstream:

- **Timer intents are excluded.** Upstream gates them on `llm_context.device_id` being a voice satellite that supports timers; a REST caller has no such device, so those intents could never match.
- **A target is required.** `vol.Any("name", "area", "floor")` in upstream's `DynamicServiceIntentHandler` is a key *matcher*, not a requirement — unmarked voluptuous keys are optional — so HA accepts an untargeted intent and matches every entity in scope. That is a fine voice-assistant default and a poor tool-call one, so the generated schemas require at least one of `name`/`area`/`floor`/`domain`. `domain` counts, so "turn off all the lights" still works.
- **Intent tools are gated on `HASS_DENY_SERVICES`.** `/api/intent/handle` takes an intent name rather than a service, so `CallService`'s policy check never runs. Each `IntentDef` therefore declares the services it can dispatch (`HassTurnOff` reaches `lock.unlock`, `cover.close_cover`, `valve.close_valve`, `button.press`), and an intent whose services include a denied one is not registered at all. Conservative by design: resolution happens inside HA's handler, so there is no later point at which to check.
- **Generated tools validate their own input.** The go-sdk only validates arguments for tools added via the generic `mcp.AddTool`, which derives and resolves a schema from a Go type. Runtime-schema tools go through `Server.AddTool`, which advertises the schema but does not enforce it, so `decodeArgs` resolves and validates against it explicitly. Without this, `home_turn_on` with no `name`/`area`/`floor` would reach Home Assistant instead of being rejected.

### Service Clients

Each integration has its own client that handles HTTP/WebSocket communication:

- **Home Assistant** (`HASS_URL`, `HASS_TOKEN`) — REST API with Bearer token auth + WebSocket for automation/script/scene config, traces, registries (areas/devices/entities/labels/floors), and long-term statistics. Two-step WS auth handshake (`auth_required` → `auth` → `auth_ok`).
- **Sonarr** (`SONARR_URL`, `SONARR_API_KEY`) — `/api/v3/` endpoints, `X-Api-Key` header auth.
- **Radarr** (`RADARR_URL`, `RADARR_API_KEY`) — Same *arr API pattern as Sonarr.
- **Frigate** (`FRIGATE_URL`) — No auth, REST API for config, events, and JPEG snapshots.
- **ESPHome** (`ESPHOME_URL`, optional `ESPHOME_PASSWORD`) — ESPHome Device Builder dashboard. All operations go over the single multiplexed `/ws` command socket (`{command, message_id, args}` → `ResultMessage` / streaming `EventMessage{event:"output"|"result"}` / `ErrorMessage`): `devices/list`, `config/get_secrets`, `devices/get_config`, `devices/update_config` (falls back to `devices/create` on `not_found`), `devices/validate` and `devices/logs` (streaming), `firmware/get_binaries` + `firmware/download_token` (binary then fetched over HTTP `/api/firmware/download?token=`). Compile and upload can run for minutes — longer than an MCP request stays open — so they use the dashboard's **async job queue**: `firmware/compile` / `firmware/upload` return a `FirmwareJob` immediately, and the caller polls `firmware/get_job` until the job is terminal (`completed`/`failed`/`cancelled`). `get_job` deliberately returns terminal jobs with an empty `output` (the log is flushed to a per-job sidecar), so `get_esphome_job` fetches the build/flash log separately via `firmware/follow_job` once the job is done — that command replays the sidecar as `output` events then a terminal `result`, reusing the same streaming reader as validate/logs. This is the same poll pattern the MCP Tasks extension would model at the protocol layer, done at the application layer because the go-sdk in use doesn't implement Tasks yet. Auth: a dashboard reporting `requires_auth` gets an `auth/login` handshake on `/ws`; an add-on on its exposed port reports `requires_auth: false`.

All tool sets are optional — the server registers only what's configured and starts even with zero tools.

### Cloudflare Tunnel Management

The `internal/tunnel/` package manages the full tunnel lifecycle via the Cloudflare API (`cloudflare-go/v4`):
- Finds an existing tunnel by name or creates a new one (with `config_src: cloudflare`)
- Updates ingress configuration to route the hostname to the local HTTP server
- Creates or updates a proxied CNAME DNS record pointing to `<tunnel-id>.cfargotunnel.com`
- Retrieves the tunnel token and runs `cloudflared tunnel run` (token via env var)

Authentication: the server auto-discovers the CF Access team domain and application AUD at startup, validates Bearer tokens (RS256 JWTs signed by CF Access), and serves `/.well-known/oauth-protected-resource` for OAuth discovery. A bridge middleware copies `Cf-Access-Jwt-Assertion` to `Authorization: Bearer` since CF Access injects the JWT in its own header. Pass `--insecure` to disable auth for local development.

## MCP Tools Provided

**Home Assistant** (requires `HASS_URL`, `HASS_TOKEN`):

| Tool | Description |
|------|-------------|
| `get_home_states` | Query entity states, optionally filtered by domain |
| `get_home_events` | Logbook entries for recent state changes |
| `call_home_service` | Call HA services (turn_on, turn_off, set_temperature, etc.) |
| `get_todo_items` | Retrieve items from a todo list entity |
| `manage_automations` | CRUD operations on automations |
| `get_automation_traces` | Debug automation execution history via WebSocket |
| `manage_helpers` | CRUD operations on helpers (input_boolean, input_number, input_text, input_select, input_datetime, input_button, counter, timer, schedule) |
| `manage_scripts` | CRUD operations on scripts |
| `manage_scenes` | CRUD plus `activate` for scenes |
| `get_home_registry` | Topology data (areas, devices, entities, labels, floors); `kind=all` returns the full registry in one call |
| `manage_registry` | Create/update/delete area/entity/device/label/floor registry entries (assign areas, rename, label) |
| `execute_script` | Run an ad-hoc action sequence (HA script syntax) without storing a script |
| `get_diagnostics` | Health/error diagnostics: error log, system health, repair issues, persistent notifications (`kind=all` by default) |
| `list_home_services` | Discover available services with their fields and target selectors before calling them |
| `get_state_history` | Time-series state history for entities (numeric trends, on/off timelines) |
| `render_template` | Evaluate a Jinja2 template against current state for compound queries |
| `get_long_term_statistics` | Long-term statistics (energy/gas/water/measurement sensors) aggregated by 5minute/hour/day/week/month |
| `get_calendar_events` | Upcoming events from HA calendar entities (lists calendars when entity_id is omitted) |
| `manage_dashboards` | List/read/save/delete Lovelace dashboard configs and create/update/delete storage dashboards (save_config overwrites the whole config) |
| `manage_dashboard_resources` | CRUD for Lovelace dashboard resources (custom JS/CSS modules) |
| `manage_entity_state` | Write or delete an entity's state directly in the state machine (virtual entities; does not control devices) |
| `fire_home_event` | Fire an event on the event bus (triggers automations listening for it) |
| `get_home_config` | Core config (version, unit system, time zone, location), loaded components, and `configuration.yaml` validation (`kind=check`) |

**Generated tools** (requires HA; registered at startup from the live instance):

These are not hand-written. At startup the server queries the instance and generates two families of tools, mirroring what Home Assistant's own MCP server does. See "Generated tool layer" below.

| Tool | Description |
|------|-------------|
| `home_turn_on` / `home_turn_off` | Turn on/open/start or turn off/close/stop anything, by entity name, area, or floor |
| `home_set_position` / `home_stop_moving` | Position or stop a cover or valve |
| `home_light_set` | Set a light's brightness percentage, color name, or color temperature |
| `home_climate_set_temperature` | Set a thermostat's target temperature |
| `home_fan_set_speed` | Set a fan's speed percentage |
| `home_media_pause` / `_unpause` / `_next` / `_previous` | Media player transport control |
| `home_set_volume` / `home_set_volume_relative` | Set absolute or relative media player volume |
| `home_media_player_mute` / `_unmute` | Mute or unmute a media player |
| `home_media_search_and_play` | Search for media and play the first result |
| `home_vacuum_start` / `_return_to_base` / `_clean_area` | Vacuum control |
| `home_humidifier_setpoint` / `home_humidifier_mode` | Humidifier target humidity and mode |
| `home_list_add_item` / `_complete_item` / `_remove_item` | To-do and shopping list items |
| `home_script_<object_id>` | One tool per script, with parameters from the script's declared `fields` |

**Lists** (requires HA):

| Tool | Description |
|------|-------------|
| `get_lists` | Retrieve all available to-do lists |
| `get_list_items` | Get items from a specific to-do list |
| `modify_list_item` | Add, remove, complete, or uncomplete items |

**Media** (requires `SONARR_URL`/`SONARR_API_KEY` and/or `RADARR_URL`/`RADARR_API_KEY`):

| Tool | Description |
|------|-------------|
| `search_movies` | Search movies by name via Radarr |
| `add_movie` | Add a movie to Radarr for downloading |
| `search_series` | Search TV series by name via Sonarr |
| `add_series` | Add a TV series to Sonarr for downloading |
| `get_download_queue` | Check download progress for Sonarr/Radarr |

**Frigate NVR** (requires `FRIGATE_URL`):

| Tool | Description |
|------|-------------|
| `list_frigate_cameras` | List all enabled cameras |
| `get_camera_snapshot` | Get current camera frame as JPEG |
| `get_frigate_events` | Recent detection events (person, car, etc.) |
| `get_event_snapshot` | Get snapshot for a specific detection event |

**ESPHome** (requires `ESPHOME_URL`):

| Tool | Description |
|------|-------------|
| `list_esphome_devices` | List dashboard devices with config file, address, online status, versions |
| `list_esphome_secrets` | List shared secrets.yaml key names (never values) |
| `read_esphome_file` | Read a config-dir file (device YAML, include, secrets.yaml) |
| `write_esphome_file` | Create/overwrite a config-dir file (push YAML + includes) |
| `validate_esphome` | Validate a config without building (streaming) |
| `compile_esphome` | Queue a firmware build; returns a `job_id` immediately (async) |
| `upload_esphome` | Queue an OTA flash of the latest build; returns a `job_id` (async, destructive) |
| `get_esphome_job` | Poll a compile/upload job's status, progress, and output |
| `download_esphome_binary` | Confirm a built image exists; report size + SHA-256 |
| `get_esphome_logs` | Capture live device logs for a bounded duration |
