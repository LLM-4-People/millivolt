package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

type testOperatorPersist struct {
	pausePersist
	save func([]byte) error
}

func (p testOperatorPersist) SavePause(_ context.Context, raw []byte) error { return p.save(raw) }
func (p testOperatorPersist) SaveDebugSessions(_ context.Context, raw []byte) error {
	return p.save(raw)
}
func (p testOperatorPersist) SaveThrottle(_ context.Context, raw []byte) error { return p.save(raw) }
func (testOperatorPersist) CountDebugSession(context.Context, string) (int64, error) {
	return 0, nil
}

type operatorCase struct {
	name, flag, enable, disable string
	handler                     func(*Server) http.HandlerFunc
}

var operatorCases = []operatorCase{
	{"pause", "paused", `{"paused":true}`, `{"paused":false}`, func(s *Server) http.HandlerFunc { return s.HandlePause }},
	{"debug", "enabled", `{"enabled":true,"clients":["client-neutral"]}`, `{"enabled":false}`, func(s *Server) http.HandlerFunc { return s.HandleDebug }},
	{"throttle", "active", `{"provider":"provider.example","concurrency":1}`, `{"provider":"provider.example","clear":true}`, func(s *Server) http.HandlerFunc { return s.HandleThrottle }},
}

func operatorResponse(h http.HandlerFunc, body string) (map[string]any, error) {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/admin/operator", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		return nil, fmt.Errorf("operator POST: %d %s", w.Code, w.Body.String())
	}
	var state map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		return nil, fmt.Errorf("operator POST response: %w", err)
	}
	return state, nil
}

func operatorPost(t *testing.T, h http.HandlerFunc, body string) map[string]any {
	t.Helper()
	state, err := operatorResponse(h, body)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestOperatorPersistenceFailureReturnsAppliedStateWarning(t *testing.T) {
	for _, tc := range operatorCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, durable := range []bool{false, true} {
				p := New(config.Default(), metrics.Noop{})
				if durable {
					store := testOperatorPersist{save: func([]byte) error { return errors.New("disk unavailable") }}
					p.pause.persist, p.throttle.persist = store, store
				}
				for i, body := range []string{tc.enable, tc.disable} {
					state := operatorPost(t, tc.handler(p), body)
					if state["ok"] != true || state[tc.flag] != (i == 0) || (state["warning"] != nil) != durable {
						t.Fatalf("durable=%v: persistence outcome hid applied runtime state: %+v", durable, state)
					}
				}
			}
		})
	}
}

func TestOperatorPersistenceWarningTracksLatestGeneration(t *testing.T) {
	for _, tc := range operatorCases {
		for _, latestFails := range []bool{false, true} {
			t.Run(tc.name+"/"+map[bool]string{false: "stale-failure-recovered", true: "stale-success-latest-failed"}[latestFails], func(t *testing.T) {
				p := New(config.Default(), metrics.Noop{})
				entered, unblock := make(chan struct{}), make(chan struct{})
				var calls atomic.Int32
				store := testOperatorPersist{save: func([]byte) error {
					if calls.Add(1) == 1 {
						close(entered)
						<-unblock
						if !latestFails {
							return errors.New("stale generation failed")
						}
						return nil
					}
					if latestFails {
						return errors.New("current generation failed")
					}
					return nil
				}}
				p.pause.persist, p.throttle.persist = store, store
				type response struct {
					state map[string]any
					err   error
				}
				first := make(chan response, 1)
				done := make(chan struct{})
				release := sync.OnceFunc(func() { close(unblock) })
				defer func() { release(); <-done }()
				go func() {
					defer close(done)
					state, err := operatorResponse(tc.handler(p), tc.enable)
					first <- response{state, err}
				}()
				select {
				case <-entered:
				case result := <-first:
					t.Fatalf("first mutation completed before persistence barrier: state=%v error=%v", result.state, result.err)
				}
				second, secondErr := operatorResponse(tc.handler(p), tc.disable)
				release()
				result := <-first
				if secondErr != nil {
					t.Fatal(secondErr)
				}
				if result.err != nil {
					t.Fatal(result.err)
				}
				for _, state := range []map[string]any{second, result.state} {
					if state[tc.flag] != false || (state["warning"] != nil) != latestFails {
						t.Fatalf("warning came from superseded generation: %+v", state)
					}
				}
				if calls.Load() != 3 {
					t.Fatalf("stale write did not repair latest generation: calls=%d", calls.Load())
				}
			})
		}
	}
}

func TestThrottleHeaderPersistenceFailureDoesNotRejectOrRewrite(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	var calls int
	p.throttle.persist = testOperatorPersist{save: func([]byte) error {
		calls++
		return errors.New("disk unavailable")
	}}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(hdrLimitConcurrency, "2")
	for i := 0; i < 2; i++ {
		if err := p.applyThrottleHeaders(r, "provider.example", "client-neutral"); err != nil {
			t.Fatalf("save failure rejected validated inference headers: %v", err)
		}
	}
	if calls != 1 || p.scheduler.ThrottleFor("provider.example").Limit.Concurrency != 2 {
		t.Fatalf("save failure rolled back runtime cap or repeated no-op persistence: calls=%d", calls)
	}
}

func TestDebugEditTransactionPreservesConcurrentPartialFields(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	defer p.applyDebug(nil)
	started := time.Now()
	p.applyDebug([]persistedDebug{{ID: "session", Clients: []string{"old-client"}, Duration: "15m", StartedAt: started, Until: started.Add(15 * time.Minute)}})
	assertOwnerLock := func() {
		if p.debug.mu.TryLock() {
			p.debug.mu.Unlock()
			t.Error("edit callback ran outside the read/merge/commit owner lock")
		}
	}
	entered, release, secondStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 2)
	go func() {
		done <- p.editDebug("session", func(prev persistedDebug) (persistedDebug, error) {
			assertOwnerLock()
			close(entered)
			<-release
			prev.Duration, prev.Until = "1h", started.Add(time.Hour)
			return prev, nil
		})
	}()
	<-entered
	go func() {
		close(secondStarted)
		done <- p.editDebug("session", func(prev persistedDebug) (persistedDebug, error) {
			assertOwnerLock()
			prev.Clients = []string{"new-client"}
			return prev, nil
		})
	}()
	<-secondStarted
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	p.debug.mu.Lock()
	got := p.debug.sessions[0]
	p.debug.mu.Unlock()
	if got.Clients[0] != "new-client" || got.Duration != "1h" || !got.Until.Equal(started.Add(time.Hour)) || !got.StartedAt.Equal(started) {
		t.Fatalf("partial edit restored stale fields: %+v", got)
	}
}

func TestDebugOmittedDurationPreservesAbsoluteDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := New(config.Default(), metrics.Noop{})
		defer p.applyDebug(nil)
		until := time.Now().Add(time.Hour)
		p.applyDebug([]persistedDebug{{ID: "session", Clients: []string{"old-client"}, Until: until}})
		operatorPost(t, p.HandleDebug, `{"enabled":true,"id":"session","clients":["new-client"]}`)
		p.debug.mu.Lock()
		got := p.debug.sessions[0].Until
		p.debug.mu.Unlock()
		if !got.Equal(until) {
			t.Fatalf("scope edit discarded restored absolute deadline: %v", got)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if p.DebugSnapshot()["enabled"] != false {
			t.Fatal("session survived its preserved absolute deadline")
		}
	})
}

func TestDebugEditExpiredSessionDoesNotReviveIt(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.debug.sessions = []persistedDebug{{ID: "expired", Clients: []string{"client-neutral"}, Until: time.Now().Add(-time.Second)}}
	w := httptest.NewRecorder()
	p.HandleDebug(w, httptest.NewRequest(http.MethodPost, "/admin/debug", strings.NewReader(`{"enabled":true,"id":"expired","duration":"1h"}`)))
	if w.Code != http.StatusNotFound || p.DebugSnapshot()["enabled"] != false {
		t.Fatalf("expired session revived: %d %s", w.Code, w.Body.String())
	}
}
