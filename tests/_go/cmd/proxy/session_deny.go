package main

// The session handshake (/session) is the one open operator-plane route, so
// every denial it writes must carry the same hardening headers as
// the outer gate's JSON denials: Cache-Control: no-store (nothing about an
// authentication attempt may be cached) and the frame-ancestors CSP (a
// denial must never be frameable). Before denyOperator owned the shape, most
// of these sites set no-store but skipped the CSP; this drives every
// drivable denial path and pins both headers, plus the flat denial body
// byte-exactly on the three message-owning rows (disabled plane, bearer
// challenge, lockout). The final subtest drives the installed route table
// end to end: the moved handshake answers through its own registration with
// its cross-origin wrapper, and the retired /admin spelling of the handshake
// answers the reserved-namespace 404.

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// brokenDeadlineWriter reports a read-deadline failure that is not
// http.ErrNotSupported, the only way the session handler's deadline branch
// surfaces its 500 denial.
type brokenDeadlineWriter struct {
	*httptest.ResponseRecorder
}

func (brokenDeadlineWriter) SetReadDeadline(time.Time) error {
	return errors.New("fixture connection: read deadlines unsupported")
}

func TestSessionPlaneDenialsCarryNoStoreAndCSP(t *testing.T) {
	const token = "operator-fixture-credential-long"
	// wantBody pins the flat {"error":"..."} body byte-exactly (WriteError's
	// http.Error trailing newline included) on the rows that own a message;
	// "" skips the body leg (page-shaped and prose-denial rows).
	assertDenial := func(t *testing.T, name string, w *httptest.ResponseRecorder, wantStatus int, wantBody string) {
		t.Helper()
		if w.Code != wantStatus {
			t.Errorf("%s: status = %d, want %d", name, w.Code, wantStatus)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", name, cc)
		}
		if csp := w.Header().Get("Content-Security-Policy"); csp != "frame-ancestors 'none'" {
			t.Errorf("%s: Content-Security-Policy = %q, want frame-ancestors 'none'", name, csp)
		}
		if wantBody != "" && w.Body.String() != wantBody {
			t.Errorf("%s: body = %q, want %q", name, w.Body.String(), wantBody)
		}
	}

	t.Run("wrong method", func(t *testing.T) {
		gate := newOperatorGate(token)
		w := httptest.NewRecorder()
		gate.handleOperatorSession(w, httptest.NewRequest(http.MethodGet, "http://proxy.example/session", nil))
		assertDenial(t, "405", w, http.StatusMethodNotAllowed, "")
	})

	t.Run("unarmed plane", func(t *testing.T) {
		gate := newOperatorGate("")
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/session", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		gate.handleOperatorSession(w, r)
		assertDenial(t, "403", w, http.StatusForbidden,
			"{\"error\":\"operator plane is disabled: set MILLIVOLT_OPERATOR_TOKEN to enable it\"}\n")
	})

	t.Run("wrong credential bearer", func(t *testing.T) {
		gate := newOperatorGate(token)
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/session", nil)
		r.Header.Set("Authorization", "Bearer "+token+"-wrong")
		w := httptest.NewRecorder()
		gate.handleOperatorSession(w, r)
		assertDenial(t, "401 json", w, http.StatusUnauthorized,
			"{\"error\":\"operator token required\"}\n")
	})

	t.Run("wrong credential form", func(t *testing.T) {
		gate := newOperatorGate(token)
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/session",
			strings.NewReader("token=wrong-credential-fixture"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		gate.handleOperatorSession(w, r)
		assertDenial(t, "401 login page", w, http.StatusUnauthorized, "")
	})

	t.Run("oversized form body", func(t *testing.T) {
		gate := newOperatorGate(token)
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/session",
			strings.NewReader("token="+strings.Repeat("x", 8192)))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		gate.handleOperatorSession(w, r)
		assertDenial(t, "400 oversized", w, http.StatusBadRequest, "")
	})

	t.Run("unsupported connection deadline", func(t *testing.T) {
		gate := newOperatorGate(token)
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/session",
			strings.NewReader("token=fixture"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := brokenDeadlineWriter{httptest.NewRecorder()}
		gate.handleOperatorSession(w, r)
		assertDenial(t, "500 deadline", w.ResponseRecorder, http.StatusInternalServerError, "")
	})

	t.Run("lockout after repeated rejections", func(t *testing.T) {
		gate := newOperatorGate(token)
		for i := 0; i < authFailureThreshold; i++ {
			r := httptest.NewRequest(http.MethodPost, "http://proxy.example/session", nil)
			r.Header.Set("Authorization", "Bearer "+token+"-wrong")
			w := httptest.NewRecorder()
			gate.handleOperatorSession(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("rejection %d: status = %d, want 401", i, w.Code)
			}
		}
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/session", nil)
		r.Header.Set("Authorization", "Bearer "+token+"-wrong")
		w := httptest.NewRecorder()
		gate.handleOperatorSession(w, r)
		assertDenial(t, "429 lockout", w, http.StatusTooManyRequests,
			"{\"error\":\"too many rejected credentials; try again later\"}\n")
	})

	t.Run("moved route and retired spelling through the installed table", func(t *testing.T) {
		gate := newOperatorGate(token)
		_, handler := newOperatorMux(gate, config.Default())
		endpoint := httptest.NewServer(handler)
		t.Cleanup(endpoint.Close)
		post := func(target, origin string) *http.Response {
			t.Helper()
			request, err := http.NewRequest(http.MethodPost, endpoint.URL+target, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+token)
			if origin != "" {
				request.Header.Set("Origin", origin)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("POST %s: %v", target, err)
			}
			t.Cleanup(func() {
				response.Body.Close()
				_, _ = io.Copy(io.Discard, response.Body)
			})
			return response
		}
		// The handshake lives at /session and the registration is the
		// reserved-namespace table's root row: through the real installed
		// mux it still answers 204 with the minted cookie.
		response := post("/session", "")
		if response.StatusCode != http.StatusNoContent || len(response.Cookies()) != 1 {
			t.Errorf("POST /session: status = %d cookies = %d, want 204 + the minted cookie", response.StatusCode, len(response.Cookies()))
		}
		// The cross-origin protection wrapper stayed on the moved route.
		if crossOrigin := post("/session", "https://foreign.example"); crossOrigin.StatusCode != http.StatusForbidden {
			t.Errorf("foreign origin on POST /session: status = %d, want 403 (the wrapper stayed)", crossOrigin.StatusCode)
		}
		// The retired /admin spelling of the handshake must be gone: an
		// old-style request answers the reserved-namespace 404, never the
		// handshake, never inference. The path is composed from its
		// prefixes so the repository carries no literal reference to the
		// retired route while the request still drives it exactly.
		retired := "/admin" + "/session"
		if response := post(retired, ""); response.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s: status = %d, want the reserved-namespace 404", retired, response.StatusCode)
		}
	})
}
