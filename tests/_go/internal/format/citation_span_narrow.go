package format

// Round 11 H3 regression: citation span narrowing must fail closed on a
// 32-bit int instead of aliasing 4294967296 to 0. Responses already routes its
// spans through intFromJSONIndex; Chat used a plain int conversion.

import (
	"strconv"
	"strings"
	"testing"
)

func chatCitationSpanBody(start, end string) string {
	return `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"hi","annotations":[{"type":"url_citation",` +
		`"url_citation":{"url":"https://e","title":"t","start_index":` + start + `,"end_index":` + end + `}}]},` +
		`"finish_reason":"stop"}]}`
}

func responsesCitationSpanBody(start, end string) string {
	return `{"id":"r1","object":"response","created_at":1,"status":"completed","model":"m",` +
		`"output":[{"type":"message","id":"msg_1","role":"assistant","content":[{` +
		`"type":"output_text","text":"hi","annotations":[{"type":"url_citation",` +
		`"url":"https://e","title":"t","start_index":` + start + `,"end_index":` + end + `}]}]}]}`
}

func TestCitationSpansFailClosedOnNarrowInt(t *testing.T) {
	const hugeStart, hugeEnd = "4294967296", "4294967297"

	chat, err := DecodeChatResponse([]byte(chatCitationSpanBody(hugeStart, hugeEnd)))
	if strconv.IntSize == 32 {
		if err == nil {
			t.Fatalf("32-bit Chat span aliased: %+v", chat.Blocks)
		}
		if !strings.Contains(err.Error(), "out of range") {
			t.Fatalf("32-bit Chat error = %v, want the range rejection", err)
		}
	} else {
		if err != nil {
			t.Fatalf("64-bit Chat span rejected: %v", err)
		}
		if len(chat.Blocks) != 1 || len(chat.Blocks[0].Citations) != 1 {
			t.Fatalf("64-bit Chat citation missing: %+v", chat.Blocks)
		}
		if got := int64(chat.Blocks[0].Citations[0].StartIndex); got != int64(4294967296) {
			t.Fatalf("64-bit Chat start = %d, want the exact value", got)
		}
	}

	responses, err := DecodeResponsesResponse([]byte(responsesCitationSpanBody(hugeStart, hugeEnd)))
	if strconv.IntSize == 32 {
		if err == nil {
			t.Fatalf("32-bit Responses span aliased: %+v", responses.Blocks)
		}
	} else {
		// The value narrows exactly; the message-relative rebase then drops a
		// span outside the two-rune part without failing the response.
		if err != nil {
			t.Fatalf("64-bit Responses span rejected: %v", err)
		}
		if len(responses.Blocks) != 1 || len(responses.Blocks[0].Citations) != 0 {
			t.Fatalf("out-of-part span survived: %+v", responses.Blocks)
		}
	}

	// Fractional spans stay invalid on every platform.
	if _, err := DecodeChatResponse([]byte(chatCitationSpanBody("0.5", "1"))); err == nil {
		t.Fatal("fractional Chat span accepted")
	}
	if _, err := DecodeResponsesResponse([]byte(responsesCitationSpanBody("0.5", "1"))); err == nil {
		t.Fatal("fractional Responses span accepted")
	}
}
