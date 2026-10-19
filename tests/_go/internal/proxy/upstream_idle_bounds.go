package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// tarpitIdleBound is the shortened shared upstream body idle bound used by the
// tarpit regressions below. Production keeps the ten-minute guardrail; these
// tests only need to prove the same canonical guard covers error-body reads.
const tarpitIdleBound = 250 * time.Millisecond

// shortUpstreamIdleBound swaps the shared idle guard for the test bound and
// restores it after every server and request cleanup has run.
func shortUpstreamIdleBound(t *testing.T) {
	t.Helper()
	old := upstreamStreamIdleTimeout
	upstreamStreamIdleTimeout = tarpitIdleBound
	t.Cleanup(func() { upstreamStreamIdleTimeout = old })
}

// tarpitFixture returns response headers and then stalls without writing a
// body or EOF, exactly the gateway shape that used to hold an upstream body
// read forever. The handler is released only by cleanup, so the proxy must
// cancel its own attempt for the test to make progress.
type tarpitFixture struct {
	server  *httptest.Server
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newTarpitFixture(t *testing.T, status int) *tarpitFixture {
	t.Helper()
	f := &tarpitFixture{
		entered: make(chan struct{}, 128),
		release: make(chan struct{}),
	}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case f.entered <- struct{}{}:
		default:
		}
		<-f.release
	}))
	t.Cleanup(func() {
		f.once.Do(func() { close(f.release) })
		f.server.Close()
	})
	return f
}

func (f *tarpitFixture) waitEntered(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-f.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("tarpit saw %d of %d upstream requests; the proxy never reached the error-body read", i, n)
		}
	}
}

// healthyFixture answers one small OpenAI-wire completion. It is deliberately a
// different upstream authority from the tarpit so scheduler pacing on the
// tarpit's provider+key group cannot delay the health probe.
func newHealthyFixture(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"healthy","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(s.Close)
	return s
}

type tarpitResult struct {
	status int
	err    error
}

// startTarpitRequest sends one client request through the proxy. chunked
// forces Transfer-Encoding: chunked, which makes admission reserve the full
// request-body cap instead of the tiny actual payload. A unique key keeps each
// request in its own scheduler group so retry pacing never serializes them.
func startTarpitRequest(ctx context.Context, proxyURL, upstreamURL, key, payload string, chunked bool) <-chan tarpitResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL+"/v1/chat/completions", strings.NewReader(payload))
	if err != nil {
		ch := make(chan tarpitResult, 1)
		ch <- tarpitResult{err: err}
		return ch
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Proxy-Base-URL", upstreamURL)
	req.Header.Set("Content-Type", "application/json")
	if chunked {
		req.ContentLength = -1
	}
	ch := make(chan tarpitResult, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			ch <- tarpitResult{err: err}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		ch <- tarpitResult{status: resp.StatusCode}
	}()
	return ch
}

func postHealthy(t *testing.T, proxyURL, upstreamURL, key string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"healthy","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Proxy-Base-URL", upstreamURL)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestTarpitErrorBodyReleasesChunkedInflightBudget pins the HIGH regression:
// a tarpit that answers 429/5xx headers and then stalls used to pin
// quota429Peek's read (429) or the absorbed-attempt body drain (5xx) forever,
// before the adopted-stream idle guard. Chunked requests reserve the full
// 32 MiB request cap each, so eight stalled requests exhaust the default
// 256 MiB inflight budget and a healthy request is refused with 503. Every
// body read must instead honor the shared upstreamStreamIdleTimeout, cancel
// the attempt, release its reservation, and let the healthy request proceed.
func TestTarpitErrorBodyReleasesChunkedInflightBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{"retryable_5xx_body_drain", http.StatusInternalServerError},
		{"quota_429_body_peek", http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shortUpstreamIdleBound(t)
			cfg := config.Default()
			// One retry guarantees the 5xx drain path runs on the first
			// attempt; the final attempt's body read is the second guarded
			// read. A 429 runs quota429Peek on both attempts.
			cfg.MaxRetries = 1
			cfg.BaseBackoff = time.Millisecond
			cfg.MaxBackoff = time.Millisecond
			// Keep the production admission shape: 32 MiB per chunked request
			// and 256 MiB of inflight body budget, so the eighth request
			// exhausts the budget.
			proxySrv := httptest.NewServer(New(cfg, metrics.Noop{}))
			t.Cleanup(proxySrv.Close)
			tarpit := newTarpitFixture(t, tc.status)
			healthy := newHealthyFixture(t)

			const stalled = 8
			payload := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
			cancels := make([]context.CancelFunc, 0, stalled)
			results := make([]<-chan tarpitResult, 0, stalled)
			t.Cleanup(func() {
				for _, cancel := range cancels {
					cancel()
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				for _, ch := range results {
					select {
					case <-ch:
					case <-ctx.Done():
					}
				}
			})
			for i := 0; i < stalled; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				cancels = append(cancels, cancel)
				results = append(results, startTarpitRequest(ctx, proxySrv.URL, tarpit.server.URL,
					fmt.Sprintf("sk-tarpit-%d-%d", tc.status, i), payload, true))
			}
			tarpit.waitEntered(t, stalled)

			deadline := time.NewTimer(8 * time.Second)
			defer deadline.Stop()
			for i, ch := range results {
				select {
				case res := <-ch:
					if res.err != nil {
						t.Fatalf("tarpit request %d failed: %v", i, res.err)
					}
					if res.status != tc.status {
						t.Fatalf("tarpit request %d status = %d, want %d", i, res.status, tc.status)
					}
				case <-deadline.C:
					t.Fatalf("tarpit request %d still holds its chunked inflight reservation after 8s; the shared idle bound %s did not cancel the error-body read", i, tarpitIdleBound)
				}
			}

			if got := postHealthy(t, proxySrv.URL, healthy.URL, "sk-healthy-budget"); got != http.StatusOK {
				t.Fatalf("healthy request status = %d, want 200 after the tarpit reservations released", got)
			}
		})
	}
}

// TestTarpitBodylessErrorReleasesRequestSlot pins the same guard for a
// bodyless request: it reserves no inflight bytes but does consume one
// process-wide request slot. With the request bound at one, a stalled error
// body used to starve every later request; canceling at the shared idle bound
// must release the slot so the healthy request proceeds.
func TestTarpitBodylessErrorReleasesRequestSlot(t *testing.T) {
	shortUpstreamIdleBound(t)
	cfg := config.Default()
	cfg.MaxRetries = 1
	cfg.BaseBackoff = time.Millisecond
	cfg.MaxBackoff = time.Millisecond
	cfg.MaxInflightRequests = 1
	cfg.MaxInflightBodyBytes = 0 // unlimited: exercise the request slot only
	proxySrv := httptest.NewServer(New(cfg, metrics.Noop{}))
	t.Cleanup(proxySrv.Close)
	tarpit := newTarpitFixture(t, http.StatusInternalServerError)
	healthy := newHealthyFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	ch := startTarpitRequest(ctx, proxySrv.URL, tarpit.server.URL, "sk-bodyless", "", false)
	t.Cleanup(func() {
		cancel()
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
		}
	})
	tarpit.waitEntered(t, 1)

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("bodyless tarpit request failed: %v", res.err)
		}
		if res.status != http.StatusInternalServerError {
			t.Fatalf("bodyless tarpit status = %d, want 500", res.status)
		}
	case <-time.After(8 * time.Second):
		t.Fatalf("bodyless tarpit request still holds its request slot after 8s; the shared idle bound %s did not cancel the error-body read", tarpitIdleBound)
	}

	if got := postHealthy(t, proxySrv.URL, healthy.URL, "sk-healthy-slot"); got != http.StatusOK {
		t.Fatalf("healthy request status = %d, want 200 after the bodyless request released its slot", got)
	}
}
