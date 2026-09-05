package web

// Gzip is the dashboard's live response compression: the JSON aggregates,
// boot snapshot, and state-bearing HTML compress roughly 9x, which is
// the difference between a multi-hundred-kilobyte and a tens-of-kilobytes
// page load. Applied per-route in cmd/proxy/main.go to the dashboard's
// buffered JSON/HTML routes ONLY - never to the LLM proxy path (the hot
// path is sacred) and never to /metrics/live/stream (SSE flushes event-by-
// event and must stay uncompressed). Wrapping routes that stream would
// buffer or corrupt the stream; the route list is the guardrail. Immutable
// CSS/JS use web.go's precomputed default-compression representation instead.

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// dashboardGzipLevel is an internal codec choice, not a user setting. Level 2
// halves compression CPU on representative dashboard HTML for about 15% more
// wire bytes than the default, favoring first-paint latency on local/LAN use.
const dashboardGzipLevel = 2

// gzipWriterPool recycles gzip Writers (the window/flate state is the
// expensive part; Reset is cheap). Internal guardrail, not a tunable.
var gzipWriterPool = sync.Pool{
	New: func() any {
		writer, err := gzip.NewWriterLevel(nil, dashboardGzipLevel)
		if err != nil {
			panic(err) // invalid internal codec choice must fail closed
		}
		return writer
	},
}

// acceptsGzip reports whether the client explicitly accepts gzip (denied by
// default: a missing header or an explicit q=0 means no; any non-zero
// qvalue - RFC 9110 §12.5.3 - is acceptance). An UNPARSEABLE qvalue is also
// a refusal, as is a non-finite/out-of-range value - deny by default.
func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part == "" {
			continue
		}
		token, params, _ := strings.Cut(part, ";")
		if strings.TrimSpace(token) != "gzip" {
			continue
		}
		if _, q, ok := strings.Cut(params, "q="); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(q), 64)
			if err != nil || !(v > 0 && v <= 1) {
				return false // explicit refusal or garbage - both refuse
			}
		}
		return true
	}
	return false
}

// gzipResponseWriter defers the Content-Encoding decision to the first
// Write (a handler that only answers 304/HEAD never claims gzip).
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.wroteHeader {
		return
	}
	g.wroteHeader = true
	if code != http.StatusNotModified {
		h := g.Header()
		h.Set("Content-Encoding", "gzip")
		h.Set("Vary", "Accept-Encoding")
		h.Del("Content-Length")
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.wroteHeader {
		g.WriteHeader(http.StatusOK)
	}
	return g.gz.Write(b)
}

// Flush forwards a mid-response flush (deflate-flush, not a commit) so a
// future streaming handler on a wrapped route still works.
func (g *gzipResponseWriter) Flush() {
	if g.gz != nil {
		_ = g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Gzip wraps next with transparent gzip when the request accepts it.
func Gzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || !acceptsGzip(r) {
			next.ServeHTTP(w, r)
			return
		}
		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(w)
		g := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		defer func() {
			_ = gz.Close() // flush the deflate trailer; empty responses are a no-op
			gzipWriterPool.Put(gz)
		}()
		next.ServeHTTP(g, r)
	})
}
