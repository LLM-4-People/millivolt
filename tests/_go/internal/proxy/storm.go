package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

func stormTestConfig() *config.Config {
	c := config.Default()
	c.StormEnabled = true
	c.StormMinSamples = 1
	c.StormInitialBackoff = 15 * time.Millisecond
	c.StormMaxBackoff = 30 * time.Millisecond
	c.StormJitterPercent = 0
	c.StormMaxWait = time.Second
	c.StormRecoverySuccesses = 1
	c.BaseBackoff = time.Millisecond
	c.MaxBackoff = time.Millisecond
	c.MaxRetries = 0
	c.QualityRetries = 0
	return c
}

func stormRequest(t *testing.T, s *Server, upstream, model, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)))
	r.Header.Set(hdrBaseURL, upstream)
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestStormRecoversBeyondOrdinaryRetryBudget(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(503)
			io.WriteString(w, `{"error":{"type":"unavailable","message":"temporary"}}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"recovered"}}]}`)
	}))
	defer up.Close()
	buf := metrics.NewBuffer(10)
	s := New(stormTestConfig(), buf)
	start := time.Now()
	w := stormRequest(t, s, up.URL, "model-a", "key-a")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "recovered") || calls.Load() != 3 {
		t.Fatalf("status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
	if time.Since(start) < 45*time.Millisecond {
		t.Fatal("storm retries did not honor exponential cooldown")
	}
	rows := buf.Snapshot()
	if len(rows) != 1 || rows[0].Retries != 2 || len(rows[0].Attempts) != 2 || rows[0].StatusCode != 200 {
		t.Fatalf("expected one finalized recovered request with two attempts: %+v", rows)
	}
	if got := s.scheduler.StormSnapshot(); len(got) != 0 {
		t.Fatalf("successful probe did not recover: %+v", got)
	}
}

func TestStormPermanentErrorsDoNotRetryOrTrip(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 501} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(status)
				io.WriteString(w, `{"error":{"type":"insufficient_quota"}}`)
			}))
			defer up.Close()
			cfg := stormTestConfig()
			cfg.StormStatusCodes = append(cfg.StormStatusCodes, "429")
			s := New(cfg, nil)
			w := stormRequest(t, s, up.URL, "model-a", "key-a")
			if w.Code != status || calls.Load() != 1 || len(s.scheduler.StormSnapshot()) != 0 {
				t.Fatalf("status=%d calls=%d storms=%+v", w.Code, calls.Load(), s.scheduler.StormSnapshot())
			}
		})
	}
}

func TestStormQueueTimeoutAndCancellation(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
	}))
	defer up.Close()
	cfg := stormTestConfig()
	cfg.StormInitialBackoff = time.Second
	cfg.StormMaxBackoff = time.Second
	cfg.StormMaxWait = 20 * time.Millisecond
	s := New(cfg, nil)
	w := stormRequest(t, s, up.URL, "model-a", "key-a")
	if w.Code != 429 || calls.Load() != 1 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("expected local bounded-queue rejection after one upstream send: %d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-a"}`)).WithContext(ctx)
	r.Header.Set(hdrBaseURL, up.URL)
	s.ServeHTTP(httptest.NewRecorder(), r)
	if calls.Load() != 1 {
		t.Fatal("canceled caller sent upstream")
	}
}

func TestStormIsolatesAffectedModelAcrossKeys(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "model-a") {
			w.WriteHeader(503)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"healthy"}}]}`)
	}))
	defer up.Close()
	cfg := stormTestConfig()
	cfg.StormMaxRetries = 0
	cfg.StormInitialBackoff = time.Second
	cfg.StormMaxBackoff = time.Second
	cfg.StormMaxWait = 20 * time.Millisecond
	s := New(cfg, nil)
	if w := stormRequest(t, s, up.URL, "model-b", "key-b"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := stormRequest(t, s, up.URL, "model-a", "key-a"); w.Code != 503 {
		t.Fatal(w.Code)
	}
	status := s.scheduler.StormSnapshot()
	if len(status) != 1 || status[0].Scope != "model" || status[0].Model != "model-a" {
		t.Fatalf("healthy active model must prevent provider-wide trip: %+v", status)
	}
	if w := stormRequest(t, s, up.URL, "model-a", "different-key"); w.Code != 429 {
		t.Fatalf("affected model bypassed storm through another key: %d", w.Code)
	}
	if w := stormRequest(t, s, up.URL, "model-b", "key-b"); w.Code != 200 {
		t.Fatalf("healthy model was blocked: %d", w.Code)
	}
}

func TestStormResponseErrorsProtectFutureRequestsWithoutReplay(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"visible content\"}}]}\n\n")
					w.(http.Flusher).Flush()
					io.WriteString(w, "data: {\"error\":{\"type\":\"unavailable\",\"message\":\"sensitive upstream detail\"}}\n\n")
				} else {
					io.WriteString(w, `{"error":{"type":"unavailable","message":"sensitive upstream detail"}}`)
				}
			}))
			defer up.Close()
			s := New(stormTestConfig(), nil)
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"model-a","stream":%t}`, stream)))
			r.Header.Set(hdrBaseURL, up.URL)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if calls.Load() != 1 {
				t.Fatalf("replayed a response error: %d calls", calls.Load())
			}
			status := s.scheduler.StormSnapshot()
			if len(status) == 0 || status[0].Reason != "response error" || status[0].Samples != 1 || status[0].Failures != 1 {
				t.Fatalf("response failure must be one safe sample: %+v", status)
			}
			if stream && !strings.Contains(w.Body.String(), "visible content") {
				t.Fatal("lost already emitted content")
			}
		})
	}
}

func TestStormRetryBudgetIsFinite(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		io.WriteString(w, `{"error":{"type":"unavailable","message":"last upstream response"}}`)
	}))
	defer up.Close()
	cfg := stormTestConfig()
	cfg.MaxRetries = 1
	cfg.StormMaxRetries = 2
	s := New(cfg, nil)
	w := stormRequest(t, s, up.URL, "model-a", "key-a")
	if calls.Load() != 4 || w.Code != 503 || !strings.Contains(w.Body.String(), "last upstream response") {
		t.Fatalf("expected final upstream failure after normal+storm budget: calls=%d status=%d body=%s", calls.Load(), w.Code, w.Body.String())
	}
	for _, status := range s.scheduler.StormSnapshot() {
		if status.ErrorRequests != 1 || status.Failures != 4 {
			t.Fatalf("retried logical request inflated errored request count: %+v", status)
		}
	}
}

func TestStormRechecksAdmissionAfterKeyQueue(t *testing.T) {
	cfg := stormTestConfig()
	cfg.StormInitialBackoff = time.Second
	cfg.StormMaxBackoff = time.Second
	cfg.StormMaxWait = 10 * time.Millisecond
	s := New(cfg, nil)
	state := &stormRequestState{rec: &metrics.Record{Provider: "neutral", Model: "model-a"}}
	ctx := context.WithValue(context.Background(), stormContextKey{}, state)
	hooks := scheduler.WaiterHooks{Provider: "neutral"}
	var err error
	_, err = s.waitStormGate(ctx, hooks, true)
	if err != nil {
		t.Fatal(err)
	}
	// A sibling fails while this request waits for its existing key slot.
	sibling, err := s.scheduler.WaitStorm(ctx, "neutral", "model-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	sibling.Observe(true, "HTTP 503", 0)
	p, err := s.waitStorm(ctx, hooks)
	if p != nil {
		p.Cancel()
	}
	if !errors.Is(err, scheduler.ErrStormMaxWait) {
		t.Fatalf("pre-queue readiness bypassed newly opened storm: %v", err)
	}
}

func TestStormReloadPreservesBannerOnlyAndResetsPolicy(t *testing.T) {
	cfg := stormTestConfig()
	s := New(cfg, nil)
	p, err := s.scheduler.WaitStorm(context.Background(), "neutral", "model-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Observe(true, "HTTP 503", 0)
	cfg = cfg.Clone()
	cfg.StormBannerEnabled = !cfg.StormBannerEnabled
	s.Reload(cfg)
	if len(s.scheduler.StormSnapshot()) == 0 {
		t.Fatal("banner visibility reset protection")
	}
	cfg = cfg.Clone()
	cfg.StormMaxRetries++
	s.Reload(cfg)
	if len(s.scheduler.StormSnapshot()) != 0 {
		t.Fatal("changed storm retry policy retained old incident")
	}
}

func TestStormExcludesFiredBodyDeadline(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var calls atomic.Int32
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
				} else {
					io.WriteString(w, `{"choices":[`)
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer up.Close()
			s := New(stormTestConfig(), nil)
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"model-a","stream":%t}`, stream)))
			r.Header.Set(hdrBaseURL, up.URL)
			r.Header.Set(hdrTimeout, "20")
			s.ServeHTTP(httptest.NewRecorder(), r)
			if status := s.scheduler.StormSnapshot(); calls.Load() != 1 || len(status) != 0 {
				t.Fatalf("caller-selected body deadline became outage evidence: calls=%d storms=%+v", calls.Load(), status)
			}
		})
	}
}

func TestStormExcludesInBandDurableQuota(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"error":{"type":"insufficient_quota","message":"account action required"}}`)
	}))
	defer up.Close()
	s := New(stormTestConfig(), nil)
	stormRequest(t, s, up.URL, "model-a", "key-a")
	if status := s.scheduler.StormSnapshot(); len(status) != 0 {
		t.Fatalf("durable in-band quota error became storm evidence: %+v", status)
	}
}
