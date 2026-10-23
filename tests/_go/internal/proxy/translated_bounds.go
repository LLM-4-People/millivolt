package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const translatedAnthropicReply = `{"id":"msg_1","model":"m","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

func translatedRequest(body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer fixture-key")
	r.Header.Set("X-Proxy-Base-URL", "http://provider.example")
	r.Header.Set("X-Proxy-Path", "/messages")
	r.Header.Set("X-Proxy-Format", "anthropic")
	return r
}

// TestTranslatedRequestDoesNotHTMLEscapeContent proves the translated request
// placed on the upstream wire carries literal <, > and & (one byte each)
// instead of encoding/json's six-byte escapes, and remains valid JSON.
func TestTranslatedRequestDoesNotHTMLEscapeContent(t *testing.T) {
	content := wireHTMLLiteralForProxy(24 << 10)
	quoted, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":` + string(quoted) + `}]}`)

	var seen []byte
	p := New(config.Default(), metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(translatedAnthropicReply)),
			Request:    r,
		}, nil
	})})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, translatedRequest(body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if bytes.Contains(seen, []byte(`\u003c`)) || bytes.Contains(seen, []byte(`\u0026`)) {
		t.Fatalf("upstream request still HTML-escapes: %q", seen[:min(len(seen), 256)])
	}
	var doc struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(seen, &doc); err != nil {
		t.Fatalf("upstream request is not valid JSON: %v", err)
	}
	if len(doc.Messages) != 1 || doc.Messages[0].Content != content {
		t.Fatalf("upstream content changed: got %d bytes, want %d", len(doc.Messages[0].Content), len(content))
	}
	if limit := len(body) + 1024; len(seen) > limit {
		t.Fatalf("upstream request = %d bytes for a %d-byte client body, want <= %d", len(seen), len(body), limit)
	}
}

// TestTranslatedResponseDoesNotHTMLEscapeContent is the downstream twin.
func TestTranslatedResponseDoesNotHTMLEscapeContent(t *testing.T) {
	content := wireHTMLLiteralForProxy(24 << 10)
	quoted, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	upstreamBody := []byte(`{"id":"msg_1","model":"m","role":"assistant","content":[{"type":"text","text":` + string(quoted) +
		`}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)

	p := New(config.Default(), metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(upstreamBody)),
			Request:    r,
		}, nil
	})})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, translatedRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if bytes.Contains(w.Body.Bytes(), []byte(`\u003c`)) || bytes.Contains(w.Body.Bytes(), []byte(`\u0026`)) {
		t.Fatalf("translated response still HTML-escapes: %q", w.Body.Bytes()[:min(w.Body.Len(), 256)])
	}
	var doc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("translated response is not valid JSON: %v", err)
	}
	if len(doc.Choices) != 1 || doc.Choices[0].Message.Content != content {
		t.Fatalf("translated content changed: got %d bytes, want %d", len(doc.Choices[0].Message.Content), len(content))
	}
	if limit := len(upstreamBody) + 1024; w.Body.Len() > limit {
		t.Fatalf("translated response = %d bytes for a %d-byte upstream body, want <= %d", w.Body.Len(), len(upstreamBody), limit)
	}
}

func wireHTMLLiteralForProxy(n int) string {
	return strings.Repeat("<&>", n/3+1)[:n]
}

// TestTranslatedRequestOutputCapFailsClosed proves a translated body that
// grows past max_request_bytes is rejected before any upstream send, even
// though the client's own body fit under the cap.
func TestTranslatedRequestOutputCapFailsClosed(t *testing.T) {
	cfg := config.Default()
	// The documented minimum is the boundary this test pins: validation accepts
	// it, so the fixture must not rely on a below-minimum direct assignment.
	cfg.MaxRequestBytes = config.MaxRequestBytesMin
	var messages strings.Builder
	for i := 0; i < 19; i++ {
		if i > 0 {
			messages.WriteByte(',')
		}
		messages.WriteString(`{"role":"tool","tool_call_id":"i","content":"x"}`)
	}
	messages.WriteString(`,{"role":"user","content":"hi"}`)
	body := []byte(`{"model":"m","messages":[` + messages.String() + `]}`)
	if len(body) > int(cfg.MaxRequestBytes) {
		t.Fatalf("fixture body = %d bytes, want within the %d-byte cap", len(body), cfg.MaxRequestBytes)
	}
	translated, err := providerformat.TranslateRequest(body, cfg.AnthropicDefaultMaxTokens, "")
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(translated)) <= int64(cfg.MaxRequestBytes) {
		t.Fatalf("fixture translated body = %d bytes, want over the %d-byte cap", len(translated), cfg.MaxRequestBytes)
	}

	calls := 0
	p := New(cfg, metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("fixture guard: upstream must not be contacted")
	})})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, translatedRequest(body))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %s)", w.Code, w.Body.String())
	}
	if calls != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls)
	}
	if !strings.Contains(w.Body.String(), "translated request body exceeds") {
		t.Fatalf("missing explicit translated-cap error: %s", w.Body.String())
	}
}

// TestTranslatedResponseOutputCapFailsClosed drives the output cap through
// the real relay: an input at cap-1 whose translated wrapper crosses the cap
// answers 502 before the status line is committed.
func TestTranslatedResponseOutputCapFailsClosed(t *testing.T) {
	const prefix = `{"id":"m","model":"m","content":[{"type":"text","text":"`
	const suffix = `"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	sample := []byte(prefix + "x" + suffix)
	sampleOut, err := providerformat.TranslateResponse(sample)
	if err != nil {
		t.Fatal(err)
	}
	diff := len(sampleOut) - len(sample)
	if diff <= 0 {
		t.Fatalf("fixture translation shrinks (%d-byte diff); cannot exercise the output cap", diff)
	}
	contentLen := maxTranslateBodyBytes - 1 - len(prefix) - len(suffix)
	if contentLen <= 0 {
		t.Fatal("fixture prefix/suffix exceed the translation cap")
	}
	upstreamBody := []byte(prefix + strings.Repeat("x", contentLen) + suffix)
	if len(upstreamBody) != maxTranslateBodyBytes-1 {
		t.Fatalf("fixture input = %d bytes, want %d", len(upstreamBody), maxTranslateBodyBytes-1)
	}

	p := New(config.Default(), metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(bytes.NewReader(upstreamBody)),
			ContentLength: int64(len(upstreamBody)),
			Request:       r,
		}, nil
	})})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, translatedRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body prefix %q)", w.Code, w.Body.String()[:min(w.Body.Len(), 200)])
	}
	if !strings.Contains(w.Body.String(), "translated response too large") {
		t.Fatalf("missing explicit translated-output error: %s", w.Body.String())
	}
	if got := p.respBudget.bytes.Load(); got != 0 {
		t.Fatalf("response budget after output-cap failure = %d, want 0", got)
	}
}

// stalledReadBody blocks until Close, modelling an upstream that sent headers
// then went silent. Close is the only unblock, as with a transport body.
type stalledReadBody struct {
	unblock chan struct{}
	once    sync.Once
}

func (b *stalledReadBody) Read([]byte) (int, error) {
	<-b.unblock
	return 0, errors.New("stalled body closed")
}

func (b *stalledReadBody) Close() error {
	b.once.Do(func() { close(b.unblock) })
	return nil
}

// delayedReadBody serves its payload after delay, or fails early when Close
// interrupts the wait. A real transport body behaves the same way: canceling
// the request context closes it and unblocks a pending Read with an error.
type delayedReadBody struct {
	delay   time.Duration
	data    []byte
	unblock chan struct{}
	once    sync.Once
	mu      sync.Mutex
	served  bool
}

func (b *delayedReadBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.served {
		b.mu.Unlock()
		return 0, io.EOF
	}
	b.served = true
	b.mu.Unlock()
	timer := time.NewTimer(b.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return copy(p, b.data), nil
	case <-b.unblock:
		return 0, errors.New("delayed body closed before completion")
	}
}

func (b *delayedReadBody) Close() error {
	b.once.Do(func() { close(b.unblock) })
	return nil
}

// TestTranslatedNonStreamReadHonorsLongerDeadline proves the 2-minute
// whole-document fallback is not an override: a 5-minute parent deadline or
// X-Proxy-Timeout-Ms, still below the 10-minute ceiling, must let a body that
// completes after 2 minutes finish normally.
func TestTranslatedNonStreamReadHonorsLongerDeadline(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*testing.T, *http.Request) *http.Request
	}{
		{
			name: "parent context",
			configure: func(t *testing.T, r *http.Request) *http.Request {
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
				t.Cleanup(cancel)
				return r.WithContext(ctx)
			},
		},
		{
			name: "header timeout",
			configure: func(t *testing.T, r *http.Request) *http.Request {
				// The per-send deadline lives on the attempt context, so a
				// 5-minute header budget must reach the body read as well.
				r.Header.Set("X-Proxy-Timeout-Ms", "300000")
				return r
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := config.Default()
				bodyDelay := cfg.WholeDocumentReadTimeout + 30*time.Second
				body := &delayedReadBody{
					delay:   bodyDelay,
					data:    []byte(translatedAnthropicReply),
					unblock: make(chan struct{}),
				}
				p := New(cfg, metrics.Noop{})
				p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode:    http.StatusOK,
						Header:        http.Header{"Content-Type": []string{"application/json"}},
						Body:          body,
						ContentLength: -1,
						Request:       r,
					}, nil
				})})
				r := translatedRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
				r = tc.configure(t, r)
				started := time.Now()
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				elapsed := time.Since(started)
				if w.Code != http.StatusOK {
					t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
				}
				if elapsed != bodyDelay {
					t.Fatalf("elapsed = %s, want %s", elapsed, bodyDelay)
				}
				if !strings.Contains(w.Body.String(), `"ok"`) {
					t.Fatalf("translated body lost content: %s", w.Body.String())
				}
				if got := p.respBudget.bytes.Load(); got != 0 {
					t.Fatalf("response budget after successful read = %d, want 0", got)
				}
			})
		})
	}
}

// trickleThenStallBody delivers one byte after delay, then stalls until Close.
// The early byte is deliberate: the shared upstream idle guard resets on every
// completed Read, so a body that makes one slow progress step and then stops
// can outlive a plain stalled body and pin its reservation under a long send
// deadline. Only the whole-document ceiling can release it.
type trickleThenStallBody struct {
	delay   time.Duration
	unblock chan struct{}
	once    sync.Once
	mu      sync.Mutex
	served  bool
}

func (b *trickleThenStallBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.served {
		b.mu.Unlock()
		<-b.unblock
		return 0, errors.New("stalled body closed")
	}
	b.served = true
	b.mu.Unlock()
	timer := time.NewTimer(b.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		if len(p) == 0 {
			return 0, nil
		}
		p[0] = ' '
		return 1, nil
	case <-b.unblock:
		return 0, errors.New("trickle body closed before its first byte")
	}
}

func (b *trickleThenStallBody) Close() error {
	b.once.Do(func() { close(b.unblock) })
	return nil
}

// TestTranslatedNonStreamReadCapsLongHeaderDeadline pins the upper guardrail:
// a client-supplied one-hour X-Proxy-Timeout-Ms must not pin a whole-document
// response-buffer reservation for an hour. The body delivers one byte and then
// stalls, which the shared idle guard would only release well past the ceiling;
// the whole-document read must answer 502 at whole_document_read_max and release
// the process-wide reservation.
func TestTranslatedNonStreamReadCapsLongHeaderDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := config.Default()
		body := &trickleThenStallBody{delay: 6 * time.Minute, unblock: make(chan struct{})}
		p := New(cfg, metrics.Noop{})
		p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
				Body:          body,
				ContentLength: -1,
				Request:       r,
			}, nil
		})})
		r := translatedRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		r.Header.Set("X-Proxy-Timeout-Ms", "3600000")
		started := time.Now()
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		elapsed := time.Since(started)
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 (body %s)", w.Code, w.Body.String())
		}
		if elapsed != cfg.WholeDocumentReadMax {
			t.Fatalf("elapsed = %s, want the %s whole-document ceiling", elapsed, cfg.WholeDocumentReadMax)
		}
		if !strings.Contains(w.Body.String(), "upstream document read exceeded") {
			t.Fatalf("the rejected read must name the whole-document guardrail: %s", w.Body.String())
		}
		if got := p.respBudget.bytes.Load(); got != 0 {
			t.Fatalf("response budget after read timeout = %d, want 0", got)
		}
		select {
		case <-body.unblock:
		default:
			t.Fatal("stalled body was not closed at the ceiling")
		}
	})
}

// TestTranslatedNonStreamReadDeadlineReleasesBudget pins the whole-document
// fallback deadline: with no X-Proxy-Timeout-Ms, a stalled translated body
// answers 502 after whole_document_read_timeout and releases the process-wide
// reservation. A shorter caller deadline wins.
func TestTranslatedNonStreamReadDeadlineReleasesBudget(t *testing.T) {
	defaults := config.Default()
	for _, tc := range []struct {
		name   string
		caller time.Duration
		want   time.Duration
	}{
		{"internal default", 0, defaults.WholeDocumentReadTimeout},
		{"caller shorter", defaults.WholeDocumentReadTimeout / 4, defaults.WholeDocumentReadTimeout / 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cfg := config.Default()
				body := &stalledReadBody{unblock: make(chan struct{})}
				p := New(cfg, metrics.Noop{})
				p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode:    http.StatusOK,
						Header:        http.Header{"Content-Type": []string{"application/json"}},
						Body:          body,
						ContentLength: -1,
						Request:       r,
					}, nil
				})})
				r := translatedRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
				if tc.caller > 0 {
					ctx, cancel := context.WithTimeout(r.Context(), tc.caller)
					defer cancel()
					r = r.WithContext(ctx)
				}
				started := time.Now()
				w := httptest.NewRecorder()
				p.ServeHTTP(w, r)
				elapsed := time.Since(started)
				if w.Code != http.StatusBadGateway {
					t.Fatalf("status = %d, want 502 (body %s)", w.Code, w.Body.String())
				}
				if elapsed != tc.want {
					t.Fatalf("elapsed = %s, want %s", elapsed, tc.want)
				}
				if got := p.respBudget.bytes.Load(); got != 0 {
					t.Fatalf("response budget after read timeout = %d, want 0", got)
				}
				select {
				case <-body.unblock:
				default:
					t.Fatal("stalled body was not closed on timeout")
				}
			})
		})
	}
}
