package cfaccess

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testKid = "test-kid"

// testKeys generates an RSA key pair plus a self-signed certificate PEM, the
// same shape Cloudflare serves from /cdn-cgi/access/certs.
func testKeys(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return key, string(certPEM)
}

// signJWT builds an RS256 JWT with the given claims, signed by key.
func signJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	signed := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	hash := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// newTestValidator builds a Validator whose teamURL points at an httptest
// server serving the given cert PEM. fetches counts certs endpoint hits.
func newTestValidator(t *testing.T, certPEM string) (*Validator, *atomic.Int64) {
	t.Helper()
	var fetches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != certsPath {
			http.NotFound(w, r)
			return
		}
		fetches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"public_certs": []map[string]string{{"kid": testKid, "cert": certPEM}},
		})
	}))
	t.Cleanup(srv.Close)

	v := New("myteam", "test-aud", slog.New(slog.DiscardHandler))
	v.teamURL = srv.URL // override the hardcoded cloudflareaccess.com URL
	return v, &fetches
}

func validClaims(issuer string) map[string]any {
	return map[string]any{
		"aud":   []string{"test-aud"},
		"email": "user@example.com",
		"iss":   issuer,
		"iat":   time.Now().Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
		"sub":   "user-id",
	}
}

func TestValidateAcceptsValidToken(t *testing.T) {
	key, certPEM := testKeys(t)
	v, _ := newTestValidator(t, certPEM)
	token := signJWT(t, key, testKid, validClaims(v.teamURL))

	claims, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.Email != "user@example.com" {
		t.Errorf("email = %q, want user@example.com", claims.Email)
	}
}

func TestValidateRejectsBadTokens(t *testing.T) {
	key, certPEM := testKeys(t)
	v, _ := newTestValidator(t, certPEM)

	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	expired := validClaims(v.teamURL)
	expired["exp"] = time.Now().Add(-time.Hour).Unix()

	wrongAud := validClaims(v.teamURL)
	wrongAud["aud"] = []string{"other-aud"}

	cases := []struct {
		name  string
		token string
	}{
		{"malformed", "not.a-jwt"},
		{"garbage segments", "a.b.c"},
		{"expired", signJWT(t, key, testKid, expired)},
		{"wrong audience", signJWT(t, key, testKid, wrongAud)},
		{"wrong issuer", signJWT(t, key, testKid, validClaims("https://evil.example.com"))},
		{"wrong signing key", signJWT(t, otherKey, testKid, validClaims(v.teamURL))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.Validate(context.Background(), tc.token); err == nil {
				t.Error("Validate accepted an invalid token")
			}
		})
	}
}

func TestValidateRejectsNonRS256(t *testing.T) {
	_, certPEM := testKeys(t)
	v, _ := newTestValidator(t, certPEM)

	// alg=none with an empty signature must never validate.
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","kid":"test-kid"}`))
	payload, _ := json.Marshal(validClaims(v.teamURL))
	token := header + "." + base64.RawURLEncoding.EncodeToString(payload) + "."

	if _, err := v.Validate(context.Background(), token); err == nil {
		t.Error("Validate accepted an alg=none token")
	}
}

func TestValidateTamperedClaims(t *testing.T) {
	key, certPEM := testKeys(t)
	v, _ := newTestValidator(t, certPEM)
	token := signJWT(t, key, testKid, validClaims(v.teamURL))

	// Swap in different claims while keeping the original signature.
	forged := validClaims(v.teamURL)
	forged["email"] = "attacker@example.com"
	payload, _ := json.Marshal(forged)
	parts := strings.Split(token, ".")
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[2]

	if _, err := v.Validate(context.Background(), tampered); err == nil {
		t.Error("Validate accepted a token with tampered claims")
	}
}

func TestKeyCacheAvoidsRefetch(t *testing.T) {
	key, certPEM := testKeys(t)
	v, fetches := newTestValidator(t, certPEM)

	for i := 0; i < 3; i++ {
		token := signJWT(t, key, testKid, validClaims(v.teamURL))
		if _, err := v.Validate(context.Background(), token); err != nil {
			t.Fatalf("Validate #%d: %v", i, err)
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("certs fetched %d times, want 1", n)
	}
}

func TestUnknownKidRefetchIsRateLimited(t *testing.T) {
	key, certPEM := testKeys(t)
	v, fetches := newTestValidator(t, certPEM)

	// Tokens signed with an unknown kid: the first triggers a fetch, the
	// rest within minRefreshInterval must not.
	for i := 0; i < 5; i++ {
		token := signJWT(t, key, fmt.Sprintf("unknown-kid-%d", i), validClaims(v.teamURL))
		if _, err := v.Validate(context.Background(), token); err == nil {
			t.Fatalf("Validate #%d accepted a token with an unknown kid", i)
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf("certs fetched %d times, want 1 (refetch flood not rate-limited)", n)
	}

	// A valid token must still work off the cache populated above.
	token := signJWT(t, key, testKid, validClaims(v.teamURL))
	if _, err := v.Validate(context.Background(), token); err != nil {
		t.Errorf("Validate with known kid after rate limiting: %v", err)
	}
}
