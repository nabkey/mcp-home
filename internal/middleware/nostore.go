package middleware

import "net/http"

// noStoreValue keeps no-transform, which the SDK sets so proxies do not
// buffer or rewrite an SSE stream.
const noStoreValue = "private, no-store, no-transform"

// NoStore marks every response as uncacheable by shared caches.
//
// MCP responses here describe one specific home (tool lists name its
// scripts and areas; tool results carry its state) and are only ever
// authorized for the caller who asked. The protocol-level hint for that is
// SEP-2549's cacheScope "private" on tools/list, but that is advice to
// MCP-aware clients only; the HTTP layer is what CDNs and proxies obey, and
// the SDK sends "no-cache", which permits a shared cache to store the
// response. This overrides it at the moment headers are written, so the
// SDK cannot set it back.
func NoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&noStoreWriter{ResponseWriter: w}, r)
	})
}

type noStoreWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *noStoreWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		w.Header().Set("Cache-Control", noStoreValue)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *noStoreWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// Flush forwards to the underlying writer so SSE is streamed.
func (w *noStoreWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying ResponseWriter for http.ResponseController.
func (w *noStoreWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
