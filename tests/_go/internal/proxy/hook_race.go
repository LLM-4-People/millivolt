package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// TestHookPublishSerializesSeededHoldAndThrottleDispatch is the race
// reproducer for the per-request hook mutex in ServeHTTP. The scheduler
// dispatches the lifecycle hooks from two goroutines: a seeded hold fires
// OnHold on the AddHold caller's goroutine (fireSeededHold -> the proxy's
// hook body writing rec.Paused and publishing the record), while the parked
// request's own wait loop fires OnThrottle/OnUnthrottle on the handler
// goroutine (writing rec.Throttled and publishing the SAME record). The
// scheduler serializes the hold hooks against each other under holdCountMu,
// but the throttle pair runs outside that lock, so the proxy's hook bodies
// need their own fence or the two goroutines race on the record fields the
// live publish reads. The stress shape: a saturated provider request budget
// parks every request behind OnThrottle, while a concurrent AddHold/RemoveHold
// cycler repeatedly seeds OnHold for the parked waiters. The seeded OnHold
// must still fire while a waiter is blocked (the scheduler's
// TestReplaceHoldOnHoldDuringOnWait pins that half); this pin holds the
// proxy half - that the concurrent dispatch is race-free and every parked
// request still completes with the hold cycling under it.
func TestHookPublishSerializesSeededHoldAndThrottleDispatch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	// The IP upstream's provider label is its host:port authority
	// (providerFromURL keeps the port for bare IPs), which is what both the
	// throttle and the cycler's hold must name.
	parsed, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	provider := parsed.Host

	// A live publisher recorder: publishUpdate must actually read the
	// record for the race window to exist (metrics.Noop publishes nothing).
	buf := metrics.NewBuffer(512)
	s := New(config.Default(), buf)
	// Saturate the provider's request budget so every request parks behind
	// OnThrottle while one admit per refill tick drains the queue.
	s.scheduler.SetThrottle(scheduler.Throttle{
		Provider:  provider,
		Limit:     scheduler.Limit{Requests: 2, ReqWindow: 20 * time.Millisecond},
		Source:    scheduler.ThrottleSourceUI,
		UpdatedBy: "reproducer",
	})
	srv := httptest.NewServer(s)
	defer srv.Close()

	const workers = 6
	const rounds = 25 // 150 requests; two admits per 20ms keeps the run short
	stop := make(chan struct{})
	var cycler sync.WaitGroup
	cycler.Add(1)
	go func() {
		defer cycler.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			// One cycler goroutine keeps AddHold/RemoveHold strictly
			// alternating (a second cycler would trip the overlap
			// rejection), so every cycle seeds OnHold for each parked
			// waiter on THIS goroutine.
			if err := s.scheduler.AddHold(scheduler.Hold{ID: "hook-race", Providers: []string{provider}}); err != nil {
				t.Errorf("AddHold: %v", err)
				return
			}
			s.scheduler.RemoveHold("hook-race")
		}
	}()

	var requesters sync.WaitGroup
	statuses := make([]int, workers*rounds)
	for w := range workers {
		requesters.Add(1)
		go func(worker int) {
			defer requesters.Done()
			client := &http.Client{Timeout: 30 * time.Second}
			for i := range rounds {
				req, err := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
					strings.NewReader(`{"model":"m"}`))
				if err != nil {
					t.Errorf("worker %d: %v", worker, err)
					return
				}
				req.Header.Set("Authorization", "Bearer sk-hook-race")
				req.Header.Set("X-Proxy-Base-URL", upstream.URL)
				resp, err := client.Do(req)
				if err != nil {
					t.Errorf("worker %d round %d: %v", worker, i, err)
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				statuses[worker*rounds+i] = resp.StatusCode
			}
		}(w)
	}
	requesters.Wait()
	close(stop)
	cycler.Wait()
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("request %d = %d, want 200 (the hold cycling must never strand a parked request)", i, status)
		}
	}
	// Every request finalized through the live recorder: the seeded-hold
	// dispatches never lost a record into a stuck publish path.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(buf.Snapshot()) >= workers*rounds {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("finalized records = %d, want %d", len(buf.Snapshot()), workers*rounds)
}
