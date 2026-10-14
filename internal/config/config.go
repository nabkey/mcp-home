// Package config defines the CLI configuration parsed by Kong.
package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/alecthomas/kong"
)

// CLI is the root configuration struct, parsed by Kong.
// Environment variables are resolved via envprefix + env tags.
//
// Kong calls Validate on every embedded group during Parse (it walks embed:""
// fields as of v1.16.0), so an optional group enforces its own all-or-nothing
// rule just by defining the method. There is deliberately no AfterApply hook
// here dispatching to them by hand.
//
// At least one front door must be configured: the Cloudflare Tunnel
// (CF_*) or the tailnet listener (TS_*). Both may run at once. The root
// Validate enforces that, since no single group can.
type CLI struct {
	Cloudflare CloudflareConfig `embed:"" prefix:"cf-"      envprefix:"CF_"`
	Insecure   bool             `env:"INSECURE" default:"false" help:"Skip Cloudflare Access JWT validation on the tunnel listener (DANGEROUS: exposes server without auth). Has no effect on the tailnet listener."`
	LogLevel   string           `env:"LOG_LEVEL" default:"info" enum:"debug,info,warn,error" help:"Log level (debug, info, warn, error)"`
	LogFormat  string           `env:"LOG_FORMAT" default:"text" enum:"text,json" help:"Log output format (text, json)"`
	Hass       HassConfig       `embed:"" prefix:"hass-"    envprefix:"HASS_"`
	Sonarr     SonarrConfig     `embed:"" prefix:"sonarr-"  envprefix:"SONARR_"`
	Radarr     RadarrConfig     `embed:"" prefix:"radarr-"  envprefix:"RADARR_"`
	Frigate    FrigateConfig    `embed:"" prefix:"frigate-" envprefix:"FRIGATE_"`
	ESPHome    ESPHomeConfig    `embed:"" prefix:"esphome-" envprefix:"ESPHOME_"`
	Tailscale  TailscaleConfig  `embed:"" prefix:"ts-"      envprefix:"TS_"`
	Version    kong.VersionFlag `short:"V" help:"Print version and exit."`
}

// Validate is called by Kong on the root struct. A server with no front door
// would start, register its tools, and be reachable by nobody, so that is a
// configuration error rather than a silent no-op.
func (c CLI) Validate() error {
	if !c.Cloudflare.Enabled() && !c.Tailscale.Enabled() {
		return fmt.Errorf("no front door configured: set CF_API_TOKEN, CF_ACCOUNT_ID, CF_ZONE_ID and CF_HOSTNAME for the Cloudflare Tunnel, or TS_AUTHKEY for the tailnet listener (or both)")
	}
	return nil
}

// TailscaleConfig holds the optional embedded-Tailscale (tsnet) listener.
// When AuthKey is set the server joins the tailnet as Hostname and serves
// /mcp over HTTPS there, authenticating callers by WhoIs identity instead of
// Cloudflare Access. It can run alongside the Cloudflare Tunnel or on its own.
type TailscaleConfig struct {
	AuthKey       string   `env:"AUTHKEY" help:"Tailscale auth key (tagged, reusable). Enables the tsnet listener."`
	Hostname      string   `env:"HOSTNAME" default:"mcp-home" help:"tsnet node hostname"`
	StateDir      string   `env:"STATE_DIR" default:"/home/nonroot/tsstate" help:"tsnet state directory (persist it)"`
	AllowedLogins []string `env:"ALLOWED_LOGINS" help:"Tailscale logins allowed to call /mcp"`
	AllowedTags   []string `env:"ALLOWED_TAGS" default:"tag:voice-agent" help:"Tailscale node tags allowed to call /mcp"`
}

// Enabled reports whether the tsnet listener should run.
func (t TailscaleConfig) Enabled() bool { return t.AuthKey != "" }

// Validate is called by Kong. Entries are judged after trimming whitespace,
// the same way the tailnet middleware matches them, so "," or " " does not
// count as an allowlist, and a tag that cannot match any node (missing the
// "tag:" prefix, or put in the login list) is reported instead of silently
// refusing the device it was meant for.
func (t TailscaleConfig) Validate() error {
	if !t.Enabled() {
		return nil
	}
	logins, tags := trimList(t.AllowedLogins), trimList(t.AllowedTags)
	if len(logins) == 0 && len(tags) == 0 {
		return fmt.Errorf("TS_ALLOWED_LOGINS or TS_ALLOWED_TAGS must list at least one login or tag when TS_AUTHKEY is set")
	}
	for _, tag := range tags {
		if !strings.HasPrefix(tag, "tag:") || len(tag) == len("tag:") || strings.ContainsAny(tag, " \t") {
			return fmt.Errorf("TS_ALLOWED_TAGS entry %q is not a Tailscale tag (expected the form tag:name)", tag)
		}
	}
	for _, login := range logins {
		if strings.HasPrefix(login, "tag:") {
			return fmt.Errorf("TS_ALLOWED_LOGINS entry %q is a tag; move it to TS_ALLOWED_TAGS", login)
		}
		if strings.ContainsAny(login, " \t") {
			return fmt.Errorf("TS_ALLOWED_LOGINS entry %q contains whitespace", login)
		}
	}
	return nil
}

// trimList trims each entry and drops empty ones.
func trimList(list []string) []string {
	var out []string
	for _, v := range list {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// CloudflareConfig holds the optional Cloudflare Tunnel + Access front door.
// The four credentials are all-or-nothing; TunnelName only matters when the
// group is enabled.
type CloudflareConfig struct {
	APIToken   string `env:"API_TOKEN"   help:"Cloudflare API token with Tunnel:Edit and DNS:Edit permissions. Enables the Cloudflare Tunnel."`
	AccountID  string `env:"ACCOUNT_ID"  help:"Cloudflare account ID"`
	ZoneID     string `env:"ZONE_ID"     help:"Cloudflare DNS zone ID"`
	Hostname   string `env:"HOSTNAME"    help:"Public hostname (e.g. mcp.example.com)"`
	TunnelName string `env:"TUNNEL_NAME" default:"mcp-server" help:"Tunnel name"`
}

// Enabled returns true if the Cloudflare Tunnel is fully configured.
func (c CloudflareConfig) Enabled() bool {
	return c.APIToken != "" && c.AccountID != "" && c.ZoneID != "" && c.Hostname != ""
}

// Validate returns an error if the Cloudflare group is partially configured.
// The hostname must be a bare DNS name: it is matched against the Access
// application's domain and written into the tunnel ingress and the DNS
// record, and a scheme, path, port or stray whitespace makes the first fail
// with "no Access application found" and the others route nowhere.
func (c CloudflareConfig) Validate() error {
	if err := validateAllOrNothing("Cloudflare", map[string]string{
		"CF_API_TOKEN":  c.APIToken,
		"CF_ACCOUNT_ID": c.AccountID,
		"CF_ZONE_ID":    c.ZoneID,
		"CF_HOSTNAME":   c.Hostname,
	}); err != nil {
		return err
	}
	if c.Hostname != "" {
		if strings.ContainsAny(c.Hostname, ":/ \t") || !strings.Contains(c.Hostname, ".") || strings.HasSuffix(c.Hostname, ".") {
			return fmt.Errorf("CF_HOSTNAME %q must be a bare hostname such as mcp.example.com (no scheme, path, port or trailing dot)", c.Hostname)
		}
	}
	return nil
}

// HassConfig holds optional Home Assistant settings.
type HassConfig struct {
	URL   string `env:"URL"   help:"Home Assistant URL"`
	Token string `env:"TOKEN" help:"Home Assistant long-lived access token"`
	// DenyServices guards against unwanted assistant actions. It applies to
	// direct service calls and ad-hoc execute_script sequences, not to stored
	// automations/scripts.
	DenyServices []string `env:"DENY_SERVICES" help:"Comma-separated domain.service patterns to refuse (e.g. lock.unlock,alarm_control_panel.*). '*' matches a whole domain or service."`
}

// Enabled returns true if Home Assistant is fully configured.
func (c HassConfig) Enabled() bool { return c.URL != "" && c.Token != "" }

// Validate returns an error if Home Assistant is partially configured.
func (c HassConfig) Validate() error {
	return validateAllOrNothing("Home Assistant", map[string]string{
		"HASS_URL":   c.URL,
		"HASS_TOKEN": c.Token,
	})
}

// SonarrConfig holds optional Sonarr settings.
type SonarrConfig struct {
	URL    string `env:"URL"     help:"Sonarr URL"`
	APIKey string `env:"API_KEY" help:"Sonarr API key"`
}

// Enabled returns true if Sonarr is fully configured.
func (c SonarrConfig) Enabled() bool { return c.URL != "" && c.APIKey != "" }

// Validate returns an error if Sonarr is partially configured.
func (c SonarrConfig) Validate() error {
	return validateAllOrNothing("Sonarr", map[string]string{
		"SONARR_URL":     c.URL,
		"SONARR_API_KEY": c.APIKey,
	})
}

// RadarrConfig holds optional Radarr settings.
type RadarrConfig struct {
	URL    string `env:"URL"     help:"Radarr URL"`
	APIKey string `env:"API_KEY" help:"Radarr API key"`
}

// Enabled returns true if Radarr is fully configured.
func (c RadarrConfig) Enabled() bool { return c.URL != "" && c.APIKey != "" }

// Validate returns an error if Radarr is partially configured.
func (c RadarrConfig) Validate() error {
	return validateAllOrNothing("Radarr", map[string]string{
		"RADARR_URL":     c.URL,
		"RADARR_API_KEY": c.APIKey,
	})
}

// FrigateConfig holds optional Frigate NVR settings.
type FrigateConfig struct {
	URL string `env:"URL" help:"Frigate NVR URL"`
}

// Enabled returns true if Frigate is configured.
func (c FrigateConfig) Enabled() bool { return c.URL != "" }

// ESPHomeConfig holds optional ESPHome dashboard settings.
type ESPHomeConfig struct {
	URL      string `env:"URL"      help:"ESPHome dashboard URL (e.g. http://homeassistant.local:6052)"`
	Password string `env:"PASSWORD" help:"ESPHome dashboard password (only if dashboard auth is enabled)"`
}

// Enabled returns true if the ESPHome dashboard URL is configured.
func (c ESPHomeConfig) Enabled() bool { return c.URL != "" }

// validateAllOrNothing checks that either all fields are set or none are.
func validateAllOrNothing(name string, fields map[string]string) error {
	var set, unset []string
	for envName, val := range fields {
		if val != "" {
			set = append(set, envName)
		} else {
			unset = append(unset, envName)
		}
	}
	if len(set) > 0 && len(unset) > 0 {
		sort.Strings(set)
		sort.Strings(unset)
		return fmt.Errorf("%s is partially configured: %v set but %v missing", name, set, unset)
	}
	return nil
}
