# mcp-home

A Go [MCP](https://modelcontextprotocol.io) server for smart home and media management. Connects Claude to Home Assistant, Sonarr, Radarr, Frigate NVR, and ESPHome over a Cloudflare Tunnel with Cloudflare Access authentication.

## How it works

```
Claude.ai / Claude Code
  → HTTPS → Cloudflare Edge
    → Cloudflare Access (OAuth 2.1)
      → cloudflared tunnel
        → MCP server (localhost)
```

The server starts on a random localhost port, creates (or reuses) a Cloudflare Tunnel via the API, and runs `cloudflared` as a subprocess. Cloudflare Access handles authentication — the server auto-discovers the team domain and application AUD at startup, validates JWTs on every request, and serves OAuth protected resource metadata for client discovery.

All tool groups are optional. The server registers only what's configured and starts even with zero tools.

## Tools

**Home Assistant** — query entity states, history, and long-term statistics; view logbook events; discover and call services (lights, climate, etc.); render Jinja2 templates; manage automations, scripts, scenes, and helpers; debug automation traces; read and organize the registry (areas, devices, entities, labels, floors); manage Lovelace dashboards and resources; run ad-hoc action sequences; read calendars; surface diagnostics (error log, system health, repairs, notifications)

**Lists** — manage Home Assistant to-do lists (view, add, complete, remove items)

**Media** — search and add movies (Radarr) and TV series (Sonarr), check download queue status

**Frigate NVR** — list cameras, get live snapshots, query detection events, get event snapshots

**ESPHome** — list dashboard devices and secret key names; read/write device YAML; validate; compile and OTA-upload firmware via the dashboard's async job queue (poll for completion); capture live device logs

The full tool catalog with per-tool descriptions is in [CLAUDE.md](CLAUDE.md#mcp-tools-provided).

## Quick start

```bash
cp .env.example .env
# Fill in your Cloudflare credentials and any optional integrations

go run ./cmd/mcp-server
```

Run `go run ./cmd/mcp-server --help` for all flags and their corresponding environment variables.

### Prerequisites

- Go 1.26+
- A Cloudflare account with a domain
- A [Cloudflare API token](https://dash.cloudflare.com/profile/api-tokens) with Tunnel:Edit, DNS:Edit, and Access:Read permissions
- A self-hosted Cloudflare Access application on your chosen hostname with OAuth enabled

`cloudflared` is auto-downloaded if not on PATH.

### Configuration

All configuration is via environment variables (see `.env.example`) or CLI flags. Integration groups are all-or-nothing — partially setting a group produces a clear error at startup.

| Group | Variables | Required |
|-------|-----------|----------|
| Cloudflare | `CF_API_TOKEN`, `CF_ACCOUNT_ID`, `CF_ZONE_ID`, `CF_HOSTNAME` | Yes |
| Home Assistant | `HASS_URL`, `HASS_TOKEN` | No |
| Sonarr | `SONARR_URL`, `SONARR_API_KEY` | No |
| Radarr | `RADARR_URL`, `RADARR_API_KEY` | No |
| Frigate | `FRIGATE_URL` | No |
| ESPHome | `ESPHOME_URL` (optional `ESPHOME_PASSWORD`) | No |

Pass `--insecure` to disable authentication for local development. `LOG_LEVEL` (`debug`, `info`, `warn`, `error`) controls log verbosity.

### Guardrails & audit

`HASS_DENY_SERVICES` refuses calls to specific Home Assistant services no matter what the assistant is asked, e.g.:

```bash
HASS_DENY_SERVICES=lock.unlock,alarm_control_panel.*
```

Patterns are `domain.service` pairs; either part may be `*`. The deny list covers direct service calls and ad-hoc `execute_script` sequences (see [SECURITY.md](SECURITY.md) for limitations). Every tool call is also audit-logged with the authenticated user's email, the calling client, tool name, arguments, and outcome. `upload_esphome` asks the user to confirm the device before flashing when the client supports elicitation.

### Docker

Prebuilt multi-arch images are published to GitHub Container Registry:

```bash
docker run --rm --env-file .env ghcr.io/nabkey/mcp-home:latest
```

Tags:
- `latest` — tip of `main`
- `vX.Y.Z`, `vX.Y` — semver releases
- `main`, `sha-<short>` — branch / commit pins

The image bundles `cloudflared`, so no runtime download is needed. Build locally with `docker build -t mcp-home .`.

## Development

The Go toolchain and golangci-lint are managed via [mise](https://mise.jdx.dev) — run `mise install` once, then:

```bash
mise run test     # go test -race ./...
mise run lint     # golangci-lint run ./...
mise run check    # everything CI runs: format check, build, vet, test, lint
```

CI runs the same checks on every pull request, and the container publish workflow is gated on them.

## Releases

Versioning follows [SemVer](https://semver.org). Releases are automated by [release-please](https://github.com/googleapis/release-please): push [conventional commits](https://www.conventionalcommits.org) (`feat:`, `fix:`, `feat!:`, etc.) to `main` and a release PR will be opened/updated automatically. Merging it creates the `vX.Y.Z` tag, which triggers the container publish workflow. The resolved version is baked into the binary — check with `mcp-server --version`.

## Security

See [SECURITY.md](SECURITY.md) for the full security model, including the authentication flow, defense layers, and threat considerations.

## License

[MIT](LICENSE)
