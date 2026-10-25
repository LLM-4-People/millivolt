package proxy

// Batch 3 regressions: every pre-commit and in-band error is rendered in the
// client's own protocol, a translated body owns its Content-Type, and an error
// message echoed from a body-controlled value stays bounded.

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

func anthropicErrorEnvelope(t *testing.T, body string) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("body %q is not JSON: %v", body, err)
	}
	if envelope["type"] != "error" {
		t.Fatalf("body %q is not an Anthropic error envelope", body)
	}
	if _, ok := envelope["error"].(map[string]any); !ok {
		t.Fatalf("body %q has no Anthropic error object", body)
	}
	return envelope
}

// TestInvalidInFormatKeepsPathEnvelope pins D1: an invalid X-Proxy-In-Format on
// a known Anthropic path is rejected in the path's own envelope, not OpenAI's.
func TestInvalidInFormatKeepsPathEnvelope(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	r := httptest.NewRequest(http.MethodPost, "http://proxy/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set(hdrBaseURL, "http://127.0.0.1:9")
	r.Header.Set(hdrInFormat, "bogus")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	anthropicErrorEnvelope(t, rec.Body.String())
	if strings.Contains(rec.Body.String(), "choices") {
		t.Fatalf("body carries an OpenAI envelope: %s", rec.Body.String())
	}
}

// TestTranslatedNonChatPreCommitErrorUsesClientEnvelope pins D3/E2: a fresh
// transport failure on a translated Anthropic client returns the Anthropic
// error envelope.
func TestTranslatedNonChatPreCommitErrorUsesClientEnvelope(t *testing.T) {
	// A listener that closes every connection immediately gives a deterministic
	// transport failure without a dial timeout, and the retry ladder is off so
	// the terminal envelope is what this fixture observes.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	cfg := config.Default()
	cfg.MaxRetries = 0
	cfg.BaseBackoff = time.Millisecond
	cfg.MaxBackoff = time.Millisecond
	s := New(cfg, metrics.Noop{})
	r := httptest.NewRequest(http.MethodPost, "http://proxy/v1/messages",
		strings.NewReader(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set(hdrBaseURL, "http://"+listener.Addr().String())
	r.Header.Set(hdrFormat, "openai")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	anthropicErrorEnvelope(t, rec.Body.String())
}

// TestCommittedPacerUsesInboundErrorShape pins D3/E1 at the sink: a committed
// Anthropic or Responses stream never receives an OpenAI error chunk or [DONE].
func TestCommittedPacerUsesInboundErrorShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inbound string
	}{
		{"anthropic", "anthropic"},
		{"responses", "openai-responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			pacer := newSSEPacer(recorder, time.Hour, 0)
			pacer.Start()
			defer pacer.Stop()
			rec := &metrics.Record{ID: "req_1", Model: "m"}
			writeClientErrorHdr(pacer, rec, protocolForTest(tc.inbound), "rate_limit_error", "slow down", http.StatusTooManyRequests, nil)
			body := recorder.Body.String()
			if strings.Contains(body, "[DONE]") || strings.Contains(body, `"choices"`) {
				t.Fatalf("%s client received an OpenAI stream: %s", tc.name, body)
			}
			if !strings.Contains(body, "event: error") {
				t.Fatalf("%s client received no protocol-native error: %s", tc.name, body)
			}
			if rec.ResponseBytes == 0 {
				t.Fatalf("generated in-band error bytes were not recorded: %+v", rec)
			}
			if tc.inbound == "anthropic" && !strings.Contains(body, "event: message_stop") {
				t.Fatalf("Anthropic client received no message_stop: %s", body)
			}
			if tc.inbound == "openai-responses" && !strings.Contains(body, "event: response.failed") {
				t.Fatalf("Responses client received no response.failed: %s", body)
			}
		})
	}
}

// TestCommittedNonOKStatusIsInBand pins E4: once the pacer has committed SSE,
// even a 2xx upstream status other than 200 must close in-band rather than
// fall through to the non-streaming JSON writer.
func TestCommittedNonOKStatusIsInBand(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			<-release
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"m\"}}\n\n")
			io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer upstream.Close()

	cfg := config.Default()
	cfg.MaxConcurrent = 1
	cfg.MaxQueueSize = 8
	buf := metrics.NewBuffer(16)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()
	srv.Client().Timeout = 10 * time.Second

	first := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
			strings.NewReader(`{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer sk")
		req.Header.Set(hdrBaseURL, upstream.URL)
		req.Header.Set(hdrFormat, "anthropic")
		resp, err := srv.Client().Do(req)
		if err != nil {
			first <- err.Error()
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		first <- string(body)
	}()

	// Wait until the single concurrency slot is occupied, then start the
	// queued request whose pacer commits SSE during the wait.
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	second := make(chan string, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages",
			strings.NewReader(`{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"again"}]}`))
		req.Header.Set("Authorization", "Bearer sk")
		req.Header.Set(hdrBaseURL, upstream.URL)
		req.Header.Set(hdrFormat, "anthropic")
		resp, err := srv.Client().Do(req)
		if err != nil {
			second <- err.Error()
			return
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		second <- string(body)
	}()
	// Give the queued request time to enter the wait (its pacer commits the
	// SSE 200 while it waits), then release the slot holder.
	time.Sleep(300 * time.Millisecond)
	close(release)
	select {
	case <-first:
	case <-time.After(8 * time.Second):
		t.Fatal("first request never completed")
	}
	var secondBody string
	select {
	case body := <-second:
		secondBody = body
		if strings.Contains(body, `"choices"`) || strings.Contains(body, "[DONE]") {
			t.Fatalf("committed non-200 status leaked a JSON/OpenAI body: %s", body)
		}
		if !strings.Contains(body, "event: error") || !strings.Contains(body, "event: message_stop") {
			t.Fatalf("committed non-200 status did not close in-band: %s", body)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("queued request never completed")
	}
	var queued *metrics.Record
	for _, rec := range waitForRecord(t, buf, 2) {
		if rec.ErrorType != "" {
			queued = rec
		}
	}
	if queued == nil {
		t.Fatalf("no record for the queued in-band error: %s", secondBody)
	}
	if queued.StatusCode != http.StatusOK {
		t.Fatalf("committed queued record status = %d, want the client's 200 rather than the unrelayable 201: %+v",
			queued.StatusCode, queued)
	}
	if queued.ErrorMsg == "" || !queued.IsError() {
		t.Fatalf("committed 201 record must carry the in-band outcome: %+v", queued)
	}
}

// TestTranslated201IsSuccess pins E5: a translated 2xx document is a success,
// not an error envelope.
func TestTranslated201IsSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m",`+
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer upstream.Close()
	srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set(hdrBaseURL, upstream.URL)
	req.Header.Set(hdrFormat, "anthropic")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
	}
	var completion struct {
		Choices []any `json:"choices"`
	}
	if err := json.Unmarshal(body, &completion); err != nil || len(completion.Choices) == 0 {
		t.Fatalf("translated 201 is not a completion: %s", body)
	}
	if strings.Contains(string(body), "upstream HTTP 201") {
		t.Fatalf("translated 201 was reported as an upstream error: %s", body)
	}
}

// TestTranslated204HasNoInventedBody pins the 204 arm of E5.
func TestTranslated204HasNoInventedBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set(hdrBaseURL, upstream.URL)
	req.Header.Set(hdrFormat, "anthropic")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204: %s", resp.StatusCode, body)
	}
	if len(body) != 0 {
		t.Fatalf("204 invented a body: %s", body)
	}
}

// TestFrameIsErrorIgnoresResponsesNullError pins E6: every Responses object
// carries "error":null, which is not a failure; a named error event is.
func TestFrameIsErrorIgnoresResponsesNullError(t *testing.T) {
	created := []byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"error\":null}}\n\n")
	if frameIsError(created) {
		t.Fatal("a Responses object with a null error key was classified as an error frame")
	}
	failed := []byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"server_error\",\"message\":\"boom\"}}}\n\n")
	if !frameIsError(failed) {
		t.Fatal("response.failed was not classified as an error frame")
	}
	chat := []byte("data: {\"error\":{\"type\":\"server_error\",\"message\":\"boom\"}}\n\n")
	if !frameIsError(chat) {
		t.Fatal("a Chat error chunk was not classified as an error frame")
	}
}

// TestTranslatedSuccessOwnsContentType pins B13: a translated non-streaming
// success is JSON even when the upstream labelled it text/plain.
func TestTranslatedSuccessOwnsContentType(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m",`+
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",`+
			`"usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer upstream.Close()
	srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set(hdrBaseURL, upstream.URL)
	req.Header.Set(hdrFormat, "anthropic")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("translated success Content-Type = %q, want application/json", got)
	}
}

// TestTranslatedStreamOwnsEventStreamContentType pins I12: a translated stream
// is SSE even when the upstream labelled the body application/json.
func TestTranslatedStreamOwnsEventStreamContentType(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `data: {"type":"message_start","message":{"id":"m1","model":"m"}}`+"\n\n")
		io.WriteString(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`+"\n\n")
		io.WriteString(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`+"\n\n")
		io.WriteString(w, `data: {"type":"content_block_stop","index":0}`+"\n\n")
		io.WriteString(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}`+"\n\n")
		io.WriteString(w, `data: {"type":"message_stop"}`+"\n\n")
	}))
	defer upstream.Close()
	srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set(hdrBaseURL, upstream.URL)
	req.Header.Set(hdrFormat, "anthropic")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("translated stream Content-Type = %q, want text/event-stream", got)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
}

// TestSSEErrorFramesCountResponseBytes pins I11: proxy-generated in-band error
// frames count toward the recorded bytes received.
func TestSSEErrorFramesCountResponseBytes(t *testing.T) {
	recorder := httptest.NewRecorder()
	rec := &metrics.Record{}
	if err := writeProtocolStreamError(recorder, rec, ir.Chat, "req_1", "m", "server_error", "boom", "failed"); err != nil {
		t.Fatal(err)
	}
	if rec.ResponseBytes != int64(recorder.Body.Len()) {
		t.Fatalf("ResponseBytes = %d, want %d", rec.ResponseBytes, recorder.Body.Len())
	}
}

// TestTranslationRejectionEchoBounded pins D7: a body-controlled warning field
// cannot amplify a translation rejection into a multi-megabyte response.
func TestTranslationRejectionEchoBounded(t *testing.T) {
	cfg := config.Default()
	cfg.TranslationLoss = config.TranslationLossReject
	s := New(cfg, metrics.Noop{})
	huge := strings.Repeat("k", 1<<20)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"metadata":{"` + huge + `":1}}`
	r := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions", strings.NewReader(body))
	r.Header.Set(hdrBaseURL, "http://127.0.0.1:9")
	r.Header.Set(hdrFormat, "anthropic")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() > 2*protocolErrorEchoMax {
		t.Fatalf("rejection body = %d bytes, want <= %d", rec.Body.Len(), 2*protocolErrorEchoMax)
	}
}

// TestDegenerateResponsesFailureWireShape pins the synthetic terminal frame a
// Responses client receives when the upstream ends without a terminal event:
// exactly one response.failed event with a stable failure shape, no Chat
// sentinel, and exact byte accounting.
func TestDegenerateResponsesFailureWireShape(t *testing.T) {
	rec := &metrics.Record{ID: "req_1"}
	var a sse.Analyzer
	w := httptest.NewRecorder()
	emitDegenerateResponses(w, &a, time.Now(), metrics.CodeTruncated, rec)
	body := w.Body.String()
	if strings.Contains(body, "[DONE]") || strings.Contains(body, `"choices"`) {
		t.Fatalf("Responses client received a Chat frame: %q", body)
	}
	payloads := responsesFramePayloads(t, body)
	if len(payloads) != 2 {
		t.Fatalf("frames = %d, want error + failed: %q", len(payloads), body)
	}
	errorEvent := payloads[0]
	if errorEvent["type"] != "error" || errorEvent["code"] != metrics.CodeTruncated {
		t.Fatalf("error event = %v, want the specific truncated code", errorEvent)
	}
	// The synthetic pair owns its own 0,1 sequence because the relay does not
	// track the provider's per-response counter. The stream is already
	// terminating; the order within the pair is what matters.
	if errorEvent["sequence_number"] != float64(0) || payloads[1]["sequence_number"] != float64(1) {
		t.Fatalf("synthetic sequence numbers = %v/%v, want 0/1",
			errorEvent["sequence_number"], payloads[1]["sequence_number"])
	}
	doc := payloads[1]
	if doc["type"] != "response.failed" {
		t.Fatalf("event type = %v", doc["type"])
	}
	response, _ := doc["response"].(map[string]any)
	if response["object"] != "response" || response["status"] != "failed" || response["id"] != "resp_req_1" {
		t.Fatalf("response = %v", response)
	}
	failure, _ := response["error"].(map[string]any)
	// response.error.code is the closed ResponseErrorCode enum; the specific
	// internal code stays on the error event and the record.
	if failure["code"] != "server_error" || failure["message"] != metrics.DegenerateMessage(metrics.CodeTruncated) {
		t.Fatalf("error = %v", failure)
	}
	if output, ok := response["output"].([]any); !ok || len(output) != 0 {
		t.Fatalf("output = %v, want an empty array", response["output"])
	}
	a.Fill(rec)
	if rec.ErrorCode != metrics.CodeTruncated || !rec.IsError() {
		t.Fatalf("record error = %q (error %v), want the specific truncated code", rec.ErrorCode, rec.IsError())
	}
	if rec.ResponseBytes != int64(len(body)) {
		t.Fatalf("recorded bytes = %d, want %d", rec.ResponseBytes, len(body))
	}

	// A terminal document owns the identity when it supplied one.
	var withID sse.Analyzer
	withID.Feed([]byte(`data: {"type":"response.completed","response":{"id":"resp_terminal","status":"completed","output":[]}}`), time.Now())
	withID.Feed([]byte(""), time.Now())
	rec2 := &metrics.Record{ID: "req_2"}
	w2 := httptest.NewRecorder()
	emitDegenerateResponses(w2, &withID, time.Now(), metrics.CodeTruncated, rec2)
	payloads2 := responsesFramePayloads(t, w2.Body.String())
	if len(payloads2) != 2 {
		t.Fatalf("frames = %d, want error + failed: %s", len(payloads2), w2.Body.String())
	}
	failed, _ := payloads2[1]["response"].(map[string]any)
	if got := failed["id"]; got != "resp_terminal" {
		t.Fatalf("synthetic failure id = %v, want the terminal response id", got)
	}
}

func protocolForTest(name string) ir.Protocol {
	switch name {
	case "anthropic":
		return ir.Anthropic
	case "openai-responses":
		return ir.Responses
	default:
		return ir.Chat
	}
}

// TestArmedPacerPreCommitUsesInboundErrorShape pins N2: an armed pacer that
// has not committed SSE is still a plain HTTP response, so its error must be
// the client protocol's own JSON envelope with the proxy-owned content type
// and the caller's extra headers.
func TestArmedPacerPreCommitUsesInboundErrorShape(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inbound string
	}{
		{"anthropic", "anthropic"},
		{"responses", "openai-responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			pacer := newSSEPacer(recorder, time.Hour, 0)
			pacer.Arm()
			defer pacer.Stop()
			rec := &metrics.Record{ID: "req_1", Model: "m"}
			extra := http.Header{"Retry-After": []string{"7"}}
			writeClientErrorHdr(pacer, rec, protocolForTest(tc.inbound), "rate_limit_error", "slow down", http.StatusTooManyRequests, extra)

			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", recorder.Code)
			}
			if got := recorder.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := recorder.Header().Get("Retry-After"); got != "7" {
				t.Fatalf("Retry-After = %q, want 7", got)
			}
			body := recorder.Body.String()
			if strings.Contains(body, "[DONE]") || strings.Contains(body, `"choices"`) {
				t.Fatalf("%s client received an OpenAI stream: %s", tc.name, body)
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatalf("body %q is not JSON: %v", body, err)
			}
			if tc.inbound == "anthropic" {
				if doc["type"] != "error" {
					t.Fatalf("Anthropic envelope = %v", doc)
				}
				failure, _ := doc["error"].(map[string]any)
				if failure["type"] != "rate_limit_error" || failure["message"] != "slow down" {
					t.Fatalf("Anthropic error = %v", failure)
				}
			} else {
				failure, _ := doc["error"].(map[string]any)
				if failure["type"] != "rate_limit_error" || failure["message"] != "slow down" {
					t.Fatalf("Responses error = %v", failure)
				}
			}
			if rec.StatusCode != http.StatusTooManyRequests || rec.ErrorType != "rate_limit_error" {
				t.Fatalf("record = %+v", rec)
			}
			if rec.ResponseBytes != 0 {
				t.Fatalf("recorded bytes = %d, want 0: a status-line envelope is not relayed body", rec.ResponseBytes)
			}
		})
	}
}

// TestQualityExhaustedSinkUsesProtocolEnvelope pins N3: the final degenerate
// completion error is a protocol-aware JSON envelope with the machine code,
// not an OpenAI text/plain body from http.Error.
func TestQualityExhaustedSinkUsesProtocolEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		body string
	}{
		{"chat", "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
		{"responses", "/v1/responses", `{"model":"m","input":"hi"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFixtureUpstream(t, "application/json", http.StatusOK, "")
			cfg := config.Default()
			cfg.QualityRetries = 0
			buf := metrics.NewBuffer(8)
			s := New(cfg, buf)
			s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

			w := proxyRequest(t, s, tc.path, "", tc.body)
			if w.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 (body %s)", w.Code, w.Body.String())
			}
			if got := w.Header().Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
			var doc map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatalf("body %q is not JSON: %v", w.Body.String(), err)
			}
			failure, _ := doc["error"].(map[string]any)
			if failure["type"] != "api_error" || failure["code"] != "empty_completion" || failure["message"] == "" {
				t.Fatalf("error = %v", failure)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 || recs[0].StatusCode != http.StatusBadGateway ||
				recs[0].ErrorCode != "empty_completion" {
				t.Fatalf("record = %+v", recs)
			}
		})
	}
}

// responsesFramePayloads decodes the typed payload of every SSE frame in one
// synthetic Responses body.
func responsesFramePayloads(t *testing.T, body string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, frame := range strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n") {
		var payload string
		for _, line := range strings.Split(frame, "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				payload = data
			}
		}
		if payload == "" {
			t.Fatalf("frame %q has no data line", frame)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(payload), &doc); err != nil {
			t.Fatalf("payload %q: %v", payload, err)
		}
		out = append(out, doc)
	}
	return out
}

// assertResponsesEnvelopeRequired checks the required Response fields the
// official schema lists, including keys whose value is null.
func assertResponsesEnvelopeRequired(t *testing.T, response map[string]any) {
	t.Helper()
	for _, key := range []string{
		"access_programs", "id", "object", "created_at", "error",
		"incomplete_details", "instructions", "model", "tools", "output",
		"parallel_tool_calls", "metadata", "tool_choice", "temperature", "top_p",
	} {
		if _, ok := response[key]; !ok {
			t.Fatalf("response envelope is missing required %q: %v", key, response)
		}
	}
}

// TestSyntheticResponsesErrorFramesSchemaComplete pins TC5-4: every synthetic
// Responses error or failure event carries sequence_number, and the failed
// event carries the complete required response envelope.
func TestSyntheticResponsesErrorFramesSchemaComplete(t *testing.T) {
	t.Run("committed stream error pair", func(t *testing.T) {
		w := httptest.NewRecorder()
		rec := &metrics.Record{}
		if err := writeProtocolStreamError(w, rec, ir.Responses, "req_1", "model-a", "server_error", "", "boom"); err != nil {
			t.Fatal(err)
		}
		payloads := responsesFramePayloads(t, w.Body.String())
		if len(payloads) != 2 {
			t.Fatalf("frames = %d, want error + failed: %s", len(payloads), w.Body.String())
		}
		errorEvent := payloads[0]
		if errorEvent["type"] != "error" {
			t.Fatalf("first frame type = %v", errorEvent["type"])
		}
		for _, key := range []string{"type", "code", "message", "param", "sequence_number"} {
			if _, ok := errorEvent[key]; !ok {
				t.Fatalf("error event is missing required %q: %v", key, errorEvent)
			}
		}
		failed := payloads[1]
		if failed["type"] != "response.failed" {
			t.Fatalf("second frame type = %v", failed["type"])
		}
		if _, ok := failed["sequence_number"]; !ok {
			t.Fatalf("failed event is missing sequence_number: %v", failed)
		}
		response, _ := failed["response"].(map[string]any)
		if response == nil {
			t.Fatalf("failed event has no response object: %v", failed)
		}
		assertResponsesEnvelopeRequired(t, response)
		if response["status"] != "failed" {
			t.Fatalf("failed status = %v", response["status"])
		}
		failure, _ := response["error"].(map[string]any)
		if failure["message"] != "boom" {
			t.Fatalf("failure error = %v", failure)
		}
	})

	t.Run("degenerate substitution", func(t *testing.T) {
		rec := &metrics.Record{ID: "req_2", Model: "model-a"}
		var a sse.Analyzer
		w := httptest.NewRecorder()
		emitDegenerateResponses(w, &a, time.Now(), metrics.CodeTruncated, rec)
		payloads := responsesFramePayloads(t, w.Body.String())
		if len(payloads) != 2 || payloads[0]["type"] != "error" || payloads[1]["type"] != "response.failed" {
			t.Fatalf("payloads = %v", payloads)
		}
		if payloads[0]["code"] != metrics.CodeTruncated {
			t.Fatalf("degenerate error event = %v, want the specific code", payloads[0])
		}
		if _, ok := payloads[1]["sequence_number"]; !ok {
			t.Fatalf("degenerate event is missing sequence_number: %v", payloads[1])
		}
		response, _ := payloads[1]["response"].(map[string]any)
		if response == nil {
			t.Fatalf("degenerate event has no response object: %v", payloads[1])
		}
		assertResponsesEnvelopeRequired(t, response)
		if response["model"] != "model-a" {
			t.Fatalf("degenerate model = %v", response["model"])
		}
	})
}
