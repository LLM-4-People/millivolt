package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// readyState is the probe dependency's verdict, so the handler test covers
// every readiness answer including the ones a real store only reaches after a
// wedge or a failed flush (storage's own test pins those transitions).
func readyState(ok bool, reason string) func() (bool, string) {
	return func() (bool, string) { return ok, reason }
}

// handleReadyz is an open probe route like /healthz, so its method gate and
// body bytes are an external contract. It differs from liveness in one way
// that matters to a scheduler: a not-ready pipeline answers 503 with the
// reason, never 200. Every row asserts no-store, the JSON transport, the
// recorded nosniff position (absent on the 405) and the exact body.
func TestReadyzMethodGateAndBody(t *testing.T) {
	for _, row := range []struct {
		name, method                        string
		ready                               func() (bool, string)
		wantStatus                          int
		wantCT, wantCC, wantAllow, wantBody string
		wantXCTO                            string
	}{
		{"POST is method-gated", http.MethodPost, readyState(true, ""), http.StatusMethodNotAllowed,
			"application/json", "no-store", "GET, HEAD", "{\"error\":\"GET or HEAD only\"}\n", ""},
		{"GET serves readiness", http.MethodGet, readyState(true, ""), http.StatusOK,
			"application/json", "no-store", "", "{\"ok\":true}\n", ""},
		{"GET names the failing reason", http.MethodGet, readyState(false, "2 durable flush(es) failed since boot"), http.StatusServiceUnavailable,
			"application/json", "no-store", "", "{\"ok\":false,\"reason\":\"2 durable flush(es) failed since boot\"}\n", ""},
		{"HEAD serves readiness with an empty body", http.MethodHead, readyState(true, ""), http.StatusOK,
			"application/json", "no-store", "", "", ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			srv := httptest.NewServer(handleReadyz(row.ready))
			defer srv.Close()
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

// TestReadycheckProbe pins the -readycheck mode end to end: it returns 0 only
// for a 200 carrying the exact success document, and 1 for a not-ready 503 (so
// Compose keeps waiting) and for a 200 with a foreign body.
func TestReadycheckProbe(t *testing.T) {
	for _, row := range []struct {
		name    string
		handler http.Handler
		want    int
	}{
		{"ready", handleReadyz(readyState(true, "")), 0},
		{"not ready", handleReadyz(readyState(false, "write queue holds 4 of 4 records")), 1},
		{"foreign 200 body", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<html>proxy login</html>")
		}), 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			srv := httptest.NewServer(row.handler)
			defer srv.Close()
			oldPath, oldListen := liveConfigPath, liveListenOverride
			t.Cleanup(func() { liveConfigPath, liveListenOverride = oldPath, oldListen })
			liveConfigPath = ""
			liveListenOverride = srv.URL[len("http://"):]
			if got := runReadycheck(); got != row.want {
				t.Fatalf("readycheck = %d, want %d", got, row.want)
			}
		})
	}
}
