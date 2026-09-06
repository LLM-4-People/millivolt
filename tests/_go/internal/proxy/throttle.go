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
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

type countingThrottlePersist struct {
	mu    sync.Mutex
	saves int
	raw   []byte
}

func (p *countingThrottlePersist) SaveThrottle(_ context.Context, raw []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.saves++
	p.raw = append(p.raw[:0], raw...)
	return nil
}

func (p *countingThrottlePersist) LoadThrottle(context.Context) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.raw...), nil
}

func TestThrottleUnchangedHeadersSkipPersistence(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	provider := "provider.example"
	initial := scheduler.Throttle{Provider: provider, Limit: scheduler.Limit{Concurrency: 3}, Source: scheduler.ThrottleSourceUI, UpdatedBy: "dashboard", UpdatedAt: time.Now().Add(-time.Hour)}
	p.applyThrottle(initial)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(hdrLimitConcurrency, "3")
	for i := 0; i < 20; i++ {
		if err := p.applyThrottleHeaders(r, provider, "another-client"); err != nil {
			t.Fatal(err)
		}
	}
	if store.saves != 1 || p.scheduler.ThrottleFor(provider) != initial {
		t.Fatal("repeated header rewrote a dashboard policy or its persistence")
	}
	// The opposite source direction is also a policy no-op.
	r.Header.Set(hdrLimitConcurrency, "4")
	if err := p.applyThrottleHeaders(r, provider, "another-client"); err != nil {
		t.Fatal(err)
	}
	headerPolicy := p.scheduler.ThrottleFor(provider)
	if store.saves != 2 || headerPolicy.Source != scheduler.ThrottleSourceHeader || headerPolicy.UpdatedBy != "another-client" || !headerPolicy.UpdatedAt.After(initial.UpdatedAt) {
		t.Fatal("changed header policy was not persisted with new attribution")
	}
	initial.Limit.Concurrency = 4
	p.applyThrottle(initial)
	if store.saves != 2 || p.scheduler.ThrottleFor(provider) != headerPolicy {
		t.Fatal("identical UI cap replaced the last actual header policy change")
	}
	r.Header.Set(hdrLimitConcurrency, "off")
	for i := 0; i < 2; i++ {
		if err := p.applyThrottleHeaders(r, provider, "another-client"); err != nil {
			t.Fatal(err)
		}
	}
	if store.saves != 3 || p.scheduler.ThrottleFor(provider).Limit.Active() {
		t.Fatal("turning the same cap off must persist exactly once")
	}
}

func TestThrottleConcurrentHeadersMergeLatestPolicy(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
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
		if got := p.scheduler.ThrottleFor(provider).Limit; got != want {
			t.Fatalf("concurrent partial updates lost a dimension: %+v", got)
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
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(hdrLimitConcurrency, "3")
	if err := p.applyThrottleHeaders(r, "provider.example", "client"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := p.applyThrottleHeaders(r, "provider.example", "client"); err != nil {
			b.Fatal(err)
		}
	}
	if store.saves != 1 {
		b.Fatalf("unchanged headers caused %d persistence calls", store.saves)
	}
}

func TestThrottleConcurrentUIAndHeaderMerge(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	provider := "provider.example"
	for i := 0; i < 40; i++ {
		p.applyThrottle(scheduler.Throttle{Provider: provider})
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			<-start
			if rr := postThrottle(t, p, `{"provider":"provider.example","concurrency":3}`); rr.Code != http.StatusOK {
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
		want := scheduler.Limit{Concurrency: 3, Tokens: 100, TokWindow: time.Minute}
		if got := p.scheduler.ThrottleFor(provider).Limit; got != want {
			t.Fatalf("UI/header merge lost a dimension: %+v", got)
		}
		// A window-only update depends on the current count under that same
		// policy lock; an invalid multi-field patch must not partially apply.
		if rr := postThrottle(t, p, `{"provider":"provider.example","token_window":"2m"}`); rr.Code != http.StatusOK {
			t.Fatalf("window-only patch: %d %s", rr.Code, rr.Body.String())
		}
		before, saves := p.scheduler.ThrottleFor(provider), store.saves
		if before.Limit.Tokens != 100 || before.Limit.TokWindow != 2*time.Minute {
			t.Fatal("window-only patch lost the current count")
		}
		if rr := postThrottle(t, p, `{"provider":"provider.example","concurrency":8,"tokens":-1}`); rr.Code != http.StatusBadRequest {
			t.Fatalf("invalid UI patch status = %d", rr.Code)
		}
		if p.scheduler.ThrottleFor(provider) != before || store.saves != saves {
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
	th := p.scheduler.ThrottleFor(testProvider(t, upstream.URL))
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

	th := p.scheduler.ThrottleFor(testProvider(t, upstream.URL))
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
	th = p.scheduler.ThrottleFor(testProvider(t, upstream.URL))
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
	th := p.scheduler.ThrottleFor(testProvider(t, upstream.URL))
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
	rr := postThrottle(t, p, `{"provider":"alpha.example","concurrency":4,"requests":30,"request_window":"1m","tokens":90000,"token_window":"1m"}`)
	if rr.Code != 200 {
		t.Fatalf("status = %d body %s", rr.Code, rr.Body.Bytes())
	}
	th := p.scheduler.ThrottleFor("alpha.example")
	if th.Limit.Concurrency != 4 || th.Limit.Requests != 30 || th.Limit.Tokens != 90000 {
		t.Fatalf("limit = %+v", th.Limit)
	}
	if th.Source != "ui" || th.UpdatedBy != "dashboard" {
		t.Fatalf("source = %s by %s", th.Source, th.UpdatedBy)
	}

	rr = postThrottle(t, p, `{"provider":"alpha.example","concurrency":0}`)
	if rr.Code != 200 {
		t.Fatalf("clear conc status = %d", rr.Code)
	}
	th = p.scheduler.ThrottleFor("alpha.example")
	if th.Limit.Concurrency != 0 || th.Limit.Requests != 30 {
		t.Fatalf("merge = %+v", th.Limit)
	}

	rr = postThrottle(t, p, `{"provider":"alpha.example","clear":true}`)
	if rr.Code != 200 {
		t.Fatalf("clear status = %d", rr.Code)
	}
	if p.scheduler.ThrottleFor("alpha.example").Limit.Active() {
		t.Fatal("clear left a cap")
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
}

func TestThrottlePersistsAndRestores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "th.db")
	d := config.Default()
	store, err := storage.Open(path, storage.Options{
		WriteChanCap: d.StorageWriteChanCap, BatchCap: d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval, QueryTimeout: d.StorageQueryTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	p := New(d, metrics.Noop{})
	p.AttachPausePersist(store)
	rr := postThrottle(t, p, `{"provider":"alpha.example","concurrency":2,"requests":20,"request_window":"1m"}`)
	if rr.Code != 200 {
		t.Fatalf("status = %d %s", rr.Code, rr.Body.Bytes())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2, err := storage.Open(path, storage.Options{
		WriteChanCap: d.StorageWriteChanCap, BatchCap: d.StorageBatchCap,
		FlushInterval: d.StorageFlushInterval, QueryTimeout: d.StorageQueryTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	p2 := New(d, metrics.Noop{})
	p2.AttachPausePersist(store2)
	th := p2.scheduler.ThrottleFor("alpha.example")
	if th.Limit.Concurrency != 2 || th.Limit.Requests != 20 {
		t.Fatalf("restored = %+v", th.Limit)
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
	th := p.scheduler.ThrottleFor(testProvider(t, upstream.URL))
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
	deadline := time.Now().Add(2 * time.Second)
	for {
		infos := p.scheduler.ListThrottles()
		if len(infos) == 1 && infos[0].InFlight == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first request never held the provider slot: %+v", infos)
		}
		time.Sleep(time.Millisecond)
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

	// A set cap shows up in both surfaces.
	setp := httptest.NewRecorder()
	p.HandleThrottle(setp, httptest.NewRequest(http.MethodPost, "/admin/throttle",
		strings.NewReader(`{"provider":"alpha.example","concurrency":2}`)))
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
}
