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
```

Go 1.26.2 is managed via mise (see `mise.toml`).

### Releases & container

Versioning is [SemVer](https://semver.org), automated via [release-please](https://github.com/googleapis/release-please) — push [conventional commits](https://www.conventionalcommits.org) (`feat:`, `fix:`, `feat!:`) to `main` and a release PR appears. Merging it tags `vX.Y.Z`, which triggers `.github/workflows/publish.yml` to build and push a multi-arch image to `ghcr.io/nabkey/mcp-home`. The version is injected into the binary via `-ldflags "-X main.version=..."` and surfaced by the `--version` (`-V`) flag. `cloudflared` is bundled in the image.

### Configuration

All config is via environment variables (or CLI flags). Uses [Kong](https://github.com/alecthomas/kong) with `envprefix` tags — run `go run ./cmd/mcp-server --help` to see all flags with their env var names.

**Cloudflare (required):** `CF_API_TOKEN`, `CF_ACCOUNT_ID`, `CF_ZONE_ID`, `CF_HOSTNAME`, `CF_TUNNEL_NAME` (default: `mcp-server`)

**Tool integrations (optional groups):** Each group is all-or-nothing — partially setting a group (e.g., `HASS_URL` without `HASS_TOKEN`) produces a clear error at startup.

- `HASS_URL`, `HASS_TOKEN` — Home Assistant
- `SONARR_URL`, `SONARR_API_KEY` — Sonarr (TV)
- `RADARR_URL`, `RADARR_API_KEY` — Radarr (Movies)
- `FRIGATE_URL` — Frigate NVR

Config struct definitions are in `internal/config/config.go`. Each optional group has `Enabled() bool` and `Validate() error` methods. Validation runs via Kong's `AfterApply` hook.

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

### Key packages

- `cmd/mcp-server/` — Entrypoint. Parses config via Kong, starts HTTP, sets up tunnel, runs cloudflared.
- `internal/config/` — Kong CLI struct with `envprefix` tags, `Enabled()`/`Validate()` methods, and `AfterApply` hook.
- `internal/server/` — Server factory. Creates `mcp.Server` and conditionally registers tool sets based on `config.CLI`.
- `internal/tunnel/` — Cloudflare Tunnel lifecycle: create/reuse tunnel via API, configure ingress rules, ensure DNS CNAME, get token, exec cloudflared. Auto-downloads cloudflared if not on PATH.
- `internal/cfaccess/` — Cloudflare Access JWT validation and auto-discovery. `Discover()` finds the team domain and application AUD from the API. `TokenVerifier()` adapts JWT validation to the go-sdk's `auth.RequireBearerToken` interface.
- `internal/middleware/` — HTTP request logging middleware.
- `internal/mcputil/` — Shared MCP result helpers (`TextResult`, `JSONResult`).
- `internal/validate/` — Input validation for path injection prevention.
- `internal/hass/` — Home Assistant REST + WebSocket client and MCP tools.
- `internal/lists/` — To-do list management tools. Depends on the HA client.
- `internal/media/` — Sonarr/Radarr client and MCP tools.
- `internal/frigate/` — Frigate NVR client and MCP tools.

### MCP Tool Registration Pattern

Tools use the go-sdk generic `mcp.AddTool[In, Out]` pattern:
- Define an args struct with `json` and `jsonschema:"..."` tags (tag value is the description directly, no `description=` prefix)
- Handler signature: `func(ctx context.Context, req *mcp.CallToolRequest, args T) (*mcp.CallToolResult, any, error)`
- Return content via `*mcp.CallToolResult` with `TextContent`; the second return value (`any`) is unused

### Service Clients

Each integration has its own client that handles HTTP/WebSocket communication:

- **Home Assistant** (`HASS_URL`, `HASS_TOKEN`) — REST API with Bearer token auth + WebSocket for automation config/traces. Two-step WS auth handshake (`auth_required` → `auth` → `auth_ok`).
- **Sonarr** (`SONARR_URL`, `SONARR_API_KEY`) — `/api/v3/` endpoints, `X-Api-Key` header auth.
- **Radarr** (`RADARR_URL`, `RADARR_API_KEY`) — Same *arr API pattern as Sonarr.
- **Frigate** (`FRIGATE_URL`) — No auth, REST API for config, events, and JPEG snapshots.

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
