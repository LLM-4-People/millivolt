package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
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
	if !p.scheduler.Stats().Paused {
		t.Fatal("scheduler not paused after POST")
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
	p.pause.clients.add("client-a")
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
	p.pause.providers.add("alpha.example")
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
	holds := p.PauseSnapshot()["holds"].([]map[string]any)
	if len(holds) != 2 {
		t.Fatalf("holds = %d, want 2", len(holds))
	}
	var idOpen string
	for _, h := range holds {
		for _, c := range h["clients"].([]string) {
			if c == "client-a" {
				idOpen = h["id"].(string)
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
	// MaxConcurrent is a per-group concurrency cap, not a queue cap: it
	// must not feed the hold's default queue capacity.
	d2 := config.Default()
	d2.MaxConcurrent = 7
	d2.MaxQueueSize = 0 // unlimited by explicit config, not by MaxConcurrent
	p2 := New(d2, metrics.Noop{})
	rr2 := httptest.NewRecorder()
	p2.HandlePause(rr2, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"clients":["c"]}`)))
	if st := pauseJSON(t, rr2); st["default_max_queued"] != float64(0) {
		t.Fatalf("default_max_queued = %v, want 0: MaxConcurrent must not feed the hold default", st["default_max_queued"])
	}
	d := config.Default()
	d.MaxQueueSize = 4
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
		t.Fatalf("default_max_queued = %v, want 4", st["default_max_queued"])
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
		t.Fatalf("lost a concurrent hold: a=%v b=%v",
			p.scheduler.Holds("a"), p.scheduler.Holds("b"))
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
