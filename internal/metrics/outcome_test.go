package metrics

import "testing"

func TestClassifyOutcome(t *testing.T) {
	cases := []struct {
		name    string
		finish  string
		term    bool
		content bool
		tools   int
		chat    bool
		want    string
	}{
		{"healthy stop with content", "stop", true, true, 0, true, ""},
		{"healthy tool_calls with call", "tool_calls", true, false, 1, true, ""},
		{"healthy stop with tool call", "stop", true, false, 1, true, ""},
		{"void stop", "stop", true, false, 0, true, CodeEmptyCompletion},
		{"void absent finish (grok style)", "", true, false, 0, true, CodeEmptyCompletion},
		{"tool_calls without calls", "tool_calls", true, false, 0, true, CodeEmptyToolCall},
		{"truncated: clean EOF, no terminator", "", false, false, 0, true, CodeTruncated},
		{"truncated even with content relayed", "stop", false, true, 0, true, CodeTruncated},
		{"length is not void (legit cutoff)", "length", true, false, 0, true, ""},
		{"content_filter is not void", "content_filter", true, false, 0, true, ""},
		{"error finish is not void (provider-signed)", "error", true, false, 0, true, ""},
		{"sensitive is not void", "sensitive", true, false, 0, true, ""},
		{"foreign protocol never classified", "", false, false, 0, false, ""},
		{"foreign protocol with clean end", "stop", true, false, 0, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyOutcome(c.finish, c.term, c.content, c.tools, c.chat); got != c.want {
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
	for _, code := range []string{CodeEmptyCompletion, CodeEmptyToolCall, CodeTruncated} {
		if got := DegenerateMessage(code); got == "" || got == code {
			t.Fatalf("DegenerateMessage(%q) = %q, want a real message", code, got)
		}
	}
	// Retryability contract: the codes must not contain the substrings that
	// downgrade client-side retry classification (vercel-ai getStatusCode).
	for _, bad := range []string{"invalid", "bad_request", "context_length", "authentication", "permission", "not_found", "overload", "timeout"} {
		for _, code := range []string{CodeEmptyCompletion, CodeEmptyToolCall, CodeTruncated} {
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
