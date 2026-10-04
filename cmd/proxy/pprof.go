package main

import (
	"net/http"
	"net/http/pprof"

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
func registerPprofRoutes(mux *http.ServeMux) {
	gated := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !metrics.RejectUnlessGet(w, r) {
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	mux.Handle("/metrics/pprof/{$}", gated(http.HandlerFunc(pprof.Index)))
	mux.Handle("/metrics/pprof/profile", gated(http.HandlerFunc(pprof.Profile)))
	for _, name := range []string{"allocs", "block", "goroutine", "heap", "mutex", "threadcreate"} {
		mux.Handle("/metrics/pprof/"+name, gated(pprof.Handler(name)))
	}
}
