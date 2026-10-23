package proxy

// N15 regressions: a terminal event name and its document are interpreted as
// one buffered unit. Multi-line/pretty-printed data is reassembled before the
// hold decision, a type-less document takes its terminal name, and a name that
// contradicts the document is dropped instead of relabelling it.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const responsesCreatedFrame = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n"

const responsesTextDocument = `{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}`

func TestResponsesTerminalBufferedAsEvent(t *testing.T) {
	cases := []struct {
		name       string
		tail       string
		wantName   string
		wantDoc    string
		wantNoName string
	}{
		{
			name: "multi-line terminal json",
			tail: "event: response.completed\n" +
				"data: {\"type\":\n" +
				"data: \"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}]}}\n\n",
			wantName: "response.completed",
			wantDoc:  "response.completed",
		},
		{
			name: "pretty printed terminal",
			tail: "event: response.completed\n" +
				"data: {\n" +
				"data:   \"type\": \"response.completed\",\n" +
				"data:   \"response\": {\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}]}\n" +
				"data: }\n\n",
			wantName: "response.completed",
			wantDoc:  "response.completed",
		},
		{
			name:     "name-only type-less document",
			tail:     "event: response.completed\ndata: {\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}]}}\n\n",
			wantName: "response.completed",
		},
		{
			name:       "mismatched terminal name over completed data",
			tail:       "event: response.incomplete\ndata: " + responsesTextDocument + "\n\n",
			wantDoc:    "response.completed",
			wantNoName: "response.incomplete",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := streamUpstream(t, responsesCreatedFrame, tc.tail)
			defer upstream.Close()
			buf := metrics.NewBuffer(8)
			srv := httptest.NewServer(New(config.Default(), buf))
			defer srv.Close()

			status, got := streamRequestTo(t, srv, upstream, "/v1/responses",
				`{"model":"m","stream":true,"input":"hi"}`)
			if status != http.StatusOK {
				t.Fatalf("status = %d (body %s)", status, got)
			}
			if !strings.Contains(got, "hello") {
				t.Fatalf("terminal text was not relayed: %s", got)
			}
			if strings.Contains(got, "response.failed") || strings.Contains(got, `"code":"truncated"`) {
				t.Fatalf("terminal document was followed by a spurious failure: %s", got)
			}
			if tc.wantName != "" {
				if names := strings.Count(got, "event: "+tc.wantName+"\n"); names != 1 {
					t.Fatalf("terminal event name %q appeared %d times, want once: %s", tc.wantName, names, got)
				}
			}
			if tc.wantDoc != "" {
				found := false
				for _, line := range strings.Split(got, "\n") {
					if strings.HasPrefix(line, "data:") && strings.Contains(line, `"type"`) && strings.Contains(line, tc.wantDoc) {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("terminal data document %q was dropped: %s", tc.wantDoc, got)
				}
			}
			if tc.wantNoName != "" && strings.Contains(got, "event: "+tc.wantNoName+"\n") {
				t.Fatalf("mismatched name %q was relayed: %s", tc.wantNoName, got)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.StatusCode != http.StatusOK || rec.FinishReason != "stop" ||
				rec.ErrorType != "" || rec.ErrorCode != "" {
				t.Fatalf("record = %+v, want a clean stop", rec)
			}
		})
	}
}

// TestResponsesMismatchedTerminalNameOverDelta pins the non-terminal data
// case: a terminal name attached to ordinary delta data is dropped, the delta
// is relayed, and the later real terminal event still resolves once.
func TestResponsesMismatchedTerminalNameOverDelta(t *testing.T) {
	upstream := streamUpstream(t,
		responsesCreatedFrame,
		"event: response.completed\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n",
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"id\":\"msg_1\",\"role\":\"assistant\",\"content\":[]}}\n\n",
		"event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[]}}\n\n",
	)
	defer upstream.Close()
	buf := metrics.NewBuffer(8)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	status, got := streamRequestTo(t, srv, upstream, "/v1/responses",
		`{"model":"m","stream":true,"input":"hi"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d (body %s)", status, got)
	}
	if !strings.Contains(got, `"delta":"hello"`) {
		t.Fatalf("delta data under a terminal name was dropped: %s", got)
	}
	if strings.Contains(got, "event: response.completed\ndata: {\"type\":\"response.output_text.delta\"") {
		t.Fatalf("terminal name was kept over delta data: %s", got)
	}
	if strings.Contains(got, "response.failed") || strings.Contains(got, `"code":"truncated"`) {
		t.Fatalf("spurious failure after a mismatched name: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 || recs[0].FinishReason != "stop" || recs[0].ErrorType != "" {
		t.Fatalf("record = %+v, want a clean stop", recs)
	}
}

// TestResponsesMismatchedTerminalNameOverFailedData pins the reverse
// mismatch: a completed name over a failed document must not relabel the
// failure as success. The data type wins and the failure is relayed once.
func TestResponsesMismatchedTerminalNameOverFailedData(t *testing.T) {
	upstream := streamUpstream(t,
		responsesCreatedFrame,
		"event: response.completed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"boom\"}}}\n\n",
	)
	defer upstream.Close()
	buf := metrics.NewBuffer(8)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	status, got := streamRequestTo(t, srv, upstream, "/v1/responses",
		`{"model":"m","stream":true,"input":"hi"}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d (body %s)", status, got)
	}
	if strings.Contains(got, "event: response.completed") {
		t.Fatalf("completed name survived over a failed document: %s", got)
	}
	if strings.Count(got, `"type":"response.failed"`) != 1 {
		t.Fatalf("failed document was not relayed exactly once: %s", got)
	}
	if strings.Contains(got, `"code":"truncated"`) {
		t.Fatalf("provider failure was replaced by a synthetic truncation: %s", got)
	}
	recs := waitForRecord(t, buf, 1)
	if len(recs) != 1 || recs[0].ErrorCode != "server_error" || recs[0].ErrorCode == "truncated" {
		t.Fatalf("record = %+v, want the provider failure", recs)
	}
}

// responsesOverBoundTerminal builds one pretty-printed terminal event whose
// size exceeds the relay's terminal hold bound. The event carries usage and
// ends with an answer text after the overflow point, so the test proves the
// buffered prefix and the later data lines stay one reassembled document.
func responsesOverBoundTerminal(typed bool) string {
	filler := strings.Repeat("x", 800)
	var b strings.Builder
	b.WriteString("event: response.completed\n")
	b.WriteString("data: {\n")
	if typed {
		b.WriteString("data:   \"type\": \"response.completed\",\n")
	}
	b.WriteString("data:   \"response\": {\"id\": \"resp_1\", \"model\": \"m\", \"status\": \"completed\", \"output\": [\n")
	for i := 0; i < 90; i++ {
		fmt.Fprintf(&b, "data:     {\"type\": \"message\", \"id\": \"msg_%d\", \"role\": \"assistant\", \"content\": [{\"type\": \"output_text\", \"text\": \"%s\"}]},\n", i, filler)
	}
	b.WriteString("data:     {\"type\": \"message\", \"id\": \"msg_last\", \"role\": \"assistant\", \"content\": [{\"type\": \"output_text\", \"text\": \"hello\"}]}\n")
	b.WriteString("data:   ]},\n")
	b.WriteString("data:   \"usage\": {\n")
	b.WriteString("data:     \"input_tokens\": 3,\n")
	b.WriteString("data:     \"output_tokens\": 5,\n")
	b.WriteString("data:     \"total_tokens\": 8\n")
	b.WriteString("data:   }\n")
	b.WriteString("data: }\n\n")
	return responsesCreatedFrame + b.String()
}

// TestResponsesOverBoundTerminalResolves pins R4-4: above terminalHoldMax a
// multi-line terminal event is released verbatim without an artificial blank
// boundary, and its reassembled document still supplies the finish reason,
// answer presence, TTFT and usage. Both a typed document and a document that
// omits its type (classified by the buffered event name) are covered.
func TestResponsesOverBoundTerminalResolves(t *testing.T) {
	for _, typed := range []bool{true, false} {
		name := "type-less"
		if typed {
			name = "typed"
		}
		t.Run(name, func(t *testing.T) {
			stream := responsesOverBoundTerminal(typed)
			if len(stream) <= 64*1024 {
				t.Fatalf("fixture is %d bytes, want above the 64 KiB hold bound", len(stream))
			}
			upstream := streamUpstream(t, stream)
			defer upstream.Close()
			buf := metrics.NewBuffer(8)
			srv := httptest.NewServer(New(config.Default(), buf))
			defer srv.Close()

			status, got := streamRequestTo(t, srv, upstream, "/v1/responses",
				`{"model":"m","stream":true,"input":"hi"}`)
			if status != http.StatusOK {
				t.Fatalf("status = %d (body %s)", status, got)
			}
			if got != stream {
				t.Fatalf("over-bound wire changed:\nIN =%q\nOUT=%q", stream, got)
			}
			if strings.Contains(got, "response.failed") || strings.Contains(got, `"code":"truncated"`) {
				t.Fatalf("over-bound terminal was followed by a synthetic failure: %s", got)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.FinishReason != "stop" || !rec.HadAnswerContent || rec.TTFTMs <= 0 ||
				rec.ErrorType != "" || rec.ErrorCode != "" {
				t.Fatalf("record = %+v, want a clean stop with an answer and TTFT", rec)
			}
			if rec.Usage.InputTokens != 3 || rec.Usage.OutputTokens != 5 ||
				rec.AnswerTokens != 5 || rec.GenTokens != 5 {
				t.Fatalf("usage = %+v answer/gen = %d/%d, want 3 input and 5/5 from the terminal document",
					rec.Usage, rec.AnswerTokens, rec.GenTokens)
			}
		})
	}
}

// TestResponsesLateTerminalEventNameKeepsOrder pins R4-5: SSE field order is
// data-then-name, so the name arrives after the terminal document. Both lines
// stay in their original positions and the record still resolves (the typed
// document by its own type, the type-less one by the late name).
func TestResponsesLateTerminalEventNameKeepsOrder(t *testing.T) {
	const typeLess = `{"response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`
	cases := []struct {
		name     string
		doc      string
		wantType int
	}{
		{name: "typed document", doc: responsesTextDocument, wantType: 1},
		{name: "type-less document", doc: typeLess},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stream := responsesCreatedFrame + "data: " + tc.doc + "\nevent: response.completed\n\n"
			upstream := streamUpstream(t, stream)
			defer upstream.Close()
			buf := metrics.NewBuffer(8)
			srv := httptest.NewServer(New(config.Default(), buf))
			defer srv.Close()

			status, got := streamRequestTo(t, srv, upstream, "/v1/responses",
				`{"model":"m","stream":true,"input":"hi"}`)
			if status != http.StatusOK {
				t.Fatalf("status = %d (body %s)", status, got)
			}
			if got != stream {
				t.Fatalf("late terminal name was not relayed in order:\nIN =%q\nOUT=%q", stream, got)
			}
			if strings.Count(got, "event: response.completed\n") != 1 {
				t.Fatalf("terminal event name appeared %d times, want once: %s",
					strings.Count(got, "event: response.completed\n"), got)
			}
			if gotType := strings.Count(got, `"type":"response.completed"`); gotType != tc.wantType {
				t.Fatalf("terminal document type count = %d, want %d: %s", gotType, tc.wantType, got)
			}
			if strings.Contains(got, "response.failed") || strings.Contains(got, `"code":"truncated"`) {
				t.Fatalf("late terminal name produced a synthetic failure: %s", got)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if rec.FinishReason != "stop" || !rec.HadAnswerContent || rec.TTFTMs <= 0 ||
				rec.ErrorType != "" || rec.ErrorCode != "" {
				t.Fatalf("record = %+v, want a clean stop with an answer and TTFT", rec)
			}
			if tc.wantType == 0 && rec.Usage.OutputTokens != 5 {
				t.Fatalf("usage output = %d, want 5 from the reassembled document", rec.Usage.OutputTokens)
			}
		})
	}
}
