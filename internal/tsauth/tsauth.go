// Package tsauth runs an embedded Tailscale node (tsnet) and identifies HTTP
// callers by their tailnet identity via WhoIs. The identity is bound to the
// WireGuard peer, so there are no passwords, cookies or bearer tokens.
package tsauth

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"tailscale.com/client/local"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tsnet"
)

// Identity is the authenticated caller.
type Identity struct {
	Login    string   // e.g. chris@example.com, or "tagged-devices" for tagged nodes
	Name     string   // display name
	Node     string   // node's MagicDNS short name
	Tags     []string // e.g. tag:voice-agent
	LoginIP  string
	IsTagged bool
}

// String is a compact label for logs.
func (i Identity) String() string {
	if i.IsTagged {
		return i.Node + "(" + strings.Join(i.Tags, ",") + ")"
	}
	return i.Login + "@" + i.Node
}

type ctxKey struct{}

// FromContext returns the Identity set by Middleware, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}

// Config for Start.
type Config struct {
	Hostname string
	AuthKey  string
	StateDir string
	Logger   *slog.Logger
}

// whoIsFunc resolves a remote address to its tailnet peer. It is the one
// tsnet call the middleware makes, split out so tests can supply identities
// without a tailnet.
type whoIsFunc func(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)

// Server wraps a running tsnet node.
type Server struct {
	ts     *tsnet.Server
	lc     *local.Client
	whoIs  whoIsFunc
	logger *slog.Logger
	// FQDN is the node's MagicDNS name, e.g. voice.tailnet.ts.net.
	FQDN string
}

// Start brings the node up and waits until it has an IP and a cert domain.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("tsnet state dir: %w", err)
	}
	ts := &tsnet.Server{
		Hostname: cfg.Hostname,
		AuthKey:  cfg.AuthKey,
		Dir:      filepath.Clean(cfg.StateDir),
		Logf:     func(string, ...any) {}, // tsnet is chatty; surface via UserLogf only
		UserLogf: func(f string, a ...any) { cfg.Logger.Debug("tsnet: " + fmt.Sprintf(f, a...)) },
	}
	upCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	st, err := ts.Up(upCtx)
	if err != nil {
		_ = ts.Close()
		return nil, fmt.Errorf("tsnet up: %w", err)
	}
	lc, err := ts.LocalClient()
	if err != nil {
		_ = ts.Close()
		return nil, fmt.Errorf("tsnet local client: %w", err)
	}
	s := &Server{ts: ts, lc: lc, whoIs: lc.WhoIs, logger: cfg.Logger}
	if len(st.CertDomains) > 0 {
		s.FQDN = st.CertDomains[0]
	} else if st.Self != nil {
		s.FQDN = strings.TrimSuffix(st.Self.DNSName, ".")
	}
	cfg.Logger.Info("tsnet up", "fqdn", s.FQDN, "ips", st.TailscaleIPs)
	return s, nil
}

// ListenTLS listens on :443 on the tailnet with a Tailscale-issued cert.
// Requires HTTPS certificates enabled in the tailnet DNS settings.
func (s *Server) ListenTLS() (net.Listener, error) {
	if s.FQDN == "" {
		return nil, fmt.Errorf("tsnet: no cert domain; enable MagicDNS + HTTPS certificates in the tailnet admin console")
	}
	return s.ts.ListenTLS("tcp", ":443")
}

// HTTPClient dials through the tailnet, so URLs like
// https://mcp-home.<tailnet>.ts.net resolve and route even when the host has
// no Tailscale of its own.
func (s *Server) HTTPClient() *http.Client { return s.ts.HTTPClient() }

// Close shuts the node down.
func (s *Server) Close() error { return s.ts.Close() }

// Middleware resolves the caller with WhoIs and applies an allowlist.
// allowedLogins / allowedTags: if both are empty, any tailnet peer the ACL
// admits is accepted (the ACL is the gate). Otherwise the caller must match
// one login or carry one tag.
//
// An admitted caller is then passed through the go-sdk's RequireBearerToken
// so the identity is recorded as auth.TokenInfo. That is the only way to set
// it — the context key is unexported — and it is what the streamable
// transport and the audit middleware read for the user, so without this
// every tailnet call would be logged as "anonymous". Whatever Authorization
// header the client sent is irrelevant here (WhoIs already authenticated the
// peer) and is replaced with a placeholder so the SDK's header parse passes.
func (s *Server) Middleware(allowedLogins, allowedTags []string) func(http.Handler) http.Handler {
	record := auth.RequireBearerToken(identityVerifier, &auth.RequireBearerTokenOptions{
		// The peer's identity lasts as long as the WireGuard session; there
		// is no token expiry to check.
		AllowMissingExpiration: true,
	})
	return func(next http.Handler) http.Handler {
		recorded := record(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			who, err := s.whoIs(r.Context(), r.RemoteAddr)
			if err != nil {
				s.logger.Warn("whois failed", "remote", r.RemoteAddr, "error", err)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			id := identityFrom(who, r.RemoteAddr)
			if !allowed(id, allowedLogins, allowedTags) {
				s.logger.Warn("denied", "who", id.String(), "path", r.URL.Path)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, id))
			r.Header.Set("Authorization", "Bearer "+placeholderToken)
			recorded.ServeHTTP(w, r)
		})
	}
}

// placeholderToken stands in for a bearer token on tailnet requests. It
// carries no secret: identityVerifier ignores it and reads the WhoIs identity
// from the context instead.
const placeholderToken = "tailnet-whois"

// identityVerifier is the auth.TokenVerifier for tailnet callers. The
// "token" is the placeholder set by Middleware; the identity comes from the
// context it stored there.
func identityVerifier(ctx context.Context, _ string, _ *http.Request) (*auth.TokenInfo, error) {
	id, ok := FromContext(ctx)
	if !ok {
		return nil, fmt.Errorf("%w: no tailnet identity in context", auth.ErrInvalidToken)
	}
	return &auth.TokenInfo{
		UserID: id.String(),
		Extra: map[string]any{
			"login": id.Login,
			"node":  id.Node,
			"tags":  id.Tags,
		},
	}, nil
}

// identityFrom builds an Identity from a WhoIs response.
func identityFrom(who *apitype.WhoIsResponse, remoteAddr string) Identity {
	id := Identity{LoginIP: remoteAddr}
	if who.Node != nil {
		id.Node = strings.TrimSuffix(strings.SplitN(who.Node.Name, ".", 2)[0], ".")
		if who.Node.IsTagged() {
			id.IsTagged = true
			id.Tags = who.Node.Tags
		}
	}
	if who.UserProfile != nil {
		id.Login = who.UserProfile.LoginName
		id.Name = who.UserProfile.DisplayName
	}
	return id
}

func allowed(id Identity, logins, tags []string) bool {
	if len(logins) == 0 && len(tags) == 0 {
		return true
	}
	if !id.IsTagged && slices.Contains(logins, id.Login) {
		return true
	}
	for _, t := range id.Tags {
		if slices.Contains(tags, t) {
			return true
		}
	}
	return false
}
