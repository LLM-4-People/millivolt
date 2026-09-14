package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// handleHealthz is the one open liveness route (Docker HEALTHCHECK and load
// balancers cannot carry the operator credential), so its method gate and
// body bytes are an external contract. This pins the real handler end to
// end over a real server round trip: POST is denied with the RFC 9110 Allow
// set and the application/json error transport, GET serves the exact
// liveness JSON, and HEAD keeps GET's headers with an empty body (the HTTP
// layer strips it - a recorder would only show the handler's pre-strip
// write). Every row also asserts the recorded nosniff position: the 405
// carries NO X-Content-Type-Options (the record-accept decision at the
// transport owner, adminjson.WriteErrorJSON's doc) and the 200 rows never
// sent it, so a transport change cannot drift the position silently.
func TestHealthzMethodGateAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(handleHealthz))
	defer srv.Close()
	for _, row := range []struct {
		name, method                        string
		wantStatus                          int
		wantCT, wantCC, wantAllow, wantBody string
		wantXCTO                            string
	}{
		{"POST is method-gated", http.MethodPost, http.StatusMethodNotAllowed,
			"application/json", "no-store", "GET, HEAD", "{\"error\":\"GET or HEAD only\"}\n", ""},
		{"GET serves the liveness body", http.MethodGet, http.StatusOK,
			"application/json", "no-store", "", "{\"ok\":true}\n", ""},
		{"HEAD serves liveness with an empty body", http.MethodHead, http.StatusOK,
			"application/json", "no-store", "", "", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			req, err := http.NewRequest(row.method, srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != row.wantStatus {
				t.Errorf("%s: status = %d, want %d", row.method, resp.StatusCode, row.wantStatus)
			}
			if ct := resp.Header.Get("Content-Type"); ct != row.wantCT {
				t.Errorf("%s: Content-Type = %q, want %q", row.method, ct, row.wantCT)
			}
			if cc := resp.Header.Get("Cache-Control"); cc != row.wantCC {
				t.Errorf("%s: Cache-Control = %q, want %q", row.method, cc, row.wantCC)
			}
			if allow := resp.Header.Get("Allow"); allow != row.wantAllow {
				t.Errorf("%s: Allow = %q, want %q", row.method, allow, row.wantAllow)
			}
			if xcto := resp.Header.Get("X-Content-Type-Options"); xcto != row.wantXCTO {
				t.Errorf("%s: X-Content-Type-Options = %q, want %q (the recorded nosniff position)", row.method, xcto, row.wantXCTO)
			}
			if string(body) != row.wantBody {
				t.Errorf("%s: body = %q, want %q", row.method, body, row.wantBody)
			}
		})
	}
}
