package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// TestSecureTransportHeaders pins the one HSTS owner: the header appears with
// the documented max-age exactly when the presentation is secure and is absent
// on plain HTTP, where a browser would ignore it anyway. The value carries no
// includeSubDomains, so the assertion is byte-exact.
func TestSecureTransportHeaders(t *testing.T) {
	for _, tc := range []struct {
		name      string
		target    string
		forwarded string
		want      string
	}{
		{"plain http", "http://proxy.example/", "", ""},
		{"tls", "https://proxy.example/", "", "max-age=" + strconv.Itoa(hstsMaxAge)},
		{"forwarded https", "http://proxy.example/", "https", "max-age=" + strconv.Itoa(hstsMaxAge)},
		{"appended https", "http://proxy.example/", "http, https", "max-age=" + strconv.Itoa(hstsMaxAge)},
		{"forwarded wss", "http://proxy.example/", "wss", "max-age=" + strconv.Itoa(hstsMaxAge)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := secureTransportHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			r := httptest.NewRequest(http.MethodGet, tc.target, nil)
			if tc.forwarded != "" {
				r.Header.Set("X-Forwarded-Proto", tc.forwarded)
			}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if got := w.Header().Get("Strict-Transport-Security"); got != tc.want {
				t.Fatalf("Strict-Transport-Security = %q, want %q", got, tc.want)
			}
			if w.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want the wrapped handler's 204", w.Code)
			}
		})
	}
}

// TestSecureTransportHeadersPrecedesABodylessRefusal pins the placement: the
// header must be set even when the wrapped layer refuses before any handler
// runs (the deadline layer's unavailable-policy 500).
func TestSecureTransportHeadersPrecedesABodylessRefusal(t *testing.T) {
	server := newHTTPServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}), 0, 0, nil, t.Context())
	if server.Handler == nil {
		t.Fatal("no handler installed")
	}
	r := httptest.NewRequest(http.MethodGet, "https://proxy.example/", nil)
	w := httptest.NewRecorder()
	server.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the unavailable-policy 500", w.Code)
	}
	if got := w.Header().Get("Strict-Transport-Security"); got == "" {
		t.Fatal("the refusal escaped without the transport header")
	}
}

// TestHSTSMaxAgeIsDocumented keeps the value and the prose from drifting: the
// operations guide states the max-age in days.
func TestHSTSMaxAgeIsDocumented(t *testing.T) {
	if hstsMaxAge != 180*24*60*60 {
		t.Fatalf("hstsMaxAge = %d, want 180 days", hstsMaxAge)
	}
}
