package format

// Round 2 TC2-5 regressions: an unknown non-empty Chat finish_reason is not
// silently canonicalized. It maps to the non-clean FinishOther and records one
// bounded approximation carrying the raw spelling; the missing-marker warning
// stays distinct.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func chatFinishBody(finish string) []byte {
	return []byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,` +
		`"message":{"role":"assistant","content":"hi"},"finish_reason":"` + finish + `"}]}`)
}

func finishReasonWarnings(resp *ir.Response) []ir.Warning {
	var out []ir.Warning
	for _, warning := range resp.Warnings {
		if warning.Field == "finish_reason" {
			out = append(out, warning)
		}
	}
	return out
}

func TestChatUnknownFinishReasonWarns(t *testing.T) {
	resp, err := DecodeChatResponse(chatFinishBody("provider_side_stop"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Finish != ir.FinishOther {
		t.Fatalf("finish = %q, want %q", resp.Finish, ir.FinishOther)
	}
	warnings := finishReasonWarnings(resp)
	if len(warnings) != 1 {
		t.Fatalf("finish_reason warnings = %d, want one: %+v", len(warnings), resp.Warnings)
	}
	if warnings[0].Kind != ir.WarningApproximated || !strings.Contains(warnings[0].Detail, "provider_side_stop") {
		t.Fatalf("unknown finish warning = %+v, want the raw spelling", warnings[0])
	}

	out, err := EncodeResponsesResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	if root := decodeJSONMap(t, out); root["status"] != "incomplete" {
		t.Fatalf("responses status = %v, want incomplete: %s", root["status"], out)
	}
}

func TestChatMissingFinishReasonKeepsMarkerWarning(t *testing.T) {
	resp, err := DecodeChatResponse(chatFinishBody(""))
	if err != nil {
		t.Fatal(err)
	}
	warnings := finishReasonWarnings(resp)
	if len(warnings) != 1 || warnings[0].Detail != "provider omitted the completion marker" {
		t.Fatalf("missing-marker warnings = %+v, want exactly the marker warning", warnings)
	}
}

func TestChatUnknownFinishReasonEchoBounded(t *testing.T) {
	long := strings.Repeat("x", 4096)
	resp, err := DecodeChatResponse(chatFinishBody(long))
	if err != nil {
		t.Fatal(err)
	}
	warnings := finishReasonWarnings(resp)
	if len(warnings) != 1 {
		t.Fatalf("finish_reason warnings = %d, want one: %+v", len(warnings), resp.Warnings)
	}
	if len(warnings[0].Detail) > 2*boundedEchoMax {
		t.Fatalf("warning detail is %d bytes, want a bounded echo: %q", len(warnings[0].Detail), warnings[0].Detail)
	}
}
