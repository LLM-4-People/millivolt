package format

// Regression tests for the run_request assembly decisions fixed after the
// 10-agent external review:
//   - a continuation whose history ends on a tool result / assistant turn must
//     send ConversationAction.resume_action (empty), never re-answer the
//     original user question ("answer the void" - cursor2api's live
//     never-converging loop bug);
//   - grok ids always carry an explicit fast parameter (the catalog declares
//     it on every grok variant).

import (
	"bytes"
	"encoding/json"
	"testing"
)

// fieldOf extracts the raw bytes of the first occurrence of wire field n
// (wire-type-agnostic scan - fields here are all length-delimited, nested
// bytes are skipped by length).
func fieldOf(t *testing.T, b []byte, n int) []byte {
	t.Helper()
	for len(b) > 0 {
		key, consumed := uvarint(b)
		if consumed <= 0 {
			t.Fatal("truncated varint")
		}
		b = b[consumed:]
		wire := key & 7
		if wire == 2 {
			l, c := uvarint(b)
			if c <= 0 {
				t.Fatal("truncated length")
			}
			if key>>3 == uint64(n) {
				return b[c : c+int(l)]
			}
			b = b[c+int(l):]
		} else if wire == 0 {
			_, c := uvarint(b)
			if c <= 0 {
				t.Fatal("truncated uvarint value")
			}
			b = b[c:]
		} else {
			t.Fatalf("unexpected wire type %d", wire)
		}
	}
	return nil
}

// uvarint reads a protobuf varint, returning its value and byte count.
func uvarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if i >= 10 {
			return 0, 0
		}
		x |= uint64(c&0x7f) << s
		if c&0x80 == 0 {
			return x, i + 1
		}
		s += 7
	}
	return 0, 0
}

func TestTranslateResumeActionOnToolTail(t *testing.T) {
	body := []byte(`{"model":"cursor-grok-4.6-xhigh","messages":[
		{"role":"system","content":"be terse"},
		{"role":"user","content":"Read budget.txt and tell me the budget."},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"budget.txt\"}"}}]},
		{"role":"tool","tool_call_id":"call-1","name":"read_file","content":"The budget is 5000 dollars."}
	],"stream":true}`)

	msg, blobs, resume, err := TranslateCursorRunRequestWithConversation(body, "conv-x")
	if err != nil {
		t.Fatal(err)
	}
	if !resume {
		t.Fatal("tool-tail continuation must be marked as a resume action")
	}
	if blobs == nil || blobs.len() == 0 {
		t.Fatal("history blobs missing")
	}
	runReq := fieldOf(t, msg, fAgentClientMessageRunRequest)
	if runReq == nil {
		t.Fatal("run_request field missing")
	}
	action := fieldOf(t, runReq, fRunRequestAction)
	if action == nil {
		t.Fatal("action field missing")
	}
	if got := fieldOf(t, action, fConversationActionResumeAction); got == nil {
		t.Fatal("expected ConversationAction.resume_action (field 2), not user_message_action")
	}
	if got := fieldOf(t, action, fConversationActionUserMessageAction); got != nil {
		t.Fatal("user_message_action present on a tool-result continuation - model would re-answer the old question")
	}
	// The original user question plus the system text must live in the history
	// blobs now (system has no action text to prepend to).
	var all string
	blobs.mu.Lock()
	for _, c := range blobs.m {
		all += string(c)
	}
	blobs.mu.Unlock()
	if !bytes.Contains([]byte(all), []byte("Read budget.txt")) || !bytes.Contains([]byte(all), []byte("be terse")) {
		t.Fatalf("history blobs should carry the system + user text: %s", all)
	}
}

func TestTranslateUserActionWhenTrailingUser(t *testing.T) {
	body := []byte(`{"model":"cursor-grok-4.6-xhigh","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"hello"},
		{"role":"user","content":"what was the number?"}
	],"stream":true}`)

	msg, _, _, err := TranslateCursorRunRequestWithConversation(body, "conv-x")
	if err != nil {
		t.Fatal(err)
	}
	runReq := fieldOf(t, msg, fAgentClientMessageRunRequest)
	action := fieldOf(t, runReq, fRunRequestAction)
	if ok := fieldOf(t, action, fConversationActionUserMessageAction); ok == nil {
		t.Fatal("a trailing user message must produce a user_message_action")
	}
	if ok := fieldOf(t, action, fConversationActionResumeAction); ok != nil {
		t.Fatal("resume_action must not be set when a fresh user text exists")
	}
}

func TestGrokEmitsFastParam(t *testing.T) {
	base, params, _ := cursorSplitModelID("cursor-grok-4.6-xhigh")
	if base != "grok-4.6" {
		t.Fatalf("base = %q, want grok-4.6", base)
	}
	found := false
	for _, p := range params {
		if p[0] == "fast" {
			found = true
			if p[1] != "false" {
				t.Fatalf("fast = %q, want false", p[1])
			}
		}
	}
	if !found {
		t.Fatal("grok must emit an explicit fast parameter (catalog declares it)")
	}
}

func TestCursorAssistantBlobArgsNeverNull(t *testing.T) {
	cases := []struct {
		name string
		args string
		want string // substring expected in the JSON args field
	}{
		{"empty", "", `"args":{}`},
		{"literal-null", "null", `"args":{}`},
		{"whitespace-null", " null ", `"args":{}`},
		{"object", `{"path":"a.txt"}`, `"args":{"path":"a.txt"}`},
		{"raw-string", `not-json`, `"args":{"_raw":"not-json"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := openaiToolCall{ID: "c1"}
			call.Function.Name = "read"
			call.Function.Arguments = tc.args
			blob := cursorAssistantBlob("", []openaiToolCall{call})
			b, err := json.Marshal(blob)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(b, []byte(tc.want)) {
				t.Fatalf("blob %s does not contain %s", b, tc.want)
			}
			if bytes.Contains(b, []byte(`"args":null`)) {
				t.Fatalf("args must never be JSON null: %s", b)
			}
			if bytes.Contains(b, []byte(`"args":""`)) {
				t.Fatalf("args must never be a bare string: %s", b)
			}
		})
	}
}

func TestTranslateSkipsEmptyAssistantBlobs(t *testing.T) {
	body := []byte(`{"model":"cursor-grok-4.6-xhigh","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":""},
		{"role":"assistant","content":"real answer"},
		{"role":"user","content":"next"}],"stream":true}`)
	msg, blobs, _, err := TranslateCursorRunRequestWithConversation(body, "conv-x")
	if err != nil {
		t.Fatal(err)
	}
	_ = msg
	if blobs.len() != 2 {
		t.Fatalf("expected 2 blobs (user+assistant, empty assistant skipped), got %d", blobs.len())
	}
}

func TestSplitModelTerminalThinking(t *testing.T) {
	// Terminal bare -thinking ids (from GetUsableModels dumps): thinking first,
	// then decompose the left side.
	base, params, _ := cursorSplitModelID("claude-4.6-opus-max-thinking")
	if base != "claude-opus-4-6" {
		t.Fatalf("base = %q, want claude-opus-4-6", base)
	}
	var thinking, effort string
	for _, p := range params {
		switch p[0] {
		case "thinking":
			thinking = p[1]
		case "effort":
			effort = p[1]
		}
	}
	if thinking != "true" || effort != "max" {
		t.Fatalf("params = %v, want thinking:true + effort:max", params)
	}
}

func TestSplitModelNoFastForGlmKimi(t *testing.T) {
	for _, id := range []string{"glm-5.2-max", "kimi-k3-max"} {
		_, params, _ := cursorSplitModelID(id)
		for _, p := range params {
			if p[0] == "fast" {
				t.Fatalf("%s: fast param emitted but the catalog does not declare it", id)
			}
		}
		gotLevel := ""
		for _, p := range params {
			if p[0] == "reasoning" {
				gotLevel = p[1]
			}
		}
		if gotLevel != "max" {
			t.Fatalf("%s: reasoning = %q, want max (params %v)", id, gotLevel, params)
		}
	}
}

func TestSplitModelXhighLiteral(t *testing.T) {
	_, params, _ := cursorSplitModelID("gpt-5.6-sol-xhigh")
	for _, p := range params {
		if p[0] == "reasoning" && p[1] != "xhigh" {
			t.Fatalf("xhigh must pass through literally, got %v", p)
		}
	}
	base, p2, _ := cursorSplitModelID("claude-opus-5-thinking-max")
	if base != "claude-opus-5" {
		t.Fatalf("base = %q", base)
	}
	for _, p := range p2 {
		if p[0] == "effort" && p[1] != "max" {
			t.Fatalf("opus-5 effort = %v", p)
		}
		if p[0] == "fast" && p[1] != "false" {
			t.Fatalf("opus-5 fast = %v", p)
		}
	}
}

func TestCursorModelBase(t *testing.T) {
	// The recorded/dashboard model name must be the canonical base id: thinking
	// level, tier, and -fast stripped, so a Cursor model groups with the same
	// model from other providers.
	cases := []struct{ in, want string }{
		{"claude-sonnet-5", "claude-sonnet-5"},
		{"claude-sonnet-5-thinking", "claude-sonnet-5"},
		{"claude-sonnet-5-thinking-high", "claude-sonnet-5"},
		{"claude-4.5-sonnet-thinking", "claude-sonnet-4-5"},
		{"claude-4.6-opus-max-thinking", "claude-opus-4-6"},
		{"cursor-grok-4.6-xhigh", "grok-4.6"},
		{"gpt-5.6-sol-xhigh", "gpt-5.6-sol"},
		{"grok-4.6-fast", "grok-4.6"},
		{"kimi-k3-max", "kimi-k3"},
		{"auto", "default"},
	}
	for _, tc := range cases {
		if got := CursorModelBase(tc.in); got != tc.want {
			t.Errorf("CursorModelBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCursorMessageTextArray(t *testing.T) {
	m := openaiMessage{Content: json.RawMessage(`"plain"`)}
	if s := cursorMessageText(m); s != "plain" {
		t.Fatalf("string content = %q", s)
	}
	m.Content = json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)
	if s := cursorMessageText(m); s != "ab" {
		t.Fatalf("array content = %q, want ab", s)
	}
	m.Content = json.RawMessage(`[{"type":"file","name":"x.ts","file_data":"AA=="}]`)
	if s := cursorMessageText(m); s != "[file:x.ts]" {
		t.Fatalf("file part = %q, want [file:x.ts]", s)
	}
	m.Content = json.RawMessage(`not json`)
	if s := cursorMessageText(m); s != "" {
		t.Fatalf("garbage content = %q, want empty", s)
	}
}

func TestTranslateKeepsEmptyToolResults(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"q"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"ping","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","name":"ping","content":""},
		{"role":"user","content":"next"}]}`)
	_, blobs, _, err := TranslateCursorRunRequestWithConversation(body, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	blobs.mu.Lock()
	for _, b := range blobs.m {
		if bytes.Contains(b, []byte(`"tool-result"`)) {
			found = true
		}
	}
	blobs.mu.Unlock()
	if !found {
		t.Fatal("empty tool result was skipped - the paired tool-call would orphan")
	}
}
