package cfaccess

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeCloudflare serves the two Access endpoints Discover calls and records the
// query the application lookup was made with.
func fakeCloudflare(t *testing.T, authDomain string, apps []map[string]any) *url.Values {
	t.Helper()
	var appQuery url.Values

	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/acct-1/access/organizations", func(w http.ResponseWriter, _ *http.Request) {
		writeCFResult(t, w, map[string]any{"auth_domain": authDomain})
	})
	mux.HandleFunc("/accounts/acct-1/access/apps", func(w http.ResponseWriter, r *http.Request) {
		appQuery = r.URL.Query()
		writeCFResult(t, w, apps)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("CLOUDFLARE_BASE_URL", srv.URL)
	return &appQuery
}

func writeCFResult(t *testing.T, w http.ResponseWriter, result any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"success": true, "errors": []any{}, "messages": []any{}, "result": result,
	}); err != nil {
		t.Errorf("encode: %v", err)
	}
}

func discoverLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDiscoverFiltersApplicationsByDomain(t *testing.T) {
	query := fakeCloudflare(t, "myteam.cloudflareaccess.com", []map[string]any{
		{"name": "MCP", "type": "self_hosted", "aud": "aud-123", "domain": "mcp.example.com"},
	})

	cfg, err := Discover(context.Background(), "token", "acct-1", "mcp.example.com", discoverLogger())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if cfg.Team != "myteam" {
		t.Errorf("Team = %q, want myteam", cfg.Team)
	}
	if cfg.AUD != "aud-123" {
		t.Errorf("AUD = %q, want aud-123", cfg.AUD)
	}
	// The domain filter is what keeps this to a single page on a large account.
	if got := query.Get("domain"); got != "mcp.example.com" {
		t.Errorf("domain query = %q", got)
	}
	if got := query.Get("exact"); got != "true" {
		t.Errorf("exact query = %q, want true", got)
	}
}

func TestDiscoverIgnoresApplicationsForOtherDomains(t *testing.T) {
	fakeCloudflare(t, "myteam.cloudflareaccess.com", []map[string]any{
		{"name": "Other", "type": "self_hosted", "aud": "aud-other", "domain": "other.example.com"},
	})

	_, err := Discover(context.Background(), "token", "acct-1", "mcp.example.com", discoverLogger())
	if err == nil {
		t.Fatal("Discover succeeded on a non-matching application")
	}
	if !strings.Contains(err.Error(), "no Access application found") {
		t.Errorf("error = %v", err)
	}
}

func TestDiscoverRequiresAuthDomain(t *testing.T) {
	fakeCloudflare(t, "", nil)

	_, err := Discover(context.Background(), "token", "acct-1", "mcp.example.com", discoverLogger())
	if err == nil || !strings.Contains(err.Error(), "auth_domain") {
		t.Fatalf("err = %v, want an auth_domain complaint", err)
	}
}
