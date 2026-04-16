// Command mcp-server runs the MCP server over HTTP behind a Cloudflare Tunnel.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/nabkey/mcp-home/internal/cfaccess"
	"github.com/nabkey/mcp-home/internal/config"
	"github.com/nabkey/mcp-home/internal/middleware"
	"github.com/nabkey/mcp-home/internal/server"
	"github.com/nabkey/mcp-home/internal/tunnel"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

func main() {
	var cli config.CLI
	kong.Parse(&cli,
		kong.Name("mcp-server"),
		kong.Description("MCP server for smart home and media management, served via Cloudflare Tunnel."),
	)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(cli, logger); err != nil {
		log.Fatal(err)
	}
}

func run(cli config.CLI, logger *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Start HTTP server on a random localhost port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	addr := listener.Addr().String()

	logger.Info("starting MCP HTTP server", "addr", addr)

	srv := server.New(cli, logger)
	handler := mcp.NewStreamableHTTPHandler(
		func(req *http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			Logger: logger,
			// Disable localhost DNS rebinding protection — requests arrive via
			// cloudflared on 127.0.0.1 with an external Host header. Auth is
			// handled by Cloudflare Access OAuth instead.
			DisableLocalhostProtection: true,
		},
	)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	if cli.Insecure {
		logger.Warn("INSECURE MODE: OAuth Bearer token validation disabled — MCP endpoints are unauthenticated")
		mux.Handle("/mcp", handler)
		mux.Handle("/mcp/sse", handler)
	} else {
		// Auto-discover Access team and MCP Portal AUD from the Cloudflare API.
		accessCfg, err := cfaccess.Discover(ctx, cli.Cloudflare.APIToken, cli.Cloudflare.AccountID, cli.Cloudflare.Hostname, logger)
		if err != nil {
			return fmt.Errorf("cloudflare access: %w (pass --insecure to bypass)", err)
		}

		validator := cfaccess.New(accessCfg.Team, accessCfg.AUD, logger)
		authServerURL := "https://" + accessCfg.Team + ".cloudflareaccess.com"
		resourceURL := "https://" + cli.Cloudflare.Hostname + "/mcp"
		metadataURL := "https://" + cli.Cloudflare.Hostname + "/.well-known/oauth-protected-resource"

		// Serve OAuth 2.0 Protected Resource Metadata (RFC 9728) so clients
		// can discover Cloudflare Access as the authorization server.
		// Mount at both the base path and the /mcp-suffixed path per the MCP spec
		// (clients try the path-suffixed version first for non-root MCP endpoints).
		metadataHandler := auth.ProtectedResourceMetadataHandler(
			&oauthex.ProtectedResourceMetadata{
				Resource:               resourceURL,
				AuthorizationServers:   []string{authServerURL},
				BearerMethodsSupported: []string{"header"},
			},
		)
		mux.Handle("/.well-known/oauth-protected-resource", metadataHandler)
		mux.Handle("/.well-known/oauth-protected-resource/mcp", metadataHandler)

		// Protect MCP endpoints with Bearer token validation.
		// Unauthenticated requests get 401 + WWW-Authenticate header pointing
		// to the protected resource metadata, enabling OAuth discovery.
		authMW := auth.RequireBearerToken(validator.TokenVerifier(), &auth.RequireBearerTokenOptions{
			ResourceMetadataURL: metadataURL,
		})
		// Bridge CF Access → OAuth: if the request has Cf-Access-Jwt-Assertion
		// but no Authorization header, copy the JWT into Authorization: Bearer.
		// This handles the CF Access edge injecting auth in its own header format.
		cfAccessBridge := func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "" {
					if cfJWT := r.Header.Get("Cf-Access-Jwt-Assertion"); cfJWT != "" {
						r.Header.Set("Authorization", "Bearer "+cfJWT)
					}
				}
				next.ServeHTTP(w, r)
			})
		}
		mux.Handle("/mcp", cfAccessBridge(authMW(handler)))
		mux.Handle("/mcp/sse", cfAccessBridge(authMW(handler)))

		logger.Info("OAuth Bearer token validation enabled",
			"team", accessCfg.Team,
			"auth_server", authServerURL,
			"resource", resourceURL,
		)
	}

	// Wrap with request logging.
	root := middleware.Logging(logger)(mux)

	httpServer := &http.Server{
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			logger.Error("http server error", "error", err)
		}
	}()

	// Set up the Cloudflare Tunnel.
	tun, err := tunnel.Setup(ctx, tunnel.Config{
		APIToken:   cli.Cloudflare.APIToken,
		AccountID:  cli.Cloudflare.AccountID,
		ZoneID:     cli.Cloudflare.ZoneID,
		Hostname:   cli.Cloudflare.Hostname,
		TunnelName: cli.Cloudflare.TunnelName,
		LocalAddr:  "http://" + addr,
		Logger:     logger,
	})
	if err != nil {
		return fmt.Errorf("tunnel setup: %w", err)
	}

	logger.Info("MCP server available", "url", "https://"+cli.Cloudflare.Hostname+"/mcp")

	// Run cloudflared — blocks until context is cancelled.
	if err := tun.Run(ctx); err != nil {
		return fmt.Errorf("tunnel run: %w", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	return nil
}
