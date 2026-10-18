package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// newStoredTestProxy builds a proxy over a fresh scratch store. Capture
// sessions require durable storage, so every test that starts a session
// through HandleDebug or builds a capture tap uses this instead of New's
// store-less default. The store closes at test cleanup.
func newStoredTestProxy(t *testing.T, rec metrics.Recorder) *Server {
	t.Helper()
	store, _ := openProxyTestStore(t, filepath.Join(t.TempDir(), "debug-capture.db"))
	t.Cleanup(func() { store.Close() })
	p := New(config.Default(), rec)
	p.AttachPausePersist(store)
	return p
}

// startDebugSession starts the single client-a capture session the capture
// regression fixtures share.
func startDebugSession(t *testing.T, p *Server) {
	t.Helper()
	w := httptest.NewRecorder()
	p.HandleDebug(w, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":true,"clients":["client-a"]}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("start debug session = %d %s", w.Code, w.Body.String())
	}
}

// runDebugCaptureRequest serves upstream behind p and sends one chat request
// scoped to client-a with Authorization plus extra headers. It drains the
// client response and returns the finalized record once spy has observed it;
// the capture write precedes Record, so the returned record's sidecar is
// durable.
func runDebugCaptureRequest(t *testing.T, p *Server, spy *liveSpy, upstream http.Handler, body string, extra http.Header) *metrics.Record {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-secret")
	req.Header.Set("X-Proxy-Base-URL", up.URL)
	req.Header.Set("X-Proxy-Client", "client-a")
	for name, values := range extra {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	var rec *metrics.Record
	for time.Now().Before(deadline) {
		if rec = spy.final.Load(); rec != nil && rec.Debug && rec.StatusCode == http.StatusOK {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rec == nil || !rec.Debug || rec.StatusCode != http.StatusOK {
		t.Fatal("finalized record never observed with debug=true and status 200")
	}
	return rec
}

// fetchDebugCapture reads one stored sidecar through the drawer route.
func fetchDebugCapture(t *testing.T, p *Server, id string) []byte {
	t.Helper()
	w := httptest.NewRecorder()
	p.HandleDebugCapture(w, httptest.NewRequest(http.MethodGet, "/admin/debug/capture?id="+id, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("capture %s: status %d body %s", id, w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}

// TestDebugNoStoreRefusesSessionAndRetainsNothing pins the fail-closed storage
// gate: with no durable store a new capture session is refused with the shared
// sentinel text, and even an in-memory session cannot build a tap, so a
// matched request never copies headers or bodies for an unfetchable sidecar.
func TestDebugNoStoreRefusesSessionAndRetainsNothing(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})

	w := httptest.NewRecorder()
	p.HandleDebug(w, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":true,"clients":["client-a"]}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-store session start = %d %s, want 503", w.Code, w.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v (%q)", err, w.Body.String())
	}
	if body.Error != storage.ErrStorageDisabled.Error() {
		t.Fatalf("error = %q, want the storage-disabled sentinel text", body.Error)
	}
	if p.matchDebug("client-a", "any", "any") != nil {
		t.Fatal("refused session still matches")
	}

	// The HTTP gate is the production path. This internal fixture proves the
	// retention boundary itself stays closed if an enabled session is present
	// in memory anyway.
	if err := p.applyDebug([]persistedDebug{{ID: "in-memory", Clients: []string{"client-a"}}}); err != nil {
		t.Fatal(err)
	}
	defer p.applyDebug(nil)
	if p.matchDebug("client-a", "any", "any") == nil {
		t.Fatal("fixture session does not match; the tap guard would be untested")
	}
	// Editing a named session stays available (it cannot create a new one),
	// but it still cannot turn on retention without a fetchable sidecar.
	w = httptest.NewRecorder()
	p.HandleDebug(w, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":true,"id":"in-memory","duration":"1h"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("no-store named edit = %d %s, want 200", w.Code, w.Body.String())
	}
	if len(p.debug.sessions) != 1 {
		t.Fatalf("named edit changed the session count to %d", len(p.debug.sessions))
	}
	req := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", strings.NewReader("body"))
	req.Header.Set("Authorization", "Bearer sk-secret")
	tap := p.beginDebugTap(req, &target{authHeader: "Authorization"}, &metrics.Record{Client: "client-a"}, []byte("secret prompt"))
	if tap != nil {
		t.Fatalf("no-store proxy built a capture tap: headers=%v body=%q", tap.reqHdr, tap.reqBody.Raw)
	}
}

// TestDebugCaptureBoundsRequestHeaders pins the request-side retention
// boundary: a hostile megabyte-scale value is cut, the per-name value count is
// bounded, ordinary headers share the total budget, and credential redaction
// still wins before truncation for the configured auth header and cookies.
func TestDebugCaptureBoundsRequestHeaders(t *testing.T) {
	spy := &liveSpy{}
	p := newStoredTestProxy(t, spy)
	startDebugSession(t, p)

	const huge = 64 << 10
	extra := http.Header{
		"X-Huge":              {strings.Repeat("h", huge)},
		"X-Many":              {"v0", "v1", "v2", "v3", "v4", "v5", "v6", "v7", "v8", "v9", "v10", "v11", "v12", "v13", "v14", "v15", "v16", "v17", "v18", "v19"},
		"X-Custom-Auth":       {strings.Repeat("s", huge)},
		"X-Proxy-Auth-Header": {"X-Custom-Auth"},
		"Cookie":              {"session=cookie-secret"},
	}
	for i := 0; i < 40; i++ {
		extra.Add(fmt.Sprintf("X-Z-Bulk-%02d", i), strings.Repeat("b", 1<<10))
	}
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[]}`))
	})
	rec := runDebugCaptureRequest(t, p, spy, upstream, `{"model":"m","messages":[]}`, extra)
	raw := fetchDebugCapture(t, p, rec.ID)

	var doc struct {
		Request struct {
			Headers []harNV `json:"headers"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("capture doc: %v", err)
	}
	headers := make(map[string]string, len(doc.Request.Headers))
	for _, h := range doc.Request.Headers {
		headers[h.Name] = h.Value
	}

	for _, name := range []string{"Authorization", "Cookie", "X-Custom-Auth"} {
		if got := headers[name]; got != "[REDACTED]" {
			t.Errorf("%s = %q, want [REDACTED] before any truncation", name, got)
		}
	}
	if got := headers["X-Huge"]; len(got) > retainedHeaderValueMax {
		t.Errorf("X-Huge retained %d bytes, want <= %d", len(got), retainedHeaderValueMax)
	} else if !strings.HasSuffix(got, retainedTruncationSuffix) {
		t.Errorf("X-Huge = %q, want the truncation marker", got)
	}
	wantMany := make([]string, retainedHeaderValuesMax)
	for i := range wantMany {
		wantMany[i] = fmt.Sprintf("v%d", i)
	}
	if got := headers["X-Many"]; got != strings.Join(wantMany, ", ") {
		t.Errorf("X-Many = %q, want the first %d values", got, retainedHeaderValuesMax)
	}
	// The configured auth credential is 64 KiB; a truncated prefix must not
	// survive, so no long run of its bytes can appear anywhere in the capture.
	if bytes.Contains(raw, []byte(strings.Repeat("s", retainedHeaderValueMax+1))) {
		t.Fatal("capture retained a prefix of the configured auth credential")
	}

	total, bulk := 0, 0
	for _, h := range doc.Request.Headers {
		if h.Value == "[REDACTED]" {
			continue
		}
		total += len(h.Name) + len(h.Value)
		if strings.HasPrefix(h.Name, "X-Z-Bulk-") {
			bulk++
		}
	}
	if total > retainedHeadersMaxBytes {
		t.Errorf("retained request header bytes = %d, want <= %d", total, retainedHeadersMaxBytes)
	}
	if bulk == 0 || bulk == 40 {
		t.Errorf("retained %d of 40 bulk headers, want a bounded subset", bulk)
	}
}

// TestDebugCaptureDoesNotHTMLEscape pins the encoder choice: <, > and & are
// stored literally rather than as six-byte \u00XX sequences, while the decoded
// document still carries exactly the captured strings.
func TestDebugCaptureDoesNotHTMLEscape(t *testing.T) {
	spy := &liveSpy{}
	p := newStoredTestProxy(t, spy)
	startDebugSession(t, p)

	const reqBody = `{"model":"m","messages":[{"role":"user","content":"<prompt> a & b"}]}`
	const respBody = `{"id":"1","choices":[{"message":{"role":"assistant","content":"<answer> c & d"}}]}`
	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(respBody))
	})
	rec := runDebugCaptureRequest(t, p, spy, upstream, reqBody, http.Header{
		"X-Html": {"<left> & <right>"},
	})
	raw := fetchDebugCapture(t, p, rec.ID)

	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if bytes.Contains(raw, []byte(escaped)) {
			t.Fatalf("capture still carries the HTML escape %s: %s", escaped, raw)
		}
	}
	var doc struct {
		Request struct {
			Headers []harNV   `json:"headers"`
			Body    debugBody `json:"body"`
		} `json:"request"`
		Response struct {
			Body debugBody `json:"body"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("capture doc: %v", err)
	}
	if doc.Request.Body.Raw != reqBody {
		t.Fatalf("request body decoded to %q, want the original %q", doc.Request.Body.Raw, reqBody)
	}
	if doc.Response.Body.Raw != respBody {
		t.Fatalf("response body decoded to %q, want the original %q", doc.Response.Body.Raw, respBody)
	}
	found := false
	for _, h := range doc.Request.Headers {
		if h.Name == "X-Html" {
			found = true
			if h.Value != "<left> & <right>" {
				t.Fatalf("X-Html decoded to %q, want the original", h.Value)
			}
		}
	}
	if !found {
		t.Fatal("X-Html header missing from the request capture")
	}
}
