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
- `alg` is exactly `RS256` (no `none`, no HMAC, no PSS) and the header has no `crit` extensions
- RS256 signature against CF Access public keys (fetched from `<team>.cloudflareaccess.com/cdn-cgi/access/certs`, cached 15 min; non-RSA certs ignored)
- Audience claim (string or array) contains the Access application's AUD tag; a validator with no AUD refuses everything
- Issuer matches the team URL exactly
- Token is not expired (`exp` required, strict) and not used before `nbf` (1 minute leeway)
- The token names someone: `email`, or `common_name` for an Access service token (audited as `service:<client id>`)

The 401 body is a fixed "token rejected"; which check failed (and the expected AUD and issuer) goes to the server log only.

Key availability: an unknown `kid` triggers at most one certs fetch per minute. If a refresh fails, the keys from the last successful fetch keep validating for up to 24 hours (Cloudflare publishes the previous key well past rotation), failed fetches back off for 30 seconds, and a key the validator never had is never accepted. With no successful fetch at all, nothing validates. A client hanging up mid-fetch cannot fail the shared fetch for others.

### 3. Localhost Binding

The HTTP server binds to `127.0.0.1:0` (random port). Not reachable from the network — only through the cloudflared subprocess or other local processes. Combined with Bearer token validation, local processes cannot execute tools without a valid CF Access-signed JWT. Nothing about the connection is trusted: not the loopback source address, not `Host`, not `X-Forwarded-*`/`Cf-Connecting-Ip`, and not `Cf-Access-Authenticated-User-Email` (all pinned by `TestCloudflareDoorRejectsWithoutValidToken`). The SDK's DNS-rebinding check is disabled because cloudflared forwards the public `Host`; with `--insecure` that leaves a local browser able to reach the port by rebinding, which is one more reason `--insecure` is for development only.

Cross-origin browser requests (a hostile page in a logged-in browser) cannot call tools: the MCP endpoint requires `Content-Type: application/json`, which a cross-origin page can only send after a CORS preflight, and no CORS grant is ever returned (`TestBrowserCrossOriginCannotCall`). `http.CrossOriginProtection` is not enabled, because a desktop client that sends `Origin` would be refused.

### 4. No Sessions to Hijack

The streamable transport runs stateless (`internal/server/http.go`), so there is no session to steal: no `Mcp-Session-Id` is issued, and nothing on the server outlives a single request.

Each POST is authorized on its own. Every request carries the CF Access JWT, which `auth.RequireBearerToken` validates in full before the handler runs, so authorization cannot be inherited from an earlier request. This replaces the previous defense, in which the go-sdk bound `TokenInfo.UserID` to a long-lived MCP session and rejected later requests from a different user — that binding is unreachable in stateless mode, and the property it protected now holds because there is no shared state to carry a stale identity.

### 5. Input Validation

All user-supplied values in URL paths (HA service domain/name, automation IDs, Frigate camera/event IDs) are validated against `^[a-zA-Z0-9][a-zA-Z0-9._-]*$`, at most 255 bytes, to prevent path traversal. A fuzz target (`FuzzIdentifier`) checks that anything accepted is unchanged by path escaping, is not a dot segment and cannot move a path it is joined into.

Request bodies: the gate runs before the body is read, so an unauthenticated oversized POST costs a 401; authenticated, the SDK caps bodies at 4 MiB (413). On protocol `2026-07-28` the `Mcp-Method`/`Mcp-Name` headers must match the body (`-32020`). That check applies only when the client sends `MCP-Protocol-Version: 2026-07-28` or later, so nothing in front of this server should make a security decision from those headers alone.

### 5a. Confirmation Binding

Destructive tools ask the user before acting (`mcputil.Confirm`). The question carries a `requestState` that is an HMAC over the tool name, the canonicalized arguments and a 15-minute expiry. A "yes" counts only alongside a valid state for that exact call: a client that pre-answers without asking, or replays a "yes" onto a call with different arguments, is asked again (`TestPreAnsweredConfirmationOverHTTP`, `TestConfirmRejectsUnboundAccept`). The key is derived from the configured secrets so answers survive a redeploy, and is random when none are set. This is a guardrail against an assistant acting without the user, not an authentication boundary: an authorized client can always show the prompt to nobody and answer it itself.

### 5b. HTTP Caching

Every MCP response carries `Cache-Control: private, no-store, no-transform` (`middleware.NoStore`). The SEP-2549 `cacheScope: "private"` on `tools/list` is advice to MCP-aware clients only, and the SDK's own `no-cache` would let a shared HTTP cache store a response.

### 6. Tunnel Token

The cloudflared tunnel token is passed via `TUNNEL_TOKEN` environment variable, not CLI arguments, preventing exposure via `ps aux`.

### 7. Service Deny List

`HASS_DENY_SERVICES` refuses calls to listed Home Assistant services (e.g. `lock.unlock,alarm_control_panel.*`). It is enforced for direct service calls (`call_home_service`, scene activation, to-do changes) and ad-hoc `execute_script` sequences (including nested choose/repeat blocks, both `action:` and legacy `service:` keys).

**Limitation:** this is a guardrail against unwanted assistant actions (including prompt injection driving the model), not a security boundary. Stored automations and scripts created via `manage_automations`/`manage_scripts` execute inside Home Assistant and are not inspected, and templated action names cannot be statically checked. An attacker with a valid CF Access session has the full tool surface minus the denied services.

### 8. Audit Logging

Every tool call is logged with the authenticated user, tool name, truncated arguments, outcome, and duration — a per-user audit trail of everything the assistant did in the home. On the Cloudflare path the user is the CF Access email from the JWT. On the tailnet path it is the WhoIs identity: `login@node` for a user-owned device, `node(tag:...)` for a tagged one. Both arrive via `auth.TokenInfo.UserID`; each door also records itself in `TokenInfo.Extra["front_door"]`, which the audit line carries as `front_door` (`cloudflare`, `tailnet`, or `cloudflare-insecure` for `--insecure`). An Access service token is logged as `service:<client id>`.

Each line also records the arguments, the outcome (`ok`, `tool_error` with the tool's error text, `error`, `awaiting_input`), the confirmation answer if any, the negotiated protocol and the duration — enough to answer "who unlocked the door at 3am, through which door, with what, and did it work" from the log alone, in either `LOG_FORMAT`. Credentials are redacted from arguments and error text by key name and by pattern (JWTs, bearer/basic credentials, `token=` query parameters, YAML `password:`/`key:` lines; `!secret` references are kept). Redaction is pattern-based and best effort. Attacker-controlled strings cannot forge a second log line in either format.

### 9. Tailnet Listener

When `TS_AUTHKEY` is set the server runs an embedded Tailscale node (tsnet) and serves `/mcp` on the tailnet over TLS with a Tailscale-issued certificate. Nothing is published on the host's network interfaces; tsnet dials out to the coordination server and peers the same way cloudflared dials out to Cloudflare.

Trust boundary:
- **Reachability** is governed by the tailnet ACL. A peer the ACL does not admit to this node on 443 never completes a WireGuard handshake, so its requests never reach the process.
- **Identity** comes from `WhoIs` on the request's remote address, which maps the WireGuard peer to a node and (for user-owned devices) a login. It cannot be spoofed by anything in the HTTP request; there is no token, cookie or header to forge.
- **Authorization** is the `TS_ALLOWED_TAGS` / `TS_ALLOWED_LOGINS` allowlist, applied on every request. Entries are trimmed and matched exactly (a tag that merely starts with an allowed one does not match). Config validation refuses an allowlist with no usable entry (`","` or `" "` used to pass as one entry), and the middleware itself refuses every caller if it is ever built with none — it used to treat an empty allowlist as "whole tailnet". A tagged node is matched only by tag, never by login (tagged nodes report the synthetic `tagged-devices` login), and an identity with no login never matches.
- Any `Authorization` header the client sends is discarded before the request reaches the MCP handler, so a tailnet peer cannot present a Cloudflare JWT or anything else to escalate.

Limits:
- `--insecure` does not apply to this listener and cannot disable the WhoIs gate.
- The node key lives in `TS_STATE_DIR`. Anyone who can read that directory can impersonate the node on the tailnet; it is persisted so restarts do not re-register, and should be on a volume with the same protection as the container's other state.
- The auth key should be tagged (so the node carries `tag:mcp-home` rather than a user's identity), reusable only if you need it to be, and revocable from the admin console.

## Threat Model and Tests

Every way in, the attack tried, and the test that fails if the defence is removed. "Fixed" means the attack worked before this table was written.

| Vector | Attack | Test | Status |
|---|---|---|---|
| CF listener, no/forged token | none, `alg=none`, forged bearer or `Cf-Access-Jwt-Assertion`, Basic, bare token, two tokens, tailnet/insecure placeholders | `cmd/mcp-server` `TestCloudflareDoorRejectsWithoutValidToken` | held |
| CF listener, spoofed identity | `X-Forwarded-For`, `Cf-Connecting-Ip`, `Cf-Access-Authenticated-User-Email`, `Tailscale-User-Login`, localhost `Host` | same | held |
| CF listener, path tricks | `/mcp/`, `/MCP`, `//mcp`, `/mcp/../mcp`, `%2F`, dot segments, every method | `TestCloudflareDoorPathVariants` | held |
| JWT forgery | HS256 keyed with the public key, `none`/`None`/`rs256`/`PS256`, `crit`, unknown/empty kid, missing/now/past `exp`, future `nbf`, wrong/empty/object `aud`, issuer prefix, stripped/padded signature, tampered claims | `internal/cfaccess` `TestForgedTokensRejected`, `TestValidateTamperedClaims` | held; `nbf`/`crit` **fixed** |
| JWT, no identity | signed token without `email`/`common_name` | `TestForgedTokensRejected/no_identity`, `TestServiceTokenAttributed` | **fixed** (audited as anonymous) |
| 401 oracle | read the 401 body for expected AUD/issuer/kid | `TestRejectionDoesNotLeakConfiguration` | **fixed** |
| Empty AUD | validator built with `""` vs token `aud: [""]` | `TestEmptyConfiguredAudienceFailsClosed` | **fixed** |
| JWKS rotation | new kid after rotation; retired key | `TestKeyRotation` | held |
| JWKS outage | certs 5xx before any fetch; after cache expiry; garbage certs; refetch flood | `TestCertsOutageAtStartFailsClosed`, `TestCertsOutageServesStaleKeysBounded`, `TestUnknownKidRefetchIsRateLimited` | **fixed** (lockout + fetch per request) |
| JWKS fetch poisoning | caller hangs up mid-fetch | `TestCancelledRequestDoesNotFailFetch` | **fixed** |
| Access app discovery | lookalike domain, sibling path app, host vs `/mcp` app | `TestDiscoverIgnoresLookalikes`, `TestDiscoverPrefersPathApplication` | held / improved |
| Tailnet allowlist | tag prefix/extension, login case/prefix, `tagged-devices` login, identity with no login | `internal/tsauth` `TestAllowed` | held |
| Tailnet blank allowlist | `TS_ALLOWED_LOGINS=","` → `[""]` matched a peer with no login; empty allowlist = whole tailnet | `TestMiddlewareCleansAllowlist`, `TestMiddlewareEmptyAllowlistFailsClosed`, `config` `TestParseRejectsBlankAllowlistEntries` | **fixed** |
| Tailnet headers | client `Authorization`, `Cf-Access-Jwt-Assertion`, `Tailscale-User-Login`, `X-Forwarded-For` | `TestMiddlewareDeniesWhateverHeadersArrive`, `TestMiddlewareUsesRemoteAddrOnly`, `cmd` `TestTailnetDoorIgnoresCloudflareCredentials` | held |
| Placeholder bearer | `tailnet-whois` without the WhoIs context, or on the CF door | `TestPlaceholderTokenAloneIsWorthless`, CF reject test | held |
| `--insecure` | reaches tailnet; insecure calls indistinguishable in audit | `tailnetMux` takes no config; `TestInsecureDoorIsLabelled` | held / **fixed** (labelled) |
| Path traversal | `..`, encoded, unicode lookalikes, NUL, CR/LF, overlong | `internal/validate` `TestIdentifierRejectsTraversal`, `FuzzIdentifier` | held (+255-byte cap) |
| Oversized body | unauthenticated multi-MB POST; authenticated | `TestOversizedBodies` | held |
| Header/body mismatch | `Mcp-Method`, `Mcp-Name`, missing header | `TestHeaderBodyMismatchRejected` | held (see 5: header-version caveat) |
| CSRF from a browser | preflight, `text/plain` simple POST | `TestBrowserCrossOriginCannotCall` | held |
| Confirmation bypass | pre-answered yes, yes replayed onto other args/tool, tampered/expired state | `TestPreAnsweredConfirmationOverHTTP`, `internal/mcputil` `TestConfirmRejectsUnboundAccept`, `TestConfirmStateExpires` | **fixed** |
| Shared caching | HTTP cache stores a tool list/result | `TestResponsesNotStorableByHTTPCaches` | **fixed** |
| Audit leaks | passwords/keys/JWTs/bearer/`token=` in args or error text; forged log line | `TestAuditRedactsSecrets`, `TestAuditRedactsErrorText`, `TestAuditLineCannotBeForged` | **fixed** |
| Attribution | who, which door, outcome, protocol, both formats | `TestAuditAnswersWhoWhatWhereOutcome`, `cmd` `TestTailnetDoorAttributesWhoIs`, `TestCloudflareDoorAttributesUser` | **fixed** (no door before) |

## Failure Modes

What the operator sees, and what the household sees.

| Failure | Behaviour | Test |
|---|---|---|
| Partial config group, malformed `CF_HOSTNAME`, blank/malformed allowlist | refuses to start, names the variable | `internal/config` tests |
| CF API 429/5xx/network at boot | retried with backoff (discovery and tunnel setup), then refuses to start | `TestSetupRetriesTransientFailures`, `cfapi` `TestRetry` |
| CF API 401/403/404 | refuses to start at once, naming `CF_API_TOKEN`, the permission, or `CF_ACCOUNT_ID`/`CF_ZONE_ID` | `TestSetupPermanentFailureIsActionable`, `TestDiscoverFailuresAreActionable` |
| No Access app for the hostname | refuses to start, lists related domains and says where to create one | `TestDiscoverIgnoresLookalikes` |
| Non-CNAME record on `CF_HOSTNAME` | refuses to start, says to delete it | message in `ensureDNS` |
| cloudflared missing / cannot execute | refuses to start (boot) or stops the group (runtime) | `TestSuperviseMissingBinaryIsFatal` |
| cloudflared crashes | logged as an error, restarted with backoff after re-setup; tailnet unaffected | `TestSuperviseRestartsAfterCrash` |
| Tunnel deleted out from under us | recreated, DNS repointed, cloudflared restarted with the new token | `TestSuperviseRecreatesDeletedTunnel` |
| Access certs endpoint down | cached keys keep working up to 24h; warning logged | `TestCertsOutageServesStaleKeysBounded` |
| `TS_STATE_DIR` not creatable/writable | refuses to start naming `TS_STATE_DIR` and the uid | `TestCheckStateDir` |
| tsnet cannot come up (bad key, approval, no route) | refuses to start after 90s with the likely causes | `TestUpErrorIsActionable` |
| One door fails at boot | the other door's processes are stopped before exit | `TestRunDoorsStopsOthersOnFailedStart` |
| SIGTERM during startup | exits 0 | `TestRunDoorsSignalDuringStartup` |
| SIGTERM during a tool call | listener drains up to 8s, cloudflared gets SIGTERM with the same grace, tsnet closed after the drain | `TestShutdownDrainsInFlightRequest`, `TestRunStopsCloudflaredGracefully` |
| Redeploy | connected clients keep working on the new instance on both protocols; confirmations survive | `TestRedeployIsInvisibleToClients`, `TestConfirmSecretAcrossRestart` |

## Open Questions

### Rate limiting
No server-side rate limiting. Cloudflare's edge provides some protection, but a compromised authenticated session could flood the server.

### cloudflared download
When `cloudflared` is not on PATH it is downloaded from the latest GitHub release over HTTPS with no checksum or signature check. The container image bundles it, so this only affects bare-metal runs.

### API token scope
The `CF_API_TOKEN` has tunnel management + Access read permissions. Consider using separate tokens with narrower scopes.

### Concurrent requests
The server holds no sessions to limit, but it also does not cap concurrent in-flight requests. A misbehaving client could still issue many at once; each one occupies a goroutine and can reach the home network. `PropagateRequestCancellation` bounds the damage from clients that hang up mid-call, but only for those on protocol `2026-07-28` — an older client's abandoned request runs to completion.
