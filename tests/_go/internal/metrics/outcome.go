package metrics

import "testing"

func TestClassifyOutcome(t *testing.T) {
	cases := []struct {
		name   string
		finish string
		term   bool
		answer bool
		reason bool
		tools  int
		chat   bool
		want   string
	}{
		{"healthy stop with content", "stop", true, true, false, 0, true, ""},
		{"healthy tool_calls with call", "tool_calls", true, false, false, 1, true, ""},
		{"healthy stop with tool call", "stop", true, false, false, 1, true, ""},
		{"healthy stop after reasoning and answer", "stop", true, true, true, 0, true, ""},
		{"reasoning-only stop is its own class", "stop", true, false, true, 0, true, CodeReasoningOnly},
		{"reasoning-only absent finish is its own class", "", true, false, true, 0, true, CodeReasoningOnly},
		{"reasoning-only interruption finish is rescuable void", "insufficient_system_resource", true, false, true, 0, true, CodeReasoningOnly},
		{"reasoning-only aborted finish is rescuable void", "aborted", true, false, true, 0, true, CodeReasoningOnly},
		{"reasoning-only length is the client's cap, never a void", "length", true, false, true, 0, true, ""},
		{"reasoning-only content_filter is provider-signed, never a void", "content_filter", true, false, true, 0, true, ""},
		{"reasoning does not rescue a lying tool_calls finish", "tool_calls", true, false, true, 0, true, CodeEmptyToolCall},
		{"void stop", "stop", true, false, false, 0, true, CodeEmptyCompletion},
		{"void absent finish (grok style)", "", true, false, false, 0, true, CodeEmptyCompletion},
		{"void interruption finish stays provider-signed", "insufficient_system_resource", true, false, false, 0, true, ""},
		{"tool_calls without calls", "tool_calls", true, false, false, 0, true, CodeEmptyToolCall},
		{"truncated: clean EOF, no terminator", "", false, false, false, 0, true, CodeTruncated},
		{"truncated even with reasoning relayed", "stop", false, false, true, 0, true, CodeTruncated},
		{"length is not void (legit cutoff)", "length", true, false, false, 0, true, ""},
		{"content_filter is not void", "content_filter", true, false, false, 0, true, ""},
		{"error finish is not void (provider-signed)", "error", true, false, false, 0, true, ""},
		{"sensitive is not void", "sensitive", true, false, false, 0, true, ""},
		{"foreign protocol never classified", "", false, false, false, 0, false, ""},
		{"foreign protocol with clean end", "stop", true, false, false, 0, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyOutcome(c.finish, c.term, c.answer, c.reason, c.tools, c.chat); got != c.want {
				t.Fatalf("ClassifyOutcome = %q, want %q", got, c.want)
			}
		})
	}
}

func TestClassifyNonStreamBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"healthy", `{"id":"1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}]}`, ""},
		{"void stop", `{"id":"1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":""}}]}`, CodeEmptyCompletion},
		{"void null content", `{"id":"1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":null}}]}`, CodeEmptyCompletion},
		{"tool mismatch", `{"id":"1","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null}}]}`, CodeEmptyToolCall},
		{"real tool call", `{"id":"1","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"1","function":{"name":"x"}}]}}]}`, ""},
		{"content array", `{"id":"1","choices":[{"finish_reason":"stop","message":{"content":[{"type":"text","text":"y"}]}}]}`, ""},
		{"empty content array is void", `{"id":"1","choices":[{"finish_reason":"stop","message":{"content":[]}}]}`, CodeEmptyCompletion},
		{"length is not void", `{"id":"1","choices":[{"finish_reason":"length","message":{"content":"part"}}]}`, ""},
		// The streaming-aligned reasoning rows: a reasoning-only body is the
		// reasoning_only class (previously mis-classified empty_completion,
		// re-running a request the model had already billed for thinking).
		{"reasoning_content only is reasoning_only (deepseek shape)", `{"id":"1","choices":[{"finish_reason":"stop","message":{"content":null,"reasoning_content":"thought hard"}}]}`, CodeReasoningOnly},
		{"bare reasoning only is reasoning_only (openrouter shape)", `{"id":"1","choices":[{"finish_reason":"stop","message":{"content":"","reasoning":"thought"}}]}`, CodeReasoningOnly},
		{"reasoning_details array only is reasoning_only", `{"id":"1","choices":[{"finish_reason":"stop","message":{"content":null,"reasoning_details":[{"type":"thinking","thinking":"hmm"}]}}]}`, CodeReasoningOnly},
		{"empty reasoning_content is still a void (qwen role-chunk decoy)", `{"id":"1","choices":[{"finish_reason":"stop","message":{"content":null,"reasoning_content":""}}]}`, CodeEmptyCompletion},
		{"reasoning with answer is healthy", `{"id":"1","choices":[{"finish_reason":"stop","message":{"content":"hi","reasoning_content":"thought"}}]}`, ""},
		{"reasoning-only length is healthy (client cap)", `{"id":"1","choices":[{"finish_reason":"length","message":{"content":null,"reasoning_content":"ran out"}}]}`, ""},
		{"reasoning-only tool mismatch keeps the tool defect", `{"id":"1","choices":[{"finish_reason":"tool_calls","message":{"content":null,"reasoning_content":"thought","tool_calls":[]}}]}`, CodeEmptyToolCall},
		{"responses api shape unclassified", `{"object":"response","output":[]}`, ""},
		{"usage-only body unclassified", `{"choices":[],"usage":{"prompt_tokens":1}}`, ""},
		{"garbage", `not json`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyNonStreamBody([]byte(c.body)); got != c.want {
				t.Fatalf("ClassifyNonStreamBody = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDegenerateMessage(t *testing.T) {
	for _, code := range []string{CodeEmptyCompletion, CodeEmptyToolCall, CodeTruncated, CodeReasoningOnly} {
		if got := DegenerateMessage(code); got == "" || got == code {
			t.Fatalf("DegenerateMessage(%q) = %q, want a real message", code, got)
		}
	}
	// Retryability contract: the codes must not contain the substrings that
	// downgrade client-side retry classification (vercel-ai getStatusCode).
	for _, bad := range []string{"invalid", "bad_request", "context_length", "authentication", "permission", "not_found", "overload", "timeout"} {
		for _, code := range []string{CodeEmptyCompletion, CodeEmptyToolCall, CodeTruncated, CodeReasoningOnly} {
			if contains(code, bad) {
				t.Fatalf("code %q contains non-retryable substring %q", code, bad)
			}
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
