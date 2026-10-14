// Package tsauth runs an embedded Tailscale node (tsnet) and identifies HTTP
// callers by their tailnet identity via WhoIs. The identity is bound to the
// WireGuard peer, so there are no passwords, cookies or bearer tokens.
package tsauth

import (
	"context"
	"errors"
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

// FrontDoorKey is the auth.TokenInfo.Extra key naming the listener a request
// arrived on, so the audit log can say which door a call came through.
const FrontDoorKey = "front_door"

// FrontDoorTailnet is the FrontDoorKey value for tailnet requests.
const FrontDoorTailnet = "tailnet"

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

// WhoIsFunc resolves a remote address to its tailnet peer. It is the one
// tsnet call the middleware makes, split out so tests can supply identities
// without a tailnet.
type WhoIsFunc func(ctx context.Context, remoteAddr string) (*apitype.WhoIsResponse, error)

// Server wraps a running tsnet node.
type Server struct {
	ts     *tsnet.Server
	lc     *local.Client
	whoIs  WhoIsFunc
	logger *slog.Logger
	// FQDN is the node's MagicDNS name, e.g. voice.tailnet.ts.net.
	FQDN string
}

// upTimeout bounds how long Start waits for the node to come up.
const upTimeout = 90 * time.Second

// CheckStateDir makes sure dir exists and that this process can write to it.
// tsnet persists the node key there; if it cannot, it fails deep inside its
// own startup with an error that does not name the variable to fix, and a
// directory that silently fails to persist means a fresh node registration
// (and a fresh auth key) on every restart.
func CheckStateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("TS_STATE_DIR %q cannot be created: %w (mount a writable volume there, or point TS_STATE_DIR at one)", dir, err)
	}
	f, err := os.CreateTemp(dir, ".write-probe-*")
	if err != nil {
		return fmt.Errorf("TS_STATE_DIR %q is not writable by this process (uid %d): %w (fix the volume's ownership or permissions)", dir, os.Getuid(), err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

// Start brings the node up and waits until it has an IP and a cert domain.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if err := CheckStateDir(cfg.StateDir); err != nil {
		return nil, err
	}
	ts := &tsnet.Server{
		Hostname: cfg.Hostname,
		AuthKey:  cfg.AuthKey,
		Dir:      filepath.Clean(cfg.StateDir),
		Logf:     func(string, ...any) {}, // tsnet is chatty; surface via UserLogf only
		UserLogf: func(f string, a ...any) { cfg.Logger.Debug("tsnet: " + fmt.Sprintf(f, a...)) },
	}
	upCtx, cancel := context.WithTimeout(ctx, upTimeout)
	defer cancel()
	st, err := ts.Up(upCtx)
	if err != nil {
		_ = ts.Close()
		return nil, upError(err, upCtx.Err(), ctx.Err())
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

// upError turns a failed tsnet Up into something an operator can act on. The
// common causes are a bad or expired auth key (reported by the backend), a
// node waiting for admin approval, and no route to the coordination server;
// the last two only show up as the timeout expiring.
func upError(err, upCtxErr, parentErr error) error {
	switch {
	case parentErr != nil:
		return fmt.Errorf("tsnet up: interrupted: %w", err)
	case errors.Is(upCtxErr, context.DeadlineExceeded):
		return fmt.Errorf("tsnet up: node not running after %s: %w (check that TS_AUTHKEY is valid and unexpired, "+
			"that the node is not waiting for device approval in the admin console, and that the host can reach "+
			"controlplane.tailscale.com)", upTimeout, err)
	default:
		return fmt.Errorf("tsnet up: %w (if this names the auth key, generate a new TS_AUTHKEY; "+
			"a persisted TS_STATE_DIR only needs it for the first registration)", err)
	}
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

// Middleware resolves the caller with WhoIs and applies an allowlist: the
// caller must match one login or carry one tag. Entries are compared exactly
// after trimming surrounding whitespace, and empty entries are ignored. If no
// usable entry remains the middleware refuses every caller: config validation
// rejects an empty allowlist, and this is the second line of that defence
// rather than an "open to the whole tailnet" mode.
//
// An admitted caller is then passed through the go-sdk's RequireBearerToken
// so the identity is recorded as auth.TokenInfo. That is the only way to set
// it — the context key is unexported — and it is what the streamable
// transport and the audit middleware read for the user, so without this
// every tailnet call would be logged as "anonymous". Whatever Authorization
// header the client sent is irrelevant here (WhoIs already authenticated the
// peer) and is replaced with a placeholder so the SDK's header parse passes.
func (s *Server) Middleware(allowedLogins, allowedTags []string) func(http.Handler) http.Handler {
	return NewMiddleware(s.whoIs, s.logger, allowedLogins, allowedTags)
}

// NewMiddleware is Middleware with the WhoIs lookup supplied by the caller,
// for wiring tests that have no tailnet.
func NewMiddleware(whoIs WhoIsFunc, logger *slog.Logger, allowedLogins, allowedTags []string) func(http.Handler) http.Handler {
	logins, tags := Clean(allowedLogins), Clean(allowedTags)
	if len(logins) == 0 && len(tags) == 0 {
		logger.Error("tailnet allowlist is empty: every tailnet request will be refused; set TS_ALLOWED_TAGS or TS_ALLOWED_LOGINS")
	}
	record := auth.RequireBearerToken(identityVerifier, &auth.RequireBearerTokenOptions{
		// The peer's identity lasts as long as the WireGuard session; there
		// is no token expiry to check.
		AllowMissingExpiration: true,
	})
	return func(next http.Handler) http.Handler {
		recorded := record(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			who, err := whoIs(r.Context(), r.RemoteAddr)
			if err != nil || who == nil {
				logger.Warn("tailnet request denied: whois failed", "remote", r.RemoteAddr, "path", r.URL.Path, "error", err)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			id := identityFrom(who, r.RemoteAddr)
			if !allowed(id, logins, tags) {
				logger.Warn("tailnet request denied: not in allowlist",
					"who", id.String(), "login", id.Login, "tags", id.Tags, "path", r.URL.Path)
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, id))
			r.Header.Set("Authorization", "Bearer "+placeholderToken)
			recorded.ServeHTTP(w, r)
		})
	}
}

// Clean trims each allowlist entry and drops empty ones, so "tag:a, tag:b"
// and a trailing comma mean what the operator meant, instead of leaving an
// entry nobody matches (" tag:b") or one that matches an identity with no
// login at all ("").
func Clean(list []string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
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
			FrontDoorKey: FrontDoorTailnet,
			"login":      id.Login,
			"node":       id.Node,
			"tags":       id.Tags,
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

// allowed reports whether id matches the (already cleaned) allowlist. An
// empty allowlist admits nobody. A tagged node is matched by tag only: its
// login is the synthetic "tagged-devices". Matching is exact, so a tag that
// merely starts with an allowed one ("tag:voice-agent-test") does not match.
func allowed(id Identity, logins, tags []string) bool {
	if !id.IsTagged && id.Login != "" && slices.Contains(logins, id.Login) {
		return true
	}
	for _, t := range id.Tags {
		if slices.Contains(tags, t) {
			return true
		}
	}
	return false
}
