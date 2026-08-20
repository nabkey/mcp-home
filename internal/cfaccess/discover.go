package cfaccess

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cloudflare/cloudflare-go/v5"
	"github.com/cloudflare/cloudflare-go/v5/option"
	"github.com/cloudflare/cloudflare-go/v5/zero_trust"
)

// accessAppPageSize bounds the Access application lookup. The domain filter is
// exact, so a correctly configured account returns exactly one match; asking
// for a handful leaves room to notice a duplicate configuration.
const accessAppPageSize = 10

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
		return nil, fmt.Errorf("access organization has no auth_domain configured")
	}

	// auth_domain is "myteam.cloudflareaccess.com" — strip suffix for team name.
	team := strings.TrimSuffix(authDomain, ".cloudflareaccess.com")

	logger.Info("discovered Access team", "team", team, "auth_domain", authDomain)

	// Find the Access application protecting our hostname. Domain+Exact filters
	// server-side, so an account with hundreds of applications costs one page
	// instead of paging through all of them.
	apps, err := client.ZeroTrust.Access.Applications.List(ctx, zero_trust.AccessApplicationListParams{
		AccountID: cloudflare.F(accountID),
		Domain:    cloudflare.F(hostname),
		Exact:     cloudflare.F(true),
		PerPage:   cloudflare.F(int64(accessAppPageSize)),
	})
	if err != nil {
		return nil, fmt.Errorf("list Access applications: %w", err)
	}

	for _, app := range apps.Result {
		// Exact=true is matched by the API, but re-check locally so a change in
		// filter semantics can never point us at the wrong application's AUD.
		if app.Domain != hostname {
			continue
		}
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

	return nil, fmt.Errorf("no Access application found for hostname %q — create one in the Zero Trust dashboard under Access > Applications", hostname)
}
