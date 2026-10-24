package format

// B3/H7 regressions: unused response echoes are dropped, used index/span
// fields accept integral spellings, and the Chat stream created value survives
// an integral spelling.

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/jsonnum"
)

func TestChatResponseIndexAndSpanSpellings(t *testing.T) {
	body := func(start, end string) string {
		return `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0.0,"message":{"role":"assistant","content":"hi","annotations":[{"type":"url_citation","url_citation":{"url":"https://example.com","title":"t","start_index":` + start + `,"end_index":` + end + `}}]},"finish_reason":"stop"}]}`
	}
	// The message-relative span must stay inside the two-rune content; the
	// exponent spelling still exercises exact integral narrowing.
	resp, err := DecodeChatResponse([]byte(body("0.0", "1e0")))
	if err != nil {
		t.Fatalf("integral spans rejected: %v", err)
	}
	if len(resp.Blocks) == 0 || len(resp.Blocks[0].Citations) != 1 {
		t.Fatalf("blocks = %+v, want one citation", resp.Blocks)
	}
	citation := resp.Blocks[0].Citations[0]
	if citation.StartIndex != 0 || citation.EndIndex != 1 {
		t.Fatalf("citation = %+v, want span 0..1", citation)
	}
	// An out-of-message exponent span is dropped instead of attached.
	resp, err = DecodeChatResponse([]byte(body("0.0", "1e3")))
	if err != nil {
		t.Fatalf("out-of-range integral span failed the response: %v", err)
	}
	if len(resp.Blocks) != 0 && len(resp.Blocks[0].Citations) != 0 {
		t.Fatalf("out-of-range span attached: %+v", resp.Blocks)
	}
	if _, err := DecodeChatResponse([]byte(body("0.5", "1"))); err == nil {
		t.Fatal("fractional Chat start_index accepted")
	}
}

func TestResponsesResponseIndexAndSpanSpellings(t *testing.T) {
	body := func(start, end string) string {
		return `{"id":"r1","object":"response","created_at":1,"status":"completed","model":"m","max_output_tokens":100.0,"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[{"type":"url_citation","url":"https://example.com","title":"t","start_index":` + start + `,"end_index":` + end + `,"index":0.0}]}]}]}`
	}
	// The message-relative span must stay inside the two-rune text part; the
	// exponent spelling still exercises exact integral narrowing.
	resp, err := DecodeResponsesResponse([]byte(body("0.0", "1e0")))
	if err != nil {
		t.Fatalf("integral spans or unused echoes rejected: %v", err)
	}
	if len(resp.Blocks) == 0 || len(resp.Blocks[0].Citations) != 1 {
		t.Fatalf("blocks = %+v, want one citation", resp.Blocks)
	}
	citation := resp.Blocks[0].Citations[0]
	if citation.StartIndex != 0 || citation.EndIndex != 1 {
		t.Fatalf("citation = %+v, want span 0..1", citation)
	}
	if _, err := DecodeResponsesResponse([]byte(body("0.5", "1"))); err == nil {
		t.Fatal("fractional Responses start_index accepted")
	}
}

func TestChatStreamIndexSpellings(t *testing.T) {
	source := `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0.0,"delta":{"role":"assistant","tool_calls":[{"index":0.0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":1}"}}]}}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0.0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	var buf bytes.Buffer
	if err := TranslateStreamBetween(ir.Chat, ir.Chat, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
		t.Fatalf("integral Chat stream indexes rejected: %v", err)
	}
	if calls := streamedToolCalls(t, ir.Chat, buf.String()); len(calls) != 1 || calls[0].name != "lookup" {
		t.Fatalf("calls = %+v, want one lookup call: %s", calls, buf.String())
	}

	fractional := strings.Replace(source, `"tool_calls":[{"index":0.0`, `"tool_calls":[{"index":0.5`, 1)
	if err := TranslateStreamBetween(ir.Chat, ir.Chat, &buf, strings.NewReader(fractional), nil, nil, StreamOptions{}); err == nil {
		t.Fatal("fractional Chat tool index accepted")
	}
}

func TestResponsesStreamIndexSpellings(t *testing.T) {
	integral := []byte(`{"type":"response.output_text.delta","sequence_number":1.0,"output_index":0.0,"content_index":0.0,"summary_index":0.0,"delta":"hi"}`)
	var ev responsesStreamEvent
	if err := json.Unmarshal(integral, &ev); err != nil {
		t.Fatalf("integral Responses stream indexes rejected: %v", err)
	}
	if int64(ev.SequenceNumber) != 1 || int(ev.OutputIndex) != 0 || int(ev.ContentIndex) != 0 || int(ev.SummaryIndex) != 0 {
		t.Fatalf("decoded indexes = %+v", ev)
	}
	for _, field := range []string{"sequence_number", "output_index", "content_index", "summary_index"} {
		body := strings.Replace(string(integral), `"`+field+`":1.0`, `"`+field+`":1.5`, 1)
		body = strings.Replace(body, `"`+field+`":0.0`, `"`+field+`":0.5`, 1)
		var probe responsesStreamEvent
		if err := json.Unmarshal([]byte(body), &probe); err == nil {
			t.Fatalf("fractional %s accepted", field)
		}
	}
}

// TestIntFromJSONIndexPlatformGuard pins the shared narrowing rule: an index
// beyond the platform int range fails closed on a 32-bit build instead of
// aliasing onto another map key, while a 64-bit build retains it exactly.
func TestIntFromJSONIndexPlatformGuard(t *testing.T) {
	huge := jsonnum.Int64(1 << 32)
	got, err := intFromJSONIndex("output_index", huge)
	if strconv.IntSize == 32 {
		if err == nil {
			t.Fatalf("32-bit index %d aliased to %d", int64(huge), got)
		}
		if !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("error = %v, want the range rejection", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("64-bit index %d rejected: %v", int64(huge), err)
	}
	if int64(got) != int64(huge) {
		t.Fatalf("index = %d, want %d", got, int64(huge))
	}
	if small, err := intFromJSONIndex("content_index", 7); err != nil || small != 7 {
		t.Fatalf("small index = %d/%v, want 7", small, err)
	}
}
