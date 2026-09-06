package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

type terminalReadChunks []string

func (c *terminalReadChunks) Read(p []byte) (int, error) {
	if len(*c) == 0 {
		return 0, io.EOF
	}
	n := copy(p, (*c)[0])
	(*c)[0] = (*c)[0][n:]
	if (*c)[0] == "" {
		*c = (*c)[1:]
	}
	return n, nil
}

func TestRegressionRedirectAllowlist(t *testing.T) {
	var hit atomic.Int32
	var leaked string
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Add(1)
		leaked = r.Header.Get("X-Secret-Key")
		io.WriteString(w, realBody)
	}))
	defer denied.Close()
	allowedUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, denied.URL+"/private", http.StatusTemporaryRedirect)
	}))
	defer allowedUp.Close()
	cfg := config.Default()
	cfg.AllowedBaseURLs = []string{allowedUp.URL}
	cfg.MaxRetries = 0
	p := New(cfg, nil)
	req := httptest.NewRequest("POST", "http://proxy/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set(hdrBaseURL, allowedUp.URL)
	req.Header.Set(hdrAuthHeader, "X-Secret-Key")
	req.Header.Set(hdrAuthPrefix, "")
	req.Header.Set(hdrKey, "audit-secret")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	if hit.Load() != 0 {
		t.Fatalf("allowed=%s forbidden=%s forbidden_hits=%d credential=%q client_status=%d", allowedUp.URL, denied.URL, hit.Load(), leaked, w.Code)
	}
}

func TestRegressionInvalidHeaderBoundary(t *testing.T) {
	r := httptest.NewRequest("POST", "http://proxy/v1/chat/completions", nil)
	r.Header.Set(hdrBaseURL, "https://old.example")
	r.Header.Set(hdrAuthHeader, "X:Invalid")
	if _, err := resolveTarget(r, config.Default()); err == nil {
		t.Fatal("invalid header X:Invalid accepted by routing boundary")
	}
}

func TestRegressionSplitTerminalUsage(t *testing.T) {
	wire := terminalReadChunks{
		`data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}` + "\n\n" +
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			`data: {"usage":{"prompt_`,
		`tokens":100,"completion_tokens":50,"total_tokens":150,"cost":0.02},"choices":[]}` + "\n\ndata: [DONE]\n\n",
	}
	p := New(config.Default(), nil)
	rec := new(metrics.Record)
	w := httptest.NewRecorder()
	p.streamBody(context.Background(), w, &wire, rec)
	if rec.Usage.InputTokens != 100 || rec.Usage.OutputTokens != 50 || rec.Cost != 0.02 {
		t.Fatalf("fragmented terminal usage: input=%d output=%d cost=%g finish=%q body=%q", rec.Usage.InputTokens, rec.Usage.OutputTokens, rec.Cost, rec.FinishReason, w.Body.String())
	}
}

func TestRegressionAnalyzerOwnsUsage(t *testing.T) {
	raw := []byte(`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`)
	var a sse.Analyzer
	a.Feed(raw, time.Now())
	for i := range raw {
		raw[i] = ' '
	}
	a.Feed([]byte("data: [DONE]"), time.Now())
	rec := new(metrics.Record)
	a.Fill(rec)
	if rec.Usage.InputTokens != 100 || rec.Usage.OutputTokens != 50 {
		t.Fatalf("reused feed buffer erased usage: input=%d output=%d", rec.Usage.InputTokens, rec.Usage.OutputTokens)
	}
}

func TestRegressionSynthetic502RecordedStatus(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, voidBody) }))
	defer up.Close()
	cfg := config.Default()
	cfg.QualityRetries = 0
	buf := metrics.NewBuffer(5)
	p := New(cfg, buf)
	r := httptest.NewRequest("POST", "http://proxy/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	r.Header.Set(hdrBaseURL, up.URL)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	rec := waitForRecord(t, buf, 1)[0]
	if rec.StatusCode != w.Code {
		t.Fatalf("client_status=%d recorded_status=%d error_type=%q", w.Code, rec.StatusCode, rec.ErrorType)
	}
}

func TestRegressionResponseConnectionNominated(t *testing.T) {
	src := http.Header{"Connection": []string{"X-Private-Hop"}, "X-Private-Hop": []string{"hop-only"}}
	dst := make(http.Header)
	copyResponseHeaders(dst, src)
	if dst.Get("X-Private-Hop") != "" {
		t.Fatalf("hop-by-hop response header forwarded: %#v", dst)
	}
}

func TestDebugWrappedPacerKeepsErrorFraming(t *testing.T) {
	w := httptest.NewRecorder()
	p := newSSEPacer(w, time.Hour)
	p.Start()
	defer p.Stop()
	tap := &debugTap{respBody: &cappedWriter{limit: 1024}}
	rec := new(metrics.Record)
	writeClientError(tap.wrapWriter(p), rec, "upstream_unreachable", "failed", 502)
	if !strings.Contains(w.Body.String(), "data: ") || !strings.Contains(w.Body.String(), "data: [DONE]") {
		t.Fatalf("debug wrapper lost SSE framing: %s", w.Body.String())
	}
}

func TestDebugHeaderInjectionRedactsCredentials(t *testing.T) {
	h := http.Header{hdrHeaders: []string{`{"X-Api-Key":["audit-secret"]}`}}
	b, _ := json.Marshal(headersToHAR(h, ""))
	if bytes.Contains(b, []byte("audit-secret")) {
		t.Fatalf("debug request capture retained nested credential: %s", b)
	}
}

func TestBidiHTTPSCannotReachPlaintextServer(t *testing.T) {
	var hit atomic.Int32
	up := httptest.NewServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Add(1); io.WriteString(w, "ok") }), &http2.Server{}))
	defer up.Close()
	p := New(config.Default(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", strings.Replace(up.URL, "http://", "https://", 1), nil)
	resp, err := p.cursorClientFor().Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil || hit.Load() != 0 {
		t.Fatalf("HTTPS used plaintext on non-default port: err=%v hits=%d", err, hit.Load())
	}
}
