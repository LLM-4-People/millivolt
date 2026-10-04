package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// cursorReaskUpstream serves a sequence of agent.v1 Run exchanges: the first
// call ends with an EMPTY turn (the void - no deltas, turn_ended immediately),
// and subsequent calls answer with a text delta. matchAction, when non-nil,
// runs on each run_request payload so a test can pin the action encoding.
// Each run also carries its own X-Request-Id, so a recovered void's record
// can prove WHICH response its upstream metadata describes.
func cursorReaskUpstream(t *testing.T, matchAction func(payload []byte)) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body := r.Body
		w.Header().Set("Content-Type", "application/connect+proto")
		if n == 1 {
			w.Header().Set("X-Request-Id", "req-void")
		} else {
			w.Header().Set("X-Request-Id", "req-reask")
		}
		fl := w.(http.Flusher)
		w.WriteHeader(200)

		// 1. Read the run_request envelope.
		_, payload := readFrame(t, body)
		if matchAction != nil {
			matchAction(payload)
		}
		// 2. KV pull → answer; request-context handshake → answer.
		cursorConnectHandshake(t, w, body, fl)

		if n == 1 {
			// The void: turn ends immediately with no tokens and no deltas.
			w.Write(cframe(cmsg(1, cmsg(14, nil))))
			w.Write(cend("{}"))
			fl.Flush()
			return
		}
		// A real answer.
		w.Write(cframe(cmsg(1, cmsg(1, cstr(1, "finally an answer")))))
		w.Write(cframe(cmsg(1, cmsg(8, cvint(1, 3)))))
		w.Write(cframe(cmsg(1, cmsg(14, nil))))
		w.Write(cend("{}"))
		fl.Flush()
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	t.Cleanup(upstream.Close)
	return upstream
}

func cursorVoidRequest(srv *httptest.Server, upstream *httptest.Server) *http.Request {
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[
			{"role":"user","content":"Plan the tweet."},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"plan","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call-1","name":"plan","content":"the sky is blue"}
		],"stream":true}`))
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Format", "cursor")
	return req
}

// TestCursorVoidReaskFreshUser: a resume-action continuation that voids is
// transparently re-driven ONCE as a fresh user turn (user_message_action) on a
// new run; the client sees only the re-ask's answer on the same stream -
// never an empty_turn error, never a [DONE] between the attempts.
func TestCursorVoidReaskFreshUser(t *testing.T) {
	upstream := cursorReaskUpstream(t, nil)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	resp, err := http.DefaultClient.Do(cursorVoidRequest(srv, upstream))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	got := string(b)

	if !strings.Contains(got, `"content":"finally an answer"`) {
		t.Fatalf("re-ask answer missing: %s", got)
	}
	if strings.Contains(got, `"code":"empty_turn"`) {
		t.Fatalf("a recovered re-ask must not surface empty_turn: %s", got)
	}
	if strings.Count(got, "data: [DONE]") != 1 {
		t.Fatalf("exactly one [DONE] across the transparent re-ask: %s", got)
	}

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() {
		t.Fatalf("recovered turn must not be an error: %+v", rec)
	}
	if len(rec.Attempts) != 1 || rec.Attempts[0].ErrorType != "empty_turn" {
		t.Fatalf("attempts = %+v, want one absorbed empty_turn", rec.Attempts)
	}
	if rec.Retries != 1 {
		t.Fatalf("retries = %d, want 1", rec.Retries)
	}
	if rec.Usage.OutputTokens != 3 {
		t.Fatalf("output tokens = %d, want the re-ask's 3", rec.Usage.OutputTokens)
	}
	// The upstream-response metadata follows the ADOPTED response, like the
	// generic quality loops: the record must describe the re-ask's response,
	// and the absorbed void attempt keeps the void run's facts - a distinct
	// request id per run makes any mix-up visible (without the re-ask's
	// re-capture at adoption, the record kept the void run's request id).
	if rec.ProviderRequestID != "req-reask" {
		t.Fatalf("record must carry the re-ask response's request id, got %q", rec.ProviderRequestID)
	}
	if len(rec.Attempts) != 1 || rec.Attempts[0].ProviderRequestID != "req-void" {
		t.Fatalf("the absorbed void attempt must keep the void run's request id, got %+v", rec.Attempts)
	}
}

// TestCursorVoidReaskStillVoidSurfaces: when the re-ask ALSO returns an empty
// turn, the void is surfaced exactly like today - in-band empty_turn error.
// (The upstream answers empty on every call.)
func TestCursorVoidReaskStillVoidSurfaces(t *testing.T) {
	var calls atomic.Int32
	upstream := cursorVoidUpstream(t, &calls)

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	resp, err := http.DefaultClient.Do(cursorVoidRequest(srv, upstream))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	got := string(b)

	if !strings.Contains(got, `"code":"empty_turn"`) {
		t.Fatalf("double void must surface empty_turn: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream Run calls = %d, want 2 (attempt + one re-ask)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.ErrorCode != "empty_turn" {
		t.Fatalf("record must flag the void: type=%q code=%q", rec.ErrorType, rec.ErrorCode)
	}
	if len(rec.Attempts) != 1 {
		t.Fatalf("attempts = %+v, want the one absorbed void", rec.Attempts)
	}
}

// TestCursorReaskDisabledByConfig: quality_retries 0 turns the void back into
// today's behavior - in-band empty_turn on the FIRST void, one upstream call.
func TestCursorReaskDisabledByConfig(t *testing.T) {
	var calls atomic.Int32
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body := r.Body
		w.Header().Set("Content-Type", "application/connect+proto")
		fl := w.(http.Flusher)
		w.WriteHeader(200)
		readFrame(t, body)
		cursorConnectHandshake(t, w, body, fl)
		w.Write(cframe(cmsg(1, cmsg(14, nil))))
		w.Write(cend("{}"))
		fl.Flush()
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(newConfigWithQuality(0), buf))
	defer srv.Close()

	resp, err := http.DefaultClient.Do(cursorVoidRequest(srv, upstream))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	got := string(b)

	if !strings.Contains(got, `"code":"empty_turn"`) {
		t.Fatalf("void must surface empty_turn when re-ask is disabled: %s", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream Run calls = %d, want 1 (re-ask disabled)", n)
	}
}

// TestCursorVoidReaskResetsTurnMetrics pins resetTurnMetrics's effect through
// reaskAfterVoid: the voided first turn streams a text delta (so the record
// carries a first-turn FirstTokenAt/LastTokenAt) but NO token delta (Output
// stays 0, keeping the void classification), then the re-ask runs. The
// finalized record must report only the second turn: TTFT measured from the
// re-ask's first delta (the first delta is separated from it by a fixed
// 200ms gap in the fixture, so a stale kept timestamp cannot pass) and the
// re-ask's token count.
func TestCursorVoidReaskResetsTurnMetrics(t *testing.T) {
	const voidGap = 200 * time.Millisecond
	var firstDeltaAt time.Time
	var calls atomic.Int32
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := r.Body
		w.Header().Set("Content-Type", "application/connect+proto")
		fl := w.(http.Flusher)
		w.WriteHeader(200)

		// 1. Read the run_request envelope.
		readFrame(t, body)
		// 2. KV pull → answer; request-context handshake → answer.
		cursorConnectHandshake(t, w, body, fl)

		if calls.Add(1) == 1 {
			// The void: one tokenless text delta (the timing footprint the
			// reset must erase), a fixed gap, then turn end with zero tokens.
			firstDeltaAt = time.Now()
			w.Write(cframe(cmsg(1, cmsg(1, cstr(1, "tokenless fragment")))))
			fl.Flush()
			time.Sleep(voidGap)
			w.Write(cframe(cmsg(1, cmsg(14, nil))))
			w.Write(cend("{}"))
			fl.Flush()
			return
		}
		// The re-ask answers with its own delta and token count.
		w.Write(cframe(cmsg(1, cmsg(1, cstr(1, "finally an answer")))))
		w.Write(cframe(cmsg(1, cmsg(8, cvint(1, 3)))))
		w.Write(cframe(cmsg(1, cmsg(14, nil))))
		w.Write(cend("{}"))
		fl.Flush()
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	resp, err := http.DefaultClient.Do(cursorVoidRequest(srv, upstream))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"content":"finally an answer"`) {
		t.Fatalf("re-ask answer missing: %s", b)
	}

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.Usage.OutputTokens != 3 {
		t.Fatalf("output tokens = %d, want the re-ask's 3 (per-turn usage window)", rec.Usage.OutputTokens)
	}
	// TTFT must come from the re-ask's first delta: the voided turn's delta
	// landed a full voidGap earlier, so a FirstTokenAt kept from it sits well
	// before firstDeltaAt + half the gap.
	if rec.FirstTokenAt.Before(firstDeltaAt.Add(voidGap / 2)) {
		t.Fatalf("FirstTokenAt = %v, want it re-stamped by the re-ask (after %v); the voided turn's delta landed at %v",
			rec.FirstTokenAt, firstDeltaAt.Add(voidGap/2), firstDeltaAt)
	}
	if rec.LastTokenAt.Before(rec.FirstTokenAt) {
		t.Fatalf("LastTokenAt %v before FirstTokenAt %v", rec.LastTokenAt, rec.FirstTokenAt)
	}
}

// TestCursorVoidReaskFailureKeepsVoidTimeline pins the failed-re-ask contract:
// when the void turn emitted a tokenless text delta and the re-ask then fails,
// the surfaced response is still the VOID's, so the record must keep
// describing the voided attempt. The re-ask's admission stamps FinalAttemptAt
// (Retries > 0 after the absorb) and its >= 400 response adopts transport
// timing inside openCursorHTTP; left in place, TTFT computes
// FirstTokenAt - FinalAttemptAt < 0 and the timing describes an attempt whose
// response the client never received. Both must roll back to the void's.
func TestCursorVoidReaskFailureKeepsVoidTimeline(t *testing.T) {
	// voidGap separates the void's tokenless delta from the turn end, so a
	// re-ask-anchored TTFT is negative by a wide, unambiguous margin.
	// reaskDelay delays the failed re-ask's 500 headers, so its adopted
	// ttfb sits structurally far above the void run's own fast send.
	const voidGap = 200 * time.Millisecond
	const reaskDelay = 300 * time.Millisecond
	var voidDeltaAt time.Time
	var calls atomic.Int32
	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := r.Body
		if calls.Add(1) == 1 {
			// The void: one tokenless text delta (the FirstTokenAt
			// footprint), a fixed gap, then turn end with zero tokens.
			w.Header().Set("Content-Type", "application/connect+proto")
			w.Header().Set("X-Request-Id", "req-void")
			fl := w.(http.Flusher)
			w.WriteHeader(200)
			readFrame(t, body)
			cursorConnectHandshake(t, w, body, fl)
			voidDeltaAt = time.Now()
			w.Write(cframe(cmsg(1, cmsg(1, cstr(1, "tokenless fragment")))))
			fl.Flush()
			time.Sleep(voidGap)
			w.Write(cframe(cmsg(1, cmsg(14, nil))))
			w.Write(cend("{}"))
			fl.Flush()
			return
		}
		// The re-ask fails: a 500 whose headers sit reaskDelay out, so the
		// timing it would adopt is distinguishable from the void run's.
		time.Sleep(reaskDelay)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		w.Write([]byte(`{"error":{"message":"upstream exploded"}}`))
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	// StormMaxRetries 0 keeps the re-ask's 500 a single deterministic send:
	// no storm-budget retry ladder runs before the failure surfaces.
	cfg := config.Default()
	cfg.StormMaxRetries = 0
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	resp, err := http.DefaultClient.Do(cursorVoidRequest(srv, upstream))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	got := string(b)
	if !strings.Contains(got, `"code":"empty_turn"`) {
		t.Fatalf("a failed re-ask must surface the void in-band: %s", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream Run calls = %d, want 2 (void + one failed re-ask)", n)
	}

	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.ErrorCode != cursorEmptyTurn {
		t.Fatalf("record must flag the surfaced void: type=%q code=%q", rec.ErrorType, rec.ErrorCode)
	}
	if rec.Retries != 1 || len(rec.Attempts) != 1 || rec.Attempts[0].ErrorType != cursorEmptyTurn {
		t.Fatalf("the absorbed void attempt must stay logged: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
	}
	if rec.FirstTokenAt.IsZero() {
		t.Fatal("the void's tokenless text delta must have stamped FirstTokenAt, or the TTFT pin below is vacuous")
	}
	if rec.TTFTMs < 0 {
		t.Fatalf("TTFTMs = %d, want >= 0 (the surfaced void's own token timeline; a FinalAttemptAt kept from the failed re-ask's admission lands after the void's token and turns TTFT negative)", rec.TTFTMs)
	}
	// FinalAttemptAt must describe the surfaced void's own send, which began
	// before the void's delta landed. The failed re-ask's admission stamp
	// sits a full voidGap after the delta and fails this.
	if !rec.FinalAttemptAt.Before(voidDeltaAt) {
		t.Fatalf("FinalAttemptAt = %v, want the surfaced void's own attempt (before the void's delta at %v), never the failed re-ask's admission", rec.FinalAttemptAt, voidDeltaAt)
	}
	// The failed re-ask's 500 response adopted its transport timing inside
	// openCursorHTTP before the opener discarded it; the surfaced record must
	// keep the void run's fast send instead (the ~%dms header wait is the
	// failed re-ask's, never the client's response).
	if rec.UpstreamTTFBMs >= reaskDelay.Milliseconds()/2 {
		t.Fatalf("UpstreamTTFBMs = %d, want the void run's own fast send (< %d), not the failed re-ask's ~%dms header wait",
			rec.UpstreamTTFBMs, reaskDelay.Milliseconds()/2, reaskDelay.Milliseconds())
	}
}
