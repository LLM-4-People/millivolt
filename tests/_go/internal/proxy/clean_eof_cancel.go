package proxy

// Round 7 E1 regression: a clean EOF that lands after the local client canceled
// must not trigger a truncation or reasoning rescue. The upstream FIN can beat
// the transport's own cancellation, so the rescue gates need the same
// ctx.Err() == nil guard as the read-error gate; finishHold then stamps 499.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

type cancelAtEOFBody struct {
	data   []byte
	offset int
	cancel func()
	once   sync.Once
}

func (b *cancelAtEOFBody) Read(p []byte) (int, error) {
	if b.offset < len(b.data) {
		n := copy(p, b.data[b.offset:])
		b.offset += n
		return n, nil
	}
	b.once.Do(b.cancel)
	return 0, io.EOF
}

func (b *cancelAtEOFBody) Close() error { return nil }

type cancelAtEOFTransport struct {
	body  func() *cancelAtEOFBody
	calls atomic.Int32
}

func (t *cancelAtEOFTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       t.body(),
		Request:    req,
	}, nil
}

func TestCleanEOFAfterClientCancelDoesNotRetry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delta string
	}{
		{
			name:  "empty truncation",
			delta: `{"role":"assistant"}`,
		},
		{
			name:  "reasoning truncation",
			delta: `{"reasoning_content":"why"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":` + tc.delta + `,"finish_reason":null}]}` + "\n\n"

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			transport := &cancelAtEOFTransport{body: func() *cancelAtEOFBody {
				return &cancelAtEOFBody{data: []byte(source), cancel: cancel}
			}}

			buf := metrics.NewBuffer(8)
			cfg := config.Default()
			cfg.MaxRetries = 0
			cfg.QualityRetries = 1
			cfg.ThinkingRetries = 1
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: transport})

			r := httptest.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
				strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
			r = r.WithContext(ctx)
			r.Header.Set("Authorization", "Bearer sk-test")
			r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)

			if got := transport.calls.Load(); got != 1 {
				t.Fatalf("upstream attempts = %d, want 1", got)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.Retries != 0 || len(rec.Attempts) != 0 {
				t.Fatalf("phantom retry recorded: retries=%d attempts=%d", rec.Retries, len(rec.Attempts))
			}
			if rec.StatusCode != 499 || !rec.ClientDisconnected {
				t.Fatalf("record = status %d disconnected %v, want 499/true",
					rec.StatusCode, rec.ClientDisconnected)
			}
		})
	}
}
