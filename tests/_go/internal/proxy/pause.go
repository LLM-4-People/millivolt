package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
	"github.com/LLM-4-People/millivolt/internal/storage"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
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
	if !p.PauseStats().Paused {
		t.Fatal("scheduler not paused after POST")
	}
}

// TestPauseQueuesNewRequestsUntilUnpause is the end-to-end operator-pause
// contract: an in-flight upstream call finishes, a request that arrives
// while paused never hits upstream until resume.
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
	deadline := time.Now().Add(time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() != 1 {
		t.Fatalf("first request never reached upstream (hits=%d)", hits.Load())
	}

	p.SetPaused(true)

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
	parkDeadline := time.Now().Add(2 * time.Second)
	for p.PauseStats().Queued < 1 && time.Now().Before(parkDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if q := p.PauseStats().Queued; q != 1 {
		t.Fatalf("paused request never parked (queued=%d, upstream hits=%d)", q, hits.Load())
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

	p.SetPaused(false)
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
	p.noteClient("client-a")
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

func TestPauseTimerUnpauses(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.applyHolds([]persistedPause{{All: true, Until: time.Now().Add(50 * time.Millisecond)}})
	if !p.scheduler.Paused() {
		t.Fatal("expected hold")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && p.scheduler.Paused() {
		time.Sleep(10 * time.Millisecond)
	}
	if p.scheduler.Paused() {
		t.Fatal("timer did not clear the hold")
	}
}

func TestPausePersistsAndRestores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pause.db")
	d := config.Default()
	store, err := storage.Open(path, storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval,
		QueryTimeout:  d.StorageQueryTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}

	p := New(d, metrics.Noop{})
	p.AttachPausePersist(store)
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"clients":["client-a"],"duration":"1h"}`)))
	if rr.Code != 200 {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.Bytes())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := storage.Open(path, storage.Options{
		WriteChanCap:  d.StorageWriteChanCap,
		BatchCap:      d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval,
		QueryTimeout:  d.StorageQueryTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()

	p2 := New(d, metrics.Noop{})
	p2.AttachPausePersist(store2)
	if !p2.scheduler.Holds("client-a") {
		t.Fatal("restored scheduler did not hold the client")
	}
	if p2.scheduler.Holds("other") {
		t.Fatal("restored hold leaked to other clients")
	}
	if len(p2.pause.holds) != 1 || p2.pause.holds[0].Duration != "1h" {
		t.Fatalf("restored holds = %+v, want duration 1h", p2.pause.holds)
	}
}

type liveSpy struct {
	metrics.Noop
	last atomic.Pointer[metrics.Record]
}

func (s *liveSpy) PublishLive(_ string, r *metrics.Record) {
	if r == nil {
		return
	}
	c := *r
	s.last.Store(&c)
}

func TestPausedRequestPublishedAsPaused(t *testing.T) {
	spy := &liveSpy{}
	p := New(config.Default(), spy)
	p.SetPaused(true)

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

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if rec := spy.last.Load(); rec != nil && rec.Paused {
			p.SetPaused(false)
			<-done
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	p.SetPaused(false)
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
	deadline := time.Now().Add(time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() != 1 {
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
	deadline = time.Now().Add(time.Second)
	for p.PauseStats().Queued < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.PauseStats().Queued != 1 {
		t.Fatalf("queued = %d, want 1", p.PauseStats().Queued)
	}

	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"all":false,"new":false,"clients":[],"providers":["`+provider+`"],"duration":"","id":"`+id+`","max_queued":0}`)))
	if rr.Code != 200 {
		t.Fatalf("update = %d %s", rr.Code, rr.Body.Bytes())
	}

	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if rec := spy.last.Load(); rec != nil && rec.Paused {
			close(gate)
			<-firstDone
			p.removePause(id, time.Time{})
			<-secondDone
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(gate)
	<-firstDone
	p.removePause(id, time.Time{})
	<-secondDone
	t.Fatal("live record stayed streaming after hold update blocked the waiter")
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
	p.noteProvider("alpha.example")
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
	holds := p.scheduler.HoldsList()
	if len(holds) != 2 {
		t.Fatalf("holds = %d, want 2", len(holds))
	}
	var idOpen string
	for _, h := range holds {
		for _, c := range h.Clients {
			if c == "client-a" {
				idOpen = h.ID
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
	d := config.Default()
	d.MaxConcurrent = 4
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
		t.Fatalf("default_max_queued = %v", st["default_max_queued"])
	}
}

func TestOneHoldTimerLeavesTheOther(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.applyHolds([]persistedPause{
		{Clients: []string{"short"}, Until: time.Now().Add(50 * time.Millisecond)},
		{Clients: []string{"long"}, Duration: "1h", Until: time.Now().Add(time.Hour)},
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && p.scheduler.Holds("short") {
		time.Sleep(10 * time.Millisecond)
	}
	if p.scheduler.Holds("short") {
		t.Fatal("short hold timer did not fire")
	}
	if !p.scheduler.Holds("long") {
		t.Fatal("long hold was cleared with the short timer")
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
		t.Fatalf("lost a concurrent hold: a=%v b=%v list=%v",
			p.scheduler.Holds("a"), p.scheduler.Holds("b"), p.scheduler.HoldsList())
	}
}

func TestSameUntilTimersClearBoth(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	until := time.Now().Add(50 * time.Millisecond)
	p.applyHolds([]persistedPause{
		{Clients: []string{"a"}, Until: until},
		{Clients: []string{"b"}, Until: until},
	})
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && p.scheduler.Paused() {
		time.Sleep(10 * time.Millisecond)
	}
	if p.scheduler.Paused() || p.scheduler.Holds("a") || p.scheduler.Holds("b") {
		t.Fatal("same-Until timers did not clear both holds")
	}
}

func TestGetOmitsExpiredHold(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.scheduler.RestoreHolds([]scheduler.Hold{{ID: "live", Clients: []string{"y"}}})
	p.pause.mu.Lock()
	p.pause.holds = []persistedPause{
		{ID: "dead", Clients: []string{"x"}, Until: time.Now().Add(-time.Second)},
		{ID: "live", Clients: []string{"y"}},
	}
	p.pause.mu.Unlock()
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodGet, "/admin/pause", nil))
	st := pauseJSON(t, rr)
	holds, _ := st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("holds = %v, want only the live one", st["holds"])
	}
	h := holds[0].(map[string]any)
	if h["id"] != "live" {
		t.Fatalf("hold id = %v", h["id"])
	}
}

func TestModelsBypassesPause(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()
	p := New(config.Default(), metrics.Noop{})
	p.SetPaused(true)
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

func TestHandlePauseReplaceKeepsID(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"clients":["client-a"],"duration":"1h","max_queued":2}`)))
	if rr.Code != 200 {
		t.Fatalf("add = %d %s", rr.Code, rr.Body.Bytes())
	}
	st := pauseJSON(t, rr)
	holds, _ := st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("holds = %v", st["holds"])
	}
	id := holds[0].(map[string]any)["id"].(string)
	until := holds[0].(map[string]any)["until"]
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"id":"`+id+`","clients":["client-a","client-b"],"duration":"1h","max_queued":5}`)))
	if rr.Code != 200 {
		t.Fatalf("replace = %d %s", rr.Code, rr.Body.Bytes())
	}
	st = pauseJSON(t, rr)
	holds, _ = st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("after replace holds = %v", st["holds"])
	}
	h := holds[0].(map[string]any)
	if h["id"] != id {
		t.Fatalf("id changed %v -> %v", id, h["id"])
	}
	if h["until"] != until {
		t.Fatalf("same duration reset until %v -> %v", until, h["until"])
	}
	if h["max_queued"] != float64(5) {
		t.Fatalf("max_queued = %v, want 5", h["max_queued"])
	}
	if !p.scheduler.Holds("client-a") || !p.scheduler.Holds("client-b") {
		t.Fatal("replaced hold did not cover both clients")
	}
	if n := len(p.scheduler.HoldsList()); n != 1 {
		t.Fatalf("holds = %d, want 1 (update, not add)", n)
	}
}

// TestHandlePauseReplaceFrontendUpdateKeepsProviderScope posts the exact
// dashboard Update JSON (paused/all/new/clients/providers/id/max_queued).
// A no-op edit of a provider-only hold must not become All or client-wide.
func TestHandlePauseReplaceFrontendUpdateKeepsProviderScope(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"all":false,"new":false,"clients":[],"providers":["gamma.example"],"duration":"","max_queued":0}`)))
	if rr.Code != 200 {
		t.Fatalf("add = %d %s", rr.Code, rr.Body.Bytes())
	}
	st := pauseJSON(t, rr)
	holds, _ := st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("holds = %v", st["holds"])
	}
	id := holds[0].(map[string]any)["id"].(string)
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"all":false,"new":false,"clients":[],"providers":["gamma.example"],"duration":"","id":"`+id+`","max_queued":0}`)))
	if rr.Code != 200 {
		t.Fatalf("update = %d %s", rr.Code, rr.Body.Bytes())
	}
	st = pauseJSON(t, rr)
	holds, _ = st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("after update holds = %v", st["holds"])
	}
	h := holds[0].(map[string]any)
	if h["id"] != id {
		t.Fatalf("id changed %v -> %v", id, h["id"])
	}
	if h["all"] != false {
		t.Fatalf("update set all=%v, want false", h["all"])
	}
	if !p.scheduler.HoldsTarget("client-a", "gamma.example") {
		t.Fatal("provider hold dropped")
	}
	if p.scheduler.HoldsTarget("client-a", "beta.example") || p.scheduler.Holds("client-a") {
		t.Fatal("update applied the hold to every request")
	}
	rel, err := p.scheduler.AcquireWith(context.Background(), "k", 0, scheduler.WaiterHooks{
		Client: "client-a", Provider: "beta.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	rel(0)
}

// TestHandlePauseReplaceDurationOnlyKeepsProviderScope is the dashboard
// "I only changed the timer" path: same named providers, new duration.
// Until must move; other providers must still grant.
func TestHandlePauseReplaceDurationOnlyKeepsProviderScope(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"all":false,"new":false,"clients":[],"providers":["gamma.example"],"duration":"","max_queued":0}`)))
	if rr.Code != 200 {
		t.Fatalf("add = %d %s", rr.Code, rr.Body.Bytes())
	}
	id := pauseJSON(t, rr)["holds"].([]any)[0].(map[string]any)["id"].(string)
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"all":false,"new":false,"clients":[],"providers":["gamma.example"],"duration":"15m","id":"`+id+`","max_queued":0}`)))
	if rr.Code != 200 {
		t.Fatalf("timer update = %d %s", rr.Code, rr.Body.Bytes())
	}
	st := pauseJSON(t, rr)
	h := st["holds"].([]any)[0].(map[string]any)
	if h["id"] != id || h["all"] != false || h["duration"] != "15m" || h["until"] == nil {
		t.Fatalf("timer update mutated hold %v", h)
	}
	provs, _ := h["providers"].([]any)
	if len(provs) != 1 || provs[0] != "gamma.example" {
		t.Fatalf("providers = %v, want [gamma.example]", h["providers"])
	}
	if p.scheduler.HoldsTarget("client-a", "beta.example") || p.scheduler.Holds("client-a") {
		t.Fatal("timer-only update applied the hold to every request")
	}
	if !p.scheduler.HoldsTarget("client-a", "gamma.example") {
		t.Fatal("timer-only update dropped the provider hold")
	}
	rel, err := p.scheduler.AcquireWith(context.Background(), "k", 0, scheduler.WaiterHooks{
		Client: "client-a", Provider: "beta.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	rel(0)
}

// TestHandlePauseReplaceOmittedScopeKeepsPrevious pins deny-by-default on
// edit: {"paused":true,"id"} without restated scope must keep the hold,
// not promote it to All (compat {"paused":true} is only for a new hold).
func TestHandlePauseReplaceOmittedScopeKeepsPrevious(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"providers":["gamma.example"],"duration":"1h"}`)))
	if rr.Code != 200 {
		t.Fatalf("add = %d %s", rr.Code, rr.Body.Bytes())
	}
	st := pauseJSON(t, rr)
	h0 := st["holds"].([]any)[0].(map[string]any)
	id := h0["id"].(string)
	until := h0["until"]
	rr = httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"id":"`+id+`"}`)))
	if rr.Code != 200 {
		t.Fatalf("replace omitted scope = %d %s", rr.Code, rr.Body.Bytes())
	}
	st = pauseJSON(t, rr)
	h := st["holds"].([]any)[0].(map[string]any)
	if h["all"] != false || h["until"] != until {
		t.Fatalf("omitted-scope replace mutated hold %v", h)
	}
	if p.scheduler.HoldsTarget("c", "beta.example") || !p.scheduler.HoldsTarget("c", "gamma.example") {
		t.Fatalf("omitted-scope replace became %+v", p.scheduler.HoldsList())
	}
}

func TestHandlePauseReplaceUnknown404(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"id":"missing","clients":["c"]}`)))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestHandlePauseReplaceOverlap409(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	for _, body := range []string{
		`{"paused":true,"clients":["a"]}`,
		`{"paused":true,"clients":["b"]}`,
	} {
		rr := httptest.NewRecorder()
		p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause", strings.NewReader(body)))
		if rr.Code != 200 {
			t.Fatalf("add = %d %s", rr.Code, rr.Body.Bytes())
		}
	}
	var idA string
	for _, h := range p.scheduler.HoldsList() {
		for _, c := range h.Clients {
			if c == "a" {
				idA = h.ID
			}
		}
	}
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"id":"`+idA+`","clients":["b"]}`)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	if !p.scheduler.Holds("a") || !p.scheduler.Holds("b") {
		t.Fatal("failed replace must leave both holds")
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
	p.SetPaused(true)
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

// TestPauseIgnoresProxyTimeoutUntilUnpause pins X-Proxy-Timeout-Ms as an
// upstream-header deadline, not a hold deadline: a 50ms timeout must not
// abort a request that is still parked.
func TestPauseIgnoresProxyTimeoutUntilUnpause(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"x","choices":[]}`))
	}))
	defer upstream.Close()

	p := New(config.Default(), metrics.Noop{})
	p.SetPaused(true)
	srv := httptest.NewServer(p)
	defer srv.Close()

	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Timeout-Ms", "50")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			done <- -1
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	// The fixed window IS the negative proof: it lets the 50ms header timeout
	// deadline pass while parked, so a buggy arming would have fired by now.
	time.Sleep(80 * time.Millisecond)
	if hits.Load() != 0 {
		t.Fatal("X-Proxy-Timeout-Ms fired during pause")
	}
	p.SetPaused(false)
	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("status = %d, want 200 after unpause", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish after unpause")
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
}

// TestRetryHoldWaitNotBoundedByProxyTimeout pins the retry-side half of the
// X-Proxy-Timeout-Ms contract: the header timeout is a PER-UPSTREAM-SEND
// deadline (headers + body), so a RETRY parked on an operator hold in WaitSend
// must not burn the budget - the retried send gets a fresh deadline when the
// hold releases. (The first attempt never waits on holds in WaitSend, which is
// why TestPauseIgnoresProxyTimeoutUntilUnpause passes either way.)
func TestRetryHoldWaitNotBoundedByProxyTimeout(t *testing.T) {
	var calls atomic.Int32
	firstAttempt := make(chan struct{})
	cfg := config.Default()
	cfg.MaxRetries = 2
	cfg.BaseBackoff = 10 * time.Millisecond
	cfg.MaxBackoff = 20 * time.Millisecond
	p := New(cfg, metrics.Noop{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// Arm the hold DURING the first attempt: Acquire already granted
			// (in-flight slots finish) and attempt 0's WaitSend ignores holds,
			// so the hold deterministically parks the RETRY's WaitSend.
			p.SetPaused(true)
			w.WriteHeader(500)
			w.Write([]byte(`{"error":{"message":"boom","type":"server_error"}}`))
			close(firstAttempt)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"id":"x","choices":[]}`))
	}))
	defer upstream.Close()

	srv := httptest.NewServer(p)
	defer srv.Close()

	done := make(chan int, 1)
	go func() {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m"}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Timeout-Ms", "300")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			done <- -1
			return
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- resp.StatusCode
	}()

	select {
	case <-firstAttempt:
	case <-time.After(2 * time.Second):
		t.Fatal("first attempt never reached upstream")
	}
	// Hold well past the 300ms header timeout, then release: the retry must
	// proceed with a fresh per-send budget, not a burned one.
	time.Sleep(600 * time.Millisecond)
	p.SetPaused(false)

	select {
	case code := <-done:
		if code != 200 {
			t.Fatalf("status = %d, want 200 after unpause (retry hold wait must not burn X-Proxy-Timeout-Ms)", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retried request did not finish after unpause")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2", n)
	}
}

// TestQualityRetryHonorsOperatorHold pins the hold contract for the generic
// quality retry: the degenerate-200 re-send is a RETRY ("a retry is a new
// send and waits"), so a hold armed during the void attempt must park the
// re-send in WaitSend exactly like a 429/5xx retry - the operator hold is
// never bypassed (X-Proxy-Timeout-Ms is unset here; it must not bound the
// hold either). Pre-fix, doWithRetry's opening WaitSend ran with
// honorHold=false and the re-send fired while the hold was live.
func TestQualityRetryHonorsOperatorHold(t *testing.T) {
	var calls atomic.Int32
	p := New(newConfigWithQuality(1), metrics.Noop{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		if n == 1 {
			// Arm the global hold DURING the degenerate attempt: Acquire
			// already granted (in-flight slots finish), so the hold
			// deterministically parks the quality RE-SEND's WaitSend.
			p.SetPaused(true)
			w.Write([]byte(voidBody))
			return
		}
		w.Write([]byte(realBody))
	}))
	defer upstream.Close()

	srv := httptest.NewServer(p)
	defer srv.Close()

	done := make(chan struct{})
	var gotStatus int
	var gotBody []byte
	go func() {
		defer close(done)
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk-test")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		gotStatus = resp.StatusCode
		gotBody, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}()

	// Wait for the degenerate attempt to land (the hold is armed inside it).
	// The re-send may already have fired too (that is the bug under test) -
	// the window assertion below is the actual proof.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() < 1 {
		t.Fatalf("void attempt never reached upstream (calls=%d)", calls.Load())
	}
	// Negative-proof window: the re-send must NOT fire while the hold is
	// live. Pre-fix it went out within microseconds of the void attempt.
	time.Sleep(300 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("quality re-send hit upstream %d× while the operator hold was live, want 0 (a retry is a new send and waits)", n-1)
	}

	p.SetPaused(false)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("re-send did not run after unpause")
	}
	if gotStatus != 200 || string(gotBody) != realBody {
		t.Fatalf("client got status=%d body=%q, want the healthy re-send response", gotStatus, gotBody)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2 (void absorbed + re-send after unpause)", n)
	}
}

// TestFiredSendTimeoutIsNotRetried pins the third half of the X-Proxy-Timeout
// contract: a FIRED per-send deadline must surface immediately as a 502 -
// never absorbed as a retryable transport timeout (context.DeadlineExceeded
// satisfies net.Error.Timeout(), so without the attempt-ctx check in
// doWithRetry's never-retry branch the fired budget would be re-sent up to
// max_retries times, multiplying the client's own timeout).
func TestFiredSendTimeoutIsNotRetried(t *testing.T) {
	var hits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Stall past any budget: hold the handler open until the proxy's
		// per-send deadline aborts the connection, with a bounded fallback
		// so upstream.Close() cannot hang either.
		select {
		case <-r.Context().Done():
		case <-time.After(750 * time.Millisecond):
		}
	}))
	defer upstream.Close()

	p := New(config.Default(), metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m"}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Timeout-Ms", "50")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 after the fired send deadline", resp.StatusCode)
	}
	if !strings.Contains(string(body), "upstream_unreachable") {
		t.Fatalf("body = %q, want upstream_unreachable", body)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("upstream hits = %d, want 1 (a fired per-send deadline must not be retried)", n)
	}
}

// TestCursorFiredSendTimeoutIsNotPaced is the cursor mirror of
// TestFiredSendTimeoutIsNotRetried: a FIRED per-send budget
// (X-Proxy-Timeout-Ms) on the bidi path must surface 502 with the canonical
// "context deadline exceeded" message - never the canceled request
// context's misleading "context canceled" - must NOT record a client
// disconnect (the local client is still there; its budget expired), and must
// NOT FailSend-pace the provider+key group: the client's own deadline is not
// an upstream health signal (in the doCursorUntil race window the surfaced
// error is context.DeadlineExceeded, which isRetryableTransportErr treats as
// a net timeout - pacing every sibling for a client-controlled expiry).
func TestCursorFiredSendTimeoutIsNotPaced(t *testing.T) {
	var hits atomic.Int32
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			// Stall without response headers past any budget: the proxy's
			// per-send deadline aborts the Do. Bounded fallback so
			// upstream.Close() cannot hang.
			select {
			case <-r.Context().Done():
			case <-time.After(750 * time.Millisecond):
			}
			return
		}
		// Follow-up requests get a complete minimal turn.
		w.Header().Set("Content-Type", "application/connect+proto")
		fl := w.(http.Flusher)
		w.WriteHeader(200)
		_, _ = readFrame(t, r.Body)
		w.Write(cframe(cmsg(1, cmsg(1, cstr(1, "ok")))))
		w.Write(cframe(cmsg(1, cmsg(14, nil))))
		w.Write(cend("{}"))
		fl.Flush()
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	cfg := config.Default()
	cfg.BaseBackoff = time.Second // any FailSend pacing would be unmistakable
	cfg.MaxBackoff = 2 * time.Second
	buf := metrics.NewBuffer(10)
	p := New(cfg, buf)
	srv := httptest.NewServer(p)
	defer srv.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	groupKey := providerFromURL(u) + "|" + hashKey("cursor-key")

	newReq := func() *http.Request {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer cursor-key")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		req.Header.Set("X-Proxy-Format", "cursor")
		req.Header.Set("X-Proxy-Timeout-Ms", "50")
		return req
	}

	resp, err := http.DefaultClient.Do(newReq())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 after the fired send deadline", resp.StatusCode)
	}
	if !strings.Contains(string(body), "context deadline exceeded") {
		t.Fatalf("502 body = %q, want the canonical 'context deadline exceeded' (the generic path's message), never a bare 'context canceled'", body)
	}
	// The doCursorUntil race window surfaces the RAW deadline error
	// (context.DeadlineExceeded - a retryable-class net timeout per
	// isRetryableTransportErr). The classifier must still refuse to pace it.
	if paced, _ := cursorSendFailure(context.DeadlineExceeded, context.DeadlineExceeded); paced {
		t.Error("cursorSendFailure must not pace a fired per-send budget even in the raw-DeadlineExceeded race shape")
	}

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.ClientDisconnected {
		t.Error("ClientDisconnected = true, want false (the local client is connected; its budget expired)")
	}
	if rec.StatusCode != http.StatusBadGateway || rec.ErrorType != "upstream_unreachable" || !rec.IsError() {
		t.Errorf("record = status %d type %q, want 502/upstream_unreachable error", rec.StatusCode, rec.ErrorType)
	}
	// The direct no-pacing proof: FailSend would leave the base backoff set.
	if got := p.scheduler.RequestBackoff(groupKey); got != 0 {
		t.Errorf("request backoff = %v, want 0 (a fired client budget must not pace the group)", got)
	}

	// The follow-up on the same provider|key must complete immediately.
	start := time.Now()
	resp2, err := http.DefaultClient.Do(newReq())
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("follow-up took %v; the group was paced by the fired budget (base_backoff=%v)", elapsed, cfg.BaseBackoff)
	}
	if resp2.StatusCode != 200 || !strings.Contains(string(body2), `"content":"ok"`) {
		t.Fatalf("follow-up status = %d body = %q, want the completed turn", resp2.StatusCode, body2)
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
	deadline := time.Now().Add(time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() != 1 {
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
	time.Sleep(30 * time.Millisecond)

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

// TestStreamKeepaliveThenUpstreamErrorIsInBand: after the pacer has
// committed 200, an exhausted 429 must not be relayed as raw JSON.
func TestStreamKeepaliveThenUpstreamErrorIsInBand(t *testing.T) {
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
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200 (pacer committed during TTFB)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "data: ") || !strings.Contains(got, "rate_limit_error") {
		t.Fatalf("body %q, want in-band SSE error, not raw JSON", got)
	}
	if !strings.Contains(got, "data: [DONE]") {
		t.Fatalf("body %q, want [DONE]", got)
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
