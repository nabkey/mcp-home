package config

import (
	"strings"
	"testing"
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

func TestAfterApply(t *testing.T) {
	cli := CLI{Radarr: RadarrConfig{APIKey: "key-without-url"}}
	if err := cli.AfterApply(); err == nil {
		t.Error("expected AfterApply to reject partially configured Radarr")
	}

	cli = CLI{
		Hass:   HassConfig{URL: "u", Token: "t"},
		Sonarr: SonarrConfig{URL: "u", APIKey: "k"},
	}
	if err := cli.AfterApply(); err != nil {
		t.Errorf("AfterApply() = %v, want nil", err)
	}
}
