package tsauth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"
)

func TestAllowed(t *testing.T) {
	user := Identity{Login: "chris@example.com"}
	tagged := Identity{IsTagged: true, Tags: []string{"tag:voice-agent"}}

	cases := []struct {
		name   string
		id     Identity
		logins []string
		tags   []string
		want   bool
	}{
		{"open when no lists", user, nil, nil, true},
		{"login match", user, []string{"chris@example.com"}, nil, true},
		{"login mismatch", user, []string{"other@example.com"}, nil, false},
		{"tag match", tagged, nil, []string{"tag:voice-agent"}, true},
		{"tagged node cannot use login list", tagged, []string{"chris@example.com"}, nil, false},
		{"user cannot use tag list", user, nil, []string{"tag:voice-agent"}, false},
	}
	for _, c := range cases {
		if got := allowed(c.id, c.logins, c.tags); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

// Middleware must both gate the request and leave the caller's identity
// where the go-sdk transport and the audit middleware look for it
// (auth.TokenInfoFromContext); otherwise every tailnet call is logged as
// "anonymous".
func TestMiddlewareRecordsTokenInfo(t *testing.T) {
	s := &Server{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		whoIs: func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
			return &apitype.WhoIsResponse{
				Node:        &tailcfg.Node{Name: "voice.tailnet.ts.net.", Tags: []string{"tag:voice-agent"}},
				UserProfile: &tailcfg.UserProfile{LoginName: "tagged-devices"},
			}, nil
		},
	}

	var got *auth.TokenInfo
	var seenAuthz string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = auth.TokenInfoFromContext(r.Context())
		seenAuthz = r.Header.Get("Authorization")
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
	if seenAuthz != "Bearer "+placeholderToken {
		t.Errorf("Authorization reaching handler = %q, want placeholder", seenAuthz)
	}
	id, ok := FromContext(req.Context())
	if ok {
		t.Errorf("identity leaked onto the caller's request context: %+v", id)
	}
}

func TestMiddlewareDenies(t *testing.T) {
	s := &Server{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		whoIs: func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
			return &apitype.WhoIsResponse{
				Node:        &tailcfg.Node{Name: "laptop.tailnet.ts.net."},
				UserProfile: &tailcfg.UserProfile{LoginName: "someone@example.com"},
			}, nil
		},
	}
	called := false
	h := s.Middleware([]string{"chris@example.com"}, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusForbidden || called {
		t.Errorf("status = %d, called = %v; want 403 and not called", rec.Code, called)
	}
}

func TestMiddlewareWhoIsFailure(t *testing.T) {
	s := &Server{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		whoIs: func(_ context.Context, _ string) (*apitype.WhoIsResponse, error) {
			return nil, errors.New("no such peer")
		},
	}
	h := s.Middleware(nil, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("handler called") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}
