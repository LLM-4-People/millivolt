package proxy

// Round 13 F2 regression: a pretty-printed Responses terminal whose last data
// line ends with a newline but has no blank separator is a complete event. It
// must be resolved before truncation classification and framed once for the
// client, never followed by a synthetic failure.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const terminalFramingCreated = `event: response.created
data: {"type":"response.created","response":{"id":"r1","model":"m","status":"in_progress"}}

`

const terminalFramingTerminal = `data: {
data:   "type": "response.completed",
data:   "response": {
data:     "id": "r1",
data:     "model": "m",
data:     "status": "completed",
data:     "output": [
data:       {
data:         "type": "message",
data:         "id": "msg_1",
data:         "role": "assistant",
data:         "content": [{"type": "output_text", "text": "hi"}]
data:       }
data:     ]
data:   }
data: }
`

func TestResponsesPrettyTerminalNoBlank(t *testing.T) {
	cases := []struct {
		name        string
		stream      string
		wantCode    string
		wantHealthy bool
	}{
		{
			name:        "trailing newline no blank",
			stream:      terminalFramingCreated + terminalFramingTerminal,
			wantHealthy: true,
		},
		{
			name:        "proper blank control",
			stream:      terminalFramingCreated + terminalFramingTerminal + "\n",
			wantHealthy: true,
		},
		{
			name: "incomplete final document remains truncated",
			// Drop the root closing line and its newline: the JSON is genuinely
			// incomplete, so no EOF boundary can repair it.
			stream:   terminalFramingCreated + strings.TrimSuffix(strings.TrimSuffix(terminalFramingTerminal, "data: }\n"), "\n"),
			wantCode: metrics.CodeTruncated,
		},
		{
			name:        "first event terminal",
			stream:      terminalFramingTerminal + "\n",
			wantHealthy: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newFixtureUpstream(t, "text/event-stream", http.StatusOK, tc.stream)
			cfg := config.Default()
			cfg.QualityRetries = 0
			cfg.ThinkingRetries = 0
			buf := metrics.NewBuffer(8)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			status, got := streamRequestTo(t, srv, upstream.server, "/v1/responses",
				`{"model":"m","stream":true,"input":"hi"}`)
			if status != http.StatusOK {
				t.Fatalf("status = %d (body %s)", status, got)
			}
			recs := waitForRecord(t, buf, 1)
			if len(recs) != 1 {
				t.Fatalf("records = %d, want 1", len(recs))
			}
			rec := recs[0]
			if tc.wantHealthy {
				if rec.ErrorType != "" || rec.ErrorCode != "" || rec.IsError() {
					t.Fatalf("healthy terminal record = type %q code %q error %v: %s",
						rec.ErrorType, rec.ErrorCode, rec.IsError(), got)
				}
				if strings.Count(got, "response.completed") != 1 {
					t.Fatalf("terminal count = %d, want 1: %s", strings.Count(got, "response.completed"), got)
				}
				if strings.Contains(got, "event: error") || strings.Contains(got, "event: response.failed") {
					t.Fatalf("healthy terminal gained a synthetic failure: %s", got)
				}
				if !strings.HasSuffix(got, "data: }\n\n") {
					t.Fatalf("client body does not end with one blank after the final data line: %q", got)
				}
			} else {
				if rec.ErrorCode != tc.wantCode {
					t.Fatalf("record code = %q, want %q: %s", rec.ErrorCode, tc.wantCode, got)
				}
			}
			if upstream.calls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1", upstream.calls.Load())
			}
		})
	}
}
