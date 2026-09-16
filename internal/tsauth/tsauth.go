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

	"tailscale.com/client/local"
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

// Server wraps a running tsnet node.
type Server struct {
	ts     *tsnet.Server
	lc     *local.Client
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
	s := &Server{ts: ts, lc: lc, logger: cfg.Logger}
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
func (s *Server) Middleware(allowedLogins, allowedTags []string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			who, err := s.lc.WhoIs(r.Context(), r.RemoteAddr)
			if err != nil {
				s.logger.Warn("whois failed", "remote", r.RemoteAddr, "error", err)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			id := Identity{
				Node:    strings.TrimSuffix(strings.SplitN(who.Node.Name, ".", 2)[0], "."),
				LoginIP: r.RemoteAddr,
			}
			if who.UserProfile != nil {
				id.Login = who.UserProfile.LoginName
				id.Name = who.UserProfile.DisplayName
			}
			if who.Node.IsTagged() {
				id.IsTagged = true
				id.Tags = who.Node.Tags
			}
			if !allowed(id, allowedLogins, allowedTags) {
				s.logger.Warn("denied", "who", id.String(), "path", r.URL.Path)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
		})
	}
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
