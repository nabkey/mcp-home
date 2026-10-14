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
	"errors"
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
	// minRefreshInterval bounds how often an unknown kid can trigger a certs
	// refetch, so a flood of garbage tokens cannot amplify into a flood of
	// outbound requests.
	minRefreshInterval = time.Minute
	// failedFetchBackoff is how long to wait after a failed certs fetch
	// before trying again. Without it every request during an outage of the
	// certs endpoint queues behind its own 10s fetch.
	failedFetchBackoff = 30 * time.Second
	// maxStale bounds how long keys from the last successful fetch keep
	// validating tokens while the certs endpoint cannot be reached. Cloudflare
	// rotates Access signing keys every six weeks and keeps the previous key
	// published for a further period, so a key that validated a day ago is
	// still the issuer's. Past this the validator fails closed.
	maxStale = 24 * time.Hour
	// nbfLeeway tolerates a token minted by a clock slightly ahead of ours.
	// It only widens the not-before check; expiry is checked strictly.
	nbfLeeway = time.Minute
)

// FrontDoorKey and FrontDoorCloudflare mark a TokenInfo as having come
// through the Cloudflare listener, for the audit log. The key matches
// tsauth.FrontDoorKey.
const (
	FrontDoorKey        = "front_door"
	FrontDoorCloudflare = "cloudflare"
)

// Validator validates Cloudflare Access JWTs.
type Validator struct {
	teamURL  string // e.g., "https://myteam.cloudflareaccess.com"
	audience string
	logger   *slog.Logger

	// fetchMu serializes refreshes so a burst of requests with an unknown kid
	// results in a single certs fetch.
	fetchMu sync.Mutex

	mu          sync.RWMutex
	keys        map[string]crypto.PublicKey // kid -> public key
	lastFetch   time.Time                   // last successful fetch
	lastFailure time.Time                   // last failed fetch
	httpClient  *http.Client
	now         func() time.Time
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
		now: time.Now,
	}
}

// errRejected is the only thing an unauthenticated caller learns about why
// its token failed. The detail (expected audience, issuer, key ids) goes to
// the server log; the go-sdk middleware writes the verifier's error text into
// the 401 body, so returning it would hand a prober the application's AUD and
// a step-by-step oracle for which check it got past.
var errRejected = fmt.Errorf("%w: token rejected", auth.ErrInvalidToken)

// TokenVerifier returns an auth.TokenVerifier compatible with the go-sdk's
// auth.RequireBearerToken middleware. It validates the Bearer token as a
// Cloudflare Access JWT (RS256 signature, audience, issuer, expiry).
func (v *Validator) TokenVerifier() auth.TokenVerifier {
	return func(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
		claims, err := v.Validate(ctx, token)
		if err != nil {
			path := ""
			if req != nil {
				path = req.URL.Path
			}
			v.logger.Warn("bearer token rejected", "error", err, "path", path)
			return nil, errRejected
		}
		user := claims.Identity()
		v.logger.Debug("bearer token validated", "user", user)
		return &auth.TokenInfo{
			UserID:     user,
			Expiration: time.Unix(claims.Expiry, 0),
			Extra: map[string]any{
				FrontDoorKey: FrontDoorCloudflare,
				"sub":        claims.Subject,
			},
		}, nil
	}
}

// Claims represents the validated claims from a Cloudflare Access JWT.
type Claims struct {
	Audience audience `json:"aud"`
	Email    string   `json:"email"`
	// CommonName is set instead of Email on tokens issued to an Access
	// service token (machine-to-machine); it is the service token's client ID.
	CommonName string `json:"common_name"`
	Issuer     string `json:"iss"`
	IssuedAt   int64  `json:"iat"`
	NotBefore  int64  `json:"nbf"`
	Expiry     int64  `json:"exp"`
	Subject    string `json:"sub"`
}

// Identity is who the audit log attributes a call to: the user's email, or
// for a service token "service:<client id>". Validate refuses a token with
// neither, so this is never empty for validated claims.
func (c *Claims) Identity() string {
	if c.Email != "" {
		return c.Email
	}
	if c.CommonName != "" {
		return "service:" + c.CommonName
	}
	return ""
}

// audience accepts both forms RFC 7519 allows for "aud": a string or an
// array of strings.
type audience []string

func (a *audience) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*a = audience{s}
		return nil
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("aud is neither a string nor an array of strings")
	}
	*a = list
	return nil
}

// Validate parses and validates a Cloudflare Access JWT string.
func (v *Validator) Validate(ctx context.Context, tokenStr string) (*Claims, error) {
	if v.audience == "" {
		// Fail closed: an empty expected audience would otherwise match a
		// token whose aud is [""].
		return nil, errors.New("validator has no audience configured")
	}
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
		Alg  string `json:"alg"`
		Kid  string `json:"kid"`
		Crit any    `json:"crit"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, fmt.Errorf("parse JWT header: %w", err)
	}
	if header.Alg != "RS256" {
		return nil, fmt.Errorf("unsupported JWT algorithm: %q", header.Alg)
	}
	if header.Crit != nil {
		// RFC 7515 §4.1.11: a recipient that does not understand a critical
		// extension must reject the token.
		return nil, errors.New("JWT header carries unsupported crit extensions")
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

	now := v.now()

	// Validate expiry. A missing exp decodes as 0 and is rejected here.
	if now.Unix() >= claims.Expiry {
		return nil, fmt.Errorf("JWT expired at %s", time.Unix(claims.Expiry, 0))
	}
	if claims.NotBefore != 0 && now.Add(nbfLeeway).Unix() < claims.NotBefore {
		return nil, fmt.Errorf("JWT not valid before %s", time.Unix(claims.NotBefore, 0))
	}

	// Validate audience.
	if !containsAudience(claims.Audience, v.audience) {
		return nil, fmt.Errorf("JWT audience %v does not contain %s", []string(claims.Audience), v.audience)
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

	// Checked last, after the signature, so this is a statement about a
	// token Cloudflare really issued: one with no identity cannot be
	// attributed in the audit log, and nothing legitimate looks like that.
	if claims.Identity() == "" {
		return nil, errors.New("JWT carries neither email nor common_name")
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
	if key, ok := v.keys[kid]; ok && v.now().Sub(v.lastFetch) < cacheDuration {
		v.mu.RUnlock()
		return key, nil
	}
	v.mu.RUnlock()

	// Refresh keys. fetchMu serializes concurrent refreshes; once inside,
	// re-check the cache in case another goroutine just refreshed, and skip
	// the fetch entirely if one happened within minRefreshInterval or one
	// failed within failedFetchBackoff.
	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()

	v.mu.RLock()
	key, ok := v.keys[kid]
	now := v.now()
	recentFetch := now.Sub(v.lastFetch) < minRefreshInterval
	recentFailure := now.Sub(v.lastFailure) < failedFetchBackoff
	v.mu.RUnlock()
	if ok && recentFetch {
		return key, nil
	}
	if recentFetch {
		return nil, fmt.Errorf("key %q not found in recently fetched Cloudflare Access certs", kid)
	}

	var fetchErr error
	if recentFailure {
		fetchErr = errors.New("certs endpoint failed recently; not retrying yet")
	} else {
		fetchErr = v.fetchKeys(ctx)
	}
	if fetchErr != nil {
		// Keep serving with what we last fetched rather than locking the
		// household out because the certs endpoint blipped, but only for a
		// bounded time and only for keys we already had.
		v.mu.RLock()
		key, ok := v.keys[kid]
		age := v.now().Sub(v.lastFetch)
		v.mu.RUnlock()
		if ok && age < maxStale {
			v.logger.Warn("Cloudflare Access certs refresh failed; using cached key", "kid", kid, "key_age", age.Round(time.Second), "error", fetchErr)
			return key, nil
		}
		return nil, fetchErr
	}

	v.mu.RLock()
	defer v.mu.RUnlock()
	key, ok = v.keys[kid]
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

func (v *Validator) fetchKeys(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			v.mu.Lock()
			v.lastFailure = v.now()
			v.mu.Unlock()
		}
	}()

	// Detach from the request: the fetch serves every waiter, and a caller
	// that hangs up must not abort it for the others (or mark it failed).
	ctx = context.WithoutCancel(ctx)
	req, err := http.NewRequestWithContext(ctx, "GET", v.teamURL+certsPath, nil)
	if err != nil {
		return fmt.Errorf("create certs request: %w", err)
	}

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetch certs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

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
		if _, ok := cert.PublicKey.(*rsa.PublicKey); !ok {
			v.logger.Warn("ignoring non-RSA certificate", "kid", entry.Kid, "type", fmt.Sprintf("%T", cert.PublicKey))
			continue
		}
		keys[entry.Kid] = cert.PublicKey
	}

	if len(keys) == 0 {
		return fmt.Errorf("no valid keys found in Cloudflare Access certs")
	}

	v.mu.Lock()
	v.keys = keys
	v.lastFetch = v.now()
	v.mu.Unlock()

	v.logger.Info("refreshed Cloudflare Access signing keys", "count", len(keys))
	return nil
}
