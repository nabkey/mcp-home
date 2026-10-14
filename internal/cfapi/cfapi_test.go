package cfapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v4"
)

func apiErr(status int) error { return &cloudflare.Error{StatusCode: status} }

func TestTransient(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"429":       {apiErr(http.StatusTooManyRequests), true},
		"503":       {fmt.Errorf("wrapped: %w", apiErr(http.StatusServiceUnavailable)), true},
		"401":       {apiErr(http.StatusUnauthorized), false},
		"403":       {apiErr(http.StatusForbidden), false},
		"404":       {apiErr(http.StatusNotFound), false},
		"network":   {&net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		"deadline":  {context.DeadlineExceeded, true},
		"cancelled": {context.Canceled, false},
		"nil":       {nil, false},
	}
	for name, c := range cases {
		if got := Transient(c.err); got != c.want {
			t.Errorf("%s: Transient = %v, want %v", name, got, c.want)
		}
	}
}

func TestExplainNamesTheFix(t *testing.T) {
	cases := map[int]string{
		http.StatusUnauthorized:       "CF_API_TOKEN",
		http.StatusForbidden:          "Zone: DNS Edit",
		http.StatusNotFound:           "CF_ZONE_ID",
		http.StatusTooManyRequests:    "rate limited",
		http.StatusServiceUnavailable: "unavailable",
	}
	for status, want := range cases {
		err := Explain(apiErr(status), "list DNS records", "Zone: DNS Edit")
		if !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "list DNS records") {
			t.Errorf("%d: %v, want it to mention %q", status, err, want)
		}
		var ce *cloudflare.Error
		if !errors.As(err, &ce) {
			t.Errorf("%d: Explain lost the underlying error", status)
		}
	}
}

func TestRetry(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	calls := 0
	err := Retry(context.Background(), 5, time.Millisecond, logger, "x", func() error {
		calls++
		if calls < 3 {
			return apiErr(http.StatusBadGateway)
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Errorf("transient: err=%v calls=%d", err, calls)
	}

	calls = 0
	err = Retry(context.Background(), 5, time.Millisecond, logger, "x", func() error { calls++; return apiErr(http.StatusForbidden) })
	if err == nil || calls != 1 {
		t.Errorf("permanent: err=%v calls=%d, want one attempt", err, calls)
	}

	calls = 0
	err = Retry(context.Background(), 3, time.Millisecond, logger, "x", func() error { calls++; return apiErr(http.StatusInternalServerError) })
	if err == nil || calls != 3 {
		t.Errorf("exhausted: err=%v calls=%d", err, calls)
	}
}
