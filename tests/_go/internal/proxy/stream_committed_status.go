package proxy

// Round 3 batch 7 regressions (E1, E3): a failure sink must never write a
// 502/503 body under a status line it already committed, and an in-band
// provider error that reached the client must survive a later write failure.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestStreamRequestNonSSEBodyFailureIs502(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusCreated, "not-json")
	buf := metrics.NewBuffer(10)
	s := New(config.Default(), buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", body)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "translate upstream response") {
		t.Fatalf("body does not carry the translation failure: %s", w.Body.String())
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	if recs[0].StatusCode != http.StatusBadGateway || recs[0].ErrorType != "transform_error" {
		t.Fatalf("record = status %d type %q, want 502/transform_error", recs[0].StatusCode, recs[0].ErrorType)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no retry after a non-SSE body)", up.calls.Load())
	}
}

func TestStreamRequestNonSSEBodySuccessKeeps201(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", http.StatusCreated,
		`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	buf := metrics.NewBuffer(10)
	s := New(config.Default(), buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := proxyRequest(t, s, "/v1/chat/completions", "anthropic", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want the upstream 201 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"object":"chat.completion"`) {
		t.Fatalf("body is not the translated completion: %s", w.Body.String())
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 || recs[0].StatusCode != http.StatusCreated {
		t.Fatalf("record = %+v, want status 201", recs)
	}
}

// failAfterFirstWrite delivers the first frame and then reports a dead client,
// exactly like a socket reset between the provider error frame and the
// terminal frame.
type failAfterFirstWrite struct {
	hdr    http.Header
	writes int
	buf    bytes.Buffer
}

func (w *failAfterFirstWrite) Header() http.Header { return w.hdr }
func (w *failAfterFirstWrite) WriteHeader(int)     {}
func (w *failAfterFirstWrite) Flush()              {}
func (w *failAfterFirstWrite) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("write tcp 127.0.0.1:1->127.0.0.1:2: write: connection reset by peer")
	}
	return w.buf.Write(p)
}

func TestTranslatedInBandErrorSurvivesClientDisconnect(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	rec := &metrics.Record{ID: "translated-abort", StatusCode: http.StatusOK}
	w := &failAfterFirstWrite{hdr: http.Header{}}
	stream := "data: {\"error\":{\"message\":\"upstream exploded\",\"type\":\"api_error\",\"code\":\"server_error\"}}\n\n" +
		"data: [DONE]\n\n"
	s.streamBodyTranslated(context.Background(), w, strings.NewReader(stream), rec, w, ir.Chat)

	if !rec.ClientDisconnected || rec.StatusCode != metrics.StatusClientClosedRequest {
		t.Fatalf("disconnect accounting = disconnected %v status %d, want true/499", rec.ClientDisconnected, rec.StatusCode)
	}
	if rec.ErrorType != "api_error" || rec.ErrorCode != "server_error" || rec.ErrorMsg != "upstream exploded" {
		t.Fatalf("record lost the delivered provider error: type=%q code=%q msg=%q", rec.ErrorType, rec.ErrorCode, rec.ErrorMsg)
	}
}

// TestStreamRequestTruncatedJSONBodyIs502 pins the 200 row of the same owner:
// a stream request whose upstream returned a JSON body that ends before its
// declared length must surface a real 502, not a committed 200 with an error
// document under it.
func TestStreamRequestTruncatedJSONBodyIs502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"c1"`)
	}))
	t.Cleanup(upstream.Close)
	buf := metrics.NewBuffer(10)
	s := New(config.Default(), buf)
	s.client.Store(&http.Client{Transport: rewriteTransport{target: upstream.URL}})

	body := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	w := proxyRequest(t, s, "/v1/chat/completions", "", body)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", w.Code, w.Body.String())
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 || recs[0].StatusCode != http.StatusBadGateway {
		t.Fatalf("record = %+v, want status 502", recs)
	}
}
