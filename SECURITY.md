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

## Open Questions

### Rate limiting
No server-side rate limiting. Cloudflare's edge provides some protection, but a compromised authenticated session could flood the server.

### API token scope
The `CF_API_TOKEN` has tunnel management + Access read permissions. Consider using separate tokens with narrower scopes.

### Concurrent sessions
The server does not limit the number of concurrent MCP sessions. A misbehaving client could create unlimited sessions.
