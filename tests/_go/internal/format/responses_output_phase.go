package format

// Round 7 B3 regression: an output message phase is decoded by the wire struct
// but was dropped without a warning. The official schema documents it and asks
// callers to preserve it on follow-up requests, so the loss must be reported
// once per response.

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesOutputPhaseWarns(t *testing.T) {
	body := func(phase string) []byte {
		item := `{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hi"}]}`
		if phase != "" {
			item = `{"type":"message","id":"msg_1","role":"assistant","phase":"` + phase + `","content":[{"type":"output_text","text":"hi"}]}`
		}
		return []byte(`{"id":"resp_1","model":"m","status":"completed","output":[` + item + `]}`)
	}

	resp, err := DecodeResponsesResponse(body("final_answer"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Blocks) != 1 || resp.Blocks[0].Text != "hi" {
		t.Fatalf("phase drop changed the content: %+v", resp.Blocks)
	}
	if got := warningCount(resp.Warnings, ir.WarningDropped, "output.phase"); got != 1 {
		t.Fatalf("output.phase warnings = %d, want 1 (%+v)", got, resp.Warnings)
	}

	clean, err := DecodeResponsesResponse(body(""))
	if err != nil {
		t.Fatal(err)
	}
	if warningField(clean.Warnings, "output.phase") {
		t.Fatalf("phase-free response warned: %+v", clean.Warnings)
	}

	// One warning per response even when several items carry a phase.
	multi := []byte(`{"id":"resp_1","model":"m","status":"completed","output":[` +
		`{"type":"message","id":"msg_1","role":"assistant","phase":"analysis","content":[{"type":"output_text","text":"a"}]},` +
		`{"type":"message","id":"msg_2","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"b"}]}]}`)
	resp, err = DecodeResponsesResponse(multi)
	if err != nil {
		t.Fatal(err)
	}
	if got := warningCount(resp.Warnings, ir.WarningDropped, "output.phase"); got != 1 {
		t.Fatalf("multi-item output.phase warnings = %d, want 1 (%+v)", got, resp.Warnings)
	}

	// The request-side input.phase warning is a separate contract and stays.
	req, err := DecodeResponsesRequest([]byte(`{"model":"m","input":[{"type":"message","role":"user","phase":"final_answer","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !warningField(req.Warnings, "input.phase") {
		t.Fatalf("input.phase warning disappeared: %+v", req.Warnings)
	}
}
