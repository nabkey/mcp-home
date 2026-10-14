package tsauth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestAllowed(t *testing.T) {
	user := Identity{Login: "chris@example.com"}
	tagged := Identity{IsTagged: true, Login: "tagged-devices", Tags: []string{"tag:voice-agent"}}
	noLogin := Identity{Node: "orphan"}

	cases := []struct {
		name   string
		id     Identity
		logins []string
		tags   []string
		want   bool
	}{
		// An empty allowlist used to mean "whole tailnet". It now admits
		// nobody, so a config that slipped past validation fails closed.
		{"empty allowlist admits nobody", user, nil, nil, false},
		{"empty allowlist admits no tagged node", tagged, nil, nil, false},
		{"login match", user, []string{"chris@example.com"}, nil, true},
		{"login mismatch", user, []string{"other@example.com"}, nil, false},
		{"login is case-sensitive", user, []string{"Chris@Example.com"}, nil, false},
		{"login prefix does not match", user, []string{"chris@example.co"}, nil, false},
		{"tag match", tagged, nil, []string{"tag:voice-agent"}, true},
		{"tag that a node's tag merely starts with", Identity{IsTagged: true, Tags: []string{"tag:voice-agent-evil"}}, nil, []string{"tag:voice-agent"}, false},
		{"tag that is a prefix of the allowed one", Identity{IsTagged: true, Tags: []string{"tag:voice"}}, nil, []string{"tag:voice-agent"}, false},
		{"tagged node cannot use login list", tagged, []string{"chris@example.com"}, nil, false},
		{"tagged node cannot match the synthetic login", tagged, []string{"tagged-devices"}, nil, false},
		{"user cannot use tag list", user, nil, []string{"tag:voice-agent"}, false},
		{"identity with no login never matches", noLogin, []string{"chris@example.com"}, nil, false},
	}
	for _, c := range cases {
		if got := allowed(c.id, Clean(c.logins), Clean(c.tags)); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// Kong splits "a@b.com," into ["a@b.com"], but "," into [""] and
// "tag:a, tag:b" into ["tag:a", " tag:b"]. Without cleaning, the empty
// entry matched an untagged peer whose WhoIs carried no user profile, and the
// space-prefixed tag matched nobody.
func TestMiddlewareCleansAllowlist(t *testing.T) {
	noProfile := func(context.Context, string) (*apitype.WhoIsResponse, error) {
		return &apitype.WhoIsResponse{Node: &tailcfg.Node{Name: "orphan.tailnet.ts.net."}}, nil
	}
	h := NewMiddleware(noProfile, discard(), []string{""}, []string{" "})(okHandler(t, false))
	if rec := do(h, nil); rec.Code != http.StatusForbidden {
		t.Errorf("empty-string login admitted a peer with no login: %d", rec.Code)
	}

	second := whoIsTagged("tag:b")
	h = NewMiddleware(second, discard(), nil, []string{"tag:a", " tag:b"})(okHandler(t, true))
	if rec := do(h, nil); rec.Code != http.StatusNoContent {
		t.Errorf("whitespace-padded tag did not match: %d", rec.Code)
	}
}

func whoIsTagged(tags ...string) WhoIsFunc {
	return func(context.Context, string) (*apitype.WhoIsResponse, error) {
		return &apitype.WhoIsResponse{
			Node:        &tailcfg.Node{Name: "voice.tailnet.ts.net.", Tags: tags},
			UserProfile: &tailcfg.UserProfile{LoginName: "tagged-devices"},
		}, nil
	}
}

func okHandler(t *testing.T, wantCalled bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wantCalled {
			t.Error("handler reached")
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func do(h http.Handler, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Middleware must both gate the request and leave the caller's identity
// where the go-sdk transport and the audit middleware look for it
// (auth.TokenInfoFromContext); otherwise every tailnet call is logged as
// "anonymous".
func TestMiddlewareRecordsTokenInfo(t *testing.T) {
	s := &Server{logger: discard(), whoIs: whoIsTagged("tag:voice-agent")}

	var got *auth.TokenInfo
	var seenAuthz, seenCF string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = auth.TokenInfoFromContext(r.Context())
		seenAuthz = r.Header.Get("Authorization")
		seenCF = r.Header.Get("Cf-Access-Jwt-Assertion")
		w.WriteHeader(http.StatusNoContent)
	})

	h := s.Middleware(nil, []string{"tag:voice-agent"})(next)
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	// A client-supplied header must not leak through as if it were verified.
	req.Header.Set("Authorization", "Bearer attacker-controlled")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %q", rec.Code, rec.Body.String())
	}
	if got == nil {
		t.Fatal("TokenInfo not set in context")
	}
	if want := "voice(tag:voice-agent)"; got.UserID != want {
		t.Errorf("UserID = %q, want %q", got.UserID, want)
	}
	if got.Extra[FrontDoorKey] != FrontDoorTailnet {
		t.Errorf("front door = %v, want %q", got.Extra[FrontDoorKey], FrontDoorTailnet)
	}
	if seenAuthz != "Bearer "+placeholderToken {
		t.Errorf("Authorization reaching handler = %q, want placeholder", seenAuthz)
	}
	if seenCF != "" {
		t.Errorf("unexpected Cf-Access header %q", seenCF)
	}
	id, ok := FromContext(req.Context())
	if ok {
		t.Errorf("identity leaked onto the caller's request context: %+v", id)
	}
}

// A denied peer gets 403 whatever it sends: a well-formed bearer token or a
// Cloudflare Access assertion is not a second way in on this listener.
func TestMiddlewareDeniesWhateverHeadersArrive(t *testing.T) {
	s := &Server{
		logger: discard(),
		whoIs: func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
			return &apitype.WhoIsResponse{
				Node:        &tailcfg.Node{Name: "laptop.tailnet.ts.net."},
				UserProfile: &tailcfg.UserProfile{LoginName: "someone@example.com"},
			}, nil
		},
	}
	h := s.Middleware([]string{"chris@example.com"}, nil)(okHandler(t, false))
	for name, mutate := range map[string]func(*http.Request){
		"none":       nil,
		"bearer":     func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+placeholderToken) },
		"cf access":  func(r *http.Request) { r.Header.Set("Cf-Access-Jwt-Assertion", "eyJhbGciOiJSUzI1NiJ9.e30.sig") },
		"forwarded":  func(r *http.Request) { r.Header.Set("X-Forwarded-For", "100.64.0.1") },
		"login hdr":  func(r *http.Request) { r.Header.Set("Tailscale-User-Login", "chris@example.com") },
		"remoteaddr": func(r *http.Request) { r.RemoteAddr = "127.0.0.1:1" },
	} {
		if rec := do(h, mutate); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", name, rec.Code)
		}
	}
}

// The identity comes from the connection's remote address, never from a
// header: Tailscale's serve proxy sets Tailscale-User-Login, and a
// tsnet listener receives no such proxying, so honouring it would let any
// admitted-to-the-ACL peer claim any login.
func TestMiddlewareUsesRemoteAddrOnly(t *testing.T) {
	var asked string
	s := &Server{logger: discard(), whoIs: func(_ context.Context, addr string) (*apitype.WhoIsResponse, error) {
		asked = addr
		return nil, errors.New("unknown")
	}}
	h := s.Middleware([]string{"chris@example.com"}, nil)(okHandler(t, false))
	do(h, func(r *http.Request) {
		r.RemoteAddr = "100.100.1.2:4444"
		r.Header.Set("X-Forwarded-For", "100.64.0.9")
		r.Header.Set("Tailscale-User-Login", "chris@example.com")
	})
	if asked != "100.100.1.2:4444" {
		t.Errorf("WhoIs asked about %q, want the connection's remote address", asked)
	}
}

func TestMiddlewareWhoIsFailure(t *testing.T) {
	for name, fn := range map[string]WhoIsFunc{
		"error": func(context.Context, string) (*apitype.WhoIsResponse, error) {
			return nil, errors.New("no such peer")
		},
		"nil response": func(context.Context, string) (*apitype.WhoIsResponse, error) { return nil, nil },
	} {
		s := &Server{logger: discard(), whoIs: fn}
		h := s.Middleware([]string{"chris@example.com"}, []string{"tag:voice-agent"})(okHandler(t, false))
		if rec := do(h, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d, want 403", name, rec.Code)
		}
	}
}

// Validation refuses an empty allowlist, but the middleware must not depend
// on that: built with nothing usable it refuses everyone, including a tagged
// node and a user.
func TestMiddlewareEmptyAllowlistFailsClosed(t *testing.T) {
	for _, lists := range [][2][]string{{nil, nil}, {{""}, {" ", ""}}} {
		h := NewMiddleware(whoIsTagged("tag:voice-agent"), discard(), lists[0], lists[1])(okHandler(t, false))
		if rec := do(h, nil); rec.Code != http.StatusForbidden {
			t.Errorf("allowlist %q: status = %d, want 403", lists, rec.Code)
		}
	}
}

// The identity verifier only trusts the context the middleware set. Called
// without it — e.g. if someone wired RequireBearerToken(identityVerifier)
// alone in front of the handler — the placeholder token is worthless.
func TestPlaceholderTokenAloneIsWorthless(t *testing.T) {
	h := auth.RequireBearerToken(identityVerifier, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(okHandler(t, false))
	rec := do(h, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+placeholderToken) })
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestCheckStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := CheckStateDir(dir); err != nil {
		t.Fatalf("fresh dir: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("probe file left behind: %v", entries)
	}

	// A path under a regular file can never be created; the error must name
	// the variable to fix.
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := CheckStateDir(filepath.Join(file, "state"))
	if err == nil || !strings.Contains(err.Error(), "TS_STATE_DIR") {
		t.Errorf("err = %v, want one naming TS_STATE_DIR", err)
	}

	if os.Getuid() == 0 {
		t.Log("running as root: skipping the read-only directory case, root bypasses permissions")
		return
	}
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	err = CheckStateDir(ro)
	if err == nil || !strings.Contains(err.Error(), "not writable") {
		t.Errorf("read-only dir: err = %v, want 'not writable'", err)
	}
}

func TestUpErrorIsActionable(t *testing.T) {
	base := errors.New("tsnet.Up: context deadline exceeded")
	if msg := upError(base, context.DeadlineExceeded, nil).Error(); !strings.Contains(msg, "TS_AUTHKEY") || !strings.Contains(msg, "approval") {
		t.Errorf("timeout message not actionable: %s", msg)
	}
	if msg := upError(errors.New("backend: invalid key"), nil, nil).Error(); !strings.Contains(msg, "TS_AUTHKEY") {
		t.Errorf("backend error message not actionable: %s", msg)
	}
	if msg := upError(base, context.Canceled, context.Canceled).Error(); !strings.Contains(msg, "interrupted") {
		t.Errorf("shutdown during startup should say so: %s", msg)
	}
}
