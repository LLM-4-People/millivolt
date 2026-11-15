package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestClampProfileSeconds pins the capture-window bound: pprof accepts any
// whole-second value, so an operator (or a script) could hold a connection and
// the one CPU-profiler slot far past the delivery write bound. The table covers
// the absent parameter, pprof's own default, an in-range request, an oversized
// one, a zero and negative one, and a non-positive bound.
func TestClampProfileSeconds(t *testing.T) {
	const bound = 10 * time.Minute
	for _, row := range []struct {
		name string
		raw  string
		want int
	}{
		{"absent keeps the default", "", defaultProfileSeconds},
		{"in range is preserved", "45", 45},
		{"oversized is clamped to the bound", "86400", 600},
		{"zero keeps the default", "0", defaultProfileSeconds},
		{"negative keeps the default", "-5", defaultProfileSeconds},
		{"garbage keeps the default", "2m", defaultProfileSeconds},
		{"the bound itself is allowed", "600", 600},
	} {
		if got := profileSeconds(row.raw, bound); got != row.want {
			t.Errorf("%s: profileSeconds(%q) = %d, want %d", row.name, row.raw, got, row.want)
		}
	}
	// A bound below one second still yields a usable capture window instead of
	// a zero (pprof reads 0 as "use 30").
	if got := profileSeconds("60", 500*time.Millisecond); got != 1 {
		t.Errorf("sub-second bound: profileSeconds = %d, want 1", got)
	}
}

// TestClampProfileSecondsRewritesTheQuery pins that the wrapper hands the
// clamped value to pprof itself, not only to a header: the mounted handler is
// reached with the rewritten query and the delivery path keeps working.
func TestClampProfileSecondsRewritesTheQuery(t *testing.T) {
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.FormValue("seconds")
		w.WriteHeader(http.StatusOK)
	})
	handler := clampProfileSeconds(next, func() time.Duration { return time.Minute })

	req := httptest.NewRequest(http.MethodGet, "/metrics/pprof/profile?seconds=3600", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if seen != "60" {
		t.Fatalf("handler saw seconds=%q, want 60", seen)
	}

	// An absent parameter still reaches the handler as an explicit value.
	seen = ""
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics/pprof/profile", nil))
	if seen != "30" {
		t.Fatalf("absent parameter reached the handler as %q, want the default 30", seen)
	}
}
