// Package cfapi turns Cloudflare API failures into errors an operator can act
// on, and tells transient failures (worth retrying) from permanent ones (a
// configuration problem that retrying cannot fix).
package cfapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/cloudflare/cloudflare-go/v4"
)

// Explain wraps err from a Cloudflare API call made to do what (e.g. "list
// tunnels") with what to change when the cause is on the operator's side.
// perm names the API token permission the call needs.
func Explain(err error, what, perm string) error {
	if err == nil {
		return nil
	}
	var apiErr *cloudflare.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Errorf("%s: Cloudflare rejected CF_API_TOKEN (HTTP 401): check the token is current and not revoked: %w", what, err)
		case http.StatusForbidden:
			return fmt.Errorf("%s: CF_API_TOKEN lacks permission (HTTP 403): grant it %q for this account/zone: %w", what, perm, err)
		case http.StatusNotFound:
			return fmt.Errorf("%s: not found (HTTP 404): check CF_ACCOUNT_ID and CF_ZONE_ID belong to the account the token is for: %w", what, err)
		case http.StatusTooManyRequests:
			return fmt.Errorf("%s: rate limited by the Cloudflare API (HTTP 429), will retry: %w", what, err)
		}
		if apiErr.StatusCode >= 500 {
			return fmt.Errorf("%s: Cloudflare API unavailable (HTTP %d), will retry: %w", what, apiErr.StatusCode, err)
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	if Transient(err) {
		return fmt.Errorf("%s: cannot reach the Cloudflare API (network), will retry: %w", what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// Transient reports whether err is worth retrying: rate limiting, a server
// error, or a network failure. Authentication, permission and not-found
// errors are configuration problems and are not transient. A cancelled
// context is not transient either: the caller is shutting down.
func Transient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var apiErr *cloudflare.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// The SDK sometimes flattens transport errors into strings.
	msg := err.Error()
	return strings.Contains(msg, "connection refused") || strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "no such host") || strings.Contains(msg, "EOF")
}
