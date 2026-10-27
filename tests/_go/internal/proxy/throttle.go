package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/mcp"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

type countingThrottlePersist struct {
	mu    sync.Mutex
	saves int
	bytes int64
	max   int
	raw   []byte
}

func (p *countingThrottlePersist) SaveThrottle(_ context.Context, raw []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saves++
	p.bytes += int64(len(raw))
	if len(raw) > p.max {
		p.max = len(raw)
	}
	p.raw = append(p.raw[:0], raw...)
	return nil
}

func (p *countingThrottlePersist) LoadThrottle(context.Context) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.raw...), nil
}

func (p *countingThrottlePersist) stats() (saves int, raw []byte, bytes int64, max int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saves, append([]byte(nil), p.raw...), p.bytes, p.max
}

func (p *countingThrottlePersist) saveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saves
}

// deferThrottleWriter parks the coalescing writer behind a long debounce so a
// test can assert exactly which writes the request and operator paths made.
// CloseThrottlePersist interrupts the timer; no test sleeps for this window.
func deferThrottleWriter(p *Server) {
	p.throttle.debounce = time.Hour
}

// schedThrottleFor reads one provider's installed cap through the production
// list path (ListThrottles is the dashboard GET). A provider with no gate
// reads as a zero Throttle, matching a cleared cap.
func schedThrottleFor(sched *scheduler.Scheduler, provider string) scheduler.Throttle {
	for _, info := range sched.ListThrottles() {
		if info.Provider == provider {
			return info.Throttle
		}
	}
	return scheduler.Throttle{}
}

func TestThrottleUnchangedHeadersSkipPersistence(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	deferThrottleWriter(p)
	t.Cleanup(func() {
		if err := p.CloseThrottlePersist(context.Background()); err != nil {
			t.Errorf("close throttle persister: %v", err)
		}
	})
	provider := "provider.example"
	// Seed the anti-burst pair so every header merge below must carry it: an
	// unchanged header is a whole-policy no-op only if both fields survive.
	initial := scheduler.Throttle{Provider: provider, Limit: scheduler.Limit{Concurrency: 3, AntiBurst: true, RampDuration: 10 * time.Second}, Source: scheduler.ThrottleSourceUI, UpdatedBy: "dashboard", UpdatedAt: time.Now().Add(-time.Hour)}
	p.applyThrottle(initial)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(hdrLimitConcurrency, "3")
	for i := 0; i < 20; i++ {
		if err := p.applyThrottleHeaders(r, provider, "another-client"); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if saves := store.saveCount(); saves != 1 || schedThrottleFor(p.scheduler, provider) != initial {
		t.Fatal("repeated header rewrote a dashboard policy or its persistence")
	}
	// The opposite source direction is also a policy no-op.
	r.Header.Set(hdrLimitConcurrency, "4")
	if err := p.applyThrottleHeaders(r, provider, "another-client"); err != nil {
		t.Fatal(err)
	}
	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatal(err)
	}
	headerPolicy := schedThrottleFor(p.scheduler, provider)
	if saves := store.saveCount(); saves != 2 || headerPolicy.Source != scheduler.ThrottleSourceHeader || headerPolicy.UpdatedBy != "another-client" || !headerPolicy.UpdatedAt.After(initial.UpdatedAt) {
		t.Fatal("changed header policy was not persisted with new attribution")
	}
	initial.Limit.Concurrency = 4
	p.applyThrottle(initial)
	if saves := store.saveCount(); saves != 2 || schedThrottleFor(p.scheduler, provider) != headerPolicy {
		t.Fatal("identical UI cap replaced the last actual header policy change")
	}
	r.Header.Set(hdrLimitConcurrency, "off")
	for i := 0; i < 2; i++ {
		if err := p.applyThrottleHeaders(r, provider, "another-client"); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatal(err)
	}
	if saves := store.saveCount(); saves != 3 || schedThrottleFor(p.scheduler, provider).Limit.Active() {
		t.Fatal("turning the same cap off must persist exactly once")
	}
}

func TestThrottleConcurrentHeadersMergeLatestPolicy(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	deferThrottleWriter(p)
	t.Cleanup(func() {
		if err := p.CloseThrottlePersist(context.Background()); err != nil {
			t.Errorf("close throttle persister: %v", err)
		}
	})
	provider := "provider.example"
	for i := 0; i < 40; i++ {
		p.applyThrottle(scheduler.Throttle{Provider: provider})
		start := make(chan struct{})
		var wg sync.WaitGroup
		for name, value := range map[string]string{hdrLimitConcurrency: "3", hdrLimitRequests: "10/1m", hdrLimitTokens: "100/1m"} {
			wg.Go(func() {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				r.Header.Set(name, value)
				<-start
				if err := p.applyThrottleHeaders(r, provider, "client"); err != nil {
					t.Error(err)
				}
			})
		}
		close(start)
		wg.Wait()
		want := scheduler.Limit{Concurrency: 3, Requests: 10, ReqWindow: time.Minute, Tokens: 100, TokWindow: time.Minute}
		if got := schedThrottleFor(p.scheduler, provider).Limit; got != want {
			t.Fatalf("concurrent partial updates lost a dimension: %+v", got)
		}
		if err := p.FlushThrottles(context.Background()); err != nil {
			t.Fatal(err)
		}
		raw, _ := store.LoadThrottle(context.Background())
		items := decodeThrottles(raw)
		if len(items) != 1 {
			t.Fatalf("persisted stale clear: %s", raw)
		}
		got, err := persistedToThrottle(items[0])
		if err != nil || got.Limit != want {
			t.Fatalf("persisted stale partial update: %s (%v)", raw, err)
		}
	}
}

func BenchmarkThrottleHeadersUnchanged(b *testing.B) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	deferThrottleWriter(p)
	b.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(hdrLimitConcurrency, "3")
	if err := p.applyThrottleHeaders(r, "provider.example", "client"); err != nil {
		b.Fatal(err)
	}
	if err := p.FlushThrottles(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := p.applyThrottleHeaders(r, "provider.example", "client"); err != nil {
			b.Fatal(err)
		}
	}
	if err := p.FlushThrottles(context.Background()); err != nil {
		b.Fatal(err)
	}
	if saves := store.saveCount(); saves != 1 {
		b.Fatalf("unchanged headers caused %d persistence calls", saves)
	}
}

func TestThrottleConcurrentUIAndHeaderMerge(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	deferThrottleWriter(p)
	t.Cleanup(func() {
		if err := p.CloseThrottlePersist(context.Background()); err != nil {
			t.Errorf("close throttle persister: %v", err)
		}
	})
	provider := "provider.example"
	for i := 0; i < 40; i++ {
		p.applyThrottle(scheduler.Throttle{Provider: provider})
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			if rr := postThrottle(t, p, `{"provider":"provider.example","concurrency":3,"anti_burst":true,"ramp_duration":"10s"}`); rr.Code != http.StatusOK {
				t.Errorf("UI patch: %d %s", rr.Code, rr.Body.String())
			}
		})
		wg.Go(func() {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			r.Header.Set(hdrLimitTokens, "100/1m")
			<-start
			if err := p.applyThrottleHeaders(r, provider, "client"); err != nil {
				t.Error(err)
			}
		})
		close(start)
		wg.Wait()
		want := scheduler.Limit{Concurrency: 3, Tokens: 100, TokWindow: time.Minute, AntiBurst: true, RampDuration: 10 * time.Second}
		if got := schedThrottleFor(p.scheduler, provider).Limit; got != want {
			t.Fatalf("UI/header merge lost a dimension: %+v", got)
		}
		// A window-only update depends on the current count under that same
		// policy lock; an invalid multi-field patch must not partially apply.
		if rr := postThrottle(t, p, `{"provider":"provider.example","token_window":"2m"}`); rr.Code != http.StatusOK {
			t.Fatalf("window-only patch: %d %s", rr.Code, rr.Body.String())
		}
		before, saves := schedThrottleFor(p.scheduler, provider), store.saveCount()
		if before.Limit.Tokens != 100 || before.Limit.TokWindow != 2*time.Minute {
			t.Fatal("window-only patch lost the current count")
		}
		if !before.Limit.AntiBurst || before.Limit.RampDuration != 10*time.Second {
			t.Fatalf("window-only patch lost the anti-burst pair: %+v", before.Limit)
		}
		if rr := postThrottle(t, p, `{"provider":"provider.example","concurrency":8,"tokens":-1}`); rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid UI patch status = %d", rr.Code)
		}
		if schedThrottleFor(p.scheduler, provider) != before || store.saveCount() != saves {
			t.Fatal("invalid UI patch partly changed or persisted the policy")
		}
	}
}

func testProvider(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return providerFromURL(u)
}

func getThrottleState(t *testing.T, p *Server) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	p.HandleThrottle(rr, httptest.NewRequest(http.MethodGet, "/admin/throttle", nil))
	if rr.Code != 200 {
		t.Fatalf("GET /admin/throttle status = %d body %s", rr.Code, rr.Body.Bytes())
	}
	var st map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func postThrottle(t *testing.T, p *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	p.HandleThrottle(rr, httptest.NewRequest(http.MethodPost, "/admin/throttle", strings.NewReader(body)))
	return rr
}

func throttleChat(t *testing.T, srv *httptest.Server, upstreamURL, key string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Proxy-Base-URL", upstreamURL)
	req.Header.Set("X-Proxy-Key", key)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestThrottleHeaderSetsProviderWideAndShowsInAPI(t *testing.T) {
	upstream, _ := mockUpstream(t, false)
	defer upstream.Close()
	p := New(config.Default(), metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp := throttleChat(t, srv, upstream.URL, "sk-a", map[string]string{
		"X-Proxy-Limit-Concurrency": "3",
		"X-Proxy-Limit-Requests":    "60/1m",
		"X-Proxy-Limit-Tokens":      "100000/1m",
		"X-Proxy-Client":            "client-a",
	})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	st := getThrottleState(t, p)
	list, _ := st["throttles"].([]any)
	if len(list) != 1 {
		t.Fatalf("throttles = %#v", st["throttles"])
	}
	row := list[0].(map[string]any)
	if row["concurrency"].(float64) != 3 {
		t.Errorf("concurrency = %v", row["concurrency"])
	}
	if row["requests"].(float64) != 60 {
		t.Errorf("requests = %v", row["requests"])
	}
	if row["request_window"] != "1m" {
		t.Errorf("request_window = %v", row["request_window"])
	}
	if row["tokens"].(float64) != 100000 {
		t.Errorf("tokens = %v", row["tokens"])
	}
	if row["source"] != "header" {
		t.Errorf("source = %v", row["source"])
	}
	if row["updated_by"] != "client-a" {
		t.Errorf("updated_by = %v", row["updated_by"])
	}

	// A different key does not need to resend the headers; the cap is provider-wide.
	th := schedThrottleFor(p.scheduler, testProvider(t, upstream.URL))
	if th.Limit.Concurrency != 3 {
		t.Fatalf("scheduler cap = %+v", th.Limit)
	}
}

func TestThrottleHeaderOffClearsDimension(t *testing.T) {
	upstream, _ := mockUpstream(t, false)
	defer upstream.Close()
	p := New(config.Default(), metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp := throttleChat(t, srv, upstream.URL, "sk-a", map[string]string{
		"X-Proxy-Limit-Concurrency": "2",
		"X-Proxy-Limit-Requests":    "10/1m",
	})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	resp = throttleChat(t, srv, upstream.URL, "sk-b", map[string]string{
		"X-Proxy-Limit-Concurrency": "0",
	})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	th := schedThrottleFor(p.scheduler, testProvider(t, upstream.URL))
	if th.Limit.Concurrency != 0 {
		t.Errorf("concurrency still %d", th.Limit.Concurrency)
	}
	if th.Limit.Requests != 10 {
		t.Errorf("requests cleared as a side effect: %d", th.Limit.Requests)
	}

	resp = throttleChat(t, srv, upstream.URL, "sk-c", map[string]string{
		"X-Proxy-Limit-Requests": "off",
		"X-Proxy-Limit-Tokens":   "none",
	})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	th = schedThrottleFor(p.scheduler, testProvider(t, upstream.URL))
	if th.Limit.Active() {
		t.Fatalf("wanted all off, got %+v", th.Limit)
	}
}

func TestThrottleHeaderInvalidIsAtomic(t *testing.T) {
	upstream, _ := mockUpstream(t, false)
	defer upstream.Close()
	p := New(config.Default(), metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	resp := throttleChat(t, srv, upstream.URL, "sk-a", map[string]string{
		"X-Proxy-Limit-Concurrency": "2",
	})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	resp = throttleChat(t, srv, upstream.URL, "sk-a", map[string]string{
		"X-Proxy-Limit-Concurrency": "9",
		"X-Proxy-Limit-Requests":    "not-a-rate",
	})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(body), "X-Proxy-Limit-Requests") {
		t.Errorf("error body = %s", body)
	}
	th := schedThrottleFor(p.scheduler, testProvider(t, upstream.URL))
	if th.Limit.Concurrency != 2 {
		t.Fatalf("partial apply: concurrency = %d, want still 2", th.Limit.Concurrency)
	}
}

func TestThrottleHeaderMissingWindowRejected(t *testing.T) {
	upstream, _ := mockUpstream(t, false)
	defer upstream.Close()
	p := New(config.Default(), metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()
	resp := throttleChat(t, srv, upstream.URL, "sk-a", map[string]string{
		"X-Proxy-Limit-Requests": "60",
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestThrottlePOSTAndClear(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	rr := postThrottle(t, p, `{"provider":"alpha.example","concurrency":4,"requests":30,"request_window":"1m","tokens":90000,"token_window":"1m","anti_burst":true,"ramp_duration":"10s"}`)
	if rr.Code != 200 {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.Bytes())
	}
	th := schedThrottleFor(p.scheduler, "alpha.example")
	if th.Limit.Concurrency != 4 || th.Limit.Requests != 30 || th.Limit.Tokens != 90000 {
		t.Fatalf("limit = %+v", th.Limit)
	}
	if !th.Limit.AntiBurst || th.Limit.RampDuration != 10*time.Second {
		t.Fatalf("ramp = %+v", th.Limit)
	}
	if th.Source != "ui" || th.UpdatedBy != "dashboard" {
		t.Fatalf("source = %s by %s", th.Source, th.UpdatedBy)
	}

	// Clearing one cap leaves the policy active, so the ramp stays installed.
	rr = postThrottle(t, p, `{"provider":"alpha.example","concurrency":0}`)
	if rr.Code != 200 {
		t.Fatalf("clear conc status = %d", rr.Code)
	}
	th = schedThrottleFor(p.scheduler, "alpha.example")
	if th.Limit.Concurrency != 0 || th.Limit.Requests != 30 {
		t.Fatalf("merge = %+v", th.Limit)
	}
	if !th.Limit.AntiBurst || th.Limit.RampDuration != 10*time.Second {
		t.Fatalf("clearing concurrency dropped the ramp: %+v", th.Limit)
	}

	rr = postThrottle(t, p, `{"provider":"alpha.example","clear":true}`)
	if rr.Code != 200 {
		t.Fatalf("clear status = %d", rr.Code)
	}
	if schedThrottleFor(p.scheduler, "alpha.example").Limit.Active() {
		t.Fatal("clear left a cap")
	}
	if th := schedThrottleFor(p.scheduler, "alpha.example").Limit; th.AntiBurst || th.RampDuration != 0 {
		t.Fatalf("clear left a ramp: %+v", th)
	}
}

func TestThrottlePOSTDenyByDefault(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	if postThrottle(t, p, `{}`).Code != 400 {
		t.Fatal("missing provider")
	}
	if postThrottle(t, p, `{"provider":"x"}`).Code != 400 {
		t.Fatal("missing fields")
	}
	if postThrottle(t, p, `{"provider":"x","concurrency":-1}`).Code != 400 {
		t.Fatal("negative concurrency")
	}
	// The ramp pair never activates a provider by itself, so enabling it on a
	// fresh name is refused instead of answering 200 for a dropped policy.
	if postThrottle(t, p, `{"provider":"x","anti_burst":true}`).Code != 400 {
		t.Fatal("anti-burst without a cap")
	}
	if postThrottle(t, p, `{"provider":"x","ramp_duration":"10s"}`).Code != 400 {
		t.Fatal("ramp duration without a cap")
	}
}

func TestThrottlePersistsAndRestores(t *testing.T) {
	p2 := persistRestoreProxy(t, "th.db", func(p *Server) *httptest.ResponseRecorder {
		return postThrottle(t, p, `{"provider":"alpha.example","concurrency":2,"requests":20,"request_window":"1m"}`)
	})
	th := schedThrottleFor(p2.scheduler, "alpha.example")
	if th.Limit.Concurrency != 2 || th.Limit.Requests != 20 {
		t.Fatalf("restored = %+v", th.Limit)
	}
}

// TestPersistedThrottleBytesMatchPolicyListing pins the durable document to
// the policy-only listing. It runs while one provider has an in-flight slot
// and a queued waiter, then compares the saved bytes with the document derived
// from the dashboard's ListThrottles snapshot for the same policies. Occupancy
// is ephemeral, so the two documents must be byte-identical and contain no
// occupancy fields.
func TestPersistedThrottleBytesMatchPolicyListing(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)

	base := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	policies := []scheduler.Throttle{
		{Provider: "alpha.example", Limit: scheduler.Limit{Requests: 30, ReqWindow: time.Minute}, Source: scheduler.ThrottleSourceUI, UpdatedBy: "dashboard", UpdatedAt: base},
		{Provider: "busy.example", Limit: scheduler.Limit{Concurrency: 1}, Source: scheduler.ThrottleSourceHeader, UpdatedBy: "client-a", UpdatedAt: base.Add(time.Minute)},
		{Provider: "ramp.example", Limit: scheduler.Limit{Concurrency: 4, AntiBurst: true, RampDuration: 10 * time.Second}, Source: scheduler.ThrottleSourceUI, UpdatedBy: "dashboard", UpdatedAt: base.Add(90 * time.Second)},
		{Provider: "zulu.example", Limit: scheduler.Limit{Tokens: 9000, TokWindow: time.Hour}, Source: scheduler.ThrottleSourceUI, UpdatedBy: "dashboard", UpdatedAt: base.Add(2 * time.Minute)},
	}
	for _, th := range policies {
		if err := p.applyThrottle(th); err != nil {
			t.Fatalf("apply %s: %v", th.Provider, err)
		}
	}

	holder, err := p.scheduler.AcquireWith(context.Background(), "busy.example|holder", 0,
		scheduler.WaiterHooks{Provider: "busy.example"})
	if err != nil {
		t.Fatal(err)
	}
	waiterDone := make(chan struct{})
	go func() {
		defer close(waiterDone)
		release, err := p.scheduler.AcquireWith(context.Background(), "busy.example|queued", 0,
			scheduler.WaiterHooks{Provider: "busy.example"})
		if err != nil {
			t.Errorf("queued acquire: %v", err)
			return
		}
		release(0)
	}()
	if !waitFor(t, func() bool { return p.scheduler.Stats().Queued != 0 }) {
		holder(0)
		<-waiterDone
		t.Fatal("waiter never queued")
	}
	defer func() {
		holder(0)
		<-waiterDone
	}()

	// A policy change on another provider saves the document while busy.example
	// still holds its in-flight slot and parked waiter.
	changed := policies[0]
	changed.Limit.Requests = 31
	if err := p.applyThrottle(changed); err != nil {
		t.Fatalf("apply changed policy: %v", err)
	}

	infos := p.scheduler.ListThrottles()
	legacy := make([]persistedThrottle, 0, len(infos))
	busyLive := false
	for _, inf := range infos {
		if inf.Provider == "busy.example" && inf.InFlight == 1 && inf.Queued == 1 {
			busyLive = true
		}
		legacy = append(legacy, throttleToPersisted(inf.Throttle))
	}
	if !busyLive {
		t.Fatalf("dashboard listing lost live occupancy: %+v", infos)
	}
	wantRaw, err := json.Marshal(persistedThrottleDoc{Throttles: legacy})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.LoadThrottle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, wantRaw) {
		t.Fatalf("saved throttle document differs from ListThrottles-derived bytes\n got: %s\nwant: %s", raw, wantRaw)
	}
	snap := p.latestThrottleSnap()
	if !reflect.DeepEqual(snap, legacy) {
		t.Fatalf("latestThrottleSnap = %+v, want %+v", snap, legacy)
	}
	snapRaw, err := json.Marshal(persistedThrottleDoc{Throttles: snap})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, snapRaw) {
		t.Fatalf("latestThrottleSnap bytes differ from the saved document\n got: %s\nwant: %s", snapRaw, raw)
	}
	for _, field := range []string{"in_flight", "queued", "requests_remaining", "tokens_remaining", "requests_capacity", "tokens_capacity"} {
		if bytes.Contains(raw, []byte(field)) {
			t.Fatalf("saved throttle document contains occupancy field %q: %s", field, raw)
		}
	}
}

func TestThrottleHeaderOnModelsDoesNotConsume(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer upstream.Close()
	p := New(config.Default(), metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Key", "sk-a")
	req.Header.Set("X-Proxy-Limit-Concurrency", "1")
	req.Header.Set("X-Proxy-Limit-Requests", "1/1h")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("models status = %d", resp.StatusCode)
	}
	th := schedThrottleFor(p.scheduler, testProvider(t, upstream.URL))
	if th.Limit.Concurrency != 1 || th.Limit.Requests != 1 {
		t.Fatalf("models did not set cap: %+v", th.Limit)
	}
	info := p.scheduler.ListThrottles()
	if len(info) != 1 || info[0].InFlight != 0 {
		t.Fatalf("models listing consumed a slot: %+v", info)
	}
}

func TestThrottleCrossKeyConcurrencyAtProxy(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()
	p := New(config.Default(), metrics.Noop{})
	srv := httptest.NewServer(p)
	defer srv.Close()

	started := make(chan struct{})
	go func() {
		close(started)
		resp := throttleChat(t, srv, upstream.URL, "sk-1", map[string]string{
			"X-Proxy-Limit-Concurrency": "1",
		})
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	<-started
	// The provider slot being held IS observable: poll the throttle's live
	// in-flight count until the first request is admitted, so the second key
	// truly tests the cap rather than a start race.
	if !waitFor(t, func() bool {
		infos := p.scheduler.ListThrottles()
		return len(infos) == 1 && infos[0].InFlight == 1
	}) {
		t.Fatalf("first request never held the provider slot: %+v", p.scheduler.ListThrottles())
	}

	done := make(chan struct{})
	go func() {
		resp := throttleChat(t, srv, upstream.URL, "sk-2", nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("second key was not throttled")
	case <-time.After(80 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second key stuck after release")
	}
}

func TestThrottleLimitHeadersNeverLeakUpstream(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"1","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer upstream.Close()
	srv := proxyServer(t)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Key", "sk-test")
	req.Header.Set("X-Proxy-Limit-Concurrency", "2")
	req.Header.Set("X-Proxy-Limit-Requests", "10/1m")
	req.Header.Set("X-Proxy-Limit-Tokens", "1000/1m")
	req.Header.Set("X-Proxy-Headers", `{"X-Proxy-Limit-Concurrency":["9"],"X-Proxy-Limit-Requests":["1/1s"]}`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	for _, h := range []string{"X-Proxy-Limit-Concurrency", "X-Proxy-Limit-Requests", "X-Proxy-Limit-Tokens"} {
		if v := got.Get(h); v != "" {
			t.Errorf("control header %s leaked upstream with value %q", h, v)
		}
	}
}

func TestParseLimitRate(t *testing.T) {
	n, w, off, err := parseLimitRate("60/1m", maxLimitRequests)
	if err != nil || off || n != 60 || w != time.Minute {
		t.Fatalf("60/1m = %d %s off=%v err=%v", n, w, off, err)
	}
	_, _, off, err = parseLimitRate("off", maxLimitRequests)
	if err != nil || !off {
		t.Fatalf("off: %v %v", off, err)
	}
	if _, _, _, err := parseLimitRate("60", maxLimitRequests); err == nil {
		t.Fatal("bare count should fail")
	}
	if _, _, _, err := parseLimitRate("60/1d", maxLimitRequests); err != nil {
		t.Fatalf("1d: %v", err)
	}
	if _, _, _, err := parseLimitRate("60/48h", maxLimitRequests); err == nil {
		t.Fatal("48h should exceed max window")
	}
	if _, _, _, err := parseLimitRate("-1/1m", maxLimitRequests); err == nil {
		t.Fatal("negative")
	}
}

// TestThrottleSnapshotMatchesGET: ThrottleSnapshot is the single owner of
// the /admin/throttle state shape - the GET body and the dashboard
// bootstrap section must never drift.
func TestThrottleSnapshotMatchesGET(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})

	get := httptest.NewRecorder()
	p.HandleThrottle(get, httptest.NewRequest(http.MethodGet, "/admin/throttle", nil))
	snap, err := json.Marshal(p.ThrottleSnapshot())
	if err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("GET body %s != ThrottleSnapshot %s", get.Body.String(), snap)
	}

	// A set cap shows up in both surfaces, ramp fields included.
	setp := httptest.NewRecorder()
	p.HandleThrottle(setp, httptest.NewRequest(http.MethodPost, "/admin/throttle",
		strings.NewReader(`{"provider":"alpha.example","concurrency":2,"anti_burst":true,"ramp_duration":"10s"}`)))
	if setp.Code != 200 {
		t.Fatalf("POST set throttle → %d body %s", setp.Code, setp.Body.String())
	}
	get2 := httptest.NewRecorder()
	p.HandleThrottle(get2, httptest.NewRequest(http.MethodGet, "/admin/throttle", nil))
	var st map[string]any
	if err := json.Unmarshal(get2.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["active"] != true {
		t.Fatalf("active = %v, want true after set", st["active"])
	}
	throttles, ok := st["throttles"].([]any)
	if !ok || len(throttles) != 1 {
		t.Fatalf("throttles = %v, want 1", st["throttles"])
	}
	row, _ := throttles[0].(map[string]any)
	if row["anti_burst"] != true || row["ramp_duration"] != "10s" {
		t.Fatalf("GET row ramp = %+v", row)
	}
}

// TestMCPThrottleSchemaMirrorsTheProxyBands pins the model-visible throttle
// argument documentation to the proxy constants that enforce it. The MCP
// server duplicates the bands so a typo becomes a readable tool message
// instead of a request the proxy refuses after assembly; the proxy owner is
// the source, and the schema text is what the model reads, so a band change on
// either side must fail here.
func TestMCPThrottleSchemaMirrorsTheProxyBands(t *testing.T) {
	windowBand := fmt.Sprintf("%s to %dh", minLimitWindow, int(maxLimitWindow/time.Hour))
	for _, tc := range []struct {
		field string
		want  string
	}{
		{"Concurrency", "0 to " + strconv.Itoa(maxLimitConcurrency)},
		{"Requests", "0 to " + strconv.FormatInt(maxLimitRequests, 10)},
		{"Tokens", "0 to " + strconv.FormatInt(maxLimitTokens, 10)},
		{"RequestWindow", windowBand},
		{"TokenWindow", windowBand},
		// The anti-burst ramp reuses the window band, and the flag's inertness
		// is the proxy's own rule (scheduler.Limit.rampActive): a schema that
		// promised a ramp without those two conditions would tell a model its
		// change takes effect when the scheduler ignores it.
		{"RampDuration", windowBand},
		{"AntiBurst", "off by default"},
		{"AntiBurst", "inert unless concurrency is at least 2"},
	} {
		field, ok := reflect.TypeOf(mcp.SetThrottleInput{}).FieldByName(tc.field)
		if !ok {
			t.Fatalf("mcp.SetThrottleInput has no %s field", tc.field)
		}
		if tag := string(field.Tag.Get("jsonschema")); !strings.Contains(tag, tc.want) {
			t.Fatalf("mcp.SetThrottleInput.%s schema %q must state the proxy band %q", tc.field, tag, tc.want)
		}
	}
}
