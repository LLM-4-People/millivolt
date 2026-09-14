package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	if calls != 1 || schedThrottleFor(p.scheduler, "provider.example").Limit.Concurrency != 2 {
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

// TestOperatorStateMethodGates pins the RFC 9110 method gate the three
// operator-state endpoints share (rejectUnlessGetPost via operatorStateGet):
// a wrong method gets 405 with the Allow header, never a fall-through to a
// decode or the LLM proxy. The restart endpoint's gate is pinned separately
// in the cmd/proxy suite; nothing pinned these three.
func TestOperatorStateMethodGates(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	served := map[string]func(http.ResponseWriter, *http.Request){
		"/admin/pause":    p.HandlePause,
		"/admin/debug":    p.HandleDebug,
		"/admin/throttle": p.HandleThrottle,
	}
	for path, handler := range served {
		for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
			rec := httptest.NewRecorder()
			handler(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", method, path, rec.Code)
			}
			if allow := rec.Header().Get("Allow"); allow != "GET, POST" {
				t.Errorf("%s %s Allow = %q, want \"GET, POST\"", method, path, allow)
			}
			if !strings.Contains(rec.Body.String(), "GET or POST") {
				t.Errorf("%s %s body = %q, want the shared gate text", method, path, rec.Body.String())
			}
		}
	}
}

// TestOperatorStateUntilRFC3339UTC pins rfc3339OrNil's wire format on every
// surface that renders it: each pause hold's until, the pause document's
// soonest until, each debug session's until, and the debug document's
// soonest until. Format(time.RFC3339) only emits the trailing Z through the
// explicit UTC() call - the round-seven mutation dropped that call silently,
// changing the zone without breaking a single test. A value is valid only if
// it parses as RFC 3339 AND carries the UTC Z designator; a zero Until
// renders no field at all (nil omission is the existing contract).
func TestOperatorStateUntilRFC3339UTC(t *testing.T) {
	// Force a non-UTC local zone so the pin catches the dropped .UTC() call
	// on any machine: with a UTC local zone the mutation is invisible.
	zoneShift := time.FixedZone("fixture-shift", 3600)
	savedLocal := time.Local
	time.Local = zoneShift
	defer func() { time.Local = savedLocal }()

	p := New(config.Default(), metrics.Noop{})
	rec := httptest.NewRecorder()
	p.HandlePause(rec, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"clients":["client-a"],"duration":"15m"}`)))
	if rec.Code != 200 {
		t.Fatalf("pause add = %d %s", rec.Code, rec.Body.Bytes())
	}
	rec = httptest.NewRecorder()
	p.HandlePause(rec, httptest.NewRequest(http.MethodGet, "/admin/pause", nil))
	st := pauseJSON(t, rec)
	holds, _ := st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("holds = %v, want the one timed hold", st["holds"])
	}
	untils := []any{holds[0].(map[string]any)["until"], st["until"]}
	for i, raw := range untils {
		s, ok := raw.(string)
		if !ok {
			t.Fatalf("pause until[%d] = %v, want an RFC 3339 string", i, raw)
		}
		if !strings.HasSuffix(s, "Z") {
			t.Fatalf("pause until[%d] = %q, want the UTC Z designator", i, s)
		}
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			t.Fatalf("pause until[%d] = %q does not parse as RFC 3339: %v", i, s, err)
		}
	}

	rec = httptest.NewRecorder()
	p.HandleDebug(rec, httptest.NewRequest(http.MethodPost, "/admin/debug",
		strings.NewReader(`{"enabled":true,"clients":["client-a"],"duration":"15m"}`)))
	if rec.Code != 200 {
		t.Fatalf("debug add = %d %s", rec.Code, rec.Body.Bytes())
	}
	rec = httptest.NewRecorder()
	p.HandleDebug(rec, httptest.NewRequest(http.MethodGet, "/admin/debug", nil))
	dst := pauseJSON(t, rec)
	sessions, _ := dst["sessions"].([]any)
	if len(sessions) != 1 {
		t.Fatalf("debug sessions = %v, want the one timed session", dst["sessions"])
	}
	untils = []any{sessions[0].(map[string]any)["until"], dst["until"]}
	for i, raw := range untils {
		s, ok := raw.(string)
		if !ok {
			t.Fatalf("debug until[%d] = %v, want an RFC 3339 string", i, raw)
		}
		if !strings.HasSuffix(s, "Z") {
			t.Fatalf("debug until[%d] = %q, want the UTC Z designator", i, s)
		}
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			t.Fatalf("debug until[%d] = %q does not parse as RFC 3339: %v", i, s, err)
		}
	}
}

// TestKnownSetsSeedFromRealRequest pins the operator known-name content: a
// proxied request seeds exactly the client it carried and the provider its
// base URL derives, while an EMPTY name never seeds a checkbox row (the
// nameSet.add empty guard - a request body with no model field records an
// empty rec.Model, and the round-seven mutation round proved removing the
// guard went unnoticed). The direct add("") rows pin the same guard for the
// client and provider sets, which the HTTP path cannot reach with an empty
// value (classifyClient and providerFromURL both fall back to a label).
func TestKnownSetsSeedFromRealRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	p := New(config.Default(), metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	// A JSON body with NO model field: the record's model stays empty, and
	// the known-model checkbox list must not gain an empty row.
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-test")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Client", "seeded-client")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	known := func(t *testing.T, doc map[string]any, key string) []string {
		t.Helper()
		raw, _ := doc[key].([]any)
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			out = append(out, v.(string))
		}
		return out
	}
	rec := httptest.NewRecorder()
	p.HandlePause(rec, httptest.NewRequest(http.MethodGet, "/admin/pause", nil))
	st := pauseJSON(t, rec)
	if got := known(t, st, "known_clients"); strings.Join(got, ",") != "seeded-client" {
		t.Fatalf("known_clients = %v, want exactly the request's client", got)
	}
	provider := providerFromURL(mustParseURL(t, upstream.URL))
	if got := known(t, st, "known_providers"); strings.Join(got, ",") != provider {
		t.Fatalf("known_providers = %v, want exactly %q (the base URL's derived label)", got, provider)
	}

	rec = httptest.NewRecorder()
	p.HandleDebug(rec, httptest.NewRequest(http.MethodGet, "/admin/debug", nil))
	dst := pauseJSON(t, rec)
	if got := known(t, dst, "known_models"); len(got) != 0 {
		t.Fatalf("known_models = %v, want no entry for the model-less request", got)
	}

	// The guard itself: a direct empty add on any dimension is a no-op.
	p.pause.clients.add("")
	p.pause.providers.add("")
	p.pause.models.add("")
	rec = httptest.NewRecorder()
	p.HandlePause(rec, httptest.NewRequest(http.MethodGet, "/admin/pause", nil))
	st = pauseJSON(t, rec)
	for _, key := range []string{"known_clients", "known_providers"} {
		for _, name := range known(t, st, key) {
			if name == "" {
				t.Fatalf("%s gained an empty checkbox row", key)
			}
		}
	}
	rec = httptest.NewRecorder()
	p.HandleDebug(rec, httptest.NewRequest(http.MethodGet, "/admin/debug", nil))
	for _, name := range known(t, pauseJSON(t, rec), "known_models") {
		if name == "" {
			t.Fatal("known_models gained an empty checkbox row")
		}
	}
}
