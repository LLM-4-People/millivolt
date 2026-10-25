package proxy

// Round 13 E-U2 regression: the debug tap wraps the original writer before the
// streaming pacer, so pacer-owned keepalives and pre-commit/in-band errors are
// captured as client-facing response bytes. The tap used to sit above the
// pacer and recorded an empty body for exactly those writes.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

type pacerCaptureBody struct {
	Raw       string `json:"raw"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

type pacerCaptureRun struct {
	queued     *metrics.Record
	held       *metrics.Record
	queuedBody string
	queuedTap  pacerCaptureBody
	heldTap    pacerCaptureBody
}

func newPacerCaptureProxy(t *testing.T, maxBytes int64, mutate ...func(*config.Config)) (*Server, *metrics.Buffer) {
	t.Helper()
	cfg := config.Default()
	cfg.MaxConcurrent = 1
	cfg.MaxQueueWait = 80 * time.Millisecond
	cfg.SSEKeepaliveInterval = time.Millisecond
	cfg.DebugCaptureMaxBytes = config.ByteSize(maxBytes)
	for _, apply := range mutate {
		apply(cfg)
	}
	store, _ := openProxyTestStore(t, filepath.Join(t.TempDir(), "pacer-capture.db"))
	t.Cleanup(func() { store.Close() })
	buf := metrics.NewBuffer(8)
	p := New(cfg, buf)
	p.AttachPausePersist(store)
	startDebugSession(t, p)
	return p, buf
}

func runPacerCapture(t *testing.T, maxBytes int64) pacerCaptureRun {
	t.Helper()
	p, buf := newPacerCaptureProxy(t, maxBytes)
	srv := httptest.NewServer(p)
	defer srv.Close()

	hold := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"id":"x","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"held","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-secret")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Client", "client-a")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	// The first request holds the only concurrency slot; the queued request
	// must observe the keepalive-committed wait.
	if !waitFor(t, func() bool { return p.scheduler.Stats().InFlight >= 1 }) {
		t.Fatal("first request never occupied the concurrency slot")
	}

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"queued","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-secret")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Client", "client-a")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	close(hold)
	<-firstDone

	records := waitForRecord(t, buf, 2)
	var run pacerCaptureRun
	for _, rec := range records {
		switch rec.Model {
		case "queued":
			run.queued = rec
		case "held":
			run.held = rec
		}
	}
	if run.queued == nil || run.held == nil {
		t.Fatalf("records = %+v, want the held and queued requests", records)
	}
	run.queuedBody = string(body)
	run.queuedTap = captureBodyOf(t, p, run.queued.ID)
	run.heldTap = captureBodyOf(t, p, run.held.ID)
	return run
}

func captureBodyOf(t *testing.T, p *Server, id string) pacerCaptureBody {
	t.Helper()
	var doc struct {
		Response struct {
			Body pacerCaptureBody `json:"body"`
		} `json:"response"`
	}
	raw := fetchDebugCapture(t, p, id)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("capture %s does not decode: %v (%s)", id, err, raw)
	}
	return doc.Response.Body
}

func TestDebugCaptureIncludesPacerPreCommitError(t *testing.T) {
	run := runPacerCapture(t, 4096)
	if !strings.Contains(run.queuedBody, "proxy queue full or wait exceeded") {
		t.Fatalf("client body = %q, want the queue-full error", run.queuedBody)
	}
	if !strings.Contains(run.queuedTap.Raw, "proxy queue full or wait exceeded") {
		t.Fatalf("capture = %q, want the client-facing error", run.queuedTap.Raw)
	}
	if !strings.Contains(run.queuedTap.Raw, ": keepalive") {
		t.Fatalf("capture = %q, want the pacer keepalive", run.queuedTap.Raw)
	}
	if run.queuedTap.Truncated || run.queuedTap.Bytes != len(run.queuedTap.Raw) {
		t.Fatalf("capture below the cap = %+v, want the full body", run.queuedTap)
	}
	if run.heldTap.Raw == "" {
		t.Fatal("completed stream capture has an empty response body")
	}

	t.Run("cap truncates pacer bytes", func(t *testing.T) {
		small := runPacerCapture(t, 64)
		if !small.queuedTap.Truncated {
			t.Fatalf("capture above the cap = %+v, want truncated", small.queuedTap)
		}
		if len(small.queuedTap.Raw) > 32 || small.queuedTap.Bytes <= len(small.queuedTap.Raw) {
			t.Fatalf("capture cap = raw %d bytes, seen %d, want at most 32 retained",
				len(small.queuedTap.Raw), small.queuedTap.Bytes)
		}
	})

	t.Run("transport failure before commit", func(t *testing.T) {
		p, buf := newPacerCaptureProxy(t, 4096, func(cfg *config.Config) { cfg.MaxRetries = 0 })
		srv := httptest.NewServer(p)
		defer srv.Close()
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL := dead.URL
		dead.Close()

		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"transport","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-secret")
		req.Header.Set("X-Proxy-Base-URL", deadURL)
		req.Header.Set("X-Proxy-Client", "client-a")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d body %s, want the pre-commit 502", resp.StatusCode, body)
		}
		records := waitForRecord(t, buf, 1)
		if len(records) != 1 {
			t.Fatalf("records = %d, want 1", len(records))
		}
		tap := captureBodyOf(t, p, records[0].ID)
		if tap.Raw == "" {
			t.Fatal("pre-commit JSON error was not captured")
		}
		if strings.TrimSpace(tap.Raw) != strings.TrimSpace(string(body)) {
			t.Fatalf("capture body = %q, want the client-facing JSON %q", tap.Raw, body)
		}
	})
}
