// Package middleware provides HTTP middleware for the MCP server.
package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

// statusWriter wraps ResponseWriter to capture the status code.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// Logging returns middleware that logs each HTTP request with method, path,
// status code, and duration.
func Logging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}

			next.ServeHTTP(sw, r)

			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.code,
				"duration", time.Since(start).Round(time.Millisecond),
				"remote", r.RemoteAddr,
			)
		})
	}
}
