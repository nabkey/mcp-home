// Package tunnel manages Cloudflare Tunnel lifecycle for the MCP server.
// It creates/reuses a named tunnel, configures ingress, sets up DNS,
// and runs cloudflared as a supervised subprocess.
package tunnel

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cloudflare/cloudflare-go/v4/zero_trust"
	"github.com/nabkey/mcp-home/internal/cfapi"
)

// GracePeriod is how long cloudflared keeps serving in-flight requests after
// it is asked to stop. It fits inside Docker's default 10s stop timeout,
// and the HTTP servers drain for the same period, so a tool call that is
// already running when a redeploy starts can still deliver its result.
const GracePeriod = 8 * time.Second

// Restart and retry pacing. Variables so tests can run them quickly.
var (
	// restartMin and restartMax bound the backoff between cloudflared
	// restarts; a run that lasted stableRun resets it.
	restartMin = time.Second
	restartMax = time.Minute
	stableRun  = time.Minute
	// setupAttempts and setupBackoff pace retries of transient Cloudflare
	// API failures during Setup, so a brief API blip at boot does not take
	// the whole server (and the tailnet door with it) down.
	setupAttempts = 5
	setupBackoff  = 2 * time.Second
)

// Config holds the configuration needed to create and run a Cloudflare Tunnel.
type Config struct {
	// APIToken is a Cloudflare API token with Tunnel:Edit and DNS:Edit permissions.
	APIToken string
	// AccountID is the Cloudflare account ID.
	AccountID string
	// ZoneID is the Cloudflare DNS zone ID for the hostname.
	ZoneID string
	// Hostname is the public hostname (e.g., "mcp.example.com").
	Hostname string
	// TunnelName is the name of the tunnel to create/reuse.
	TunnelName string
	// LocalAddr is the local address to proxy to (e.g., "http://localhost:8080").
	LocalAddr string
	// Logger for status messages.
	Logger *slog.Logger
	// APIOptions are extra cloudflare-go request options (tests point the
	// client at a fake API with them).
	APIOptions []option.RequestOption
	// Binary is the cloudflared path; empty means look it up (and download
	// it if missing) when the tunnel first runs.
	Binary string
}

// Tunnel represents a configured Cloudflare Tunnel ready to run.
type Tunnel struct {
	ID     string
	token  string
	config Config
}

// Setup creates or reuses a Cloudflare Tunnel, configures ingress, and ensures
// a DNS CNAME record exists. Returns a Tunnel ready to Run.
//
// Transient API failures (rate limiting, 5xx, network) are retried a few
// times with backoff; permanent ones (bad token, missing permission, wrong
// account or zone) return at once with what to fix.
func Setup(ctx context.Context, cfg Config) (*Tunnel, error) {
	var lastErr error
	delay := setupBackoff
	for attempt := 1; attempt <= setupAttempts; attempt++ {
		t, err := setupOnce(ctx, cfg)
		if err == nil {
			return t, nil
		}
		lastErr = err
		if !cfapi.Transient(err) || attempt == setupAttempts {
			break
		}
		cfg.Logger.Warn("tunnel setup failed, retrying", "attempt", attempt, "of", setupAttempts, "in", delay, "error", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("tunnel setup interrupted: %w", lastErr)
		case <-time.After(delay):
		}
		delay *= 2
	}
	return nil, lastErr
}

func setupOnce(ctx context.Context, cfg Config) (*Tunnel, error) {
	client := cloudflare.NewClient(append([]option.RequestOption{option.WithAPIToken(cfg.APIToken)}, cfg.APIOptions...)...)

	tunnelID, err := findOrCreateTunnel(ctx, client, cfg)
	if err != nil {
		return nil, err
	}

	cfg.Logger.Info("tunnel ready", "id", tunnelID, "name", cfg.TunnelName)

	if err := configureIngress(ctx, client, cfg, tunnelID); err != nil {
		return nil, cfapi.Explain(err, "configure tunnel ingress", "Account: Cloudflare Tunnel Edit")
	}

	cfg.Logger.Info("ingress configured", "hostname", cfg.Hostname, "service", cfg.LocalAddr)

	if err := ensureDNS(ctx, client, cfg, tunnelID); err != nil {
		return nil, err
	}

	cfg.Logger.Info("DNS configured", "hostname", cfg.Hostname, "target", tunnelID+".cfargotunnel.com")

	token, err := client.ZeroTrust.Tunnels.Cloudflared.Token.Get(ctx, tunnelID, zero_trust.TunnelCloudflaredTokenGetParams{
		AccountID: cloudflare.F(cfg.AccountID),
	})
	if err != nil {
		return nil, cfapi.Explain(err, "get tunnel token", "Account: Cloudflare Tunnel Edit")
	}
	if token == nil || *token == "" {
		return nil, errors.New("get tunnel token: Cloudflare returned an empty token")
	}

	return &Tunnel{
		ID:     tunnelID,
		token:  *token,
		config: cfg,
	}, nil
}

// Supervise runs cloudflared until ctx ends. An unexpected exit is logged
// and cloudflared is restarted with backoff, after re-running Setup: that
// picks up a rotated token and recreates a tunnel that was deleted out from
// under us, either of which is a common reason for cloudflared to exit. The
// rest of the server keeps running meanwhile (the tailnet door in
// particular), so a cloudflared crash costs the Cloudflare path a few
// seconds instead of taking the whole process down.
//
// It returns nil once ctx ends, and an error only when the cloudflared
// binary cannot be found at all.
func (t *Tunnel) Supervise(ctx context.Context) error {
	logger := t.config.Logger
	delay := restartMin
	for {
		start := time.Now()
		err := t.Run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		var nf *binaryError
		if errors.As(err, &nf) {
			return err
		}
		if time.Since(start) >= stableRun {
			delay = restartMin
		}
		logger.Error("cloudflared exited unexpectedly; the Cloudflare door is down until it restarts",
			"error", err, "restart_in", delay)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay = min(delay*2, restartMax)

		fresh, err := Setup(ctx, t.config)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			logger.Error("tunnel re-setup failed; restarting cloudflared with the previous token", "error", err)
			continue
		}
		if fresh.ID != t.ID {
			logger.Warn("tunnel was replaced", "old_id", t.ID, "new_id", fresh.ID)
		}
		t.ID, t.token = fresh.ID, fresh.token
	}
}

// binaryError marks a failure to obtain the cloudflared binary, which no
// amount of restarting fixes.
type binaryError struct{ err error }

func (e *binaryError) Error() string { return "cloudflared: " + e.err.Error() }
func (e *binaryError) Unwrap() error { return e.err }

// Run starts cloudflared once and blocks until the context is cancelled or
// cloudflared exits. On cancellation cloudflared gets SIGTERM, not SIGKILL,
// so it stops taking new requests and finishes the ones in flight within
// GracePeriod; it is killed only if it overstays that.
func (t *Tunnel) Run(ctx context.Context) error {
	path := t.config.Binary
	if path == "" {
		p, err := EnsureCloudflared(ctx, t.config.Logger)
		if err != nil {
			return &binaryError{err}
		}
		t.config.Binary, path = p, p
	}

	t.config.Logger.Info("starting cloudflared", "path", path, "tunnel", t.config.TunnelName, "id", t.ID)

	cmd := exec.CommandContext(ctx, path, "tunnel", "--no-autoupdate", "run")
	// The token goes in the environment, not argv, so it is not visible in
	// ps output. TUNNEL_GRACE_PERIOD is cloudflared's own drain window.
	cmd.Env = append(os.Environ(), "TUNNEL_TOKEN="+t.token, "TUNNEL_GRACE_PERIOD="+GracePeriod.String())
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = GracePeriod + 2*time.Second
	out := newLineLogger(t.config.Logger)
	defer func() { _ = out.Close() }()
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return &binaryError{fmt.Errorf("start %s: %w", path, err)}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil // context cancelled, clean shutdown
		}
		return fmt.Errorf("cloudflared exited: %w", err)
	}
	if ctx.Err() == nil {
		return errors.New("cloudflared exited with status 0 while the server was still running")
	}
	return nil
}

// newLineLogger returns a writer that turns cloudflared's output into log
// records, so it follows LOG_FORMAT (one JSON object per line) instead of
// interleaving raw text with structured logs.
func newLineLogger(logger *slog.Logger) io.WriteCloser {
	pr, pw := io.Pipe()
	l := &lineLogger{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line != "" {
				logger.Info("cloudflared", "line", line)
			}
		}
		_, _ = io.Copy(io.Discard, pr)
	}()
	return l
}

type lineLogger struct {
	pw   *io.PipeWriter
	done chan struct{}
}

func (l *lineLogger) Write(p []byte) (int, error) { return l.pw.Write(p) }

// Close flushes the last line and waits until it has been logged.
func (l *lineLogger) Close() error {
	err := l.pw.Close()
	<-l.done
	return err
}

func findOrCreateTunnel(ctx context.Context, client *cloudflare.Client, cfg Config) (string, error) {
	page, err := client.ZeroTrust.Tunnels.Cloudflared.List(ctx, zero_trust.TunnelCloudflaredListParams{
		AccountID: cloudflare.F(cfg.AccountID),
		Name:      cloudflare.F(cfg.TunnelName),
		IsDeleted: cloudflare.F(false),
	})
	if err != nil {
		return "", cfapi.Explain(err, "list tunnels", "Account: Cloudflare Tunnel Edit")
	}

	for _, t := range page.Result {
		if t.Name == cfg.TunnelName && t.DeletedAt.IsZero() {
			cfg.Logger.Info("reusing existing tunnel", "id", t.ID, "name", t.Name)
			return t.ID, nil
		}
	}

	cfg.Logger.Info("creating new tunnel", "name", cfg.TunnelName)
	tunnel, err := client.ZeroTrust.Tunnels.Cloudflared.New(ctx, zero_trust.TunnelCloudflaredNewParams{
		AccountID: cloudflare.F(cfg.AccountID),
		Name:      cloudflare.F(cfg.TunnelName),
		ConfigSrc: cloudflare.F(zero_trust.TunnelCloudflaredNewParamsConfigSrcCloudflare),
	})
	if err != nil {
		return "", cfapi.Explain(err, "create tunnel", "Account: Cloudflare Tunnel Edit")
	}

	return tunnel.ID, nil
}

func configureIngress(ctx context.Context, client *cloudflare.Client, cfg Config, tunnelID string) error {
	_, err := client.ZeroTrust.Tunnels.Cloudflared.Configurations.Update(ctx, tunnelID,
		zero_trust.TunnelCloudflaredConfigurationUpdateParams{
			AccountID: cloudflare.F(cfg.AccountID),
			Config: cloudflare.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
				Ingress: cloudflare.F([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
					{
						Hostname: cloudflare.F(cfg.Hostname),
						Service:  cloudflare.F(cfg.LocalAddr),
					},
					{
						Service: cloudflare.F("http_status:404"),
					},
				}),
			}),
		},
	)
	return err
}

func ensureDNS(ctx context.Context, client *cloudflare.Client, cfg Config, tunnelID string) error {
	const perm = "Zone: DNS Edit"
	target := tunnelID + ".cfargotunnel.com"

	page, err := client.DNS.Records.List(ctx, dns.RecordListParams{
		ZoneID: cloudflare.F(cfg.ZoneID),
		Name:   cloudflare.F(dns.RecordListParamsName{Exact: cloudflare.F(cfg.Hostname)}),
		Type:   cloudflare.F(dns.RecordListParamsTypeCNAME),
	})
	if err != nil {
		return cfapi.Explain(err, "list DNS records", perm)
	}

	for _, r := range page.Result {
		if r.Name == cfg.Hostname {
			if r.Content != target || !r.Proxied {
				cfg.Logger.Info("updating DNS record", "id", r.ID, "old_target", r.Content, "new_target", target)
				_, err := client.DNS.Records.Update(ctx, r.ID, dns.RecordUpdateParams{
					ZoneID: cloudflare.F(cfg.ZoneID),
					Body: dns.CNAMERecordParam{
						Type:    cloudflare.F(dns.CNAMERecordTypeCNAME),
						Name:    cloudflare.F(cfg.Hostname),
						Content: cloudflare.F(target),
						Proxied: cloudflare.F(true),
						TTL:     cloudflare.F(dns.TTL(1)),
					},
				})
				return cfapi.Explain(err, "update DNS record", perm)
			}
			cfg.Logger.Info("DNS record already correct")
			return nil
		}
	}

	_, err = client.DNS.Records.New(ctx, dns.RecordNewParams{
		ZoneID: cloudflare.F(cfg.ZoneID),
		Body: dns.CNAMERecordParam{
			Type:    cloudflare.F(dns.CNAMERecordTypeCNAME),
			Name:    cloudflare.F(cfg.Hostname),
			Content: cloudflare.F(target),
			Proxied: cloudflare.F(true),
			TTL:     cloudflare.F(dns.TTL(1)),
		},
	})
	if err != nil {
		return fmt.Errorf("%w (if CF_HOSTNAME already has an A, AAAA or other non-CNAME record, delete it so the tunnel CNAME can be created)",
			cfapi.Explain(err, "create DNS record for "+cfg.Hostname, perm))
	}
	return nil
}
