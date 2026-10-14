// Command mcp-server runs the MCP server over HTTP behind a Cloudflare Tunnel
// and/or on a tailnet via an embedded Tailscale node.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/nabkey/mcp-home/internal/cfaccess"
	"github.com/nabkey/mcp-home/internal/cfapi"
	"github.com/nabkey/mcp-home/internal/config"
	"github.com/nabkey/mcp-home/internal/middleware"
	"github.com/nabkey/mcp-home/internal/server"
	"github.com/nabkey/mcp-home/internal/tsauth"
	"github.com/nabkey/mcp-home/internal/tunnel"
	"golang.org/x/sync/errgroup"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// drainTimeout is how long a listener keeps serving in-flight requests after
// shutdown starts. It matches cloudflared's grace period, so a request that
// is mid-flight through the tunnel can still be answered, and both fit in
// Docker's default 10s stop timeout.
const drainTimeout = tunnel.GracePeriod

func main() {
	var cli config.CLI
	kong.Parse(&cli,
		kong.Name("mcp-server"),
		kong.Description("MCP server for smart home and media management, served via Cloudflare Tunnel and/or Tailscale."),
		kong.Vars{"version": version},
	)

	logger := newLogger(os.Stderr, cli.LogLevel, cli.LogFormat)
	logger.Info("mcp-server starting", "version", version, "log_level", cli.LogLevel, "log_format", cli.LogFormat)

	if err := run(cli, logger); err != nil {
		// Through the logger, not log.Fatal, so the reason for exiting is a
		// structured record in the same format as everything else.
		logger.Error("mcp-server exiting", "error", err)
		os.Exit(1)
	}
	logger.Info("mcp-server stopped")
}

// newLogger builds the process logger. Kong's enum tags guarantee both
// values are valid, so an unparsable level falls back to info and any format
// other than json is text.
func newLogger(w io.Writer, level, format string) *slog.Logger {
	var lvl slog.Level
	_ = lvl.UnmarshalText([]byte(level))
	opts := &slog.HandlerOptions{Level: lvl}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

func run(cli config.CLI, logger *slog.Logger) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv := server.New(ctx, cli, version, logger)
	handler := server.NewHTTPHandler(srv, logger)

	var doors []frontDoor
	if cli.Cloudflare.Enabled() {
		doors = append(doors, func(ctx context.Context, g *errgroup.Group) error {
			return serveCloudflare(ctx, g, cli, handler, logger)
		})
	} else if cli.Insecure {
		logger.Warn("--insecure has no effect: the Cloudflare front door is not configured, and it never applies to the tailnet")
	}
	if cli.Tailscale.Enabled() {
		doors = append(doors, func(ctx context.Context, g *errgroup.Group) error {
			return serveTailnet(ctx, g, cli, handler, logger)
		})
	}
	return runDoors(ctx, logger, doors)
}

// frontDoor sets up one listener. It returns once the door is serving,
// having added its long-running pieces to g, or with an error if it could
// not be brought up.
type frontDoor func(ctx context.Context, g *errgroup.Group) error

// runDoors brings every front door up concurrently, so a slow one (Cloudflare
// API retries, a tailnet login) does not hold up the other, then serves
// until ctx ends or a running piece fails.
//
// If any door fails to come up the server refuses to start, but first stops
// whatever the others already started: returning straight away would exit
// the process with cloudflared or the tsnet node still running.
func runDoors(parent context.Context, logger *slog.Logger, doors []frontDoor) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)

	errs := make([]error, len(doors))
	var wg sync.WaitGroup
	for i, door := range doors {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := door(gctx, g); err != nil {
				errs[i] = err
				cancel()
			}
		}()
	}
	wg.Wait()

	if err := firstCause(errs); err != nil {
		cancel()
		if werr := g.Wait(); werr != nil {
			logger.Debug("while stopping after a failed start", "error", werr)
		}
		if parent.Err() != nil {
			// SIGTERM during startup: an orderly stop, not a failure.
			logger.Info("stopped during startup", "reason", parent.Err())
			return nil
		}
		return fmt.Errorf("refusing to start: %w", err)
	}
	return g.Wait()
}

// firstCause picks the error that caused a failed start. Doors interrupted
// because another one failed report a cancellation; that is not the cause.
func firstCause(errs []error) error {
	var fallback error
	for _, err := range errs {
		if err == nil {
			continue
		}
		if !errors.Is(err, context.Canceled) {
			return err
		}
		if fallback == nil {
			fallback = err
		}
	}
	return fallback
}

// healthHandler answers /health on every listener. It is unauthenticated on
// purpose: it reveals nothing and lets the tunnel and tailnet probe liveness.
func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// serveHTTP runs srv on ln in the group. When ctx ends it stops accepting
// connections, lets in-flight requests finish for up to drainTimeout, and
// then calls each of after in order — the place to tear down whatever
// carries the traffic (the tsnet node), which must outlive the drain.
func serveHTTP(ctx context.Context, g *errgroup.Group, srv *http.Server, ln net.Listener, name string, logger *slog.Logger, after ...func()) {
	g.Go(func() error {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("%s http server: %w", name, err)
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), drainTimeout)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("in-flight requests did not finish before the drain timeout", "listener", name, "timeout", drainTimeout, "error", err)
			_ = srv.Close()
		}
		for _, f := range after {
			f()
		}
		return nil
	})
}

// newServer builds the http.Server for a listener.
func newServer(h http.Handler, name string, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           middleware.Logging(logger.With("listener", name))(h),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// cloudflareMux builds the routes for the Cloudflare listener. verifier nil
// means --insecure: /mcp is served without validation (and audited as
// anonymous on the insecure door).
func cloudflareMux(handler http.Handler, verifier auth.TokenVerifier, hostname, authServerURL string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)

	if verifier == nil {
		gate := insecureIdentity
		mux.Handle("/mcp", gate(handler))
		mux.Handle("/mcp/sse", gate(handler))
		return mux
	}

	resourceURL := "https://" + hostname + "/mcp"
	metadataURL := "https://" + hostname + "/.well-known/oauth-protected-resource"

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
	authMW := auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: metadataURL,
	})
	mux.Handle("/mcp", cfAccessBridge(authMW(handler)))
	mux.Handle("/mcp/sse", cfAccessBridge(authMW(handler)))
	return mux
}

// cfAccessBridge copies Cf-Access-Jwt-Assertion into Authorization: Bearer
// when the request has no Authorization header. Cloudflare Access injects
// the JWT in its own header; the go-sdk middleware reads the standard one.
// Either way the token is validated in full, so a client choosing which
// header to send gains nothing.
func cfAccessBridge(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if cfJWT := r.Header.Get("Cf-Access-Jwt-Assertion"); cfJWT != "" {
				r.Header.Set("Authorization", "Bearer "+cfJWT)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// insecureIdentity records an explicit anonymous identity on the insecure
// door, so its calls are audited as front_door=cloudflare-insecure rather
// than indistinguishable from a request that slipped past authentication.
func insecureIdentity(next http.Handler) http.Handler {
	record := auth.RequireBearerToken(func(context.Context, string, *http.Request) (*auth.TokenInfo, error) {
		return &auth.TokenInfo{UserID: "anonymous", Extra: map[string]any{cfaccess.FrontDoorKey: "cloudflare-insecure"}}, nil
	}, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Authorization", "Bearer insecure")
		record.ServeHTTP(w, r)
	})
}

// serveCloudflare binds a localhost listener, gates /mcp with Cloudflare
// Access JWT validation (unless --insecure), and runs the tunnel that
// publishes it. cloudflared is supervised: if it exits it is restarted
// (after re-running tunnel setup) rather than taking the process down.
func serveCloudflare(ctx context.Context, g *errgroup.Group, cli config.CLI, handler http.Handler, logger *slog.Logger) error {
	// Find (or fetch) cloudflared before touching the Cloudflare API, so a
	// host that cannot run it fails at boot with that, not minutes later.
	binary, err := tunnel.EnsureCloudflared(ctx, logger)
	if err != nil {
		return fmt.Errorf("cloudflared not available: %w (install it on PATH, or allow the host to download it from github.com)", err)
	}

	var verifier auth.TokenVerifier
	authServerURL := ""
	if cli.Insecure {
		logger.Warn("INSECURE MODE: OAuth Bearer token validation disabled — tunnel MCP endpoints are unauthenticated")
	} else {
		// Auto-discover Access team and application AUD from the Cloudflare API.
		// Transient API failures are retried so a blip at boot does not
		// crash-loop the server (taking the tailnet door down with it).
		var accessCfg *cfaccess.Config
		err := cfapi.Retry(ctx, 5, 2*time.Second, logger, "Access discovery", func() error {
			var derr error
			accessCfg, derr = cfaccess.Discover(ctx, cli.Cloudflare.APIToken, cli.Cloudflare.AccountID, cli.Cloudflare.Hostname, logger)
			return derr
		})
		if err != nil {
			return fmt.Errorf("cloudflare access: %w", err)
		}
		verifier = cfaccess.New(accessCfg.Team, accessCfg.AUD, logger).TokenVerifier()
		authServerURL = "https://" + accessCfg.Team + ".cloudflareaccess.com"
		logger.Info("OAuth Bearer token validation enabled", "team", accessCfg.Team, "auth_server", authServerURL)
	}

	// Random localhost port: only cloudflared, on this host, needs to reach it.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	addr := listener.Addr().String()

	// Set up the Cloudflare Tunnel before serving, so a setup failure does
	// not leave a listener behind.
	tun, err := tunnel.Setup(ctx, tunnel.Config{
		APIToken:   cli.Cloudflare.APIToken,
		AccountID:  cli.Cloudflare.AccountID,
		ZoneID:     cli.Cloudflare.ZoneID,
		Hostname:   cli.Cloudflare.Hostname,
		TunnelName: cli.Cloudflare.TunnelName,
		LocalAddr:  "http://" + addr,
		Logger:     logger,
		Binary:     binary,
	})
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("tunnel setup: %w", err)
	}

	logger.Info("starting MCP HTTP server", "addr", addr)
	serveHTTP(ctx, g, newServer(cloudflareMux(handler, verifier, cli.Cloudflare.Hostname, authServerURL), "tunnel", logger),
		listener, "tunnel", logger)

	// cloudflared stops on the same signal, with its own grace period, so
	// requests already in the tunnel are answered while the listener drains.
	g.Go(func() error {
		if err := tun.Supervise(ctx); err != nil {
			return fmt.Errorf("tunnel: %w", err)
		}
		return nil
	})
	logger.Info("MCP server available", "url", "https://"+cli.Cloudflare.Hostname+"/mcp")
	return nil
}

// tailnetMux builds the routes for the tailnet listener. --insecure never
// reaches this function: the gate is always the WhoIs allowlist.
func tailnetMux(handler http.Handler, gate func(http.Handler) http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.Handle("/mcp", gate(handler))
	mux.Handle("/mcp/sse", gate(handler))
	return mux
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

	gate := ts.Middleware(cli.Tailscale.AllowedLogins, cli.Tailscale.AllowedTags)
	// The node is closed only after the listener has drained: closing it
	// first would cut the WireGuard path under requests still being answered.
	serveHTTP(ctx, g, newServer(tailnetMux(handler, gate), "tailnet", logger), ln, "tailnet", logger, func() {
		if err := ts.Close(); err != nil {
			logger.Warn("tsnet close", "error", err)
		}
	})

	logger.Info("MCP server available on tailnet", "url", "https://"+ts.FQDN+"/mcp",
		"allowed_logins", cli.Tailscale.AllowedLogins, "allowed_tags", cli.Tailscale.AllowedTags)
	return nil
}
