package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// TestProbeNamespaceReservesItsSubtree pins R1-16: a probe whose path a
// monitoring tool spells with a trailing slash must reach millivolt's own
// answer, never the inference catch-all. The probe roots are open (a scraper
// carries no operator credential), are registered through the same namespace
// table as the operator plane, and their subtrees answer the reserved 404, so
// every spelling stays on the owned surface.
func TestProbeNamespaceReservesItsSubtree(t *testing.T) {
	gate := newOperatorGate("probe-credential")
	mux, handler := newOperatorMux(gate, config.Default())
	var upstream []string
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream = append(upstream, r.URL.Path)
		w.WriteHeader(http.StatusTeapot)
	}))
	get := func(path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder
	}

	for _, path := range []string{"/healthz", "/readyz"} {
		if recorder := get(path); recorder.Code != http.StatusOK {
			t.Errorf("GET %s without a credential = %d, want 200 (open probe root): %s", path, recorder.Code, recorder.Body)
		}
	}
	for _, path := range []string{"/healthz/", "/readyz/"} {
		if recorder := get(path); recorder.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (reserved subtree), not the inference catch-all: %s", path, recorder.Code, recorder.Body)
		}
	}
	if len(upstream) != 0 {
		t.Errorf("probe spellings reached the inference catch-all: %v", upstream)
	}

	// Both spellings must come from the namespace table, so a stub probe
	// registration cannot make the mux assertions above pass on its own.
	reserved := map[string]bool{}
	for _, namespace := range operatorNamespaces(gate, handler, config.Default()) {
		reserved[namespace.pattern] = true
	}
	for _, root := range []string{"/healthz", "/readyz"} {
		for _, pattern := range []string{root, root + "/"} {
			if !reserved[pattern] {
				t.Errorf("namespace table does not generate %s", pattern)
			}
		}
	}
}
