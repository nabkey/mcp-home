package cfaccess

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cloudflare/cloudflare-go/v4/zero_trust"
)

// Config holds the discovered Cloudflare Access settings needed for JWT validation.
type Config struct {
	Team string // e.g. "myteam" (without .cloudflareaccess.com)
	AUD  string
}

// Discover fetches the Access team domain and the AUD for the Access application
// protecting the given hostname. The hostname should match the CF_HOSTNAME used
// for the tunnel (e.g. "mcp.example.com").
func Discover(ctx context.Context, apiToken, accountID, hostname string, logger *slog.Logger) (*Config, error) {
	client := cloudflare.NewClient(
		option.WithAPIToken(apiToken),
	)

	// Get team domain from Access organization.
	org, err := client.ZeroTrust.Organizations.List(ctx, zero_trust.OrganizationListParams{
		AccountID: cloudflare.F(accountID),
	})
	if err != nil {
		return nil, fmt.Errorf("fetch Access organization: %w", err)
	}

	authDomain := org.AuthDomain
	if authDomain == "" {
		return nil, fmt.Errorf("Access organization has no auth_domain configured")
	}

	// auth_domain is "myteam.cloudflareaccess.com" — strip suffix for team name.
	team := strings.TrimSuffix(authDomain, ".cloudflareaccess.com")

	logger.Info("discovered Access team", "team", team, "auth_domain", authDomain)

	// Find the Access application protecting our hostname.
	apps := client.ZeroTrust.Access.Applications.ListAutoPaging(ctx, zero_trust.AccessApplicationListParams{
		AccountID: cloudflare.F(accountID),
	})

	for apps.Next() {
		app := apps.Current()
		if app.Domain == hostname {
			logger.Info("discovered Access application",
				"name", app.Name,
				"type", app.Type,
				"aud", app.AUD,
				"domain", app.Domain,
			)
			return &Config{
				Team: team,
				AUD:  app.AUD,
			}, nil
		}
	}
	if err := apps.Err(); err != nil {
		return nil, fmt.Errorf("iterate Access applications: %w", err)
	}

	return nil, fmt.Errorf("no Access application found for hostname %q — create one in the Zero Trust dashboard under Access > Applications", hostname)
}
