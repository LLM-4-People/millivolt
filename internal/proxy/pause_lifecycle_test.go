package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func pausePost(t *testing.T, p *Server, body string) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause", strings.NewReader(body)))
	if rr.Code != http.StatusOK {
		t.Fatalf("pause POST: %d %s", rr.Code, rr.Body.String())
	}
	var state map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestPauseStaleTimerCannotRemoveExtendedHold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := New(config.Default(), metrics.Noop{})
		defer p.SetPaused(false)
		until := time.Now().Add(time.Second)
		p.applyHolds([]persistedPause{{ID: "hold", All: true, Until: until}})
		// Let the old AfterFunc start while the policy mutex is held, then
		// extend/rearm the hold before that expired callback can acquire it.
		p.pause.mu.Lock()
		time.Sleep(time.Second)
		p.pause.holds[0].Until = until.Add(time.Hour)
		if err := p.scheduler.ReplaceHold(toSchedHold(p.pause.holds[0])); err != nil {
			t.Fatal(err)
		}
		p.rearmTimersLocked()
		p.pause.mu.Unlock()
		synctest.Wait()
		if !p.scheduler.Paused() || len(p.PauseSnapshot()["holds"].([]map[string]any)) != 1 {
			t.Fatal("superseded expiry callback removed the extended hold")
		}
	})
}

func TestPauseConcurrentPartialEditsPreserveLatestFields(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	defer p.SetPaused(false)
	for i := 0; i < 40; i++ {
		p.applyHolds([]persistedPause{{ID: "hold", Clients: []string{"client"}, Duration: "1h", Until: time.Now().Add(time.Hour), MaxQueued: 1}})
		start := make(chan struct{})
		var wg sync.WaitGroup
		for _, body := range []string{`{"paused":true,"id":"hold","max_queued":17}`, `{"paused":true,"id":"hold","duration":"6h"}`} {
			wg.Go(func() { <-start; pausePost(t, p, body) })
		}
		close(start)
		wg.Wait()
		p.pause.mu.Lock()
		h := p.pause.holds[0]
		p.pause.mu.Unlock()
		if h.MaxQueued != 17 || h.Duration != "6h" || len(h.Clients) != 1 || h.Clients[0] != "client" || time.Until(h.Until) < 5*time.Hour {
			t.Fatalf("concurrent edit overwrote omitted fields: %+v", h)
		}
	}
}

func TestPauseOmittedDurationPreservesAbsoluteDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := New(config.Default(), metrics.Noop{})
		defer p.SetPaused(false)
		until := time.Now().Add(time.Hour)
		p.applyHolds([]persistedPause{{ID: "hold", All: true, Until: until}})
		pausePost(t, p, `{"paused":true,"id":"hold","max_queued":17}`)
		p.pause.mu.Lock()
		got := p.pause.holds[0].Until
		p.pause.mu.Unlock()
		if !got.Equal(until) {
			t.Fatalf("cap edit discarded restored absolute deadline: %v", got)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		state := p.PauseSnapshot()
		if state["paused"] != false || len(state["holds"].([]map[string]any)) != 0 {
			t.Fatal("hold did not expire at its preserved deadline")
		}
	})
}

func TestPauseInvalidIdentifiersAndScopesFailClosed(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.SetPaused(true)
	defer p.SetPaused(false)
	for _, body := range []string{
		`{"paused":false,"id":""}`, `{"paused":false,"id":"  "}`, `{"paused":false,"id":null}`, `{"paused":false,"id":7}`,
		`{"paused":true,"clients":null}`, `{"paused":true,"providers":null}`, `{"paused":true,"all":null}`, `{"paused":true,"new":null}`,
		`{"paused":false,"unknown":true}`, `{"paused":true,"paused":false}`, `{"paused":false} {}`, `null`,
	} {
		rr := httptest.NewRecorder()
		p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause", strings.NewReader(body)))
		if rr.Code != http.StatusBadRequest || !p.scheduler.Paused() {
			t.Fatalf("invalid body mutated pause: %s => %d %s", body, rr.Code, rr.Body.String())
		}
	}
}

type pauseBlockedWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	unblock chan struct{}
	once    sync.Once
}

func (w *pauseBlockedWriter) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.unblock
	return w.ResponseRecorder.Write(b)
}

func (w *pauseBlockedWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

func TestPauseEditDoesNotWaitForQueuedClientWrite(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.SetPaused(true)
	id := p.PauseSnapshot()["holds"].([]map[string]any)[0]["id"].(string)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`)).WithContext(ctx)
	r.Header.Set("X-Proxy-Base-URL", "https://provider.example")
	r.Header.Set("X-Proxy-Key", "fixture-key")
	w := &pauseBlockedWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), unblock: make(chan struct{})}
	requestDone := make(chan struct{})
	go func() { p.ServeHTTP(w, r); close(requestDone) }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		cancel()
		close(w.unblock)
		<-requestDone
		t.Fatal("queued request never reached the blocked keepalive write")
	}
	editDone := make(chan int, 1)
	go func() {
		rr := httptest.NewRecorder()
		p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause", strings.NewReader(`{"paused":true,"id":"`+id+`","max_queued":17}`)))
		editDone <- rr.Code
	}()
	select {
	case status := <-editDone:
		if status != http.StatusOK {
			t.Errorf("pause edit status = %d", status)
		}
	case <-time.After(time.Second):
		t.Error("pause edit blocked on queued client I/O while holding global accounting lock")
	}
	// Cancel before releasing the fake socket: this fixture must never send
	// a provider request, even if the pause is concurrently being changed.
	cancel()
	close(w.unblock)
	<-requestDone
	p.SetPaused(false)
}
