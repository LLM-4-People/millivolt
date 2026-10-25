package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestCompletionsPromptStreamTextHealthy: a legacy /v1/completions prompt
// stream carries its answer in choices[].text. Its finish chunk and exactly
// one [DONE] must relay verbatim - no synthetic empty_completion error, no
// error record - because text is answer content just like chat delta.content.
func TestCompletionsPromptStreamTextHealthy(t *testing.T) {
	chunks := []string{
		`data: {"id":"cmpl-1","object":"text_completion","choices":[{"text":"Hello","index":0,"finish_reason":null}]}` + "\n\n",
		`data: {"id":"cmpl-1","object":"text_completion","choices":[{"text":" world","index":0,"finish_reason":null}]}` + "\n\n",
		`data: {"id":"cmpl-1","object":"text_completion","choices":[{"text":"","index":0,"finish_reason":"stop"}]}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}
	want := strings.Join(chunks, "")
	upstream := streamUpstream(t, chunks...)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	status, got := streamRequestTo(t, srv, upstream, "/v1/completions",
		`{"model":"m","stream":true,"prompt":"hi"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if got != want {
		t.Fatalf("prompt stream must relay verbatim:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, `"error"`) {
		t.Fatalf("no synthetic error may replace the terminal region: %s", got)
	}
	if n := strings.Count(got, "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] count = %d, want exactly 1: %s", n, got)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() || rec.ErrorType != "" || rec.ErrorCode != "" {
		t.Fatalf("healthy prompt stream flagged as error: %+v", rec)
	}
	if !rec.HadAnswerContent || rec.FinishReason != "stop" {
		t.Fatalf("record must carry the prompt completion answer: content=%v finish=%q",
			rec.HadAnswerContent, rec.FinishReason)
	}
}

// TestStreamRefusalOnlyStopHealthy: a refusal-only chat stream that stops
// cleanly delivered visible answer-shaped output; the terminal region must be
// released verbatim instead of being rewritten as empty_completion.
func TestStreamRefusalOnlyStopHealthy(t *testing.T) {
	chunks := []string{
		`data: {"id":"A","choices":[{"delta":{"refusal":"I cannot help with that"}}]}` + "\n\n",
		thinkingStopFrames,
	}
	want := strings.Join(chunks, "")
	upstream := streamUpstream(t, chunks...)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if got != want {
		t.Fatalf("refusal-only stop must relay verbatim:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, `"error"`) {
		t.Fatalf("a refusal-only answer must not be rewritten as an error: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	if rec := recs[0]; rec.IsError() || rec.ErrorType != "" || rec.FinishReason != "stop" {
		t.Fatalf("refusal-only stop must record a clean answer: %+v", rec)
	}
}

// TestStreamFunctionCallOnlyStopHealthy: the deprecated delta.function_call
// declaration is answer-shaped visible output too - a clean stop after it is
// not a degenerate empty completion.
func TestStreamFunctionCallOnlyStopHealthy(t *testing.T) {
	chunks := []string{
		`data: {"id":"A","choices":[{"delta":{"function_call":{"name":"lookup","arguments":"{}"}}}]}` + "\n\n",
		thinkingStopFrames,
	}
	want := strings.Join(chunks, "")
	upstream := streamUpstream(t, chunks...)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if got != want {
		t.Fatalf("function_call-only stop must relay verbatim:\n got %q\nwant %q", got, want)
	}
	if strings.Contains(got, `"error"`) {
		t.Fatalf("a function_call declaration must not be rewritten as an error: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	if rec := recs[0]; rec.IsError() || rec.ErrorType != "" || rec.FinishReason != "stop" {
		t.Fatalf("function_call-only stop must record a clean answer: %+v", rec)
	}
}

// TestStreamEmptyRefusalStopStillVoid is the control for the answer-shaped
// rule: an EMPTY refusal string is not output, so a clean stop after it keeps
// the degenerate empty_completion class.
func TestStreamEmptyRefusalStopStillVoid(t *testing.T) {
	chunks := []string{
		`data: {"id":"A","choices":[{"delta":{"refusal":""}}]}` + "\n\n",
		thinkingStopFrames,
	}
	upstream := streamUpstream(t, chunks...)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if !strings.Contains(got, `"code":"`+metrics.CodeEmptyCompletion+`"`) {
		t.Fatalf("an empty refusal must keep the empty_completion class: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	if rec := recs[0]; !rec.IsError() || rec.ErrorCode != metrics.CodeEmptyCompletion {
		t.Fatalf("empty-refusal record must carry the degenerate outcome: %+v", rec)
	}
}

// TestStreamInBandErrorAfterReasoningRescuedOnThinkingBudget: a retryable
// in-band provider error arriving after reasoning-only output is rescued on
// the thinking budget - reasoning frames are auxiliary display text the
// fresh attempt appends behind, and the error frame itself never reached the
// wire (the drop happens before its write, so the client sees only the
// reasoning followed by the fresh attempt's answer).
func TestStreamInBandErrorAfterReasoningRescuedOnThinkingBudget(t *testing.T) {
	var calls atomic.Int32
	errChunk := `data: {"error":{"message":"overloaded","type":"overloaded_error","code":"overloaded"}}` + "\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		if calls.Add(1) == 1 {
			w.Write([]byte(thinkingFrame + errChunk))
			w.(http.Flusher).Flush()
			return
		}
		w.Write([]byte(freshAnswerFrames))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	// The already-relayed reasoning is preserved on the wire (auxiliary
	// display text the fresh attempt appends behind), the error frame never
	// reached the client, and the rescue's pending-event close (the blank
	// line ending the dropped event) separates the fresh attempt's frames.
	if want := thinkingFrame + "\n" + freshAnswerFrames; got != want {
		t.Fatalf("reasoning then fresh answer must relay, error frame dropped: got %q want %q", got, want)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls = %d, want 2", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() || rec.Retries != 1 || len(rec.Attempts) != 1 {
		t.Fatalf("recovered record: retries=%d attempts=%d isError=%v", rec.Retries, len(rec.Attempts), rec.IsError())
	}
	at := rec.Attempts[0]
	if at.ErrorType != "overloaded_error" || at.ErrorCode != "overloaded" {
		t.Fatalf("absorbed attempt must carry the provider's envelope: %+v", at)
	}
}

// TestStreamInBandErrorAfterReasoningZeroBudgetVerbatim: with the thinking
// budget spent (0) the same stream relays the provider's error verbatim after
// the reasoning - never substituted on top of, and the record carries it.
func TestStreamInBandErrorAfterReasoningZeroBudgetVerbatim(t *testing.T) {
	errChunk := `data: {"error":{"message":"overloaded","type":"overloaded_error","code":"overloaded"}}` + "\n\n"
	upstream := streamUpstream(t, thinkingFrame+errChunk)
	defer upstream.Close()

	cfg := config.Default()
	cfg.ThinkingRetries = 0
	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if want := thinkingFrame + errChunk; got != want {
		t.Fatalf("zero budget must relay the provider error verbatim: got %q want %q", got, want)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if !rec.IsError() || rec.ErrorType != "overloaded_error" || rec.ErrorCode != "overloaded" || rec.Retries != 0 {
		t.Fatalf("record must carry the provider's own error, unrescued: %+v", rec)
	}
}

// TestStreamTerminalHoldOverflowReasoningOnlyCommitsVerbatim: past the hold
// bound the stream commits to verbatim relay - a reasoning-only stream with
// an oversized terminal region is never rescued and never substituted; the
// finish chunk itself reaches the client.
func TestStreamTerminalHoldOverflowReasoningOnlyCommitsVerbatim(t *testing.T) {
	chunks := []string{
		thinkingFrame,
		`data: {"id":"A","choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		`data: ` + `{"pad":"` + strings.Repeat("x", 70*1024) + `"}` + "\n\n",
		`data: [DONE]` + "\n\n",
	}
	want := strings.Join(chunks, "")
	upstream := streamUpstream(t, chunks...)
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	_, got := streamRequest(t, srv, upstream)
	if got != want {
		t.Fatalf("overflowed reasoning-only stream must relay byte-for-byte")
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.IsError() || rec.Retries != 0 || len(rec.Attempts) != 0 {
		t.Fatalf("commit-to-verbatim stream must stay a clean record: %+v", rec)
	}
}

// TestStreamMixedBudgetRescues: the two rescue budgets are independent pools -
// a request may consume the thinking budget first and still be rescued by
// the quality budget afterwards, in either order, and the fresh attempt
// finally completes the stream.
func TestStreamMixedBudgetRescues(t *testing.T) {
	for _, order := range []string{"thinking-first", "quality-first"} {
		t.Run(order, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				n := calls.Add(1)
				// Attempt 3 always completes; attempts 1 and 2 are the two
				// truncation flavors in the order under test.
				if n == 3 {
					w.Write([]byte(freshAnswerFrames))
					w.(http.Flusher).Flush()
					return
				}
				thinkingDeath := n == 1 && order == "thinking-first" || n == 2 && order == "quality-first"
				if thinkingDeath {
					w.Write([]byte(thinkingFrame))
					w.(http.Flusher).Flush()
					return
				}
				w.Write([]byte(`data: {"id":"A","choices":[{"delta":{"role":"assistant"}}]}` + "\n\n"))
				w.(http.Flusher).Flush()
			}))
			defer upstream.Close()

			buf := metrics.NewBuffer(100)
			srv := httptest.NewServer(New(config.Default(), buf))
			defer srv.Close()

			_, got := streamRequest(t, srv, upstream)
			if !strings.Contains(got, `"content":"hello"`) {
				t.Fatalf("client must receive the fresh stream's answer: %s", got)
			}
			if strings.Contains(got, `"code":"`) {
				t.Fatalf("a fully rescued request must surface no error: %s", got)
			}
			if n := calls.Load(); n != 3 {
				t.Fatalf("upstream calls = %d, want 3 (two absorbed rescues + final)", n)
			}
			recs := waitForRecord(t, buf, 1)
			rec := recs[0]
			if rec.IsError() || rec.Retries != 2 || len(rec.Attempts) != 2 {
				t.Fatalf("both absorbed rescues must book: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
			}
			wantCodes := []string{metrics.CodeTruncated, metrics.CodeTruncated}
			for i, a := range rec.Attempts {
				if a.ErrorCode != wantCodes[i] || a.ErrorType != "upstream_error" {
					t.Fatalf("attempt %d not booked as provider error: %+v", i, a)
				}
			}
		})
	}
}

// TestNonStreamMixedBudgetRescues: the non-streaming twin - the two budgets
// are independent pools in either order, and the client sees only the final
// healthy body.
func TestNonStreamMixedBudgetRescues(t *testing.T) {
	for _, order := range []string{"thinking-first", "quality-first"} {
		t.Run(order, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				n := calls.Add(1)
				if n == 3 {
					w.Write([]byte(realBody))
					return
				}
				if (n == 1) == (order == "thinking-first") {
					w.Write([]byte(thinkingOnlyBody))
					return
				}
				w.Write([]byte(voidBody))
			}))
			defer upstream.Close()

			buf := metrics.NewBuffer(100)
			srv := httptest.NewServer(New(config.Default(), buf))
			defer srv.Close()

			req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
			req.Header.Set("Authorization", "Bearer sk-k")
			req.Header.Set("X-Proxy-Base-URL", upstream.URL)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			if resp.StatusCode != 200 || string(body) != realBody {
				t.Fatalf("client must see only the healthy body: status=%d body=%q", resp.StatusCode, body)
			}
			recs := waitForRecord(t, buf, 1)
			rec := recs[0]
			if rec.IsError() || rec.Retries != 2 || len(rec.Attempts) != 2 {
				t.Fatalf("both absorbed attempts must book: retries=%d attempts=%+v", rec.Retries, rec.Attempts)
			}
			first, second := rec.Attempts[0], rec.Attempts[1]
			if order == "quality-first" {
				first, second = second, first
			}
			if first.ErrorCode != metrics.CodeReasoningOnly || first.ErrorType != "upstream_error" ||
				second.ErrorType != metrics.CodeEmptyCompletion {
				t.Fatalf("attempts not booked per class in %s order: %+v", order, rec.Attempts)
			}
		})
	}
}

// TestNonStreamReasoningOnlyThinkingZeroSurfaces502: with the thinking budget
// disabled but quality retries enabled, a reasoning-only body must NOT be
// re-run on the quality budget - the class owns the thinking budget alone.
func TestNonStreamReasoningOnlyThinkingZeroSurfaces502(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(thinkingOnlyBody))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	cfg := config.Default()
	cfg.ThinkingRetries = 0
	cfg.QualityRetries = 1
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (thinking budget disabled)", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"code":"`+metrics.CodeReasoningOnly+`"`) {
		t.Fatalf("502 body must carry the reasoning_only code: %s", body)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream calls = %d, want 1 (quality budget must not rescue the thinking class)", n)
	}
	recs := waitForRecord(t, buf, 1)
	rec := recs[0]
	if rec.ErrorType != "upstream_error" || rec.ErrorCode != metrics.CodeReasoningOnly || rec.Retries != 0 {
		t.Fatalf("record must classify as the surfaced provider error, unretried: %+v", rec)
	}
}
