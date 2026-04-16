// Package tunnel manages Cloudflare Tunnel lifecycle for the MCP server.
// It creates/reuses a named tunnel, configures ingress, sets up DNS,
// and runs cloudflared as a subprocess.
package tunnel

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/dns"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cloudflare/cloudflare-go/v4/zero_trust"
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
}

// Tunnel represents a configured Cloudflare Tunnel ready to run.
type Tunnel struct {
	ID     string
	token  string
	config Config
	cmd    *exec.Cmd
}

// Setup creates or reuses a Cloudflare Tunnel, configures ingress, and ensures
// a DNS CNAME record exists. Returns a Tunnel ready to Run.
func Setup(ctx context.Context, cfg Config) (*Tunnel, error) {
	client := cloudflare.NewClient(
		option.WithAPIToken(cfg.APIToken),
	)

	tunnelID, err := findOrCreateTunnel(ctx, client, cfg)
	if err != nil {
		return nil, fmt.Errorf("tunnel setup: %w", err)
	}

	cfg.Logger.Info("tunnel ready", "id", tunnelID, "name", cfg.TunnelName)

	if err := configureIngress(ctx, client, cfg, tunnelID); err != nil {
		return nil, fmt.Errorf("tunnel ingress: %w", err)
	}

	cfg.Logger.Info("ingress configured", "hostname", cfg.Hostname, "service", cfg.LocalAddr)

	if err := ensureDNS(ctx, client, cfg, tunnelID); err != nil {
		return nil, fmt.Errorf("tunnel DNS: %w", err)
	}

	cfg.Logger.Info("DNS configured", "hostname", cfg.Hostname, "target", tunnelID+".cfargotunnel.com")

	token, err := client.ZeroTrust.Tunnels.Cloudflared.Token.Get(ctx, tunnelID, zero_trust.TunnelCloudflaredTokenGetParams{
		AccountID: cloudflare.F(cfg.AccountID),
	})
	if err != nil {
		return nil, fmt.Errorf("get tunnel token: %w", err)
	}

	return &Tunnel{
		ID:     tunnelID,
		token:  *token,
		config: cfg,
	}, nil
}

// Run starts cloudflared as a subprocess. Blocks until the context is cancelled
// or cloudflared exits. The cloudflared binary must be on PATH.
func (t *Tunnel) Run(ctx context.Context) error {
	cloudflaredPath, err := ensureCloudflared(ctx, t.config.Logger)
	if err != nil {
		return fmt.Errorf("cloudflared: %w", err)
	}

	t.config.Logger.Info("starting cloudflared", "path", cloudflaredPath, "tunnel", t.config.TunnelName)

	t.cmd = exec.CommandContext(ctx, cloudflaredPath, "tunnel", "--no-autoupdate", "run")
	t.cmd.Env = append(os.Environ(), "TUNNEL_TOKEN="+t.token)
	t.cmd.Stdout = os.Stderr
	t.cmd.Stderr = os.Stderr

	if err := t.cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil // context cancelled, clean shutdown
		}
		return fmt.Errorf("cloudflared exited: %w", err)
	}

	return nil
}

func findOrCreateTunnel(ctx context.Context, client *cloudflare.Client, cfg Config) (string, error) {
	page, err := client.ZeroTrust.Tunnels.Cloudflared.List(ctx, zero_trust.TunnelCloudflaredListParams{
		AccountID: cloudflare.F(cfg.AccountID),
		Name:      cloudflare.F(cfg.TunnelName),
		IsDeleted: cloudflare.F(false),
	})
	if err != nil {
		return "", fmt.Errorf("list tunnels: %w", err)
	}

	for _, t := range page.Result {
		if t.Name == cfg.TunnelName {
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
		return "", fmt.Errorf("create tunnel: %w", err)
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
	target := tunnelID + ".cfargotunnel.com"

	page, err := client.DNS.Records.List(ctx, dns.RecordListParams{
		ZoneID: cloudflare.F(cfg.ZoneID),
		Name:   cloudflare.F(dns.RecordListParamsName{Exact: cloudflare.F(cfg.Hostname)}),
		Type:   cloudflare.F(dns.RecordListParamsTypeCNAME),
	})
	if err != nil {
		return fmt.Errorf("list DNS records: %w", err)
	}

	for _, r := range page.Result {
		if r.Name == cfg.Hostname {
			if r.Content != target {
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
				return err
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
	return err
}
