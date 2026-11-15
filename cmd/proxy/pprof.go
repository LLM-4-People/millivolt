package main

import (
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// registerPprofRoutes mounts the standard Go profiling handlers under the
// operator plane at /metrics/pprof/. The gate needs no new code for it:
// gatedPath already treats the whole /metrics/ namespace as operator plane,
// and the reserved-namespace table answers every unregistered /metrics/*
// path with the reserved 404.
//
// pprof.Index cannot dispatch the named profiles at this prefix: it only
// dispatches a sub-profile for paths under /debug/pprof/, the prefix it
// strips itself, so mounted here it would serve the HTML listing for every
// subpath. The index is therefore registered for the exact subtree root
// (/{$}, the same form the dashboard root uses) and every named handler is
// registered individually; an unregistered subpath then falls through to the
// reserved /metrics/ 404 instead of re-rendering the index.
//
// The CPU profile is not one of the registry names: runtime/pprof registers
// no "profile" profile, so its endpoint is the pprof.Profile handler, which
// owns the StartCPUProfile/StopCPUProfile cycle and refuses a second
// concurrent capture.
//
// Every mounted handler answers GET only, enforced by
// metrics.RejectUnlessGet, the shared method gate the other /metrics read
// routes use; the stdlib registers its own /debug/pprof routes as GET
// patterns, and a wrong-method request must not fall through to inference.
func registerPprofRoutes(mux *http.ServeMux, writeBound func() time.Duration) {
	gated := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !metrics.RejectUnlessGet(w, r) {
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	mux.Handle("/metrics/pprof/{$}", gated(http.HandlerFunc(pprof.Index)))
	mux.Handle("/metrics/pprof/profile", gated(clampProfileSeconds(http.HandlerFunc(pprof.Profile), writeBound)))
	for _, name := range []string{"allocs", "block", "goroutine", "heap", "mutex", "threadcreate"} {
		mux.Handle("/metrics/pprof/"+name, gated(pprof.Handler(name)))
	}
}

// defaultProfileSeconds is pprof.Profile's own fallback window, repeated here so
// the clamp can bound an absent parameter too.
const defaultProfileSeconds = 30

// clampProfileSeconds bounds the capture window by the delivery write bound.
// pprof.Profile accepts any whole-second value, so a `?seconds=` far past the
// configured response_write_timeout keeps a connection and a CPU profiler slot
// busy outside the delivery contract. The rewritten parameter is always
// explicit, so the handler captures exactly the window the bound allows; the
// documented valid range is 1s to response_write_timeout.
func clampProfileSeconds(next http.Handler, writeBound func() time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bound := time.Duration(0)
		if writeBound != nil {
			bound = writeBound()
		}
		query := r.URL.Query()
		query.Set("seconds", strconv.Itoa(profileSeconds(query.Get("seconds"), bound)))
		r.URL.RawQuery = query.Encode()
		next.ServeHTTP(w, r)
	})
}

// profileSeconds normalizes one requested window: absent, zero or unparseable
// keeps pprof's default, the write bound lowers anything above it, and a
// non-positive bound still leaves one second of samples.
func profileSeconds(raw string, bound time.Duration) int {
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		seconds = defaultProfileSeconds
	}
	if maxSeconds := int(bound / time.Second); seconds > maxSeconds {
		seconds = maxSeconds
	}
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}
