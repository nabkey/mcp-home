// Package cfaccess validates Cloudflare Access JWTs for use as OAuth 2.1
// Bearer tokens in the MCP authorization flow.
//
// Cloudflare Access acts as the OAuth 2.1 authorization server. When a client
// completes the OAuth dance with CF Access, it receives a JWT that it sends as
// Authorization: Bearer <token>. This package validates those JWTs against
// the team's public signing keys.
package cfaccess

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

const (
	// certsPath is the path appended to the team URL to fetch signing certs.
	certsPath = "/cdn-cgi/access/certs"
	// cacheDuration controls how long we cache the public keys.
	cacheDuration = 15 * time.Minute
)

// Validator validates Cloudflare Access JWTs.
type Validator struct {
	teamURL  string // e.g., "https://myteam.cloudflareaccess.com"
	audience string
	logger   *slog.Logger

	mu         sync.RWMutex
	keys       map[string]crypto.PublicKey // kid -> public key
	lastFetch  time.Time
	httpClient *http.Client
}

// New creates a new Cloudflare Access JWT validator.
// teamDomain is just the team name (e.g., "myteam"), not the full URL.
// audience is the Application Audience (AUD) Tag from the Access dashboard.
func New(teamDomain, audience string, logger *slog.Logger) *Validator {
	return &Validator{
		teamURL:  "https://" + teamDomain + ".cloudflareaccess.com",
		audience: audience,
		logger:   logger,
		keys:     make(map[string]crypto.PublicKey),
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// TokenVerifier returns an auth.TokenVerifier compatible with the go-sdk's
// auth.RequireBearerToken middleware. It validates the Bearer token as a
// Cloudflare Access JWT (RS256 signature, audience, issuer, expiry).
func (v *Validator) TokenVerifier() auth.TokenVerifier {
	return func(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
		claims, err := v.Validate(ctx, token)
		if err != nil {
			v.logger.Warn("bearer token rejected", "error", err)
			return nil, fmt.Errorf("%w: %v", auth.ErrInvalidToken, err)
		}
		v.logger.Debug("bearer token validated", "email", claims.Email)
		return &auth.TokenInfo{
			UserID:     claims.Email,
			Expiration: time.Unix(claims.Expiry, 0),
		}, nil
	}
}

// Claims represents the validated claims from a Cloudflare Access JWT.
type Claims struct {
	Audience []string `json:"aud"`
	Email    string   `json:"email"`
	Issuer   string   `json:"iss"`
	IssuedAt int64    `json:"iat"`
	Expiry   int64    `json:"exp"`
	Subject  string   `json:"sub"`
}

// Validate parses and validates a Cloudflare Access JWT string.
func (v *Validator) Validate(ctx context.Context, tokenStr string) (*Claims, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed JWT: expected 3 parts, got %d", len(parts))
	}

	// Decode header to get kid.
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode JWT header: %w", err)
	}

	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("parse JWT header: %w", err)
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("unsupported JWT algorithm: %s", header.Alg)
	}

	// Decode and parse claims.
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode JWT claims: %w", err)
	}

	var claims Claims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return nil, fmt.Errorf("parse JWT claims: %w", err)
	}

	// Validate expiry.
	if time.Now().Unix() > claims.Expiry {
		return nil, fmt.Errorf("JWT expired at %s", time.Unix(claims.Expiry, 0))
	}

	// Validate audience.
	if !containsAudience(claims.Audience, v.audience) {
		return nil, fmt.Errorf("JWT audience %v does not contain %s", claims.Audience, v.audience)
	}

	// Validate issuer.
	if claims.Issuer != v.teamURL {
		return nil, fmt.Errorf("JWT issuer %q does not match %q", claims.Issuer, v.teamURL)
	}

	// Verify signature.
	key, err := v.getKey(ctx, header.Kid)
	if err != nil {
		return nil, fmt.Errorf("get signing key: %w", err)
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode JWT signature: %w", err)
	}

	signed := []byte(parts[0] + "." + parts[1])
	hash := sha256.Sum256(signed)

	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("expected RSA public key, got %T", key)
	}

	if err := rsa.VerifyPKCS1v15(rsaKey, crypto.SHA256, hash[:], sigBytes); err != nil {
		return nil, fmt.Errorf("JWT signature verification failed: %w", err)
	}

	return &claims, nil
}

func containsAudience(audiences []string, target string) bool {
	for _, a := range audiences {
		if a == target {
			return true
		}
	}
	return false
}

func (v *Validator) getKey(ctx context.Context, kid string) (crypto.PublicKey, error) {
	// Check cache first.
	v.mu.RLock()
	if key, ok := v.keys[kid]; ok && time.Since(v.lastFetch) < cacheDuration {
		v.mu.RUnlock()
		return key, nil
	}
	v.mu.RUnlock()

	// Refresh keys.
	if err := v.fetchKeys(ctx); err != nil {
		return nil, err
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	key, ok := v.keys[kid]
	if !ok {
		return nil, fmt.Errorf("key %q not found in Cloudflare Access certs", kid)
	}
	return key, nil
}

type certsResponse struct {
	PublicCerts []certEntry `json:"public_certs"`
}

type certEntry struct {
	Kid  string `json:"kid"`
	Cert string `json:"cert"`
}

func (v *Validator) fetchKeys(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "GET", v.teamURL+certsPath, nil)
	if err != nil {
		return fmt.Errorf("create certs request: %w", err)
	}

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch certs: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("certs endpoint returned status %d", resp.StatusCode)
	}

	var certs certsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&certs); err != nil {
		return fmt.Errorf("parse certs response: %w", err)
	}

	keys := make(map[string]crypto.PublicKey, len(certs.PublicCerts))
	for _, entry := range certs.PublicCerts {
		block, _ := pem.Decode([]byte(entry.Cert))
		if block == nil {
			v.logger.Warn("failed to decode PEM for kid", "kid", entry.Kid)
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			v.logger.Warn("failed to parse certificate", "kid", entry.Kid, "error", err)
			continue
		}
		keys[entry.Kid] = cert.PublicKey
	}

	if len(keys) == 0 {
		return fmt.Errorf("no valid keys found in Cloudflare Access certs")
	}

	v.mu.Lock()
	v.keys = keys
	v.lastFetch = time.Now()
	v.mu.Unlock()

	v.logger.Info("refreshed Cloudflare Access signing keys", "count", len(keys))
	return nil
}
