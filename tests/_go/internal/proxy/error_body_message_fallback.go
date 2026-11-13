package proxy

// Regression: an upstream failure whose reason is not in an OpenAI envelope
// must still reach the record the dashboard reads.
//
// Observed symptom: api.lyber.ai answers 503. When such a response carries
// `{"detail":"..."}`, `{"message":"..."}` or Cloudflare's
// `{"success":false,"errors":[{"message":"..."}]}`, the record was stored as
// provider_error with an EMPTY error_msg, so the drawer showed a bare status
// and the pill's hover carried only the type. parseErrorBody treated any body
// that was valid JSON as "decodable but no message".
//
// The shape matrix and the success-path negative case live with the decoder
// that owns them (tests/_go/internal/metrics/failure_body.go). This file pins
// the part only the relay can prove: the reason survives onto the finalized
// record while the client still receives the provider's bytes verbatim.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestFailureReasonReachesRecord drives one non-streaming request per gateway
// shape and asserts the reason the operator sees.
func TestFailureReasonReachesRecord(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantMsg string
	}{
		{"fastapi-detail", `{"detail":"model is not serving"}`, "model is not serving"},
		{"top-level-message", `{"message":"model is not serving"}`, "model is not serving"},
		{"cloudflare-errors", `{"success":false,"errors":[{"code":1000,"message":"DNS points to prohibited IP"}]}`, "DNS points to prohibited IP"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				w.Write([]byte(tc.body))
			}))
			defer upstream.Close()

			cfg := config.Default()
			cfg.MaxRetries = 0
			buf := metrics.NewBuffer(10)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			status, got := nonStreamFixtureRequest(t, srv, upstream)
			if status != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", status)
			}
			// The client still receives the provider's own bytes.
			if !strings.Contains(got, tc.wantMsg) {
				t.Fatalf("client body must relay the provider error verbatim, got %q", got)
			}
			rec := waitForRecord(t, buf, 1)[0]
			if rec.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("record status = %d, want 503", rec.StatusCode)
			}
			if rec.ErrorMsg != tc.wantMsg {
				t.Fatalf("record error_msg = %q, want %q", rec.ErrorMsg, tc.wantMsg)
			}
			if rec.ErrorType != metrics.TypeProviderError {
				t.Fatalf("record error_type = %q, want %q", rec.ErrorType, metrics.TypeProviderError)
			}
		})
	}
}

// TestEmptyBodyFailureStaysUnclassified pins the case that started this: a
// zero-byte 503 has no reason to recover and must keep the empty error fields,
// with the client-facing default supplied at write time rather than invented.
func TestEmptyBodyFailureStaysUnclassified(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 0
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	status, _ := nonStreamFixtureRequest(t, srv, upstream)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	rec := waitForRecord(t, buf, 1)[0]
	if rec.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("record status = %d, want 503", rec.StatusCode)
	}
	if rec.ErrorType != "" || rec.ErrorMsg != "" || rec.ErrorCode != "" {
		t.Fatalf("an empty body must leave the error fields empty, got %q/%q/%q",
			rec.ErrorType, rec.ErrorCode, rec.ErrorMsg)
	}
}

// nonStreamFixtureRequest issues one non-streaming chat request and returns the
// client status and body. It is the failure-shaped twin of streamRequest: no
// stream, so the upstream error status reaches the client unchanged.
func nonStreamFixtureRequest(t *testing.T, srv *httptest.Server, upstream *httptest.Server) (int, string) {
	t.Helper()
	body := `{"model":"neutral-model","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-fixture")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}
