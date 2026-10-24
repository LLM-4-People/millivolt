package sse

// Round 5 F1 regression: a streaming response.failed terminal is a failure even
// when its error object is missing, null, or empty. The non-stream Responses
// owner already classifies the same body, and the client did receive the failed
// terminal, so the record must not stay healthy.

import "testing"

func TestStreamingFailedTerminalWithoutErrorRecordsUpstreamError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		line     string
		wantType string
		wantCode string
	}{
		{
			name:     "bare failed",
			line:     `data: {"type":"response.failed"}`,
			wantType: "upstream_error",
		},
		{
			name:     "null nested error",
			line:     `data: {"type":"response.failed","response":{"id":"r1","status":"failed","error":null}}`,
			wantType: "upstream_error",
		},
		{
			name:     "empty nested error",
			line:     `data: {"type":"response.failed","response":{"error":{}}}`,
			wantType: "upstream_error",
		},
		{
			name:     "code without type",
			line:     `data: {"type":"response.failed","response":{"error":{"code":"x"}}}`,
			wantType: "provider_error",
			wantCode: "x",
		},
		{
			name:     "provider type kept",
			line:     `data: {"type":"response.failed","response":{"error":{"type":"rate_limit_error","code":"rate","message":"slow down"}}}`,
			wantType: "rate_limit_error",
			wantCode: "rate",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, rec := analyzeLines([]string{tc.line})
			if rec.FinishReason != "error" {
				t.Fatalf("finish = %q, want error", rec.FinishReason)
			}
			if rec.ErrorType != tc.wantType {
				t.Fatalf("error type = %q, want %q (record %+v)", rec.ErrorType, tc.wantType, rec)
			}
			if rec.ErrorCode != tc.wantCode {
				t.Fatalf("error code = %q, want %q", rec.ErrorCode, tc.wantCode)
			}
			if !rec.IsError() {
				t.Fatalf("record is not an error: %+v", rec)
			}
		})
	}

	_, rec := analyzeLines([]string{
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"answer"}`,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed"}}`,
	})
	if rec.IsError() || rec.ErrorType != "" {
		t.Fatalf("healthy completed stream recorded an error: %+v", rec)
	}
}
