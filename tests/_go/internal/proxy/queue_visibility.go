package proxy

// Live queue visibility regression: a 429 pacing window parks both the
// retrying request and every sibling for the same provider+key, and each of
// them must publish the queued live state. Before this, WaitSend parking had
// no observable state at all and a new request waited in the admission queue
// without any live flag, so the dashboard showed both as active streams.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// queueEdgeSpy records which request models published a live queued edge.
type queueEdgeSpy struct {
	metrics.Noop
	mu        sync.Mutex
	throttled map[string]bool
}

func (s *queueEdgeSpy) PublishLive(_ string, r *metrics.Record) {
	if r == nil || !r.Throttled {
		return
	}
	s.mu.Lock()
	if s.throttled == nil {
		s.throttled = make(map[string]bool)
	}
	s.throttled[r.Model] = true
	s.mu.Unlock()
}

func (s *queueEdgeSpy) saw(model string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.throttled[model]
}

func TestPacingWindowPublishesQueuedSiblings(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer upstream.Close()

	spy := &queueEdgeSpy{}
	p := New(config.Default(), spy)
	srv := httptest.NewServer(p)
	defer srv.Close()

	send := func(model string, done chan<- int) {
		req, err := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Error(err)
			done <- 0
			return
		}
		// Same bearer key for both requests: one provider|key group, so the
		// 429's pacing window covers the sibling too.
		req.Header.Set("Authorization", "Bearer sk-shared")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			done <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- resp.StatusCode
	}

	waitSaw := func(model, what string) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if spy.saw(model) {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("no live queued edge for %s (%s)", model, what)
	}

	owner := make(chan int, 1)
	go send("owner", owner)
	// The owner parks on its own Retry-After window: that edge proves the
	// pacing window is armed before the sibling starts.
	waitSaw("owner", "retrying owner waiting out its 429 window")

	sibling := make(chan int, 1)
	go send("sibling", sibling)
	waitSaw("sibling", "request queued behind the same 429 window")

	for name, ch := range map[string]chan int{"owner": owner, "sibling": sibling} {
		select {
		case code := <-ch:
			if code != http.StatusOK {
				t.Fatalf("%s status = %d, want 200 after the window cleared", name, code)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not finish", name)
		}
	}

	// The live population drains with the window: an edge left behind would
	// keep the dashboard queued forever.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st := p.scheduler.Stats(); st.Queued == 0 && st.SendWaiters == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st := p.scheduler.Stats(); st.Queued != 0 || st.SendWaiters != 0 {
		t.Fatalf("scheduler still parked after both requests completed: %+v", st)
	}
}
