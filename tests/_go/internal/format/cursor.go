package format

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// --- test-side encoders (mirror the production wire format) ---

// env frames a message as a Connect data envelope.
func env(t *testing.T, msg []byte) []byte {
	t.Helper()
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(msg)))
	return append(hdr[:], msg...)
}

// endStreamEnv frames a JSON EndStreamResponse (flags=0x02).
func endStreamEnv(t *testing.T, jsonBody string) []byte {
	t.Helper()
	var hdr [5]byte
	hdr[0] = connectFlagEndStream
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(jsonBody)))
	return append(hdr[:], jsonBody...)
}

// textDeltaMsg builds an AgentServerMessage carrying a text delta.
func textDeltaMsg(t *testing.T, text string) []byte {
	t.Helper()
	td := appendString(nil, fTextDeltaText, text)
	upd := appendMessage(nil, fInteractionUpdateTextDelta, td)
	return appendMessage(nil, fAgentServerMessageInteractionUpdate, upd)
}

// thinkingDeltaMsg builds an AgentServerMessage carrying a thinking delta.
func thinkingDeltaMsg(t *testing.T, text string) []byte {
	t.Helper()
	td := appendString(nil, fThinkingDeltaText, text)
	upd := appendMessage(nil, fInteractionUpdateThinkingDelta, td)
	return appendMessage(nil, fAgentServerMessageInteractionUpdate, upd)
}

// tokenDeltaMsg builds an AgentServerMessage carrying a cumulative token count.
func tokenDeltaMsg(t *testing.T, tokens int32) []byte {
	t.Helper()
	td := appendInt32(nil, fTokenDeltaTokens, tokens)
	upd := appendMessage(nil, fInteractionUpdateTokenDelta, td)
	return appendMessage(nil, fAgentServerMessageInteractionUpdate, upd)
}

// checkpointMsg builds an AgentServerMessage carrying a
// conversation_checkpoint_update whose ConversationStateStructure.token_details
// .used_tokens is the given value.
func checkpointMsg(t *testing.T, usedTokens uint32) []byte {
	t.Helper()
	details := appendUint32(nil, fTokenDetailsUsedTokens, usedTokens)
	cs := appendMessage(nil, fConversationStateTokenDetails, details)
	return appendMessage(nil, fAgentServerMessageConversationCheckpointUpdate, cs)
}

// turnEndedField14 builds an AgentServerMessage carrying InteractionUpdate
// field 14 - which is TurnEndedUpdate (EMPTY), NOT a usage summary. The
// regression guard: decoding it must inject NO usage.
func turnEndedField14(t *testing.T) []byte {
	t.Helper()
	upd := appendMessage(nil, fInteractionUpdateTurnEnded, nil)
	return appendMessage(nil, fAgentServerMessageInteractionUpdate, upd)
}

// TestCursorUsageFromCheckpointNotField14 is the regression test for the
// runaway-context bug: the proxy previously read InteractionUpdate field 14 as
// a "usage summary" with prompt/output/reasoning sub-fields, but field 14 is
// the empty TurnEndedUpdate marker. Decoding it as a summary misaligned the
// wire and produced multi-million phantom prompt_tokens that made clients
// compact. The accurate input is the checkpoint's token_details.used_tokens;
// the accurate per-turn output is the TokenDeltaUpdate sum.
func TestCursorUsageFromCheckpointNotField14(t *testing.T) {
	pr, pw := io.Pipe()
	run := NewCursorRun(pw, pr, nil, func() {}, 5*time.Second)
	run.Start()
	go func() {
		// A checkpoint pinning the real context occupancy, two output deltas,
		// then the (empty) field-14 turn_ended marker that must NOT be read as
		// a usage summary, then EOF to finish the turn.
		_, _ = pw.Write(env(t, checkpointMsg(t, 160503)))
		_, _ = pw.Write(env(t, tokenDeltaMsg(t, 990)))
		_, _ = pw.Write(env(t, turnEndedField14(t)))
		_ = pw.Close()
	}()

	res := run.RunTurn(t.Context(), func(map[string]any) error { return nil })
	run.Close()

	if res.Outcome != TurnFinished {
		t.Fatalf("Outcome = %v, want TurnFinished (clean EOF after turn_ended)", res.Outcome)
	}
	if res.Prompt != 160503 {
		t.Errorf("Prompt = %d, want 160503 (checkpoint used_tokens, not a field-14 misparse)", res.Prompt)
	}
	if res.Output != 990 {
		t.Errorf("Output = %d, want 990 (TokenDeltaUpdate sum)", res.Output)
	}
	if res.Reasoning != 0 {
		t.Errorf("Reasoning = %d, want 0 (the wire reports a single combined output counter)", res.Reasoning)
	}
}

// TestCursorTurnEndedField14InjectsNoUsage isolates the regression: a bare
// field-14 turn_ended frame must leave usage at zero (it is the empty
// TurnEndedUpdate, never a prompt/output/reasoning summary).
func TestCursorTurnEndedField14InjectsNoUsage(t *testing.T) {
	pr, pw := io.Pipe()
	run := NewCursorRun(pw, pr, nil, func() {}, 5*time.Second)
	run.Start()
	go func() {
		_, _ = pw.Write(env(t, turnEndedField14(t)))
		_ = pw.Close()
	}()

	res := run.RunTurn(t.Context(), func(map[string]any) error { return nil })
	run.Close()

	if res.Prompt != 0 || res.Output != 0 || res.Reasoning != 0 {
		t.Errorf("field-14 turn_ended injected usage (prompt=%d output=%d reasoning=%d); want all 0",
			res.Prompt, res.Output, res.Reasoning)
	}
}

// TestCursorTrailingUserText pins the OpenAI request shapes the resume-steer
// must catch: a trailing user message is extracted whether its content is a
// plain string, a text-parts array, or multimodal (text + image), and is ""
// when the request ends on a tool result (a pure resume with nothing to steer).
func TestCursorTrailingUserText(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "trailing user, string content",
			body: `{"messages":[{"role":"tool","tool_call_id":"c1","content":"ok"},{"role":"user","content":"also fix the tests"}]}`,
			want: "also fix the tests",
		},
		{
			name: "trailing user, text-parts array",
			body: `{"messages":[{"role":"tool","tool_call_id":"c1","content":"ok"},{"role":"user","content":[{"type":"text","text":"part one "},{"type":"text","text":"part two"}]}]}`,
			want: "part one part two",
		},
		{
			name: "trailing user, multimodal text+image keeps the text",
			body: `{"messages":[{"role":"tool","tool_call_id":"c1","content":"ok"},{"role":"user","content":[{"type":"text","text":"look at this"},{"type":"image_url","image_url":{"url":"data:..."}}]}]}`,
			want: "look at this[image_url]",
		},
		{
			name: "ends on tool result (pure resume) → nothing to steer",
			body: `{"messages":[{"role":"user","content":"earlier"},{"role":"tool","tool_call_id":"c1","content":"ok"}]}`,
			want: "",
		},
		{
			name: "trailing user with EMPTY text → nothing to steer",
			body: `{"messages":[{"role":"tool","tool_call_id":"c1","content":"ok"},{"role":"user","content":"  "}]}`,
			want: "",
		},
		{
			name: "no messages → empty",
			body: `{"messages":[]}`,
			want: "",
		},
		{
			name: "malformed body → empty",
			body: `not json`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CursorTrailingUserText([]byte(tc.body)); got != tc.want {
				t.Errorf("CursorTrailingUserText = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunTurnEmitFailureMarksClientAbort pins the classification contract: a
// failed emitDelta (writing the turn's OpenAI SSE to the LOCAL client - the
// broken pipe of a client that disconnected mid-stream) returns a TurnErrored
// result flagged ClientAbort, so the proxy records the client's own
// cancellation instead of an upstream stream_read_error.
func TestRunTurnEmitFailureMarksClientAbort(t *testing.T) {
	pr, pw := io.Pipe()
	run := NewCursorRun(pw, pr, nil, func() {}, 5*time.Second)
	run.Start()
	go func() { _, _ = pw.Write(env(t, textDeltaMsg(t, "hi"))) }()

	wantErr := fmt.Errorf("write tcp 127.0.0.1:8080->127.0.0.1:45778: write: broken pipe")
	res := run.RunTurn(t.Context(), func(map[string]any) error { return wantErr })
	run.Close()

	if res.Outcome != TurnErrored {
		t.Fatalf("Outcome = %v, want TurnErrored", res.Outcome)
	}
	if !res.ClientAbort {
		t.Errorf("ClientAbort = false, want true (an emitDelta failure IS the client disconnecting)")
	}
	if res.Err != wantErr {
		t.Errorf("Err = %v, want the emit error carried through", res.Err)
	}
}

// --- request translation ---

func TestTranslateCursorRunRequestBasic(t *testing.T) {
	msg, _, _, err := TranslateCursorRunRequestWithConversation([]byte(
		`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hello"}],"stream":true}`), "")
	if err != nil {
		t.Fatal(err)
	}
	out := AppendConnectEnvelope(nil, msg)

	// Must be a single Connect data envelope.
	cr := newConnectReader(bufio.NewReader(bytes.NewReader(out)))
	frame, err := cr.next()
	if err != nil {
		t.Fatalf("read envelope: %v", err)
	}
	if frame.endStream {
		t.Fatal("request envelope must be a data frame, not end-stream")
	}
	// Exactly one frame, then clean EOF.
	if _, err := cr.next(); err != io.EOF {
		t.Fatalf("expected single envelope then EOF, got %v", err)
	}

	// Walk AgentClientMessage -> run_request.
	asm, err := parseProtoFields(frame.payload)
	if err != nil {
		t.Fatal(err)
	}
	rr, err := subFields(firstField(asm, fAgentClientMessageRunRequest))
	if err != nil {
		t.Fatal(err)
	}
	// Independent descriptor field numbers pin conversation_id (5) and
	// conversation_group_id (16). The bridge currently uses one group per
	// conversation; these are not two aliases for one wire field.
	conversation, group := firstField(rr, 5), firstField(rr, 16)
	if conversation == nil || group == nil || conversation.wire != wireBytes ||
		group.wire != wireBytes || len(conversation.raw) == 0 || !bytes.Equal(conversation.raw, group.raw) {
		t.Fatalf("conversation/group identity mismatch: conversation=%+v group=%+v", conversation, group)
	}

	// model_id present in RequestedModel (the canonical agent.v1 field; the real
	// client omits ModelDetails).
	rm, err := subFields(firstField(rr, fRunRequestRequestedModel))
	if err != nil {
		t.Fatal(err)
	}
	if got := firstField(rm, fRequestedModelModelID); got == nil || string(got.raw) != "claude-sonnet-5" {
		t.Errorf("RequestedModel.model_id wrong: %+v", got)
	}

	// Current user text nested under action.user_message_action.user_message.text.
	act, err := subFields(firstField(rr, fRunRequestAction))
	if err != nil {
		t.Fatal(err)
	}
	uma, err := subFields(firstField(act, fConversationActionUserMessageAction))
	if err != nil {
		t.Fatal(err)
	}
	um, err := subFields(firstField(uma, fUserMessageActionUserMessage))
	if err != nil {
		t.Fatal(err)
	}
	if got := firstField(um, fUserMessageText); got == nil || string(got.raw) != "hello" {
		t.Errorf("user text wrong: %+v", got)
	}
	if got := firstField(um, fUserMessageMessageID); got == nil || len(got.raw) == 0 {
		t.Errorf("message_id missing")
	}
}

func TestTranslateCursorRunRequestRequiresModel(t *testing.T) {
	if _, _, _, err := TranslateCursorRunRequestWithConversation([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), ""); err == nil {
		t.Fatal("expected error when model is missing")
	}
}

func TestTranslateCursorRunRequestHistoryAndSystem(t *testing.T) {
	body := `{"model":"m","messages":[` +
		`{"role":"system","content":"you are helpful"},` +
		`{"role":"user","content":"q1"},` +
		`{"role":"assistant","content":"a1"},` +
		`{"role":"user","content":"q2"}]}`
	msg, blobs, resume, err := TranslateCursorRunRequestWithConversation([]byte(body), "")
	if err != nil {
		t.Fatal(err)
	}
	if resume {
		t.Fatal("a fresh trailing user message must not be marked as a resume action")
	}
	asm, _ := parseProtoFields(msg)
	rr, _ := subFields(firstField(asm, fAgentClientMessageRunRequest))

	// History: q1 + a1 become sha256-keyed blobs. root_prompt_messages_json
	// carries the DIGESTS (the ids the server pulls); the blob contents live in
	// the KV store to answer get_blob pulls.
	cs, err := subFields(firstField(rr, fRunRequestConversationState))
	if err != nil {
		t.Fatal(err)
	}
	var digestCount int
	for _, f := range cs {
		if f.num == fConversationStateRootPromptMessagesJSON {
			digestCount++
			if len(f.raw) != 32 {
				t.Errorf("history entry should be a 32-byte sha256 digest, got %d bytes", len(f.raw))
			}
			// The digest must resolve to a blob in the store.
			blob, ok := blobs.get(f.raw)
			if !ok {
				t.Errorf("digest %x has no blob in the KV store", f.raw)
				continue
			}
			var parsed map[string]any
			if err := json.Unmarshal(blob, &parsed); err != nil {
				t.Errorf("blob is not JSON: %v", err)
			}
		}
	}
	if digestCount != 3 {
		t.Fatalf("expected 3 history digests (system + q1 + a1), got %d", digestCount)
	}
	if blobs.len() != 3 {
		t.Errorf("KV store should hold 3 blobs, got %d", blobs.len())
	}
	// The system text is a FIRST blob (canonical delivery - never prepended
	// into the action text).
	var firstBlob map[string]any
	firstDigest := cs[0].raw
	{
		blob, ok := blobs.get(firstDigest)
		if !ok {
			t.Fatal("first digest missing from KV store")
		}
		if err := json.Unmarshal(blob, &firstBlob); err != nil {
			t.Fatal(err)
		}
	}
	if firstBlob["role"] != "system" || firstBlob["content"] != "you are helpful" {
		t.Errorf("first blob should be the system text, got %v", firstBlob)
	}
	// The q1 user turn is present as a {role:user, content:"q1"} blob.
	foundQ1 := false
	for _, f := range cs {
		if f.num == fConversationStateRootPromptMessagesJSON {
			blob, _ := blobs.get(f.raw)
			var p map[string]any
			if json.Unmarshal(blob, &p) == nil && p["role"] == "user" && p["content"] == "q1" {
				foundQ1 = true
			}
		}
	}
	if !foundQ1 {
		t.Errorf("q1 user-turn blob not found")
	}

	// Current text = the last user turn only (system travels as a blob, never
	// prepended - canonical client behavior).
	act, _ := subFields(firstField(rr, fRunRequestAction))
	uma, _ := subFields(firstField(act, fConversationActionUserMessageAction))
	um, _ := subFields(firstField(uma, fUserMessageActionUserMessage))
	got := string(firstField(um, fUserMessageText).raw)
	if got != "q2" {
		t.Errorf("current text should be the last user turn, got %q", got)
	}
	if strings.Contains(got, "you are helpful") {
		t.Errorf("system text must not be prepended into the action, got %q", got)
	}
}
