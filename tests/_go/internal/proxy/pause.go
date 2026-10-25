package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestHandlePauseRequiresPausedField(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	req := httptest.NewRequest(http.MethodPost, "/admin/pause", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()
	p.HandlePause(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for missing paused field", rr.Code)
	}
}

func TestHandlePauseGetAndPost(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})

	get := httptest.NewRecorder()
	p.HandlePause(get, httptest.NewRequest(http.MethodGet, "/admin/pause", nil))
	if get.Code != 200 {
		t.Fatalf("GET status = %d", get.Code)
	}
	var st map[string]any
	if err := json.Unmarshal(get.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["paused"] != false {
		t.Fatalf("GET paused = %v, want false", st["paused"])
	}

	post := httptest.NewRecorder()
	p.HandlePause(post, httptest.NewRequest(http.MethodPost, "/admin/pause", strings.NewReader(`{"paused":true}`)))
	if post.Code != 200 {
		t.Fatalf("POST status = %d body %s", post.Code, post.Body.Bytes())
	}
	if err := json.Unmarshal(post.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["paused"] != true {
		t.Fatalf("POST paused = %v, want true", st["paused"])
	}
	if !p.scheduler.Stats().Paused {
		t.Fatal("scheduler not paused after POST")
	}
}

func TestHandlePauseRejectsBadDuration(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"duration":"30s"}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandlePauseClientsAndNew(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.pause.clients.add("client-a")
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"new":true,"clients":["client-a"],"duration":"1h"}`)))
	if rr.Code != 200 {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.Bytes())
	}
	var st map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["paused"] != true {
		t.Fatalf("paused = %v, want true", st["paused"])
	}
	holds, _ := st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("holds = %v", st["holds"])
	}
	h := holds[0].(map[string]any)
	if h["new"] != true || h["all"] != false || h["duration"] != "1h" {
		t.Fatalf("hold = %v, want new=true all=false duration=1h", h)
	}
	if !p.scheduler.Holds("client-a") || !p.scheduler.Holds("stranger") {
		t.Fatal("expected named + unseen clients held")
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func pauseJSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var st map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("json: %v body %s", err, rr.Body.Bytes())
	}
	return st
}

func TestHandlePauseOverlap409(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"clients":["client-a"]}`)))
	if rr.Code != 200 {
		t.Fatalf("first = %d %s", rr.Code, rr.Body.Bytes())
	}
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"providers":["alpha.example"]}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("overlap status = %d, want 409 body %s", rr.Code, rr.Body.Bytes())
	}
}

func TestHandlePauseProviderAndSecondClient(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.pause.providers.add("alpha.example")
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"providers":["alpha.example"],"max_queued":2}`)))
	if rr.Code != 200 {
		t.Fatalf("status = %d %s", rr.Code, rr.Body.Bytes())
	}
	st := pauseJSON(t, rr)
	if st["paused"] != true {
		t.Fatalf("state = %v", st)
	}
	if !p.scheduler.HoldsTarget("anyone", "alpha.example") || p.scheduler.HoldsTarget("anyone", "beta.example") {
		t.Fatal("provider hold mismatch")
	}
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"clients":["client-a"]}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("client+provider overlap = %d", rr.Code)
	}
}

func TestHandlePauseTwoNonOverlappingAndResumeOne(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	for _, body := range []string{
		`{"paused":true,"clients":["client-a"],"duration":"1h"}`,
		`{"paused":true,"clients":["client-b"],"duration":"15m"}`,
	} {
		rr := httptest.NewRecorder()
		p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause", strings.NewReader(body)))
		if rr.Code != 200 {
			t.Fatalf("post %s = %d %s", body, rr.Code, rr.Body.Bytes())
		}
	}
	if !p.scheduler.Holds("client-a") || !p.scheduler.Holds("client-b") {
		t.Fatal("both clients should be held")
	}
	holds := p.PauseSnapshot()["holds"].([]map[string]any)
	if len(holds) != 2 {
		t.Fatalf("holds = %d, want 2", len(holds))
	}
	var idOpen string
	for _, h := range holds {
		for _, c := range h["clients"].([]string) {
			if c == "client-a" {
				idOpen = h["id"].(string)
			}
		}
	}
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":false,"id":"`+idOpen+`"}`)))
	if rr.Code != 200 {
		t.Fatalf("resume one = %d %s", rr.Code, rr.Body.Bytes())
	}
	if p.scheduler.Holds("client-a") || !p.scheduler.Holds("client-b") {
		t.Fatal("resuming one hold leaked")
	}
}

func TestHandlePauseDefaultMaxQueued(t *testing.T) {
	// MaxConcurrent is a per-group concurrency cap, not a queue cap: it
	// must not feed the hold's default queue capacity.
	d2 := config.Default()
	d2.MaxConcurrent = 7
	d2.MaxQueueSize = 0 // unlimited by explicit config, not by MaxConcurrent
	p2 := New(d2, metrics.Noop{})
	rr2 := httptest.NewRecorder()
	p2.HandlePause(rr2, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"clients":["c"]}`)))
	if st := pauseJSON(t, rr2); st["default_max_queued"] != float64(0) {
		t.Fatalf("default_max_queued = %v, want 0: MaxConcurrent must not feed the hold default", st["default_max_queued"])
	}
	d := config.Default()
	d.MaxQueueSize = 4
	p := New(d, metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"clients":["c"]}`)))
	st := pauseJSON(t, rr)
	holds, _ := st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("holds = %v", st["holds"])
	}
	h := holds[0].(map[string]any)
	if h["max_queued"] != float64(4) {
		t.Fatalf("max_queued = %v, want 4", h["max_queued"])
	}
	if st["default_max_queued"] != float64(4) {
		t.Fatalf("default_max_queued = %v, want 4", st["default_max_queued"])
	}
}

func TestHandlePauseWhitespaceClients400(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"clients":["  ",""]}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body %s", rr.Code, rr.Body.Bytes())
	}
	if p.scheduler.Paused() {
		t.Fatal("whitespace clients must not become a global hold")
	}
}

func TestHandlePauseEmptyClients400(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"clients":[]}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandlePauseTwoNew409(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"new":true}`)))
	if rr.Code != 200 {
		t.Fatalf("first new = %d %s", rr.Code, rr.Body.Bytes())
	}
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"new":true}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("second new = %d, want 409", rr.Code)
	}
}

func TestHandlePauseNegativeMaxQueued(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"max_queued":-1}`)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}

func TestHandlePauseConcurrentNonOverlap(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	var wg sync.WaitGroup
	wg.Add(2)
	codes := make(chan int, 2)
	go func() {
		defer wg.Done()
		rr := httptest.NewRecorder()
		p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
			strings.NewReader(`{"paused":true,"clients":["a"]}`)))
		codes <- rr.Code
	}()
	go func() {
		defer wg.Done()
		rr := httptest.NewRecorder()
		p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
			strings.NewReader(`{"paused":true,"clients":["b"]}`)))
		codes <- rr.Code
	}()
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 200 {
			t.Fatalf("concurrent add status = %d", code)
		}
	}
	if !p.scheduler.Holds("a") || !p.scheduler.Holds("b") {
		t.Fatalf("lost a concurrent hold: a=%v b=%v",
			p.scheduler.Holds("a"), p.scheduler.Holds("b"))
	}
}

// TestPauseStreamSendsSSEKeepalive pins the OpenAI-style comment pacer: a
// held stream=true request commits text/event-stream and flushes
// ": keepalive" before any upstream byte, so clients can sit on the hold
// indefinitely.
func TestPauseStreamSendsSSEKeepalive(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"x\"}\n\n"))
	}))
	defer upstream.Close()

	p := New(config.Default(), metrics.Noop{})
	p.applyHolds([]persistedPause{{All: true}})
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	if hits.Load() != 0 {
		t.Fatal("paused stream reached upstream before keepalive")
	}
	buf := make([]byte, 64)
	nch := make(chan int, 1)
	go func() {
		n, _ := resp.Body.Read(buf)
		nch <- n
	}()
	var n int
	select {
	case n = <-nch:
	case <-time.After(time.Second):
		t.Fatal("no keepalive comment")
	}
	got := string(buf[:n])
	if !strings.Contains(got, ": keepalive") {
		t.Fatalf("body %q, want SSE comment : keepalive", got)
	}
}

// TestQueueStreamSendsSSEKeepalive pins the writer-level pacer on a
// concurrency-cap wait (no operator hold): the queued stream commits SSE
// and flushes ": keepalive" before it is allowed upstream.
func TestQueueStreamSendsSSEKeepalive(t *testing.T) {
	var hits atomic.Int32
	hold := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			<-hold
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"x\"}\n\n"))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxConcurrent = 1
	p := New(cfg, metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	newReq := func(stream bool) *http.Request {
		body := `{"model":"m"}`
		if stream {
			body = `{"model":"m","stream":true}`
		}
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		return req
	}

	firstDone := make(chan struct{})
	go func() {
		resp, err := http.DefaultClient.Do(newReq(false))
		if err != nil {
			t.Error(err)
			close(firstDone)
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		close(firstDone)
	}()
	if !waitFor(t, func() bool { return hits.Load() != 0 }) {
		t.Fatalf("first request never reached upstream (hits=%d)", hits.Load())
	}

	resp, err := http.DefaultClient.Do(newReq(true))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("queued stream status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	if hits.Load() != 1 {
		t.Fatal("queued stream reached upstream before keepalive")
	}
	buf := make([]byte, 64)
	nch := make(chan int, 1)
	go func() {
		n, _ := resp.Body.Read(buf)
		nch <- n
	}()
	var n int
	select {
	case n = <-nch:
	case <-time.After(time.Second):
		t.Fatal("no keepalive comment on queued stream")
	}
	got := string(buf[:n])
	if !strings.Contains(got, ": keepalive") {
		t.Fatalf("body %q, want SSE comment : keepalive", got)
	}
	close(hold)
	<-firstDone
}

// TestStreamKeepaliveDuringIdle pins the same writer pacer mid-stream: a
// quiet gap after the first upstream chunk must emit ": keepalive" even
// though the request is not paused or queued.
func TestStreamKeepaliveDuringIdle(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
		if fl != nil {
			fl.Flush()
		}
		time.Sleep(70 * time.Millisecond)
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.SSEKeepaliveInterval = 20 * time.Millisecond
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, ": keepalive") {
		t.Fatalf("body %q, want SSE comment : keepalive between chunks", got)
	}
	if !strings.Contains(got, `"content":"a"`) || !strings.Contains(got, `"content":"b"`) {
		t.Fatalf("body %q, want both upstream chunks", got)
	}
}

// TestQueueWaitExceededAfterKeepaliveEmitsSSEError: OnWait commits 200 SSE,
// then max_queue_wait fires. The client must get an in-band error + [DONE],
// not a silent hang-up.
func TestQueueWaitExceededAfterKeepaliveEmitsSSEError(t *testing.T) {
	hold := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"x\"}\n\n"))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxConcurrent = 1
	cfg.MaxQueueWait = 80 * time.Millisecond
	p := New(cfg, metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	firstDone := make(chan struct{})
	go func() {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		close(firstDone)
	}()
	// The first request holds the only concurrency slot; the second must queue
	// and observe the queue-wait expiry.
	if !waitFor(t, func() bool { return p.scheduler.Stats().InFlight >= 1 }) {
		t.Fatal("first request never occupied the concurrency slot")
	}

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (already committed SSE)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, `"type":"rate_limit_error"`) {
		t.Fatalf("body %q, want in-band rate_limit_error", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("body %q, want [DONE]", got)
	}
	close(hold)
	<-firstDone
}

// TestSlowUpstreamJSONErrorKeepsStatus: the pacer does not commit SSE before
// upstream headers because the body may be JSON. A slow 429 therefore keeps
// its real status and content type instead of becoming an in-band error.
func TestSlowUpstreamJSONErrorKeepsStatus(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxRetries = 0
	cfg.SSEKeepaliveInterval = 15 * time.Millisecond
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if strings.Contains(got, ": keepalive") {
		t.Fatalf("JSON error body carried SSE keepalives: %q", got)
	}
	if !strings.Contains(got, "rate_limit_error") {
		t.Fatalf("body %q, want the provider error JSON", got)
	}
}

// TestPauseSnapshotMatchesGET: PauseSnapshot is the single owner of the
// /admin/pause state shape - the builder the HTTP GET writes must be
// byte-equivalent to what the dashboard bootstrap payload carries, so the
// two surfaces can never drift.
func TestPauseSnapshotMatchesGET(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})

	get := httptest.NewRecorder()
	p.HandlePause(get, httptest.NewRequest(http.MethodGet, "/admin/pause", nil))

	snap, err := json.Marshal(p.PauseSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if get.Body.String() != string(snap)+"\n" && get.Body.String() != string(snap) {
		// The operator response marshals the same map; compare canonically.
		var a, b map[string]any
		if err := json.Unmarshal(get.Body.Bytes(), &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(snap, &b); err != nil {
			t.Fatal(err)
		}
		ka, _ := json.Marshal(a)
		kb, _ := json.Marshal(b)
		if string(ka) != string(kb) {
			t.Fatalf("GET body %s != PauseSnapshot %s", get.Body.String(), snap)
		}
	}

	// The state must also track a live hold.
	post := httptest.NewRecorder()
	p.HandlePause(post, httptest.NewRequest(http.MethodPost, "/admin/pause", strings.NewReader(`{"paused":true,"all":true,"duration":"1h"}`)))
	if post.Code != 200 {
		t.Fatalf("POST add hold → %d body %s", post.Code, post.Body.String())
	}
	get2 := httptest.NewRecorder()
	p.HandlePause(get2, httptest.NewRequest(http.MethodGet, "/admin/pause", nil))
	var st map[string]any
	if err := json.Unmarshal(get2.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	holds, ok := st["holds"].([]any)
	if !ok || len(holds) != 1 {
		t.Fatalf("holds = %v, want 1 live hold", st["holds"])
	}
	if st["paused"] != true {
		t.Fatalf("paused = %v, want true with a live hold", st["paused"])
	}
}
