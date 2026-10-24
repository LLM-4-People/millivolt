package format

// Round 2 TC3-2 regressions: Responses message and part item ids are
// provider-controlled identity strings. They multiply into retained keys, so
// an over-bound id must fail closed before any key is built.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestTranslatedHugeMessageItemIDRejected(t *testing.T) {
	id := strings.Repeat("m", 513)
	source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"` + id + `","role":"assistant","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":"hello"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hello"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
	var buf bytes.Buffer
	err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "message id") {
		t.Fatalf("huge message id error = %v", err)
	}
}

func TestTranslatedHugeContentPartItemIDRejected(t *testing.T) {
	id := strings.Repeat("p", 513)
	source := terminalStreamCreated + `event: response.content_part.added
data: {"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"` + id + `","part":{"type":"output_text","text":"hello"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"` + id + `","delta":"hello"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}

`
	var buf bytes.Buffer
	err := TranslateStreamBetween(ir.Chat, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{})
	if err == nil || !strings.Contains(err.Error(), "item id") {
		t.Fatalf("huge content part item id error = %v", err)
	}
}
