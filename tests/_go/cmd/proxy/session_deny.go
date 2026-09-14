package main

// The session handshake (/admin/session) is the one open operator-plane
// route, so every denial it writes must carry the same hardening headers as
// the outer gate's JSON denials: Cache-Control: no-store (nothing about an
// authentication attempt may be cached) and the frame-ancestors CSP (a
// denial must never be frameable). Before denyOperator owned the shape, most
// of these sites set no-store but skipped the CSP; this drives every
// drivable denial path and pins both headers, plus the flat denial body
// byte-exactly on the three message-owning rows (disabled plane, bearer
// challenge, lockout).

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		gate.handleAdminSession(w, httptest.NewRequest(http.MethodGet, "http://proxy.example/admin/session", nil))
		assertDenial(t, "405", w, http.StatusMethodNotAllowed, "")
	})

	t.Run("unarmed plane", func(t *testing.T) {
		gate := newOperatorGate("")
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		gate.handleAdminSession(w, r)
		assertDenial(t, "403", w, http.StatusForbidden,
			"{\"error\":\"operator plane is disabled: set MILLIVOLT_OPERATOR_TOKEN to enable it\"}\n")
	})

	t.Run("wrong credential bearer", func(t *testing.T) {
		gate := newOperatorGate(token)
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session", nil)
		r.Header.Set("Authorization", "Bearer "+token+"-wrong")
		w := httptest.NewRecorder()
		gate.handleAdminSession(w, r)
		assertDenial(t, "401 json", w, http.StatusUnauthorized,
			"{\"error\":\"operator token required\"}\n")
	})

	t.Run("wrong credential form", func(t *testing.T) {
		gate := newOperatorGate(token)
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session",
			strings.NewReader("token=wrong-credential-fixture"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		gate.handleAdminSession(w, r)
		assertDenial(t, "401 login page", w, http.StatusUnauthorized, "")
	})

	t.Run("oversized form body", func(t *testing.T) {
		gate := newOperatorGate(token)
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session",
			strings.NewReader("token="+strings.Repeat("x", 8192)))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		gate.handleAdminSession(w, r)
		assertDenial(t, "400 oversized", w, http.StatusBadRequest, "")
	})

	t.Run("unsupported connection deadline", func(t *testing.T) {
		gate := newOperatorGate(token)
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session",
			strings.NewReader("token=fixture"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := brokenDeadlineWriter{httptest.NewRecorder()}
		gate.handleAdminSession(w, r)
		assertDenial(t, "500 deadline", w.ResponseRecorder, http.StatusInternalServerError, "")
	})

	t.Run("lockout after repeated rejections", func(t *testing.T) {
		gate := newOperatorGate(token)
		for i := 0; i < authFailureThreshold; i++ {
			r := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session", nil)
			r.Header.Set("Authorization", "Bearer "+token+"-wrong")
			w := httptest.NewRecorder()
			gate.handleAdminSession(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("rejection %d: status = %d, want 401", i, w.Code)
			}
		}
		r := httptest.NewRequest(http.MethodPost, "http://proxy.example/admin/session", nil)
		r.Header.Set("Authorization", "Bearer "+token+"-wrong")
		w := httptest.NewRecorder()
		gate.handleAdminSession(w, r)
		assertDenial(t, "429 lockout", w, http.StatusTooManyRequests,
			"{\"error\":\"too many rejected credentials; try again later\"}\n")
	})
}
