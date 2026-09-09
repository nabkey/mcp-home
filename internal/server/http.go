package server

import (
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewHTTPHandler wraps srv in the streamable HTTP transport that fronts the
// Cloudflare Tunnel.
//
// The transport runs stateless, which is what lets it speak protocol
// 2026-07-28: the SDK only accepts that revision when sessions are off. Every
// POST carries its own client info and is answered on the spot, so no
// Mcp-Session-Id is issued and nothing is pinned to one process. That suits
// this deployment — the container is recreated on every release, and a session
// map in memory would mean each redeploy invalidated whatever the connected
// client was holding. Older clients still work; they negotiate down to
// 2025-11-25 and simply go without a session id.
//
// Nothing here needs sessions to survive. Stateless mode forbids
// server->client requests and drops stream resumption and the standalone GET
// stream; this server issues no elicitation, sampling, roots or logging
// requests, configures no EventStore, and pushes no unsolicited
// notifications. The visible cost is that GET returns 405, so /mcp/sse serves
// POST only.
func NewHTTPHandler(srv *mcp.Server, logger *slog.Logger) http.Handler {
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			Logger: logger,

			Stateless: true,

			// Tie tool handlers to the HTTP request that started them. Tools
			// here can run long — ESPHome compiles and log captures, Frigate
			// snapshots, HA websocket round trips — and without this a client
			// that hangs up leaves the work running against the home network
			// with nowhere to deliver the result. Only applies to >= 2026-07-28,
			// where the POST is the whole request lifecycle.
			PropagateRequestCancellation: true,

			// Requests arrive via cloudflared on 127.0.0.1 carrying an external
			// Host header, which the default DNS-rebinding check would reject.
			// Cloudflare Access OAuth is the actual gate; see SECURITY.md.
			DisableLocalhostProtection: true,
		},
	)
}
