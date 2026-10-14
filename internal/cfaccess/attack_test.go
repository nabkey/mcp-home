package cfaccess

// Attack tests: each one is a token or a certs-endpoint condition an attacker
// (or an outage) could produce, and asserts the validator's answer.

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

// fakeClock is a settable clock for the validator.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// certsServer is a controllable fake of <team>.cloudflareaccess.com.
type certsServer struct {
	mu      sync.Mutex
	certs   map[string]string // kid -> PEM
	status  int
	fetches atomic.Int64
}

func (s *certsServer) set(certs map[string]string, status int) {
	s.mu.Lock()
	s.certs, s.status = certs, status
	s.mu.Unlock()
}

func (s *certsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != certsPath {
		http.NotFound(w, r)
		return
	}
	s.fetches.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != http.StatusOK {
		w.WriteHeader(s.status)
		return
	}
	var list []map[string]string
	for kid, c := range s.certs {
		list = append(list, map[string]string{"kid": kid, "cert": c})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"public_certs": list})
}

// rig is a validator wired to a fake certs endpoint and a fake clock.
type rig struct {
	v     *Validator
	certs *certsServer
	clock *fakeClock
	key   *rsa.PrivateKey
}

func newRig(t *testing.T) *rig {
	t.Helper()
	key, certPEM := testKeys(t)
	cs := &certsServer{}
	cs.set(map[string]string{testKid: certPEM}, http.StatusOK)
	srv := httptest.NewServer(cs)
	t.Cleanup(srv.Close)
	clock := &fakeClock{t: time.Now()}
	v := New("myteam", "test-aud", slog.New(slog.DiscardHandler))
	v.teamURL = srv.URL
	v.now = clock.now
	return &rig{v: v, certs: cs, clock: clock, key: key}
}

func (r *rig) claims() map[string]any {
	now := r.clock.now()
	return map[string]any{
		"aud":   []string{"test-aud"},
		"email": "user@example.com",
		"iss":   r.v.teamURL,
		"iat":   now.Unix(),
		"nbf":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
		"sub":   "user-id",
	}
}

func (r *rig) sign(t *testing.T, claims map[string]any) string {
	return signJWT(t, r.key, testKid, claims)
}

// rawJWT assembles a token from an arbitrary header and claims and signs the
// signing input with sign.
func rawJWT(header, claims map[string]any, sign func([]byte) []byte) string {
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	return in + "." + base64.RawURLEncoding.EncodeToString(sign([]byte(in)))
}

func rs256(key *rsa.PrivateKey) func([]byte) []byte {
	return func(in []byte) []byte {
		h := sha256.Sum256(in)
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
		return sig
	}
}

func TestForgedTokensRejected(t *testing.T) {
	r := newRig(t)
	pubDER, _ := x509.MarshalPKIXPublicKey(&r.key.PublicKey)

	with := func(mut func(map[string]any)) map[string]any {
		c := r.claims()
		mut(c)
		return c
	}
	cases := map[string]string{
		// Algorithm confusion: HMAC keyed with the RSA public key, which an
		// attacker can download from the certs endpoint.
		"HS256 keyed with public key": rawJWT(map[string]any{"alg": "HS256", "kid": testKid}, r.claims(), func(in []byte) []byte {
			m := hmac.New(sha256.New, pubDER)
			m.Write(in)
			return m.Sum(nil)
		}),
		"alg none":        rawJWT(map[string]any{"alg": "none", "kid": testKid}, r.claims(), func([]byte) []byte { return nil }),
		"alg None":        rawJWT(map[string]any{"alg": "None", "kid": testKid}, r.claims(), func([]byte) []byte { return nil }),
		"alg lowercase":   rawJWT(map[string]any{"alg": "rs256", "kid": testKid}, r.claims(), rs256(r.key)),
		"alg PS256":       rawJWT(map[string]any{"alg": "PS256", "kid": testKid}, r.claims(), rs256(r.key)),
		"crit extension":  rawJWT(map[string]any{"alg": "RS256", "kid": testKid, "crit": []string{"x"}, "x": 1}, r.claims(), rs256(r.key)),
		"unknown kid":     signJWT(t, r.key, "not-a-kid", r.claims()),
		"empty kid":       signJWT(t, r.key, "", r.claims()),
		"missing exp":     r.sign(t, with(func(c map[string]any) { delete(c, "exp") })),
		"exp now":         r.sign(t, with(func(c map[string]any) { c["exp"] = r.clock.now().Unix() })),
		"expired":         r.sign(t, with(func(c map[string]any) { c["exp"] = r.clock.now().Add(-time.Second).Unix() })),
		"not yet valid":   r.sign(t, with(func(c map[string]any) { c["nbf"] = r.clock.now().Add(time.Hour).Unix() })),
		"wrong audience":  r.sign(t, with(func(c map[string]any) { c["aud"] = []string{"other-aud"} })),
		"empty audience":  r.sign(t, with(func(c map[string]any) { c["aud"] = []string{} })),
		"aud as object":   r.sign(t, with(func(c map[string]any) { c["aud"] = map[string]any{"test-aud": true} })),
		"wrong issuer":    r.sign(t, with(func(c map[string]any) { c["iss"] = "https://evil.cloudflareaccess.com" })),
		"issuer prefix":   r.sign(t, with(func(c map[string]any) { c["iss"] = r.v.teamURL + ".evil.com" })),
		"no identity":     r.sign(t, with(func(c map[string]any) { delete(c, "email") })),
		"signature strip": strings.Join(strings.Split(r.sign(t, r.claims()), ".")[:2], ".") + ".",
		"four parts":      r.sign(t, r.claims()) + ".x",
		"padded base64":   strings.Replace(r.sign(t, r.claims()), ".", "=.", 1),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if c, err := r.v.Validate(context.Background(), tok); err == nil {
				t.Errorf("accepted forged token: %+v", c)
			}
		})
	}
}

// The control: the rig's own token validates, so the rejections above are
// about the mutation, not a broken rig.
func TestRigControlAccepts(t *testing.T) {
	r := newRig(t)
	c, err := r.v.Validate(context.Background(), r.sign(t, r.claims()))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Identity() != "user@example.com" {
		t.Errorf("identity = %q", c.Identity())
	}
}

// Service tokens carry common_name instead of email. Accepting them is
// correct (Access let them in), but they must be attributable rather than
// landing in the audit log as "anonymous".
func TestServiceTokenAttributed(t *testing.T) {
	r := newRig(t)
	c := r.claims()
	delete(c, "email")
	c["common_name"] = "abc123.access"
	info, err := r.v.TokenVerifier()(context.Background(), r.sign(t, c), httptest.NewRequest("POST", "/mcp", nil))
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	if info.UserID != "service:abc123.access" {
		t.Errorf("UserID = %q, want service:abc123.access", info.UserID)
	}
	if info.Extra[FrontDoorKey] != FrontDoorCloudflare {
		t.Errorf("front door = %v", info.Extra[FrontDoorKey])
	}
}

// RFC 7519 allows aud as a single string.
func TestStringAudienceAccepted(t *testing.T) {
	r := newRig(t)
	c := r.claims()
	c["aud"] = "test-aud"
	if _, err := r.v.Validate(context.Background(), r.sign(t, c)); err != nil {
		t.Errorf("string aud rejected: %v", err)
	}
}

// A validator built with no audience must refuse everything, including a
// token whose aud is [""].
func TestEmptyConfiguredAudienceFailsClosed(t *testing.T) {
	r := newRig(t)
	r.v.audience = ""
	c := r.claims()
	c["aud"] = []string{""}
	if _, err := r.v.Validate(context.Background(), r.sign(t, c)); err == nil {
		t.Error("validator with empty audience accepted a token")
	}
}

// The 401 body must not hand a prober the expected audience or issuer.
func TestRejectionDoesNotLeakConfiguration(t *testing.T) {
	r := newRig(t)
	c := r.claims()
	c["aud"] = []string{"guess"}
	h := auth.RequireBearerToken(r.v.TokenVerifier(), nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler reached")
	}))
	for _, tok := range []string{r.sign(t, c), signJWT(t, r.key, "nope", r.claims())} {
		req := httptest.NewRequest("POST", "/mcp", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", rec.Code)
		}
		body := rec.Body.String()
		for _, secret := range []string{"test-aud", r.v.teamURL, "kid", "nope"} {
			if strings.Contains(body, secret) {
				t.Errorf("401 body %q leaks %q", body, secret)
			}
		}
	}
}

// Key rotation: Cloudflare starts signing with a new kid. The first token
// with it triggers a refetch (once minRefreshInterval has passed) and then
// validates; the old key, no longer published, stops validating.
func TestKeyRotation(t *testing.T) {
	r := newRig(t)
	if _, err := r.v.Validate(context.Background(), r.sign(t, r.claims())); err != nil {
		t.Fatalf("initial: %v", err)
	}
	newKey, newPEM := testKeys(t)
	r.certs.set(map[string]string{"kid-2": newPEM}, http.StatusOK)
	r.clock.advance(minRefreshInterval + time.Second)

	if _, err := r.v.Validate(context.Background(), signJWT(t, newKey, "kid-2", r.claims())); err != nil {
		t.Fatalf("token with rotated key rejected: %v", err)
	}
	if _, err := r.v.Validate(context.Background(), r.sign(t, r.claims())); err == nil {
		t.Error("token signed with the retired key still accepted")
	}
}

// With no keys ever fetched, a certs outage means nothing validates.
func TestCertsOutageAtStartFailsClosed(t *testing.T) {
	r := newRig(t)
	r.certs.set(nil, http.StatusInternalServerError)
	if _, err := r.v.Validate(context.Background(), r.sign(t, r.claims())); err == nil {
		t.Error("accepted a token with no keys available")
	}
	// Garbage certs are no better than none.
	r.clock.advance(failedFetchBackoff + time.Second)
	r.certs.set(map[string]string{testKid: "not a pem"}, http.StatusOK)
	if _, err := r.v.Validate(context.Background(), r.sign(t, r.claims())); err == nil {
		t.Error("accepted a token against an unparseable certs response")
	}
}

// After the cache expires, a certs outage must not lock the household out:
// the keys from the last good fetch keep working for a bounded time, and the
// outage costs at most one fetch per failedFetchBackoff rather than one per
// request. Past maxStale the validator fails closed.
func TestCertsOutageServesStaleKeysBounded(t *testing.T) {
	r := newRig(t)
	if _, err := r.v.Validate(context.Background(), r.sign(t, r.claims())); err != nil {
		t.Fatalf("initial: %v", err)
	}
	r.certs.set(nil, http.StatusServiceUnavailable)
	r.clock.advance(cacheDuration + time.Second)
	before := r.certs.fetches.Load()

	for i := 0; i < 20; i++ {
		if _, err := r.v.Validate(context.Background(), r.sign(t, r.claims())); err != nil {
			t.Fatalf("request %d during outage rejected: %v", i, err)
		}
	}
	if n := r.certs.fetches.Load() - before; n != 1 {
		t.Errorf("outage caused %d certs fetches for 20 requests, want 1", n)
	}

	// A kid we never had is not rescued by the stale cache.
	if _, err := r.v.Validate(context.Background(), signJWT(t, r.key, "other", r.claims())); err == nil {
		t.Error("unknown kid accepted during outage")
	}

	r.clock.advance(maxStale)
	if _, err := r.v.Validate(context.Background(), r.sign(t, r.claims())); err == nil {
		t.Error("stale key still accepted after maxStale")
	}
}

// A client that hangs up mid-fetch must not poison the fetch for everyone
// else: the fetch is detached from the request context.
func TestCancelledRequestDoesNotFailFetch(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.v.Validate(ctx, r.sign(t, r.claims())); err != nil {
		t.Fatalf("validate with cancelled ctx: %v", err)
	}
	if !r.v.lastFailure.IsZero() {
		t.Error("cancelled request recorded a fetch failure")
	}
}
