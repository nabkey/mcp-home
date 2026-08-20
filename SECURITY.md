# Security Model

This document describes the security properties of the MCP server.

## Architecture

```
Claude.ai / Claude Code CLI
  → HTTPS → Cloudflare Edge (CF_HOSTNAME)
    → Cloudflare Access (self_hosted app, identity policy, OAuth enabled)
      → cloudflared tunnel (subprocess on host)
        → http://127.0.0.1:<random>/mcp
          → [Cf-Access-Jwt-Assertion → Bearer bridge]
            → [auth.RequireBearerToken] → StreamableHTTPHandler
```

## Authentication Flow

Clients connect directly to `https://<CF_HOSTNAME>/mcp` — no MCP Portal intermediary. Authentication uses Cloudflare Access as both the edge gateway and the OAuth 2.1 authorization server.

### First connection (OAuth discovery)

1. Client sends POST to `/mcp` without a token
2. Server returns `401 Unauthorized` with `WWW-Authenticate: Bearer resource_metadata="https://<CF_HOSTNAME>/.well-known/oauth-protected-resource"`
3. Client fetches `/.well-known/oauth-protected-resource` (or `/.well-known/oauth-protected-resource/mcp`)
4. Metadata points to `https://<team>.cloudflareaccess.com` as the authorization server
5. Client fetches `https://<team>.cloudflareaccess.com/.well-known/oauth-authorization-server` to discover OAuth endpoints
6. Client does OAuth 2.1 authorization code + PKCE with Cloudflare Access (browser redirect for user consent)
7. CF Access enforces identity policy (email allowlist)

### Subsequent requests

After the OAuth dance, Cloudflare Access injects the signed JWT as `Cf-Access-Jwt-Assertion` on every request through the tunnel. A bridge middleware copies this into `Authorization: Bearer` so the go-sdk's `auth.RequireBearerToken` can validate it.

The go-sdk middleware:
- Extracts the Bearer token
- Calls `cfaccess.Validator.Validate()` which checks RS256 signature, audience, issuer, and expiry
- Sets `TokenInfo.UserID` to the authenticated email for session binding
- Returns 401 if validation fails

## Defense Layers

### 1. Cloudflare Access (edge)

A `self_hosted` Access application on `CF_HOSTNAME` with:
- **Identity policy**: Email allowlist (configured in CF Access)
- **OAuth enabled**: Dynamic client registration for MCP client compatibility
- All unauthenticated requests are blocked at Cloudflare's edge before reaching cloudflared

This is the primary security boundary. Without a valid CF Access session, requests never reach the server.

### 2. Bearer Token Validation (server-side)

Defense-in-depth: the server validates every request's JWT independently, even though CF Access already authenticated at the edge. This protects against:
- Misconfigured Access policies
- Local processes connecting directly to the localhost port
- Any bypass of the Cloudflare edge

Validation checks:
- RS256 signature against CF Access public keys (fetched from `<team>.cloudflareaccess.com/cdn-cgi/access/certs`, cached 15 min)
- Audience claim matches the Access application's AUD tag
- Issuer matches the team URL
- Token is not expired

### 3. Localhost Binding

The HTTP server binds to `127.0.0.1:0` (random port). Not reachable from the network — only through the cloudflared subprocess or other local processes. Combined with Bearer token validation, local processes cannot execute tools without a valid CF Access-signed JWT.

### 4. Session Hijacking Prevention

The go-sdk's `StreamableHTTPHandler` binds `TokenInfo.UserID` (the authenticated email) to the MCP session. Subsequent requests must come from the same user, preventing session hijacking.

### 5. Input Validation

All user-supplied values in URL paths (HA service domain/name, automation IDs, Frigate camera/event IDs) are validated against `^[a-zA-Z0-9][a-zA-Z0-9._-]*$` to prevent path traversal.

### 6. Tunnel Token

The cloudflared tunnel token is passed via `TUNNEL_TOKEN` environment variable, not CLI arguments, preventing exposure via `ps aux`.

### 7. Service Deny List

`HASS_DENY_SERVICES` refuses calls to listed Home Assistant services (e.g. `lock.unlock,alarm_control_panel.*`). It is enforced for direct service calls (`call_home_service`, scene activation, to-do changes) and ad-hoc `execute_script` sequences (including nested choose/repeat blocks, both `action:` and legacy `service:` keys).

**Limitation:** this is a guardrail against unwanted assistant actions (including prompt injection driving the model), not a security boundary. Stored automations and scripts created via `manage_automations`/`manage_scripts` execute inside Home Assistant and are not inspected, and templated action names cannot be statically checked. An attacker with a valid CF Access session has the full tool surface minus the denied services.

### 8. Audit Logging

Every tool call is logged with the authenticated user (CF Access email from the JWT), the calling client and negotiated protocol version, tool name, truncated arguments, outcome, and duration — a per-user audit trail of everything the assistant did in the home. Client identity comes from the per-request `_meta` where the protocol supplies it, so it survives the stateless 2026-07-28 protocol, which has no initialize handshake to remember it from.

### 9. Flash Confirmation

`upload_esphome` overwrites a device's running firmware. When the connected client supports form elicitation, the tool asks the user to confirm the specific device before anything is queued, and reports a cancellation if they decline. Clients that cannot be asked flash as before — the tool is annotated destructive, and hosts prompt for tool approval in their own way — so this is a second guardrail, not the only one.

## Open Questions

### Rate limiting
No server-side rate limiting. Cloudflare's edge provides some protection, but a compromised authenticated session could flood the server.

### API token scope
The `CF_API_TOKEN` has tunnel management + Access read permissions. Consider using separate tokens with narrower scopes.

### Concurrent sessions
The server does not limit the number of concurrent MCP sessions. A misbehaving client could create unlimited sessions.
