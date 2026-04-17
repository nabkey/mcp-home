// Package config defines the CLI configuration parsed by Kong.
package config

import (
	"fmt"
	"sort"

	"github.com/alecthomas/kong"
)

// CLI is the root configuration struct, parsed by Kong.
// Environment variables are resolved via envprefix + env tags.
type CLI struct {
	Cloudflare CloudflareConfig `embed:"" prefix:"cf-"      envprefix:"CF_"`
	Insecure   bool             `env:"INSECURE" default:"false" help:"Skip Cloudflare Access JWT validation (DANGEROUS: exposes server without auth)"`
	Hass       HassConfig       `embed:"" prefix:"hass-"    envprefix:"HASS_"`
	Sonarr     SonarrConfig     `embed:"" prefix:"sonarr-"  envprefix:"SONARR_"`
	Radarr     RadarrConfig     `embed:"" prefix:"radarr-"  envprefix:"RADARR_"`
	Frigate    FrigateConfig    `embed:"" prefix:"frigate-" envprefix:"FRIGATE_"`
	Version    kong.VersionFlag `short:"V" help:"Print version and exit."`
}

// AfterApply is called by Kong after all values are resolved.
// It validates that optional groups are fully configured or not at all.
func (cli *CLI) AfterApply() error {
	for _, v := range []interface{ Validate() error }{
		cli.Hass, cli.Sonarr, cli.Radarr,
	} {
		if err := v.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// CloudflareConfig holds required Cloudflare Tunnel settings.
type CloudflareConfig struct {
	APIToken   string `env:"API_TOKEN"   required:"" help:"Cloudflare API token with Tunnel:Edit and DNS:Edit permissions"`
	AccountID  string `env:"ACCOUNT_ID"  required:"" help:"Cloudflare account ID"`
	ZoneID     string `env:"ZONE_ID"     required:"" help:"Cloudflare DNS zone ID"`
	Hostname   string `env:"HOSTNAME"    required:"" help:"Public hostname (e.g. mcp.example.com)"`
	TunnelName string `env:"TUNNEL_NAME" default:"mcp-server" help:"Tunnel name"`
}

// HassConfig holds optional Home Assistant settings.
type HassConfig struct {
	URL   string `env:"URL"   help:"Home Assistant URL"`
	Token string `env:"TOKEN" help:"Home Assistant long-lived access token"`
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
