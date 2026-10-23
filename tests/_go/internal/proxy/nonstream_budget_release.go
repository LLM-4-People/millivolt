package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// translatedJSONRequest builds a non-streaming request for the real OpenAI
// Chat Completions route against an Anthropic target. It mirrors the
// translated streaming fixture's headers so the reservation follows the same
// production path.
func translatedJSONRequest(t *testing.T, upstream *httptest.Server) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://proxy.example/v1/chat/completions",
		strings.NewReader(`{"model":"claude-test","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-ant")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Auth-Header", "x-api-key")
	req.Header.Set("X-Proxy-Auth-Prefix", "")
	req.Header.Set("X-Proxy-Path", "/messages")
	req.Header.Set("X-Proxy-Format", "anthropic")
	return req
}

// translatedJSONUpstream serves one non-streaming Anthropic response body.
func translatedJSONUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// holdWriteResponseWriter commits the status line, signals the first write and
// blocks until released, then fails the write. It lets the test observe the
// response-buffer reservation while translation has already completed and the
// body is on its way to the client.
type holdWriteResponseWriter struct {
	header  http.Header
	status  int
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newHoldWriteResponseWriter() *holdWriteResponseWriter {
	return &holdWriteResponseWriter{
		header:  make(http.Header),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *holdWriteResponseWriter) Header() http.Header { return w.header }
func (w *holdWriteResponseWriter) WriteHeader(status int) {
	w.status = status
}
func (w *holdWriteResponseWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return 0, errors.New("fixture write failure")
}

// TestTranslatedNonStreamResponseBudgetRelease pins the whole-document
// reservation around translated non-streaming responses: it must survive
// TranslateResponseBetweenIR and only be released when the branch exits, on the failure
// paths as well as success.
//
// A missing final release leaves bytes held after a translation failure. A
// release moved before translation (or a success-only release on the write
// path) is observable at the blocked write: the test requires the reservation
// to still be held there.
func TestTranslatedNonStreamResponseBudgetRelease(t *testing.T) {
	valid := `{"id":"msg-ok","model":"claude-test","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

	t.Run("translation failure", func(t *testing.T) {
		invalid := `{"id":"msg-bad","model":"claude-test","content":[],"stop_reason":""}`
		upstream := translatedJSONUpstream(t, invalid)
		cfg := config.Default()
		cfg.MaxInflightResponseBytes = config.ByteSize(len(invalid))
		p := New(cfg, metrics.Noop{})
		w := httptest.NewRecorder()
		p.ServeHTTP(w, translatedJSONRequest(t, upstream))
		if w.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 for a failed translation (body %s)", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "translate upstream response") {
			t.Fatalf("failure body lost the translation error: %s", w.Body.String())
		}
		if got := p.respBudget.bytes.Load(); got != 0 {
			t.Fatalf("response budget after translation failure = %d, want 0", got)
		}
	})

	t.Run("write failure", func(t *testing.T) {
		upstream := translatedJSONUpstream(t, valid)
		cfg := config.Default()
		cfg.MaxInflightResponseBytes = config.ByteSize(len(valid))
		buf := metrics.NewBuffer(4)
		p := New(cfg, buf)
		w := newHoldWriteResponseWriter()
		done := make(chan struct{})
		go func() {
			p.ServeHTTP(w, translatedJSONRequest(t, upstream))
			close(done)
		}()

		select {
		case <-w.started:
		case <-time.After(2 * time.Second):
			t.Fatal("translated response never reached the write")
		}
		if got := p.respBudget.bytes.Load(); got != int64(len(valid)) {
			t.Fatalf("response budget while the translated body is being written = %d, want the full reservation %d", got, len(valid))
		}
		close(w.release)
		<-done

		if w.status != http.StatusOK {
			t.Fatalf("committed status = %d, want the translated 200 before the failed body write", w.status)
		}
		if got := p.respBudget.bytes.Load(); got != 0 {
			t.Fatalf("response budget after write failure = %d, want 0", got)
		}
		recs := waitForRecord(t, buf, 1)
		if len(recs) != 1 {
			t.Fatalf("recorded %d records, want 1", len(recs))
		}
		if recs[0].StatusCode != metrics.StatusClientClosedRequest || !recs[0].ClientDisconnected {
			t.Fatalf("write-failure record = status %d disconnected %v, want 499/true", recs[0].StatusCode, recs[0].ClientDisconnected)
		}
	})
}
