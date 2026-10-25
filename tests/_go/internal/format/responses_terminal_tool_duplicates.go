package format

// Round 2 B2 regression: a failed, incomplete or cancelled terminal owns the
// provider outcome. Two terminal-only tool calls that share a target call id
// must not replace that outcome with a duplicate-id translation error. A
// completed terminal still fails closed because both calls claim success.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func duplicateTerminalToolCallStream(event, status, details string) string {
	response := `{"id":"resp_1","model":"m","status":"` + status + `"` + details +
		`,"output":[{"type":"function_call","id":"fc_1","call_id":"dup","name":"first","arguments":"{}"},` +
		`{"type":"function_call","id":"fc_2","call_id":"dup","name":"second","arguments":"{}"}]}`
	return terminalStreamCreated + "event: " + event + "\ndata: {\"type\":\"" + event +
		"\",\"response\":" + response + "}\n\n"
}

func TestResponsesTerminalDuplicateToolIDsFollowOutcome(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		status  string
		details string
		outcome string
	}{
		{
			name:    "failed",
			event:   "response.failed",
			status:  "failed",
			details: `,"error":{"type":"server_error","message":"provider failed"}`,
			outcome: "provider failed",
		},
		{
			name:    "incomplete",
			event:   "response.incomplete",
			status:  "incomplete",
			details: `,"incomplete_details":{"reason":"max_output_tokens"}`,
			outcome: `"status":"incomplete"`,
		},
		{
			name:    "cancelled",
			event:   "response.cancelled",
			status:  "cancelled",
			details: "",
			outcome: `"status":"cancelled"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			source := duplicateTerminalToolCallStream(tc.event, tc.status, tc.details)
			if err := TranslateStreamBetween(ir.Responses, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
				t.Fatalf("duplicate terminal tool ids replaced the %s outcome: %v", tc.name, err)
			}
			out := buf.String()
			if got := toolCallStarts(t, ir.Responses, out); got != 1 {
				t.Fatalf("%s output opens %d tool calls, want one: %s", tc.name, got, out)
			}
			if !strings.Contains(out, tc.outcome) {
				t.Fatalf("%s output lost the provider outcome %q: %s", tc.name, tc.outcome, out)
			}
		})
	}

	t.Run("completed still fails closed", func(t *testing.T) {
		var buf bytes.Buffer
		source := duplicateTerminalToolCallStream("response.completed", "completed", "")
		err := TranslateStreamBetween(ir.Responses, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{})
		if err == nil || !strings.Contains(err.Error(), "duplicate tool call id") {
			t.Fatalf("completed duplicate terminal tool ids = %v, want a fail-closed rejection; output: %s", err, buf.String())
		}
	})
}
