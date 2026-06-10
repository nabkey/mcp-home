package middleware

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoggingCapturesStatus(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	handler := Logging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/test", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusTeapot)
	}
}

func TestStatusWriterFlush(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	var flushable bool
	handler := Logging(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SSE handlers need a Flusher; the wrapper must not hide it.
		_, flushable = w.(http.Flusher)
		w.(http.Flusher).Flush()
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/mcp/sse", nil))

	if !flushable {
		t.Fatal("wrapped ResponseWriter does not implement http.Flusher")
	}
	if !rec.Flushed {
		t.Error("Flush was not forwarded to the underlying ResponseWriter")
	}
}

func TestStatusWriterUnwrap(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statusWriter{ResponseWriter: rec, code: http.StatusOK}

	// http.ResponseController relies on Unwrap to reach the underlying writer.
	rc := http.NewResponseController(sw)
	if err := rc.Flush(); err != nil {
		t.Errorf("ResponseController.Flush() error: %v", err)
	}
	if !rec.Flushed {
		t.Error("Flush did not reach the underlying ResponseWriter")
	}
}
