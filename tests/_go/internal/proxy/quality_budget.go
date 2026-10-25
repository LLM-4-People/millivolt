package proxy

import (
	"bytes"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestQualityRetryEmptyBody: a chat request answered with a completely empty
// 200 body is the degenerate class in its purest form - retried transparently.
func TestQualityRetryEmptyBody(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if n == 1 {
			return // empty body
		}
		w.Write([]byte(realBody))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 || string(body) != realBody {
		t.Fatalf("empty-body retry failed: status %d body %q", resp.StatusCode, body)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2", n)
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs[0].Attempts) != 1 || recs[0].Attempts[0].ErrorType != metrics.CodeEmptyCompletion {
		t.Fatalf("attempts = %+v, want one absorbed empty_completion", recs[0].Attempts)
	}
}

// TestStreamResolvesAtDoneWithoutEOF: the terminal hold resolves the moment
// the [DONE] line arrives - the client's completion is not withheld behind an
// upstream that keeps the connection open after [DONE].
func TestStreamResolvesAtDoneWithoutEOF(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		f := w.(http.Flusher)
		keepOpen := make(chan struct{})
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
			f.Flush()
			w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
			f.Flush()
			w.Write([]byte("data: [DONE]\n\n"))
			f.Flush()
			<-keepOpen // hold the upstream connection open past [DONE]
		}()
		<-r.Context().Done()
		close(keepOpen) // client closed: release the writer and finish the request
		<-writerDone
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Read incrementally with an overall deadline: [DONE] and the finish chunk
	// must arrive while the upstream is still open - well before any EOF.
	ch := make(chan []byte, 32)
	go func() {
		b := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(b)
			if n > 0 {
				ch <- append([]byte(nil), b[:n]...)
			}
			if rerr != nil {
				close(ch)
				return
			}
		}
	}()
	var acc []byte
	deadline := time.After(3 * time.Second)
	satisfied := false
	for !satisfied {
		select {
		case p, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before [DONE] arrived: %q", acc)
			}
			acc = append(acc, p...)
			satisfied = bytes.Contains(acc, []byte("data: [DONE]")) &&
				bytes.Contains(acc, []byte(`"finish_reason":"stop"`))
		case <-deadline:
			t.Fatalf("completion withheld despite [DONE] arriving: %q", acc)
		}
	}
}

// A decoy "error" key (null/empty value - metrics.ParseErrorEnvelope returns
// "" for those, exactly like the SSE analyzer treats them) must NOT ride the
// verbatim in-band branch of the non-streaming 200 path: a void completion
// carrying a sibling "error":null is still a degenerate 200 and must be
// quality-retried, never relayed as a silently-empty success.
func TestQualityRetryDecoyErrorKeyStillClassified(t *testing.T) {
	decoyVoid := `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"error":null}`
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if n == 1 {
			w.Write([]byte(decoyVoid))
			return
		}
		w.Write([]byte(realBody))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 || string(body) != realBody {
		t.Fatalf("decoy-key void must be quality-retried: status %d body %q", resp.StatusCode, body)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (decoy absorbed + retry)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("recovered request must not be an error: %+v", rec)
	}
	if rec.ErrorType != "" || rec.ErrorCode != "" || rec.ErrorMsg != "" {
		t.Fatalf("decoy key must leave no error mark: type=%q code=%q msg=%q", rec.ErrorType, rec.ErrorCode, rec.ErrorMsg)
	}
	if len(rec.Attempts) != 1 || rec.Attempts[0].ErrorType != metrics.CodeEmptyCompletion {
		t.Fatalf("attempts = %+v, want one absorbed %s", rec.Attempts, metrics.CodeEmptyCompletion)
	}
}

// Exhaustion variant: the still-void decoy body surfaces as a real 502 with
// the degenerate code - never a silently-empty 200.
func TestQualityRetryDecoyErrorKeyExhaustionFlags(t *testing.T) {
	decoyVoid := `{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"error":null}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(decoyVoid))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(newConfigWithQuality(0), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for a still-degenerate decoy void", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"code":"`+metrics.CodeEmptyCompletion+`"`) {
		t.Fatalf("502 body must carry the degenerate code: %s", body)
	}
	recs := waitForRecord(t, buf, 1)
	if !recs[0].IsError() || recs[0].ErrorCode != metrics.CodeEmptyCompletion {
		t.Fatalf("record must flag the degenerate outcome: type=%q code=%q", recs[0].ErrorType, recs[0].ErrorCode)
	}
}

// TestExhausted429OversizedBodyRelayedVerbatim: an exhausted-budget 429 whose
// body overflows the error-peek cap (maxErrBodyBytes) must still relay the
// COMPLETE body byte-for-byte - prefix + live remainder - never a truncated
// prefix ("an exhausted budget surfaces the last upstream response verbatim").
func TestExhausted429OversizedBodyRelayedVerbatim(t *testing.T) {
	// Deterministic payload comfortably past the 64 KiB peek cap.
	payload := bytes.Repeat([]byte("0123456789abcdef"), 64/16*1024+64) // 64 KiB + 1 KiB
	if len(payload) <= 64*1024 {
		t.Fatalf("test payload must exceed the peek cap: %d", len(payload))
	}
	wantSum := sha256.Sum256(payload)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write(payload)
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 0 // exhaust immediately: the 429 is the final response
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 relayed verbatim", resp.StatusCode)
	}
	if len(body) != len(payload) {
		t.Fatalf("relayed body length = %d, want the complete %d bytes (truncated prefix)", len(body), len(payload))
	}
	gotSum := sha256.Sum256(body)
	if gotSum != wantSum {
		t.Fatal("relayed body checksum mismatch: the 429 body was altered/truncated in transit")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (max_retries=0)", n)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].StatusCode != http.StatusTooManyRequests {
		t.Fatalf("record status = %d, want 429", recs[0].StatusCode)
	}
}
