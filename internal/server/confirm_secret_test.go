package server

import (
	"testing"

	"github.com/nabkey/mcp-home/internal/config"
)

// With no secret configured the confirm key must stay random: a key derived
// from empty strings would be the same for everyone, and anyone could mint a
// valid "yes".
func TestConfirmSecretNeverPublic(t *testing.T) {
	if s := confirmSecret(config.CLI{}); s != "" {
		t.Errorf("confirmSecret(empty) = %q, want empty", s)
	}
	var cfg config.CLI
	cfg.Hass.Token = "tok"
	if confirmSecret(cfg) == "" {
		t.Error("a configured secret was ignored")
	}
}
