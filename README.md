# mcp-home

A Go [MCP](https://modelcontextprotocol.io) server for smart home and media management. Connects Claude to Home Assistant, Sonarr, Radarr, and Frigate NVR over a Cloudflare Tunnel with Cloudflare Access authentication.

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

**Home Assistant** — query entity states, view logbook events, call services (lights, climate, etc.), manage automations, debug automation traces

**Lists** — manage Home Assistant to-do lists (view, add, complete, remove items)

**Media** — search and add movies (Radarr) and TV series (Sonarr), check download queue status

**Frigate NVR** — list cameras, get live snapshots, query detection events, get event snapshots

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

Pass `--insecure` to disable authentication for local development.

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

## Releases

Versioning follows [SemVer](https://semver.org). Releases are automated by [release-please](https://github.com/googleapis/release-please): push [conventional commits](https://www.conventionalcommits.org) (`feat:`, `fix:`, `feat!:`, etc.) to `main` and a release PR will be opened/updated automatically. Merging it creates the `vX.Y.Z` tag, which triggers the container publish workflow. The resolved version is baked into the binary — check with `mcp-server --version`.

## Security

See [SECURITY.md](SECURITY.md) for the full security model, including the authentication flow, defense layers, and threat considerations.

## License

[MIT](LICENSE)
