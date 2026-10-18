package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
// client response and returns the finalized record once spy has observed it.
// Record runs before the sidecar write, so a subsequent drawer fetch waits on
// the pending marker and still sees the stored capture.
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

// captureSaveGate wraps a real store and parks SaveDebugCapture until release
// closes. entered fires while the save is blocked, so an ordering test can
// observe exactly what the request path published before the sidecar write.
type captureSaveGate struct {
	pausePersist
	entered chan string
	release chan struct{}
	err     error
	once    sync.Once
	calls   atomic.Int32
}

func newCaptureSaveGate(store pausePersist) *captureSaveGate {
	return &captureSaveGate{
		pausePersist: store,
		entered:      make(chan string, 1),
		release:      make(chan struct{}),
	}
}

func (g *captureSaveGate) SaveDebugCapture(ctx context.Context, id, sessionID string, capturedAt, expiresAt int64, payload []byte) error {
	g.calls.Add(1)
	select {
	case g.entered <- id:
	default:
	}
	<-g.release
	if g.err != nil {
		return g.err
	}
	return g.pausePersist.SaveDebugCapture(ctx, id, sessionID, capturedAt, expiresAt, payload)
}

func (g *captureSaveGate) unblock() { g.once.Do(func() { close(g.release) }) }

func (g *captureSaveGate) saveCalls() int { return int(g.calls.Load()) }

// captureLoadErrorPersist keeps the real store's writes but fails the drawer
// read, pinning the unchanged 500 branch after a marker resolves.
type captureLoadErrorPersist struct{ pausePersist }

func (captureLoadErrorPersist) LoadDebugCapture(context.Context, string) ([]byte, error) {
	return nil, errors.New("capture store unavailable")
}

// startDebugCaptureRequest issues one client-a chat request against p in the
// background and reports the client-side result. The handler can still be in
// its completion defer after the response body arrives, so callers that need
// the deferred work wait on the capture seam or the pending owner instead.
func startDebugCaptureRequest(t *testing.T, p *Server, upstream http.Handler, body string) <-chan error {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	done := make(chan error, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			done <- err
			return
		}
		req.Header.Set("Authorization", "Bearer sk-secret")
		req.Header.Set("X-Proxy-Base-URL", up.URL)
		req.Header.Set("X-Proxy-Client", "client-a")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- err
			return
		}
		_, err = io.Copy(io.Discard, resp.Body)
		if closeErr := resp.Body.Close(); err == nil {
			err = closeErr
		}
		done <- err
	}()
	return done
}

// waitLiveEvent reads one lifecycle event, failing the test on timeout.
func waitLiveEvent(t *testing.T, live <-chan *metrics.LiveEvent) *metrics.LiveEvent {
	t.Helper()
	select {
	case ev := <-live:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("live lifecycle event not observed")
		return nil
	}
}

// pendingCaptureCount reports the registered in-flight sidecar markers.
func pendingCaptureCount(p *Server) int {
	p.debug.mu.Lock()
	defer p.debug.mu.Unlock()
	return len(p.debug.pendingCaptures)
}

// waitPendingCapturesEmpty waits for every in-flight marker to resolve. A
// marker left behind would hang the next drawer fetch for that id.
func waitPendingCapturesEmpty(t *testing.T, p *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pendingCaptureCount(p) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("pending capture markers did not resolve: %d left", pendingCaptureCount(p))
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

// TestDebugCaptureSaveFailureResolvesPendingMarker pins the failure path: the
// completed record and live end still publish before the sidecar write fails,
// the pending marker resolves on the error, and a later fetch falls through to
// the existing 404 because nothing was stored. A marker left registered would
// hang that fetch indefinitely.
func TestDebugCaptureSaveFailureResolvesPendingMarker(t *testing.T) {
	store, _ := openProxyTestStore(t, filepath.Join(t.TempDir(), "capture-save-failure.db"))
	gate := newCaptureSaveGate(store)
	gate.err = errors.New("sidecar unavailable")
	defer gate.unblock()
	buf := metrics.NewBuffer(8)
	p := New(config.Default(), buf)
	p.AttachPausePersist(gate)
	startDebugSession(t, p)
	live := buf.SubscribeLive()

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[]}`))
	})
	request := startDebugCaptureRequest(t, p, upstream, `{"model":"m"}`)

	begin := waitLiveEvent(t, live)
	if begin.Phase != "begin" || begin.Record == nil {
		t.Fatalf("first live event = %+v, want begin", begin)
	}
	id := begin.Record.ID
	select {
	case got := <-gate.entered:
		if got != id {
			t.Fatalf("SaveDebugCapture id = %q, want %q", got, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SaveDebugCapture never entered")
	}

	end := waitLiveEvent(t, live)
	if end.Phase != "end" || end.Record == nil || end.Record.ID != id {
		t.Fatalf("live event while failing save is blocked = %+v, want end for %s", end, id)
	}

	gate.unblock()
	if err := <-request; err != nil {
		t.Fatalf("client request: %v", err)
	}
	waitPendingCapturesEmpty(t, p)
	if calls := gate.saveCalls(); calls != 1 {
		t.Fatalf("SaveDebugCapture calls = %d, want exactly 1", calls)
	}

	w := httptest.NewRecorder()
	p.HandleDebugCapture(w, httptest.NewRequest(http.MethodGet, "/admin/debug/capture?id="+id, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("failed save fetch = %d %s, want the existing 404", w.Code, w.Body.String())
	}
	if n := pendingCaptureCount(p); n != 0 {
		t.Fatalf("pending capture markers = %d after the failed save, want 0", n)
	}

	// The other half of the existing contract: once no marker is pending, a
	// storage read error still answers 500 rather than being masked as 404.
	p.pause.persist = captureLoadErrorPersist{pausePersist: store}
	w = httptest.NewRecorder()
	p.HandleDebugCapture(w, httptest.NewRequest(http.MethodGet, "/admin/debug/capture?id="+id, nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("load-error fetch = %d %s, want the existing 500", w.Code, w.Body.String())
	}
}

// TestDebugCaptureWaitStopsWithRequestContext pins the disconnect contract:
// a drawer fetch waiting on an in-flight sidecar save stops when its request
// context ends instead of outliving the client.
func TestDebugCaptureWaitStopsWithRequestContext(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	release := p.registerPendingCapture("ctx-capture")
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan bool, 1)
	go func() { waited <- p.waitPendingCapture(ctx, "ctx-capture") }()
	cancel()
	select {
	case resolved := <-waited:
		if resolved {
			t.Fatal("wait reported the capture resolved on context cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait ignored request context cancellation")
	}
}

// TestDebugPendingCaptureOnlyExistsForRealCaptures pins the cheap paths: an
// ordinary (non-debug) request never registers a marker or reaches
// SaveDebugCapture, and a debug record without a store cannot register either.
// The blocking gate stays closed for the whole test, so a stray save would
// stall the request instead of passing silently.
func TestDebugPendingCaptureOnlyExistsForRealCaptures(t *testing.T) {
	store, _ := openProxyTestStore(t, filepath.Join(t.TempDir(), "capture-none.db"))
	gate := newCaptureSaveGate(store)
	defer gate.unblock()
	buf := metrics.NewBuffer(8)
	p := New(config.Default(), buf)
	p.AttachPausePersist(gate)

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[]}`))
	})
	request := startDebugCaptureRequest(t, p, upstream, `{"model":"m"}`)
	select {
	case err := <-request:
		if err != nil {
			t.Fatalf("client request: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("non-debug request blocked in the capture save path")
	}
	records := waitForRecord(t, buf, 1)
	if len(records) != 1 || records[0].Debug {
		t.Fatalf("finalized records = %+v, want one non-debug record", records)
	}
	if calls := gate.saveCalls(); calls != 0 {
		t.Fatalf("SaveDebugCapture calls = %d for a non-debug request, want 0", calls)
	}
	if n := pendingCaptureCount(p); n != 0 {
		t.Fatalf("pending capture markers = %d for a non-debug request, want 0", n)
	}

	if save := p.prepareDebugCapture(&debugTap{}, &metrics.Record{ID: "no-debug"}); save != nil {
		t.Fatal("a non-debug record returned a save closure")
	}
	if save := p.prepareDebugCapture(nil, &metrics.Record{ID: "no-tap", Debug: true}); save != nil {
		t.Fatal("a missing tap returned a save closure")
	}

	bare := New(config.Default(), metrics.Noop{})
	if save := bare.prepareDebugCapture(&debugTap{}, &metrics.Record{ID: "no-store", Debug: true}); save != nil {
		t.Fatal("a store-less proxy returned a save closure")
	}
	if n := pendingCaptureCount(bare); n != 0 {
		t.Fatalf("store-less pending capture markers = %d, want 0", n)
	}
}

// TestDebugCaptureFastPathAddsNoAllocation pins the cheap request path: the
// preparation seam allocates nothing when there is no tap, the record is not a
// debug capture, or no durable store is wired.
func TestDebugCaptureFastPathAddsNoAllocation(t *testing.T) {
	store, _ := openProxyTestStore(t, filepath.Join(t.TempDir(), "capture-fast-path.db"))
	p := New(config.Default(), metrics.Noop{})
	p.AttachPausePersist(store)
	bare := New(config.Default(), metrics.Noop{})
	tap := &debugTap{}
	plain := &metrics.Record{ID: "plain"}
	debugRec := &metrics.Record{ID: "debug", Debug: true}

	for _, tc := range []struct {
		name    string
		prepare func() func()
	}{
		{"no tap", func() func() { return p.prepareDebugCapture(nil, plain) }},
		{"not a debug record", func() func() { return p.prepareDebugCapture(tap, plain) }},
		{"no durable store", func() func() { return bare.prepareDebugCapture(tap, debugRec) }},
	} {
		var save func()
		allocs := testing.AllocsPerRun(100, func() { save = tc.prepare() })
		if allocs != 0 || save != nil {
			t.Errorf("%s: allocations = %v, closure returned = %t, want 0 and false", tc.name, allocs, save != nil)
		}
	}
	if n := pendingCaptureCount(p); n != 0 {
		t.Fatalf("pending capture markers = %d on the fast path, want 0", n)
	}
}

// TestDebugCaptureFetchesBeforeAndAfterMarkerResolve proves both drawer paths:
// fetches that arrive while the sidecar save is blocked wait for the same
// stored document instead of observing a transient 404, and a fetch after the
// marker is removed loads that document normally.
func TestDebugCaptureFetchesBeforeAndAfterMarkerResolve(t *testing.T) {
	store, _ := openProxyTestStore(t, filepath.Join(t.TempDir(), "capture-waiters.db"))
	gate := newCaptureSaveGate(store)
	defer gate.unblock()
	spy := &liveSpy{}
	p := New(config.Default(), spy)
	p.AttachPausePersist(gate)
	startDebugSession(t, p)

	upstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"1","choices":[]}`))
	})
	request := startDebugCaptureRequest(t, p, upstream, `{"model":"m"}`)
	var id string
	select {
	case id = <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("SaveDebugCapture never entered")
	}
	if n := pendingCaptureCount(p); n != 1 {
		t.Fatalf("pending capture markers = %d while the save is blocked, want 1", n)
	}

	const waiters = 4
	results := make(chan *httptest.ResponseRecorder, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			w := httptest.NewRecorder()
			p.HandleDebugCapture(w, httptest.NewRequest(http.MethodGet, "/admin/debug/capture?id="+id, nil))
			results <- w
		}()
	}
	select {
	case w := <-results:
		t.Fatalf("waiter answered %d before the sidecar save resolved: %s", w.Code, w.Body.String())
	case <-time.After(100 * time.Millisecond):
	}

	gate.unblock()
	if err := <-request; err != nil {
		t.Fatalf("client request: %v", err)
	}

	var stored []byte
	for i := 0; i < waiters; i++ {
		var w *httptest.ResponseRecorder
		select {
		case w = <-results:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d waiters resumed after the save resolved", i, waiters)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("waiter %d status = %d body %s, want the stored capture", i, w.Code, w.Body.String())
		}
		if i == 0 {
			stored = append([]byte(nil), w.Body.Bytes()...)
			continue
		}
		if !bytes.Equal(stored, w.Body.Bytes()) {
			t.Fatalf("waiter %d body differs from the first waiter", i)
		}
	}

	waitPendingCapturesEmpty(t, p)
	after := fetchDebugCapture(t, p, id)
	if !bytes.Equal(stored, after) {
		t.Fatal("fetch after marker removal differs from the waiting fetches")
	}
}
