package cfaccess

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudflare/cloudflare-go/v4/option"
)

// fakeCF serves the two Cloudflare API calls Discover makes.
func fakeCF(t *testing.T, authDomain string, apps []map[string]any, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false, "errors": []map[string]any{{"code": 10000, "message": "Authentication error"}}, "messages": []any{}, "result": nil,
			})
			return
		}
		var result any
		switch {
		case strings.HasSuffix(r.URL.Path, "/access/organizations"):
			result = map[string]any{"auth_domain": authDomain}
		case strings.HasSuffix(r.URL.Path, "/access/apps"):
			result = apps
		default:
			t.Errorf("unexpected API call %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "messages": []any{}, "result": result})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func discover(t *testing.T, base string) (*Config, error) {
	t.Helper()
	return Discover(context.Background(), "token", "acct", "mcp.example.com", slog.New(slog.DiscardHandler),
		option.WithBaseURL(base+"/"), option.WithMaxRetries(0))
}

func TestDiscoverFindsApplication(t *testing.T) {
	base := fakeCF(t, "myteam.cloudflareaccess.com", []map[string]any{
		{"name": "other", "domain": "other.example.com", "aud": "aud-other", "type": "self_hosted"},
		{"name": "mcp", "domain": "MCP.example.com", "aud": "aud-host", "type": "self_hosted"},
	}, http.StatusOK)
	cfg, err := discover(t, base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Team != "myteam" || cfg.AUD != "aud-host" {
		t.Errorf("cfg = %+v", cfg)
	}
}

// When both a host-wide and a /mcp-scoped application exist, the edge
// enforces the path one for /mcp, so its AUD is what tokens carry.
func TestDiscoverPrefersPathApplication(t *testing.T) {
	base := fakeCF(t, "myteam.cloudflareaccess.com", []map[string]any{
		{"name": "site", "domain": "mcp.example.com", "aud": "aud-host"},
		{"name": "mcp", "domain": "mcp.example.com/mcp", "aud": "aud-path"},
	}, http.StatusOK)
	cfg, err := discover(t, base)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AUD != "aud-path" {
		t.Errorf("AUD = %q, want the /mcp application's", cfg.AUD)
	}
}

// A lookalike domain must not be mistaken for ours.
func TestDiscoverIgnoresLookalikes(t *testing.T) {
	base := fakeCF(t, "myteam.cloudflareaccess.com", []map[string]any{
		{"name": "evil", "domain": "mcp.example.com.evil.net", "aud": "aud-evil"},
		{"name": "sub", "domain": "x.mcp.example.com", "aud": "aud-sub"},
		{"name": "other path", "domain": "mcp.example.com/admin", "aud": "aud-admin"},
	}, http.StatusOK)
	_, err := discover(t, base)
	if err == nil {
		t.Fatal("matched a lookalike application")
	}
	if !strings.Contains(err.Error(), "CF_HOSTNAME") || !strings.Contains(err.Error(), "Access > Applications") {
		t.Errorf("error not actionable: %v", err)
	}
}

func TestDiscoverFailuresAreActionable(t *testing.T) {
	cases := []struct {
		name   string
		domain string
		apps   []map[string]any
		status int
		want   string
	}{
		{"token lacks permission", "", nil, http.StatusForbidden, "lacks permission"},
		{"token rejected", "", nil, http.StatusUnauthorized, "CF_API_TOKEN"},
		{"no team domain", "", nil, http.StatusOK, "auth_domain"},
		{"foreign team domain", "login.example.com", nil, http.StatusOK, "cloudflareaccess.com"},
		{"app without AUD", "t.cloudflareaccess.com", []map[string]any{{"name": "x", "domain": "mcp.example.com"}}, http.StatusOK, "AUD"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := discover(t, fakeCF(t, c.domain, c.apps, c.status))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}
