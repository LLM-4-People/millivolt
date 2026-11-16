package main

import (
	"net/http"
	"strconv"
)

// hstsMaxAge is the Strict-Transport-Security max-age in seconds (180 days).
// It is deliberately short enough that an operator who moves the dashboard off
// TLS recovers within a working quarter, and it carries no includeSubDomains:
// millivolt cannot know whether sibling or parent hostnames are TLS-capable.
const hstsMaxAge = 180 * 24 * 60 * 60

// secureTransportHeaders is the one owner of the transport-security response
// header. It wraps the whole handler chain (installed by newHTTPServer, so the
// restart handoff's clone inherits it) and marks every response to a secure
// presentation - a TLS connection or a forwarded secure scheme - so a browser
// pins https for this origin. The header is emitted on insecure responses too
// by nothing: browsers ignore it there, and a plain-HTTP dev instance must not
// pretend otherwise.
//
// It runs outside the deadline layer so it also covers the "policy
// unavailable" refusal, and it passes the writer through untouched, keeping
// Flusher, Hijacker and ResponseController's unwrap chain intact.
func secureTransportHeaders(next http.Handler) http.Handler {
	value := "max-age=" + strconv.Itoa(hstsMaxAge)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sessionCookieSecure(r) {
			w.Header().Set("Strict-Transport-Security", value)
		}
		next.ServeHTTP(w, r)
	})
}
