# Security Model

This document describes the security properties of the MCP server.

The server has two optional front doors and needs at least one. Sections 1–3 describe the Cloudflare path; section 9 describes the tailnet path. Everything from section 4 on applies to both.

## Architecture

```
Claude.ai / Claude Code CLI
  → HTTPS → Cloudflare Edge (CF_HOSTNAME)
    → Cloudflare Access (self_hosted app, identity policy, OAuth enabled)
      → cloudflared tunnel (subprocess on host)
        → http://127.0.0.1:<random>/mcp
          → [Cf-Access-Jwt-Assertion → Bearer bridge]
            → [auth.RequireBearerToken] → StreamableHTTPHandler (stateless)
```

```
Tailnet peer
  → WireGuard → tsnet node (TS_HOSTNAME.<tailnet>.ts.net:443)
    → [tsauth.Middleware: WhoIs → allowlist]
      → [auth.RequireBearerToken, placeholder token] → StreamableHTTPHandler (stateless)
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

### 4. No Sessions to Hijack

The streamable transport runs stateless (`internal/server/http.go`), so there is no session to steal: no `Mcp-Session-Id` is issued, and nothing on the server outlives a single request.

Each POST is authorized on its own. Every request carries the CF Access JWT, which `auth.RequireBearerToken` validates in full before the handler runs, so authorization cannot be inherited from an earlier request. This replaces the previous defense, in which the go-sdk bound `TokenInfo.UserID` to a long-lived MCP session and rejected later requests from a different user — that binding is unreachable in stateless mode, and the property it protected now holds because there is no shared state to carry a stale identity.

### 5. Input Validation

All user-supplied values in URL paths (HA service domain/name, automation IDs, Frigate camera/event IDs) are validated against `^[a-zA-Z0-9][a-zA-Z0-9._-]*$` to prevent path traversal.

### 6. Tunnel Token

The cloudflared tunnel token is passed via `TUNNEL_TOKEN` environment variable, not CLI arguments, preventing exposure via `ps aux`.

### 7. Service Deny List

`HASS_DENY_SERVICES` refuses calls to listed Home Assistant services (e.g. `lock.unlock,alarm_control_panel.*`). It is enforced for direct service calls (`call_home_service`, scene activation, to-do changes) and ad-hoc `execute_script` sequences (including nested choose/repeat blocks, both `action:` and legacy `service:` keys).

**Limitation:** this is a guardrail against unwanted assistant actions (including prompt injection driving the model), not a security boundary. Stored automations and scripts created via `manage_automations`/`manage_scripts` execute inside Home Assistant and are not inspected, and templated action names cannot be statically checked. An attacker with a valid CF Access session has the full tool surface minus the denied services.

### 8. Audit Logging

Every tool call is logged with the authenticated user, tool name, truncated arguments, outcome, and duration — a per-user audit trail of everything the assistant did in the home. On the Cloudflare path the user is the CF Access email from the JWT. On the tailnet path it is the WhoIs identity: `login@node` for a user-owned device, `node(tag:...)` for a tagged one. Both arrive via `auth.TokenInfo.UserID`, so the audit middleware does not care which door the call came through.

### 9. Tailnet Listener

When `TS_AUTHKEY` is set the server runs an embedded Tailscale node (tsnet) and serves `/mcp` on the tailnet over TLS with a Tailscale-issued certificate. Nothing is published on the host's network interfaces; tsnet dials out to the coordination server and peers the same way cloudflared dials out to Cloudflare.

Trust boundary:
- **Reachability** is governed by the tailnet ACL. A peer the ACL does not admit to this node on 443 never completes a WireGuard handshake, so its requests never reach the process.
- **Identity** comes from `WhoIs` on the request's remote address, which maps the WireGuard peer to a node and (for user-owned devices) a login. It cannot be spoofed by anything in the HTTP request; there is no token, cookie or header to forge.
- **Authorization** is the `TS_ALLOWED_TAGS` / `TS_ALLOWED_LOGINS` allowlist, applied on every request. Config validation refuses an empty allowlist. A tagged node is matched only by tag, never by login (tagged nodes report the synthetic `tagged-devices` login).
- Any `Authorization` header the client sends is discarded before the request reaches the MCP handler, so a tailnet peer cannot present a Cloudflare JWT or anything else to escalate.

Limits:
- `--insecure` does not apply to this listener and cannot disable the WhoIs gate.
- The node key lives in `TS_STATE_DIR`. Anyone who can read that directory can impersonate the node on the tailnet; it is persisted so restarts do not re-register, and should be on a volume with the same protection as the container's other state.
- The auth key should be tagged (so the node carries `tag:mcp-home` rather than a user's identity), reusable only if you need it to be, and revocable from the admin console.

## Open Questions

### Rate limiting
No server-side rate limiting. Cloudflare's edge provides some protection, but a compromised authenticated session could flood the server.

### API token scope
The `CF_API_TOKEN` has tunnel management + Access read permissions. Consider using separate tokens with narrower scopes.

### Concurrent requests
The server holds no sessions to limit, but it also does not cap concurrent in-flight requests. A misbehaving client could still issue many at once; each one occupies a goroutine and can reach the home network. `PropagateRequestCancellation` bounds the damage from clients that hang up mid-call, but only for those on protocol `2026-07-28` — an older client's abandoned request runs to completion.
