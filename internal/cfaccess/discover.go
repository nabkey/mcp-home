package cfaccess

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/cloudflare/cloudflare-go/v4"
	"github.com/cloudflare/cloudflare-go/v4/option"
	"github.com/cloudflare/cloudflare-go/v4/zero_trust"
	"github.com/nabkey/mcp-home/internal/cfapi"
)

// Config holds the discovered Cloudflare Access settings needed for JWT validation.
type Config struct {
	Team string // e.g. "myteam" (without .cloudflareaccess.com)
	AUD  string
}

// Discover fetches the Access team domain and the AUD for the Access application
// protecting the given hostname. The hostname should match the CF_HOSTNAME used
// for the tunnel (e.g. "mcp.example.com").
//
// opts are extra cloudflare-go request options; tests use them to point the
// client at a fake API.
//
// An application scoped to the MCP path (domain "host/mcp") is preferred over
// one for the whole host, because that is the one Cloudflare's edge enforces
// for requests to /mcp, and so the one whose AUD the injected JWT carries.
func Discover(ctx context.Context, apiToken, accountID, hostname string, logger *slog.Logger, opts ...option.RequestOption) (*Config, error) {
	client := cloudflare.NewClient(append([]option.RequestOption{option.WithAPIToken(apiToken)}, opts...)...)

	// Get team domain from Access organization.
	org, err := client.ZeroTrust.Organizations.List(ctx, zero_trust.OrganizationListParams{
		AccountID: cloudflare.F(accountID),
	})
	if err != nil {
		return nil, cfapi.Explain(err, "fetch Access organization", "Access: Organizations, Identity Providers, and Groups Read")
	}

	authDomain := org.AuthDomain
	if authDomain == "" {
		return nil, fmt.Errorf("the Zero Trust organization for CF_ACCOUNT_ID %s has no auth_domain: finish Zero Trust onboarding (Settings > Custom Pages > team domain) first", accountID)
	}
	if !strings.HasSuffix(authDomain, ".cloudflareaccess.com") {
		return nil, fmt.Errorf("the Access auth_domain %q is not a *.cloudflareaccess.com team domain; this server only validates tokens from team domains", authDomain)
	}

	// auth_domain is "myteam.cloudflareaccess.com" — strip suffix for team name.
	team := strings.TrimSuffix(authDomain, ".cloudflareaccess.com")

	logger.Info("discovered Access team", "team", team, "auth_domain", authDomain)

	// Find the Access application protecting our hostname.
	apps := client.ZeroTrust.Access.Applications.ListAutoPaging(ctx, zero_trust.AccessApplicationListParams{
		AccountID: cloudflare.F(accountID),
	})

	host := strings.ToLower(hostname)
	var hostApp, pathApp *zero_trust.AccessApplicationListResponse
	var seen []string
	for apps.Next() {
		app := apps.Current()
		domain := strings.ToLower(strings.TrimSuffix(app.Domain, "/"))
		switch domain {
		case host:
			if hostApp == nil {
				hostApp = &app
			}
		case host + "/mcp", host + "/mcp*", host + "/mcp/*":
			if pathApp == nil {
				pathApp = &app
			}
		}
		if strings.HasPrefix(domain, host) {
			seen = append(seen, app.Domain)
		}
	}
	if err := apps.Err(); err != nil {
		return nil, cfapi.Explain(err, "list Access applications", "Access: Apps and Policies Read")
	}

	app := pathApp
	if app == nil {
		app = hostApp
	}
	if app == nil {
		hint := ""
		if len(seen) > 0 {
			hint = fmt.Sprintf(" (applications on related domains: %s; the application must cover exactly %s or %s/mcp)", strings.Join(seen, ", "), hostname, hostname)
		}
		return nil, fmt.Errorf("no Access application found for CF_HOSTNAME %q%s: create a self-hosted application for it in the Zero Trust dashboard under Access > Applications", hostname, hint)
	}
	if app.AUD == "" {
		return nil, fmt.Errorf("the Access application %q for %s has no AUD tag", app.Name, app.Domain)
	}
	logger.Info("discovered Access application",
		"name", app.Name,
		"type", app.Type,
		"aud", app.AUD,
		"domain", app.Domain,
	)
	return &Config{Team: team, AUD: app.AUD}, nil
}
