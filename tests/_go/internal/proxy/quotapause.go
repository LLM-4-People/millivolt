package proxy

import (
	"encoding/json"
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

// The durable quota/billing 429 family and its canonical envelope, shared by
// every label-neutral test in this file. Tests that pin the label
// derivation inline their own envelope spellings.
const quotaBody = `{"error":{"message":"insufficient credits available in the account","type":"insufficient_quota","code":"insufficient_credits"}}`

func quotaPauseTestConfig(mode string) *config.Config {
	c := config.Default()
	c.QuotaPauseMode = mode
	// Storm protection stays disabled: the quota gate must work without it.
	// Its recovery pacing knobs are reused for probe cooldowns.
	c.StormInitialBackoff = 20 * time.Millisecond
	c.StormMaxBackoff = 40 * time.Millisecond
	c.StormJitterPercent = 0
	c.StormMaxWait = 2 * time.Second
	c.QuotaPauseRecoverySuccesses = 1
	c.BaseBackoff = time.Millisecond
	c.MaxBackoff = time.Millisecond
	c.MaxRetries = 0
	c.QualityRetries = 0
	c.ThinkingRetries = 0
	return c
}

// providerOfURL is the provider label a base-URL routed request derives: the
// authority (an IP endpoint keeps host:port).
func providerOfURL(raw string) string {
	return strings.TrimPrefix(raw, "http://")
}

func quotaStormRow(s *Server) (scheduler.StormStatus, bool) {
	for _, row := range s.scheduler.StormSnapshot() {
		if row.Quota {
			return row, true
		}
	}
	return scheduler.StormStatus{}, false
}

// quotaPollCond polls a quota-gate predicate with a bounded deadline.
func quotaPollCond(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(what)
}

// TestQuotaPauseRetryParksTriggerAndSiblingUntilRecovery pins the confirmed
// contract: the triggering request absorbs the durable quota 429 and parks on
// the provider's recovery gate, a sibling parks at admission, and the
// operator resume closes the gate so both requests drain with the upstream
// success. The 429 carries a large Retry-After so the parked state is stable:
// a transient pacing window (~20-40ms) would race the sibling-park poll and
// the calls==1 guard under load. The resumed trigger re-sends as an ordinary
// send and resolves (the release already closed the gate, so its 2xx closes
// nothing), and the time-driven window-elapse path stays covered by
// TestQuotaPauseRetriesDoNotBurnTransientBudget.
func TestQuotaPauseRetryParksTriggerAndSiblingUntilRecovery(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// A large Retry-After parks the provider for >=29s, so the
			// parked state below is stable instead of a transient
			// ~20-40ms pacing window.
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			// A numeric business code (Z.AI 1113, balance exhausted) beside
			// the generic envelope type: the gate label must derive from
			// the matched code token.
			io.WriteString(w, `{"error":{"message":"insufficient credits available in the account","type":"provider_error","code":"1113"}}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"reloaded"}}]}`)
	}))
	defer up.Close()
	provider := providerOfURL(up.URL)
	buf := metrics.NewBuffer(10)
	s := New(quotaPauseTestConfig(config.QuotaPauseRetry), buf)

	triggerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { triggerDone <- stormRequest(t, s, up.URL, "model-a", "key-a") }()
	quotaPollCond(t, "trigger never hit upstream", func() bool { return calls.Load() >= 1 })
	// The gate is open with the hinted floor, so the parked state is stable
	// for >=29s: the sibling deterministically parks at admission.
	quotaPollCond(t, "quota gate never opened", func() bool {
		row, ok := quotaStormRow(s)
		return ok && !row.RetryAt.IsZero() && time.Until(row.RetryAt) >= 29*time.Second
	})

	// While the gate is open the sibling parks before admission and never
	// reaches the exhausted upstream.
	siblingDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { siblingDone <- stormRequest(t, s, up.URL, "model-a", "key-b") }()
	quotaPollCond(t, "sibling never parked on the quota gate", func() bool {
		row, ok := quotaStormRow(s)
		return ok && row.Provider == provider && row.Queued >= 1
	})
	if row, ok := quotaStormRow(s); !ok || row.Reason != "1113" {
		t.Fatalf("quota gate label = %q, want the matched code token 1113", row.Reason)
	}
	if calls.Load() != 1 {
		t.Fatalf("parked sibling reached upstream early (calls=%d)", calls.Load())
	}

	// The operator resume closes the gate without waiting out the hinted
	// window: the trigger re-sends as an ordinary send, and both requests
	// drain with the same upstream success.
	providerJSON, _ := json.Marshal(provider)
	rr := httptest.NewRequest(http.MethodPost, "/admin/quota",
		strings.NewReader(`{"provider":`+string(providerJSON)+`,"resume":true}`))
	rr.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	s.HandleQuotaPause(rw, rr)
	if rw.Code != 200 {
		t.Fatalf("resume endpoint status = %d body=%s", rw.Code, rw.Body)
	}
	var w, wsib *httptest.ResponseRecorder
	select {
	case w = <-triggerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("trigger never recovered")
	}
	select {
	case wsib = <-siblingDone:
	case <-time.After(2 * time.Second):
		t.Fatal("sibling never drained after recovery")
	}
	if w.Code != 200 || !strings.Contains(w.Body.String(), "reloaded") ||
		wsib.Code != 200 || !strings.Contains(wsib.Body.String(), "reloaded") {
		t.Fatalf("trigger=%d/%s sibling=%d/%s", w.Code, w.Body, wsib.Code, wsib.Body)
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream calls = %d, want 3 (quota 429 + trigger re-send + sibling)", calls.Load())
	}
	if _, ok := quotaStormRow(s); ok {
		t.Fatal("resume left the quota gate open")
	}

	// The trigger's record shows the absorbed quota attempt under the final
	// success: quota is an account condition, not a rate limit.
	recs := waitForRecord(t, buf, 2)
	var trigger *metrics.Record
	for _, rec := range recs {
		if rec.Retries > 0 {
			trigger = rec
		}
	}
	if trigger == nil || trigger.StatusCode != 200 || trigger.Retries != 1 ||
		len(trigger.Attempts) != 1 || trigger.Attempts[0].ErrorType != "provider_error" ||
		trigger.Attempts[0].ErrorCode != "1113" || trigger.RateLimited {
		t.Fatalf("trigger record = %+v attempts=%+v", trigger, trigger.Attempts)
	}
}

// TestQuotaPauseRetryHonorsRetryAfterAndOperatorRelease pins the provider
// hint contract: a Retry-After on the quota 429 is the floor for the next
// recovery probe, and the operator resume endpoint closes the gate without
// waiting out the window.
func TestQuotaPauseRetryHonorsRetryAfterAndOperatorRelease(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, quotaBody)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"reloaded"}}]}`)
	}))
	defer up.Close()
	provider := providerOfURL(up.URL)
	s := New(quotaPauseTestConfig(config.QuotaPauseRetry), metrics.Noop{})

	triggerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { triggerDone <- stormRequest(t, s, up.URL, "model-a", "key-a") }()
	quotaPollCond(t, "trigger never hit upstream", func() bool { return calls.Load() >= 1 })
	quotaPollCond(t, "quota gate never opened", func() bool {
		row, ok := quotaStormRow(s)
		return ok && !row.RetryAt.IsZero() && time.Until(row.RetryAt) >= 29*time.Second
	})
	// The parked trigger has not re-sent inside the hinted window.
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("probe re-sent before the Retry-After floor (calls=%d)", calls.Load())
	}

	// The operator resume action closes the gate immediately: the trigger
	// re-sends and resolves without waiting out the 30-second hint.
	providerJSON, _ := json.Marshal(provider)
	rr := httptest.NewRequest(http.MethodPost, "/admin/quota",
		strings.NewReader(`{"provider":`+string(providerJSON)+`,"resume":true}`))
	rr.Header.Set("Content-Type", "application/json")
	rw := httptest.NewRecorder()
	s.HandleQuotaPause(rw, rr)
	if rw.Code != 200 {
		t.Fatalf("resume endpoint status = %d body=%s", rw.Code, rw.Body)
	}
	var doc map[string]any
	if err := json.Unmarshal(rw.Body.Bytes(), &doc); err != nil || len(doc) != 3 {
		t.Fatalf("resume response = %s err=%v", rw.Body, err)
	}
	select {
	case w := <-triggerDone:
		if w.Code != 200 || !strings.Contains(w.Body.String(), "reloaded") {
			t.Fatalf("resumed trigger = %d/%s", w.Code, w.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("resumed trigger never re-sent")
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
	if _, ok := quotaStormRow(s); ok {
		t.Fatal("resume left the gate open")
	}
	// A resume with no open gate is a no-op state report, not an error.
	rw = httptest.NewRecorder()
	s.HandleQuotaPause(rw, httptest.NewRequest(http.MethodPost, "/admin/quota",
		strings.NewReader(`{"provider":"unknown.example","resume":true}`)))
	if rw.Code != 200 {
		t.Fatalf("no-op resume status = %d", rw.Code)
	}
	// The strict decoder rejects missing, malformed or unknown-field bodies,
	// and a resume command must carry resume true.
	for _, body := range []string{
		`{}`, `{"provider":"p"}`, `{"resume":true}`,
		`{"provider":"","resume":true}`, `{"provider":"p","resume":"yes"}`,
		`{"provider":"p","resume":true,"unplanned":1}`,
		`{"provider":"p","resume":false}`,
	} {
		rw = httptest.NewRecorder()
		rr = httptest.NewRequest(http.MethodPost, "/admin/quota", strings.NewReader(body))
		rr.Header.Set("Content-Type", "application/json")
		s.HandleQuotaPause(rw, rr)
		if rw.Code != 400 {
			t.Fatalf("invalid resume body %q accepted with %d", body, rw.Code)
		}
	}
}

// TestQuotaPauseManualCreatesHoldParksUntilOperatorResume pins the manual
// mode: the durable quota 429 installs an indefinite provider-scoped operator
// hold (persisted pause surface, reason labeled), the triggering request and
// siblings park on it, and the operator's resume drains them.
func TestQuotaPauseManualCreatesHoldParksUntilOperatorResume(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			// Non-canonical casing: the hold label must carry the
			// lowercased vocabulary token, never the raw envelope spelling.
			io.WriteString(w, `{"error":{"message":"insufficient credits available in the account","type":"Insufficient_Quota","code":"insufficient_credits"}}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"reloaded"}}]}`)
	}))
	defer up.Close()
	provider := providerOfURL(up.URL)
	buf := metrics.NewBuffer(10)
	s := New(quotaPauseTestConfig(config.QuotaPauseManual), buf)

	triggerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { triggerDone <- stormRequest(t, s, up.URL, "model-a", "key-a") }()
	quotaPollCond(t, "trigger never hit upstream", func() bool { return calls.Load() >= 1 })

	// The auto-created hold appears on the operator pause surface with the
	// quota reason and no deadline.
	quotaPollCond(t, "quota hold never appeared on the pause surface", func() bool {
		snap := s.PauseSnapshot()
		holds, _ := snap["holds"].([]map[string]any)
		for _, h := range holds {
			providers, _ := h["providers"].([]string)
			if len(providers) == 1 && providers[0] == provider && h["until"] == nil {
				return true
			}
		}
		return false
	})
	if holds, _ := s.PauseSnapshot()["holds"].([]map[string]any); len(holds) != 1 || holds[0]["reason"] != "insufficient_quota" {
		t.Fatalf("quota hold labels = %+v, want the lowercased vocabulary token", holds)
	}
	siblingDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { siblingDone <- stormRequest(t, s, up.URL, "model-a", "key-b") }()
	quotaPollCond(t, "sibling never parked on the quota hold", func() bool {
		return calls.Load() == 1 && s.scheduler.Stats().Queued >= 1
	})
	if calls.Load() != 1 {
		t.Fatalf("parked sibling reached upstream early (calls=%d)", calls.Load())
	}

	// Operator resume (the pause surface's clear action) drains both.
	if err := s.applyHolds(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-triggerDone:
		if w.Code != 200 || !strings.Contains(w.Body.String(), "reloaded") {
			t.Fatalf("resumed trigger = %d/%s", w.Code, w.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("trigger never resumed after operator unpause")
	}
	select {
	case w := <-siblingDone:
		if w.Code != 200 {
			t.Fatalf("resumed sibling = %d/%s", w.Code, w.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sibling never resumed after operator unpause")
	}
	if calls.Load() != 3 {
		t.Fatalf("upstream calls = %d, want 3", calls.Load())
	}
	// Only the triggering request absorbed a quota attempt; the sibling was
	// parked in the group queue (its queue wait keeps the rate-limit flag).
	recs := waitForRecord(t, buf, 2)
	var trigger *metrics.Record
	for _, rec := range recs {
		if rec.Retries > 0 {
			trigger = rec
		}
	}
	if trigger == nil || trigger.StatusCode != 200 || trigger.Retries != 1 ||
		len(trigger.Attempts) != 1 || trigger.Attempts[0].ErrorType != "Insufficient_Quota" {
		t.Fatalf("trigger record = %+v attempts=%+v", trigger, trigger.Attempts)
	}
}

// TestQuotaPauseHostileTypeNeverLabelsTheGate pins the label safety contract:
// a hostile envelope (arbitrary provider-controlled type text beside a
// vocabulary code) labels the gate with the matched code token, never the
// raw type - the classification owner bounds every label to the vocabulary.
func TestQuotaPauseHostileTypeNeverLabelsTheGate(t *testing.T) {
	hostileType := strings.Repeat("x", 300)
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"message":"insufficient credits available in the account","type":"`+hostileType+`","code":"1113"}}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"reloaded"}}]}`)
	}))
	defer up.Close()
	s := New(quotaPauseTestConfig(config.QuotaPauseRetry), metrics.Noop{})
	triggerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { triggerDone <- stormRequest(t, s, up.URL, "model-a", "key-a") }()
	quotaPollCond(t, "trigger never hit upstream", func() bool { return calls.Load() >= 1 })
	quotaPollCond(t, "quota gate never opened", func() bool {
		_, ok := quotaStormRow(s)
		return ok
	})
	row, _ := quotaStormRow(s)
	if row.Reason != "1113" {
		t.Fatalf("quota gate label = %q, want the matched code token 1113", row.Reason)
	}
	// The labeled gate still recovers: the parked trigger becomes the probe
	// and its 2xx closes the gate.
	select {
	case w := <-triggerDone:
		if w.Code != 200 || !strings.Contains(w.Body.String(), "reloaded") {
			t.Fatalf("resumed trigger = %d/%s", w.Code, w.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("trigger never recovered")
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2 (hostile 429 + probe)", calls.Load())
	}
	if _, ok := quotaStormRow(s); ok {
		t.Fatal("quota gate stayed open after a 2xx probe")
	}
}

// TestQuotaPauseSkipsNonOpenAIWireTargets pins the scope: an armed quota
// pause never triggers for a translated (anthropic) upstream - its durable
// quota 429 keeps the historical surface-immediately behavior.
func TestQuotaPauseSkipsNonOpenAIWireTargets(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, quotaBody)
	}))
	defer up.Close()
	s := New(quotaPauseTestConfig(config.QuotaPauseRetry), metrics.Noop{})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"claude-x","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set(hdrBaseURL, up.URL)
	r.Header.Set(hdrFormat, "anthropic")
	r.Header.Set("Authorization", "Bearer key-a")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 429 || calls.Load() != 1 {
		t.Fatalf("status = %d calls = %d, want verbatim 429 on first send", w.Code, calls.Load())
	}
	if _, ok := quotaStormRow(s); ok {
		t.Fatal("non-OpenAI-wire target opened the quota gate")
	}
	if snap := s.PauseSnapshot(); snap["paused"] != false {
		t.Fatal("non-OpenAI-wire target created a hold")
	}
}

// TestQuotaPauseRetriesDoNotBurnTransientBudget pins the budget separation:
// quota re-sends are unbounded by the transient budget, so a later ordinary
// rate limit still gets its full shared retry allowance after the pause.
func TestQuotaPauseRetriesDoNotBurnTransientBudget(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1, 2:
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, quotaBody)
		case 3:
			// A plain rate limit after the account was reloaded: still
			// retryable with the full max_retries budget.
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
		default:
			io.WriteString(w, `{"choices":[{"message":{"content":"done"}}]}`)
		}
	}))
	defer up.Close()
	cfg := quotaPauseTestConfig(config.QuotaPauseRetry)
	cfg.MaxRetries = 1
	buf := metrics.NewBuffer(10)
	s := New(cfg, buf)
	start := time.Now()
	w := stormRequest(t, s, up.URL, "model-a", "key-a")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "done") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body)
	}
	if calls.Load() != 4 {
		t.Fatalf("upstream calls = %d, want 4 (quota, quota, rate limit, success)", calls.Load())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("quota parks waited %v; pacing must stay on the recovery gate", elapsed)
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].Retries != 3 || len(recs[0].Attempts) != 3 || recs[0].StatusCode != 200 {
		t.Fatalf("record = %+v attempts=%+v", recs[0], recs[0].Attempts)
	}
}

// TestQuotaPauseInBandErrorOpensGate pins the in-band twin: a durable
// quota/billing error inside a 200 stream surfaces to the client verbatim
// (that request already answered) but still arms the provider gate for the
// requests that follow.
func TestQuotaPauseInBandErrorOpensGate(t *testing.T) {
	var calls atomic.Int32
	errChunk := "data: {\"error\":{\"message\":\"insufficient credits available in the account\",\"type\":\"insufficient_quota\",\"code\":\"insufficient_credits\"}}\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.Write([]byte(errChunk))
		w.(http.Flusher).Flush()
	}))
	defer up.Close()
	buf := metrics.NewBuffer(10)
	s := New(quotaPauseTestConfig(config.QuotaPauseRetry), buf)
	srv := httptest.NewServer(s)
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-key")
	req.Header.Set("X-Proxy-Base-URL", up.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "insufficient_quota") {
		t.Fatalf("status = %d body = %s, want the in-band quota error relayed", resp.StatusCode, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (quota is never rescued)", calls.Load())
	}
	recs := waitForRecord(t, buf, 1)
	if recs[0].ErrorType != "insufficient_quota" || recs[0].RateLimited {
		t.Fatalf("record = %+v, want the quota envelope without a rate-limit flag", recs[0])
	}
	quotaPollCond(t, "in-band quota error did not arm the gate", func() bool {
		row, ok := quotaStormRow(s)
		return ok && row.Reason == "insufficient_quota"
	})
}

// TestQuotaPauseManualFallsBackWhenHoldCannotParkTheRequest pins the
// per-request park promise: an operator hold on OTHER clients overlaps the
// provider pair and rejects the automatic provider hold, but it never parks
// the triggering request - so the quota 429 falls back to the historical
// surface-immediately answer instead of re-sending unparked in a hot loop.
func TestQuotaPauseManualFallsBackWhenHoldCannotParkTheRequest(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, quotaBody)
	}))
	defer up.Close()
	cfg := quotaPauseTestConfig(config.QuotaPauseManual)
	s := New(cfg, metrics.Noop{})
	// An operator holds every client EXCEPT the triggering one.
	if err := s.applyHolds([]persistedPause{{ID: "op", Clients: []string{"someone-else"}}}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.applyHolds(nil) }()
	start := time.Now()
	w := stormRequest(t, s, up.URL, "model-a", "key-a")
	if w.Code != 429 || w.Body.String() != quotaBody {
		t.Fatalf("status = %d body = %q, want the verbatim quota 429", w.Code, w.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no unparked re-send loop)", calls.Load())
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("unarmed fallback took %v; the 429 must surface immediately", elapsed)
	}
	snap := s.PauseSnapshot()
	if snap["paused"] != true {
		// The operator's own hold still parks its client; no quota hold was
		// added beside it.
		t.Fatalf("operator hold disturbed: %+v", snap)
	}
	holds, _ := snap["holds"].([]map[string]any)
	if len(holds) != 1 || holds[0]["reason"] != "" {
		t.Fatalf("a quota hold was created despite the overlap: %+v", holds)
	}
}

// TestQuotaPauseManualParksUnderExistingHoldForThisClient pins the accepted
// already-parked answer: when an operator pause that parks this request lands
// while its quota 429 is still in flight, the manual mode reuses it instead of
// creating an overlapping hold, and the request waits for that hold like any
// other - never re-sending unparked.
func TestQuotaPauseManualParksUnderExistingHoldForThisClient(t *testing.T) {
	var calls atomic.Int32
	cfg := quotaPauseTestConfig(config.QuotaPauseManual)
	s := New(cfg, metrics.Noop{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// The operator pauses everything between the request's admission
			// and its quota 429: the re-send must park on this hold.
			if err := s.applyHolds([]persistedPause{{ID: "op", All: true}}); err != nil {
				t.Error(err)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, quotaBody)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"reloaded"}}]}`)
	}))
	defer up.Close()
	defer func() { _ = s.applyHolds(nil) }()
	triggerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { triggerDone <- stormRequest(t, s, up.URL, "model-a", "key-a") }()
	quotaPollCond(t, "trigger never hit upstream", func() bool { return calls.Load() >= 1 })
	// The overlap branch is observable as a parked re-send: no second hold
	// beside the operator's, and no unparked re-send loop. A leaked park
	// promise would hammer upstream within milliseconds.
	time.Sleep(100 * time.Millisecond)
	select {
	case w := <-triggerDone:
		t.Fatalf("trigger completed unparked: %d/%s", w.Code, w.Body)
	default:
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1 while parked on the operator hold", calls.Load())
	}
	if holds, _ := s.PauseSnapshot()["holds"].([]map[string]any); len(holds) != 1 ||
		holds[0]["id"] != "op" || holds[0]["reason"] != "" {
		t.Fatalf("quota trigger created a hold beside the operator's: %+v", holds)
	}
	if err := s.applyHolds(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-triggerDone:
		if w.Code != 200 || !strings.Contains(w.Body.String(), "reloaded") {
			t.Fatalf("resumed trigger = %d/%s", w.Code, w.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("trigger never resumed after operator unpause")
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls.Load())
	}
}

// TestQuotaPauseOffKeepsVerbatimSurface pins the default: with no mode armed
// a durable quota 429 answers the client immediately, opens nothing and
// parks nothing.
func TestQuotaPauseOffKeepsVerbatimSurface(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, quotaBody)
	}))
	defer up.Close()
	s := New(quotaPauseTestConfig(config.QuotaPauseOff), metrics.Noop{})
	w := stormRequest(t, s, up.URL, "model-a", "key-a")
	if w.Code != 429 || w.Body.String() != quotaBody || calls.Load() != 1 {
		t.Fatalf("status = %d calls = %d body = %q, want the verbatim quota 429", w.Code, calls.Load(), w.Body)
	}
	if _, ok := quotaStormRow(s); ok || s.scheduler.Paused() {
		t.Fatal("unarmed quota 429 opened a gate or hold")
	}
}
