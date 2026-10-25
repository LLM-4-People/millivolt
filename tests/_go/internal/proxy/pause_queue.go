package proxy

import (
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

func TestPauseQueuesNewRequestsUntilUnpause(t *testing.T) {
	var hits atomic.Int32
	hold := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			<-hold // first request stays in-flight until we release it
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	p := New(config.Default(), metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	newReq := func() *http.Request {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		return req
	}

	// Start an in-flight request that blocks in the upstream handler.
	firstDone := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(newReq())
		if err != nil {
			t.Error(err)
			firstDone <- -1
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		firstDone <- resp.StatusCode
	}()

	// Wait until the first request has hit upstream.
	if !waitFor(t, func() bool { return hits.Load() != 0 }) {
		t.Fatalf("first request never reached upstream (hits=%d)", hits.Load())
	}

	p.applyHolds([]persistedPause{{All: true}})

	secondDone := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(newReq())
		if err != nil {
			t.Error(err)
			secondDone <- -1
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		secondDone <- resp.StatusCode
	}()

	// Parked IS observable: the hold queue counts the paused request, so poll
	// for it instead of a fixed window - if the proxy were broken and the
	// request slipped through to upstream, it would complete and never park.
	if !waitFor(t, func() bool { return p.scheduler.Stats().Queued >= 1 }) {
		t.Fatalf("paused request never parked (queued=%d, upstream hits=%d)", p.scheduler.Stats().Queued, hits.Load())
	}
	if hits.Load() != 1 {
		t.Fatalf("paused request hit upstream (hits=%d)", hits.Load())
	}

	// In-flight finishes while still paused; the queued request stays queued.
	close(hold)
	select {
	case code := <-firstDone:
		if code != 200 {
			t.Fatalf("in-flight status = %d, want 200", code)
		}
	case <-time.After(time.Second):
		t.Fatal("in-flight request did not finish")
	}
	if hits.Load() != 1 {
		t.Fatalf("queued request ran after in-flight finished while paused (hits=%d)", hits.Load())
	}

	p.applyHolds(nil)
	select {
	case code := <-secondDone:
		if code != 200 {
			t.Fatalf("queued status = %d, want 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued request did not run after unpause")
	}
	if hits.Load() != 2 {
		t.Fatalf("upstream hits = %d, want 2", hits.Load())
	}
}

type liveSpy struct {
	metrics.Noop
	last  atomic.Pointer[metrics.Record]
	final atomic.Pointer[metrics.Record]
}

func (s *liveSpy) PublishLive(_ string, r *metrics.Record) {
	if r == nil {
		return
	}
	c := *r
	s.last.Store(&c)
}

// Record captures the finalized record, the sink path live publishes never
// see (PublishLive fires at acceptance with StatusCode still 0). Tests that
// must observe a completed outcome poll final; last stays the live view.
func (s *liveSpy) Record(r *metrics.Record) {
	if r == nil {
		return
	}
	c := *r
	s.final.Store(&c)
}

func TestPausedRequestPublishedAsPaused(t *testing.T) {
	spy := &liveSpy{}
	p := New(config.Default(), spy)
	p.applyHolds([]persistedPause{{All: true}})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	srv := httptest.NewServer(p)
	defer srv.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Client", "client-a")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	if waitFor(t, func() bool { rec := spy.last.Load(); return rec != nil && rec.Paused }) {
		p.applyHolds(nil)
		<-done
		return
	}
	p.applyHolds(nil)
	<-done
	t.Fatal("live record never published with paused=true")
}

// TestReplaceHoldPublishesPausedOnCapWaiter: a request is already live
// (streaming, not paused) because it is queued for concurrency. Updating the
// existing hold so it newly matches must publish paused=true - otherwise the
// dashboard keeps the "streaming" pill while the request never goes through.
func TestReplaceHoldPublishesPausedOnCapWaiter(t *testing.T) {
	spy := &liveSpy{}
	cfg := config.Default()
	cfg.MaxConcurrent = 1
	p := New(cfg, spy)

	gate := make(chan struct{})
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-gate
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"providers":["other.land"]}`)))
	if rr.Code != 200 {
		t.Fatalf("add = %d %s", rr.Code, rr.Body.Bytes())
	}
	id := pauseJSON(t, rr)["holds"].([]any)[0].(map[string]any)["id"].(string)
	provider := providerFromURL(mustParseURL(t, upstream.URL))

	srv := httptest.NewServer(p)
	defer srv.Close()

	newReq := func() *http.Request {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Client", "client-a")
		return req
	}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		resp, err := http.DefaultClient.Do(newReq())
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	if !waitFor(t, func() bool { return hits.Load() != 0 }) {
		t.Fatal("first request never reached upstream")
	}

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		resp, err := http.DefaultClient.Do(newReq())
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	if !waitFor(t, func() bool { return p.scheduler.Stats().Queued >= 1 }) {
		t.Fatalf("queued = %d, want 1", p.scheduler.Stats().Queued)
	}

	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"all":false,"new":false,"clients":[],"providers":["`+provider+`"],"duration":"","id":"`+id+`","max_queued":0}`)))
	if rr.Code != 200 {
		t.Fatalf("update = %d %s", rr.Code, rr.Body.Bytes())
	}

	if waitFor(t, func() bool { rec := spy.last.Load(); return rec != nil && rec.Paused }) {
		close(gate)
		<-firstDone
		p.removePause(id, time.Time{})
		<-secondDone
		return
	}
	close(gate)
	<-firstDone
	p.removePause(id, time.Time{})
	<-secondDone
	t.Fatal("live record stayed streaming after hold update blocked the waiter")
}

func TestModelsBypassesPause(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()
	p := New(config.Default(), metrics.Noop{})
	p.applyHolds([]persistedPause{{All: true}})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		p.ServeHTTP(rr, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("GET /v1/models blocked on pause")
	}
	if rr.Code != 200 {
		t.Fatalf("models status = %d body %s", rr.Code, rr.Body.Bytes())
	}
}
