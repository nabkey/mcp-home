// Command mcp-server runs the MCP server over HTTP behind a Cloudflare Tunnel
// and/or on a tailnet via an embedded Tailscale node.
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
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/nabkey/mcp-home/internal/cfaccess"
	"github.com/nabkey/mcp-home/internal/config"
	"github.com/nabkey/mcp-home/internal/middleware"
	"github.com/nabkey/mcp-home/internal/server"
	"github.com/nabkey/mcp-home/internal/tsauth"
	"github.com/nabkey/mcp-home/internal/tunnel"
	"golang.org/x/sync/errgroup"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var cli config.CLI
	kong.Parse(&cli,
		kong.Name("mcp-server"),
		kong.Description("MCP server for smart home and media management, served via Cloudflare Tunnel and/or Tailscale."),
		kong.Vars{"version": version},
	)

	var level slog.Level
	// Kong's enum tag guarantees the value parses.
	_ = level.UnmarshalText([]byte(cli.LogLevel))

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	logger.Info("mcp-server starting", "version", version, "log_level", level)

	if err := run(cli, logger); err != nil {
		log.Fatal(err)
	}
}

func run(cli config.CLI, logger *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv := server.New(ctx, cli, version, logger)
	handler := server.NewHTTPHandler(srv, logger)

	// Each front door runs in the group; the first to fail (or the signal
	// context) takes the rest down. Config validation guarantees at least
	// one is enabled.
	g, gctx := errgroup.WithContext(ctx)

	if cli.Cloudflare.Enabled() {
		if err := serveCloudflare(gctx, g, cli, handler, logger); err != nil {
			return err
		}
	}
	if cli.Tailscale.Enabled() {
		if err := serveTailnet(gctx, g, cli, handler, logger); err != nil {
			return err
		}
	}
	return g.Wait()
}

// healthHandler answers /health on every listener. It is unauthenticated on
// purpose: it reveals nothing and lets the tunnel and tailnet probe liveness.
func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// serveHTTP runs srv on ln in the group and shuts it down when ctx ends.
func serveHTTP(ctx context.Context, g *errgroup.Group, srv *http.Server, ln net.Listener, name string) {
	g.Go(func() error {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("%s http server: %w", name, err)
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil
	})
}

// serveCloudflare binds a localhost listener, gates /mcp with Cloudflare
// Access JWT validation (unless --insecure), and runs the tunnel that
// publishes it. cloudflared is the long-running piece: it stays in the group
// until the context ends or it exits on its own.
func serveCloudflare(ctx context.Context, g *errgroup.Group, cli config.CLI, handler http.Handler, logger *slog.Logger) error {
	// Random localhost port: only cloudflared, on this host, needs to reach it.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	addr := listener.Addr().String()
	logger.Info("starting MCP HTTP server", "addr", addr)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)

	if cli.Insecure {
		logger.Warn("INSECURE MODE: OAuth Bearer token validation disabled — tunnel MCP endpoints are unauthenticated")
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

	serveHTTP(ctx, g, &http.Server{
		Handler:           middleware.Logging(logger)(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}, listener, "tunnel")

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

	// Run cloudflared — blocks until the context is cancelled or it exits.
	// A clean exit while the context is still live means the tunnel is gone
	// with nobody having asked; surface it so the group shuts down rather
	// than serving a listener nothing can reach.
	g.Go(func() error {
		if err := tun.Run(ctx); err != nil {
			return fmt.Errorf("tunnel run: %w", err)
		}
		if ctx.Err() == nil {
			return fmt.Errorf("tunnel run: cloudflared exited")
		}
		return nil
	})
	return nil
}

// serveTailnet joins the tailnet as an embedded node and serves the same
// MCP handler there. The gate is WhoIs identity rather than a Cloudflare
// Access JWT, so peers like the voice agent can call /mcp without OAuth or
// a static token. --insecure does not apply here.
func serveTailnet(ctx context.Context, g *errgroup.Group, cli config.CLI, handler http.Handler, logger *slog.Logger) error {
	ts, err := tsauth.Start(ctx, tsauth.Config{
		Hostname: cli.Tailscale.Hostname,
		AuthKey:  cli.Tailscale.AuthKey,
		StateDir: cli.Tailscale.StateDir,
		Logger:   logger,
	})
	if err != nil {
		return fmt.Errorf("tailscale: %w", err)
	}
	ln, err := ts.ListenTLS()
	if err != nil {
		_ = ts.Close()
		return fmt.Errorf("tailscale listen: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	gate := ts.Middleware(cli.Tailscale.AllowedLogins, cli.Tailscale.AllowedTags)
	mux.Handle("/mcp", gate(handler))
	mux.Handle("/mcp/sse", gate(handler))

	serveHTTP(ctx, g, &http.Server{
		Handler:           middleware.Logging(logger)(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}, ln, "tailnet")
	g.Go(func() error {
		<-ctx.Done()
		return ts.Close()
	})

	logger.Info("MCP server available on tailnet", "url", "https://"+ts.FQDN+"/mcp",
		"allowed_logins", cli.Tailscale.AllowedLogins, "allowed_tags", cli.Tailscale.AllowedTags)
	return nil
}
