package config

import (
	"os"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

func TestHassConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     HassConfig
		wantErr bool
	}{
		{"empty", HassConfig{}, false},
		{"full", HassConfig{URL: "http://ha:8123", Token: "tok"}, false},
		{"url only", HassConfig{URL: "http://ha:8123"}, true},
		{"token only", HassConfig{Token: "tok"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestPartialConfigErrorNamesVariables(t *testing.T) {
	err := SonarrConfig{URL: "http://sonarr:8989"}.Validate()
	if err == nil {
		t.Fatal("expected error for partial Sonarr config")
	}
	msg := err.Error()
	if !strings.Contains(msg, "SONARR_URL") || !strings.Contains(msg, "SONARR_API_KEY") {
		t.Errorf("error %q should name both the set and missing variables", msg)
	}
}

func TestEnabled(t *testing.T) {
	if (HassConfig{}).Enabled() {
		t.Error("empty HassConfig should not be enabled")
	}
	if (HassConfig{URL: "u"}).Enabled() {
		t.Error("partial HassConfig should not be enabled")
	}
	if !(HassConfig{URL: "u", Token: "t"}).Enabled() {
		t.Error("full HassConfig should be enabled")
	}
	if (FrigateConfig{}).Enabled() {
		t.Error("empty FrigateConfig should not be enabled")
	}
	if !(FrigateConfig{URL: "u"}).Enabled() {
		t.Error("FrigateConfig with URL should be enabled")
	}
}

// cliEnv is every variable the CLI struct reads. Kong resolves from the real
// process environment, so a test that only sets what it cares about would
// otherwise inherit the developer's own exports — and .env.example tells them
// to export exactly these. Clearing the lot first makes each test's
// environment the whole environment.
var cliEnv = []string{
	"CF_API_TOKEN", "CF_ACCOUNT_ID", "CF_ZONE_ID", "CF_HOSTNAME", "CF_TUNNEL_NAME",
	"TS_AUTHKEY", "TS_HOSTNAME", "TS_STATE_DIR", "TS_ALLOWED_TAGS", "TS_ALLOWED_LOGINS",
	"INSECURE", "LOG_LEVEL",
	"HASS_URL", "HASS_TOKEN", "HASS_DENY_SERVICES",
	"SONARR_URL", "SONARR_API_KEY",
	"RADARR_URL", "RADARR_API_KEY",
	"FRIGATE_URL",
	"ESPHOME_URL", "ESPHOME_PASSWORD",
}

// clearEnv unsets every CLI variable for the duration of the test. t.Setenv is
// called first purely so the testing package records the original value and
// restores it afterwards; the Unsetenv is what the parse actually sees, so
// defaults still apply (an empty LOG_LEVEL would fail its enum tag).
func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range cliEnv {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
	}
}

// parse runs Kong over the CLI struct with only the environment the test set,
// mirroring how cmd/mcp-server builds it.
func parse(t *testing.T) error {
	t.Helper()
	var cli CLI
	parser, err := kong.New(&cli,
		kong.Name("mcp-server"),
		kong.Vars{"version": "test"},
		kong.Exit(func(int) {}),
	)
	if err != nil {
		t.Fatalf("kong.New: %v", err)
	}
	_, err = parser.Parse(nil)
	return err
}

// setCloudflare sets the four Cloudflare variables so that parsing fails only
// on whatever the test is actually exercising.
func setCloudflare(t *testing.T) {
	t.Helper()
	clearEnv(t)
	t.Setenv("CF_API_TOKEN", "token")
	t.Setenv("CF_ACCOUNT_ID", "account")
	t.Setenv("CF_ZONE_ID", "zone")
	t.Setenv("CF_HOSTNAME", "mcp.example.com")
}

// Kong invokes Validate on embedded groups itself as of v1.16.0, so a
// partially configured group must fail the parse without any hook of ours.
func TestParseRejectsPartialGroup(t *testing.T) {
	setCloudflare(t)
	t.Setenv("RADARR_API_KEY", "key-without-url")

	err := parse(t)
	if err == nil {
		t.Fatal("expected parse to reject partially configured Radarr")
	}
	if !strings.Contains(err.Error(), "RADARR_URL") {
		t.Errorf("error should name the missing variable, got: %v", err)
	}
}

func TestParseAcceptsCompleteGroups(t *testing.T) {
	setCloudflare(t)
	t.Setenv("HASS_URL", "http://hass:8123")
	t.Setenv("HASS_TOKEN", "token")
	t.Setenv("SONARR_URL", "http://sonarr:8989")
	t.Setenv("SONARR_API_KEY", "key")

	if err := parse(t); err != nil {
		t.Errorf("parse() = %v, want nil", err)
	}
}

func TestCloudflareConfigValidate(t *testing.T) {
	full := CloudflareConfig{APIToken: "t", AccountID: "a", ZoneID: "z", Hostname: "h"}
	if err := full.Validate(); err != nil {
		t.Errorf("full config: %v", err)
	}
	if !full.Enabled() {
		t.Error("full config should be enabled")
	}
	if err := (CloudflareConfig{}).Validate(); err != nil {
		t.Errorf("empty config: %v", err)
	}
	if (CloudflareConfig{}).Enabled() {
		t.Error("empty config should not be enabled")
	}
	err := CloudflareConfig{APIToken: "t", Hostname: "h"}.Validate()
	if err == nil {
		t.Fatal("expected error for partial Cloudflare config")
	}
	if !strings.Contains(err.Error(), "CF_ACCOUNT_ID") || !strings.Contains(err.Error(), "CF_ZONE_ID") {
		t.Errorf("error should name the missing variables, got: %v", err)
	}
	if (CloudflareConfig{APIToken: "t", Hostname: "h"}).Enabled() {
		t.Error("partial config should not be enabled")
	}
}

// With no CF_* and no TS_AUTHKEY the server would have no listener anyone
// could reach, so the root Validate must fail the parse.
func TestParseRequiresAFrontDoor(t *testing.T) {
	clearEnv(t)
	err := parse(t)
	if err == nil {
		t.Fatal("expected parse to fail with no front door configured")
	}
	if !strings.Contains(err.Error(), "CF_API_TOKEN") || !strings.Contains(err.Error(), "TS_AUTHKEY") {
		t.Errorf("error should point at both options, got: %v", err)
	}
}

// --insecure only relaxes the Cloudflare listener; on its own it configures
// nothing, so it must not satisfy the front-door requirement.
func TestParseInsecureAloneIsNotAFrontDoor(t *testing.T) {
	clearEnv(t)
	t.Setenv("INSECURE", "true")
	if err := parse(t); err == nil {
		t.Fatal("expected parse to fail: --insecure without any listener")
	}
}

func TestParseAcceptsTailnetOnly(t *testing.T) {
	clearEnv(t)
	t.Setenv("TS_AUTHKEY", "tskey-auth-test")
	if err := parse(t); err != nil {
		t.Errorf("parse() = %v, want nil (tailnet with default TS_ALLOWED_TAGS)", err)
	}
}

func TestParseAcceptsCloudflareOnly(t *testing.T) {
	setCloudflare(t)
	if err := parse(t); err != nil {
		t.Errorf("parse() = %v, want nil", err)
	}
}

func TestParseAcceptsBothFrontDoors(t *testing.T) {
	setCloudflare(t)
	t.Setenv("TS_AUTHKEY", "tskey-auth-test")
	if err := parse(t); err != nil {
		t.Errorf("parse() = %v, want nil", err)
	}
}

func TestParseRejectsPartialCloudflare(t *testing.T) {
	clearEnv(t)
	t.Setenv("TS_AUTHKEY", "tskey-auth-test")
	t.Setenv("CF_API_TOKEN", "token")
	err := parse(t)
	if err == nil {
		t.Fatal("expected parse to reject partially configured Cloudflare")
	}
	if !strings.Contains(err.Error(), "CF_HOSTNAME") {
		t.Errorf("error should name a missing variable, got: %v", err)
	}
}

func TestParseRejectsTailnetWithoutAllowlist(t *testing.T) {
	clearEnv(t)
	t.Setenv("TS_AUTHKEY", "tskey-auth-test")
	t.Setenv("TS_ALLOWED_TAGS", "")
	if err := parse(t); err == nil {
		t.Fatal("expected parse to reject TS_AUTHKEY with an empty allowlist")
	}
}
