package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// nonStreamChoicesResponse builds one non-streaming completion with n choice
// objects. The first choice carries healthy content so an over-bound body
// would otherwise classify as a normal success.
func nonStreamChoicesResponse(n int) string {
	var b strings.Builder
	b.WriteString(`{"id":"chat-1","model":"m","choices":[{"finish_reason":"stop","message":{"content":"hello"}}`)
	for i := 1; i < n; i++ {
		b.WriteString(`,{}`)
	}
	b.WriteString(`]}`)
	return b.String()
}

func postNonStreamChoiceFixture(t *testing.T, proxyURL, upstreamURL string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstreamURL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, rerr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if rerr != nil {
		t.Fatal(rerr)
	}
	return resp, string(body)
}

// TestNonStreamChoicesBound pins the relay contract for a spooled 200 whose
// top-level choices array is over the analysis bound: the provider's bytes
// still relay verbatim, but the record carries the non-retryable analysis
// error and no choice-derived record fields are applied. The 40,000 tiny
// choices are the regression shape that previously decoded into tens of MiB.
func TestNonStreamChoicesBound(t *testing.T) {
	heavy := nonStreamChoicesResponse(40_000)
	if len(heavy) >= qualitySpoolMax {
		t.Fatalf("fixture body %d bytes must fit the %d-byte quality spool", len(heavy), qualitySpoolMax)
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(heavy)))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, heavy)
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(10)
	p := New(config.Default(), buf)
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, got := postNonStreamChoiceFixture(t, srv.URL, upstream.URL)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the provider's 200 relayed", resp.StatusCode)
	}
	if got != heavy {
		t.Fatalf("relayed %d bytes, want the %d-byte body verbatim", len(got), len(heavy))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (the over-bound class is not retried)", n)
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("recorded %d records, want 1", len(recs))
	}
	rec := recs[0]
	if !rec.IsError() || rec.ErrorType != "response_too_large" || rec.StatusCode != http.StatusOK {
		t.Fatalf("record = status %d type %q, want a 200 response_too_large error", rec.StatusCode, rec.ErrorType)
	}
	if rec.HadAnswerContent || rec.ProviderModel != "" || rec.ProviderRequestID != "" {
		t.Fatalf("over-bound body applied record fields: answer=%v model=%q id=%q", rec.HadAnswerContent, rec.ProviderModel, rec.ProviderRequestID)
	}
}

// TestNonStreamChoicesNormalMultiChoice is the control: a normal multi-choice
// body stays fully classified and healthy.
func TestNonStreamChoicesNormalMultiChoice(t *testing.T) {
	body := nonStreamChoicesResponse(3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(10)
	p := New(config.Default(), buf)
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp, got := postNonStreamChoiceFixture(t, srv.URL, upstream.URL)
	if resp.StatusCode != http.StatusOK || got != body {
		t.Fatalf("status/body = %d/%d bytes, want the healthy 200 relayed verbatim", resp.StatusCode, len(got))
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("recorded %d records, want 1", len(recs))
	}
	rec := recs[0]
	if rec.IsError() || !rec.HadAnswerContent || rec.ProviderModel != "m" || rec.ProviderRequestID != "chat-1" {
		t.Fatalf("normal multi-choice record = %+v, want a healthy analyzed completion", rec)
	}
}
