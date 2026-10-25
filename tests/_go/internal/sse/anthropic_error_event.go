package sse

// Round 6 F1 regression: an Anthropic error event whose nested object carries
// only a message or only a code is still a failure. The shared error parser
// classifies it as provider_error; an empty error object is an upstream_error
// because the error event itself is the provider's terminal statement.

import (
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnthropicStreamErrorWithoutTypeRecordsFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		line     string
		wantType string
		wantCode string
		wantMsg  string
	}{
		{
			name:     "message only",
			line:     `data: {"type":"error","error":{"message":"boom"}}`,
			wantType: "provider_error",
			wantMsg:  "boom",
		},
		{
			name:     "code only",
			line:     `data: {"type":"error","error":{"code":"overloaded"}}`,
			wantType: "provider_error",
			wantCode: "overloaded",
		},
		{
			name:     "empty object",
			line:     `data: {"type":"error","error":{}}`,
			wantType: TypeUpstreamError,
			wantMsg:  "upstream response failed",
		},
		{
			name:     "typed provider error",
			line:     `data: {"type":"error","error":{"type":"overloaded_error","message":"busy"}}`,
			wantType: "overloaded_error",
			wantMsg:  "busy",
		},
		{
			name:     "null object",
			line:     `data: {"type":"error","error":null}`,
			wantType: TypeUpstreamError,
			wantMsg:  "upstream response failed",
		},
		{
			name:     "absent object",
			line:     `data: {"type":"error"}`,
			wantType: TypeUpstreamError,
			wantMsg:  "upstream response failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := strings.Join([]string{
				`event: message_start`,
				`data: {"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1,"output_tokens":0}}}`,
				``,
				`event: error`,
				tc.line,
				``,
			}, "\n")
			var a Analyzer
			feedAnthropicLines(t, &a, stream)
			if !a.Terminated() {
				t.Fatal("error event did not terminate the analyzer")
			}
			var rec metrics.Record
			a.Fill(&rec)
			if rec.ErrorType != tc.wantType || rec.ErrorCode != tc.wantCode || rec.ErrorMsg != tc.wantMsg {
				t.Fatalf("error = %q/%q/%q, want %q/%q/%q",
					rec.ErrorType, rec.ErrorCode, rec.ErrorMsg, tc.wantType, tc.wantCode, tc.wantMsg)
			}
			if !rec.IsError() {
				t.Fatalf("record is not an error: %+v", rec)
			}
		})
	}

	var a Analyzer
	feedAnthropicLines(t, &a, strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","model":"m"}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n"))
	var rec metrics.Record
	a.Fill(&rec)
	if rec.IsError() || rec.ErrorType != "" {
		t.Fatalf("clean stream recorded an error: %+v", rec)
	}
}

// The shared standalone-error owner also covers a Responses error event that
// names neither code nor message.
func TestResponsesStandaloneErrorWithoutFieldsRecordsFailure(t *testing.T) {
	var a Analyzer
	feedLines(&a,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress"}}`,
		``,
		`event: error`,
		`data: {"type":"error"}`,
		``,
	)
	if !a.Terminated() {
		t.Fatal("standalone error did not terminate the analyzer")
	}
	var rec metrics.Record
	a.Fill(&rec)
	if !rec.IsError() || rec.ErrorType != TypeUpstreamError || rec.ErrorMsg != "upstream response failed" {
		t.Fatalf("record error = %q/%q/%q, want upstream_error with the generic message",
			rec.ErrorType, rec.ErrorCode, rec.ErrorMsg)
	}
}
