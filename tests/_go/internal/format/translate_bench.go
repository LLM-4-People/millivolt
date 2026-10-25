package format

// Translation cost had no measurement: internal/format is 17k lines on the
// translated-stream hot path and carried one unrelated benchmark. This file
// owns the byte-for-byte corpus for the five codec pairs the proxy actually
// serves (a translated stream is always source wire -> client wire, with the
// IR in between) plus one 1 MiB non-stream pair.
//
// The corpus doubles as the regression oracle: TestTranslateStreamCorpus
// asserts every emitted frame, and BenchmarkTranslateStream re-asserts the same
// golden once before timing, so a codec change that alters a byte cannot hide
// behind a performance number. `created` is the one volatile field: the
// Responses and Anthropic sources carry no creation timestamp, so a Chat target
// stamps time.Now(); those pairs zero it in the canonical form and the pairs
// whose source does carry one pin it in the golden.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// corpusCreated is the fixed creation timestamp of the Chat source; a golden
// that carries it proves the Chat->X mapping, and the pairs whose source has no
// timestamp normalize it away instead of accepting wall-clock noise.
const corpusCreated = 1_700_000_000

// corpusChatStream is a Chat Completions upstream stream with the shapes a
// translated request must survive: the role-start frame, answer text, reasoning
// content, a tool call split across two argument fragments, a finish frame and
// the usage-only tail. It is the source for four of the five pairs.
func corpusChatStream() string {
	const head = `data: {"id":"chatcmpl-corpus","object":"chat.completion.chunk","created":1700000000,"model":"corpus-model","choices":`
	return head + `[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

` + head + `[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

` + head + `[{"index":0,"delta":{"reasoning_content":"weighing options"},"finish_reason":null}]}

` + head + `[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]},"finish_reason":null}]}

` + head + `[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":null}]}

` + head + `[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

` + head + `[],"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}

data: [DONE]

`
}

// corpusResponsesStream is a Responses upstream stream: the shared corpus start
// frame (terminalStreamCreated), streamed text, a function call whose arguments
// stream in one fragment, and the terminal document that owns usage.
func corpusResponsesStream() string {
	return terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","content":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"Hello"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":""}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"item_id":"fc_1","delta":"{\"q\":\"x\"}"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"corpus-model","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"Hello"}]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}],"usage":{"input_tokens":11,"output_tokens":22,"total_tokens":33}}}

`
}

// corpusAnthropicStream is an Anthropic upstream stream: message_start, a text
// block, a thinking block, a tool_use whose input streams as partial JSON, the
// stop reasons, and message_stop.
func corpusAnthropicStream() string {
	return `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"corpus-model","content":[],"stop_reason":null,"usage":{"input_tokens":11,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"weighing options"}}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1","name":"lookup","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"q\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"x\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":22}}

event: message_stop
data: {"type":"message_stop"}

`
}

type translateCorpusCase struct {
	name   string
	target ir.Protocol // client wire the encoder writes
	source ir.Protocol // upstream wire the decoder reads
	input  func() string
	// sourceHasCreated marks a source start event that carries a real creation
	// timestamp, so the golden pins the mapping instead of normalizing it.
	sourceHasCreated bool
	want             string // pinned canonical frames, one per line
}

func translateCorpus() []translateCorpusCase {
	return []translateCorpusCase{
		{
			name: "chat_to_chat", target: ir.Chat, source: ir.Chat,
			input: corpusChatStream, sourceHasCreated: true,
			want: corpusChatToChatGolden,
		},
		{
			name: "chat_to_responses", target: ir.Responses, source: ir.Chat,
			input: corpusChatStream, sourceHasCreated: true,
			want: corpusChatToResponsesGolden,
		},
		{
			name: "responses_to_chat", target: ir.Chat, source: ir.Responses,
			input: corpusResponsesStream,
			want:  corpusResponsesToChatGolden,
		},
		{
			name: "anthropic_to_chat", target: ir.Chat, source: ir.Anthropic,
			input: corpusAnthropicStream,
			want:  corpusAnthropicToChatGolden,
		},
		{
			name: "chat_to_anthropic", target: ir.Anthropic, source: ir.Chat,
			input: corpusChatStream, sourceHasCreated: true,
			want: corpusChatToAnthropicGolden,
		},
	}
}

// synthesizedID matches an identity the target encoder had to invent because
// the source wire has no equivalent field (Responses item ids, Chat chunk ids).
// Such an id is random per run, so the canonical form collapses it; a source
// that carried a real id keeps it in the golden.
var synthesizedID = regexp.MustCompile(`^(msg|rs|fc|resp)_[0-9a-f]{24}$|^chatcmpl-[0-9a-f]{24}$`)

func normalizeVolatileIDs(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for key, value := range t {
			t[key] = normalizeVolatileIDs(value)
		}
		return t
	case []any:
		for i := range t {
			t[i] = normalizeVolatileIDs(t[i])
		}
		return t
	case string:
		if synthesizedID.MatchString(t) {
			return "<synthesized-id>"
		}
		return t
	default:
		return v
	}
}

// corpusChatToChatGolden is the pinned canonical output of the chat_to_chat pair.
const corpusChatToChatGolden = `|{"choices":[{"delta":{"role":"assistant"},"finish_reason":null,"index":0}],"created":1700000000,"id":"chatcmpl-corpus","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[{"delta":{"content":"Hello"},"finish_reason":null,"index":0}],"created":1700000000,"id":"chatcmpl-corpus","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[{"delta":{"reasoning_content":"weighing options"},"finish_reason":null,"index":0}],"created":1700000000,"id":"chatcmpl-corpus","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"","name":"lookup"},"id":"call_1","index":0,"type":"function"}]},"finish_reason":null,"index":0}],"created":1700000000,"id":"chatcmpl-corpus","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"q\":"},"index":0}]},"finish_reason":null,"index":0}],"created":1700000000,"id":"chatcmpl-corpus","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"x\"}"},"index":0}]},"finish_reason":null,"index":0}],"created":1700000000,"id":"chatcmpl-corpus","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[{"delta":{},"finish_reason":"tool_calls","index":0}],"created":1700000000,"id":"chatcmpl-corpus","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[],"created":1700000000,"id":"chatcmpl-corpus","model":"corpus-model","object":"chat.completion.chunk","usage":{"completion_tokens":22,"prompt_tokens":11,"prompt_tokens_details":{"cache_write_tokens":0,"cached_tokens":0},"total_tokens":33}}
[DONE]`

// corpusChatToResponsesGolden is the pinned canonical output of the chat_to_responses pair.
const corpusChatToResponsesGolden = `response.created|{"response":{"access_programs":null,"created_at":1700000000,"error":null,"id":"chatcmpl-corpus","incomplete_details":null,"instructions":null,"max_output_tokens":null,"metadata":{},"model":"corpus-model","object":"response","output":[],"parallel_tool_calls":true,"previous_response_id":null,"reasoning":null,"status":"in_progress","store":false,"temperature":null,"text":{"format":{"type":"text"}},"tool_choice":"auto","tools":[],"top_p":null,"truncation":"disabled"},"sequence_number":0,"type":"response.created"}
response.in_progress|{"response":{"access_programs":null,"created_at":1700000000,"error":null,"id":"chatcmpl-corpus","incomplete_details":null,"instructions":null,"max_output_tokens":null,"metadata":{},"model":"corpus-model","object":"response","output":[],"parallel_tool_calls":true,"previous_response_id":null,"reasoning":null,"status":"in_progress","store":false,"temperature":null,"text":{"format":{"type":"text"}},"tool_choice":"auto","tools":[],"top_p":null,"truncation":"disabled"},"sequence_number":1,"type":"response.in_progress"}
response.output_item.added|{"item":{"content":[],"id":"\u003csynthesized-id\u003e","role":"assistant","status":"in_progress","type":"message"},"output_index":0,"sequence_number":2,"type":"response.output_item.added"}
response.content_part.added|{"content_index":0,"item_id":"\u003csynthesized-id\u003e","output_index":0,"part":{"annotations":[],"logprobs":[],"text":"","type":"output_text"},"sequence_number":3,"type":"response.content_part.added"}
response.output_text.delta|{"content_index":0,"delta":"Hello","item_id":"\u003csynthesized-id\u003e","logprobs":[],"output_index":0,"sequence_number":4,"type":"response.output_text.delta"}
response.output_text.done|{"content_index":0,"item_id":"\u003csynthesized-id\u003e","logprobs":[],"output_index":0,"sequence_number":5,"text":"Hello","type":"response.output_text.done"}
response.content_part.done|{"content_index":0,"item_id":"\u003csynthesized-id\u003e","output_index":0,"part":{"annotations":[],"logprobs":[],"text":"Hello","type":"output_text"},"sequence_number":6,"type":"response.content_part.done"}
response.output_item.done|{"item":{"content":[{"annotations":[],"logprobs":[],"text":"Hello","type":"output_text"}],"id":"\u003csynthesized-id\u003e","role":"assistant","status":"completed","type":"message"},"output_index":0,"sequence_number":7,"type":"response.output_item.done"}
response.output_item.added|{"item":{"id":"\u003csynthesized-id\u003e","status":"in_progress","summary":[],"type":"reasoning"},"output_index":1,"sequence_number":8,"type":"response.output_item.added"}
response.reasoning_summary_part.added|{"item_id":"\u003csynthesized-id\u003e","output_index":1,"part":{"text":"","type":"summary_text"},"sequence_number":9,"summary_index":0,"type":"response.reasoning_summary_part.added"}
response.reasoning_summary_text.delta|{"delta":"weighing options","item_id":"\u003csynthesized-id\u003e","output_index":1,"sequence_number":10,"summary_index":0,"type":"response.reasoning_summary_text.delta"}
response.reasoning_summary_text.done|{"item_id":"\u003csynthesized-id\u003e","output_index":1,"sequence_number":11,"summary_index":0,"text":"weighing options","type":"response.reasoning_summary_text.done"}
response.reasoning_summary_part.done|{"item_id":"\u003csynthesized-id\u003e","output_index":1,"part":{"text":"weighing options","type":"summary_text"},"sequence_number":12,"summary_index":0,"type":"response.reasoning_summary_part.done"}
response.output_item.done|{"item":{"id":"\u003csynthesized-id\u003e","status":"completed","summary":[{"text":"weighing options","type":"summary_text"}],"type":"reasoning"},"output_index":1,"sequence_number":13,"type":"response.output_item.done"}
response.output_item.added|{"item":{"arguments":"","call_id":"call_1","id":"\u003csynthesized-id\u003e","name":"lookup","status":"in_progress","type":"function_call"},"output_index":2,"sequence_number":14,"type":"response.output_item.added"}
response.function_call_arguments.delta|{"delta":"{\"q\":","item_id":"\u003csynthesized-id\u003e","output_index":2,"sequence_number":15,"type":"response.function_call_arguments.delta"}
response.function_call_arguments.delta|{"delta":"\"x\"}","item_id":"\u003csynthesized-id\u003e","output_index":2,"sequence_number":16,"type":"response.function_call_arguments.delta"}
response.function_call_arguments.done|{"arguments":"{\"q\":\"x\"}","item_id":"\u003csynthesized-id\u003e","output_index":2,"sequence_number":17,"type":"response.function_call_arguments.done"}
response.output_item.done|{"item":{"arguments":"{\"q\":\"x\"}","call_id":"call_1","id":"\u003csynthesized-id\u003e","name":"lookup","status":"completed","type":"function_call"},"output_index":2,"sequence_number":18,"type":"response.output_item.done"}
response.completed|{"response":{"access_programs":null,"created_at":1700000000,"error":null,"id":"chatcmpl-corpus","incomplete_details":null,"instructions":null,"max_output_tokens":null,"metadata":{},"model":"corpus-model","object":"response","output":[{"content":[{"annotations":[],"logprobs":[],"text":"Hello","type":"output_text"}],"id":"\u003csynthesized-id\u003e","role":"assistant","status":"completed","type":"message"},{"id":"\u003csynthesized-id\u003e","status":"completed","summary":[{"text":"weighing options","type":"summary_text"}],"type":"reasoning"},{"arguments":"{\"q\":\"x\"}","call_id":"call_1","id":"\u003csynthesized-id\u003e","name":"lookup","status":"completed","type":"function_call"}],"parallel_tool_calls":true,"previous_response_id":null,"reasoning":null,"status":"completed","store":false,"temperature":null,"text":{"format":{"type":"text"}},"tool_choice":"auto","tools":[],"top_p":null,"truncation":"disabled","usage":{"input_tokens":11,"input_tokens_details":{"cache_write_tokens":0,"cached_tokens":0},"output_tokens":22,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":33}},"sequence_number":19,"type":"response.completed"}`

// corpusResponsesToChatGolden is the pinned canonical output of the responses_to_chat pair.
const corpusResponsesToChatGolden = `|{"choices":[{"delta":{"role":"assistant"},"finish_reason":null,"index":0}],"id":"resp_1","model":"m","object":"chat.completion.chunk"}
|{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"","name":"lookup"},"id":"call_1","index":0,"type":"function"}]},"finish_reason":null,"index":0}],"id":"resp_1","model":"m","object":"chat.completion.chunk"}
|{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"q\":\"x\"}"},"index":0}]},"finish_reason":null,"index":0}],"id":"resp_1","model":"m","object":"chat.completion.chunk"}
|{"choices":[{"delta":{"content":"Hello"},"finish_reason":null,"index":0}],"id":"resp_1","model":"m","object":"chat.completion.chunk"}
|{"choices":[{"delta":{},"finish_reason":"tool_calls","index":0}],"id":"resp_1","model":"m","object":"chat.completion.chunk"}
|{"choices":[],"id":"resp_1","model":"m","object":"chat.completion.chunk","usage":{"completion_tokens":22,"prompt_tokens":11,"prompt_tokens_details":{"cache_write_tokens":0,"cached_tokens":0},"total_tokens":33}}
[DONE]`

// corpusAnthropicToChatGolden is the pinned canonical output of the anthropic_to_chat pair.
const corpusAnthropicToChatGolden = `|{"choices":[{"delta":{"role":"assistant"},"finish_reason":null,"index":0}],"id":"msg_1","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[{"delta":{"content":"Hello"},"finish_reason":null,"index":0}],"id":"msg_1","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[{"delta":{"reasoning_content":"weighing options"},"finish_reason":null,"index":0}],"id":"msg_1","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"","name":"lookup"},"id":"call_1","index":0,"type":"function"}]},"finish_reason":null,"index":0}],"id":"msg_1","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"q\":"},"index":0}]},"finish_reason":null,"index":0}],"id":"msg_1","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"x\"}"},"index":0}]},"finish_reason":null,"index":0}],"id":"msg_1","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[{"delta":{},"finish_reason":"tool_calls","index":0}],"id":"msg_1","model":"corpus-model","object":"chat.completion.chunk"}
|{"choices":[],"id":"msg_1","model":"corpus-model","object":"chat.completion.chunk","usage":{"completion_tokens":22,"prompt_tokens":11,"prompt_tokens_details":{"cache_write_tokens":0,"cached_tokens":0},"total_tokens":33}}
[DONE]`

// corpusChatToAnthropicGolden is the pinned canonical output of the chat_to_anthropic pair.
const corpusChatToAnthropicGolden = `message_start|{"message":{"container":null,"content":[],"diagnostics":null,"id":"chatcmpl-corpus","model":"corpus-model","role":"assistant","stop_details":null,"stop_reason":null,"stop_sequence":null,"type":"message","usage":{"cache_creation":null,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"inference_geo":null,"input_tokens":0,"output_tokens":0,"output_tokens_details":null,"server_tool_use":null,"service_tier":null,"speed":null}},"type":"message_start"}
content_block_start|{"content_block":{"text":"","type":"text"},"index":0,"type":"content_block_start"}
content_block_delta|{"delta":{"text":"Hello","type":"text_delta"},"index":0,"type":"content_block_delta"}
content_block_stop|{"index":0,"type":"content_block_stop"}
content_block_start|{"content_block":{"signature":"","thinking":"","type":"thinking"},"index":1,"type":"content_block_start"}
content_block_delta|{"delta":{"thinking":"weighing options","type":"thinking_delta"},"index":1,"type":"content_block_delta"}
content_block_stop|{"index":1,"type":"content_block_stop"}
content_block_start|{"content_block":{"id":"call_1","input":{},"name":"lookup","type":"tool_use"},"index":2,"type":"content_block_start"}
content_block_delta|{"delta":{"partial_json":"{\"q\":","type":"input_json_delta"},"index":2,"type":"content_block_delta"}
content_block_delta|{"delta":{"partial_json":"\"x\"}","type":"input_json_delta"},"index":2,"type":"content_block_delta"}
content_block_stop|{"index":2,"type":"content_block_stop"}
message_delta|{"delta":{"stop_reason":"tool_use","stop_sequence":null},"type":"message_delta","usage":{"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"input_tokens":11,"output_tokens":22,"output_tokens_details":null,"server_tool_use":null}}
message_stop|{"type":"message_stop"}`

// canonicalFrames reduces an SSE body to one stable line per frame: the event
// name (when present) plus the JSON payload with sorted keys. Normalization is
// limited to fields that are random or wall-clock by construction: a
// target-invented identity and the Chat target's synthesized `created` for a
// source that had no timestamp (Responses/Anthropic starts).
func canonicalFrames(t testing.TB, body string, sourceHasCreated bool) string {
	t.Helper()
	var lines []string
	for _, frame := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		event, data := "", ""
		for _, line := range strings.Split(frame, "\n") {
			if name, ok := strings.CutPrefix(line, "event: "); ok {
				event = name
			}
			if payload, ok := strings.CutPrefix(line, "data: "); ok {
				data = payload
			}
		}
		if data == "" {
			t.Fatalf("frame without data: %q", frame)
		}
		if data == "[DONE]" {
			lines = append(lines, event+"[DONE]")
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(data), &doc); err != nil {
			t.Fatalf("frame %q: %v", data, err)
		}
		if !sourceHasCreated {
			delete(doc, "created")
		}
		normalized, err := json.Marshal(normalizeVolatileIDs(doc))
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, event+"|"+string(normalized))
	}
	return strings.Join(lines, "\n")
}

// runTranslate drives the production facade, not a hand-built codec pair, so a
// change to the decoder/encoder selection is covered too. It returns the body
// and the flush count the client would have seen.
func runTranslate(t *testing.T, tc translateCorpusCase) (string, int) {
	t.Helper()
	var out flushBuffer
	err := TranslateStreamBetween(tc.target, tc.source, &out, strings.NewReader(tc.input()), &out, nil,
		StreamOptions{IncludeUsage: true, FallbackModel: "corpus-model"})
	if err != nil {
		t.Fatalf("%s: %v", tc.name, err)
	}
	return out.buf.String(), out.flushes
}

// TestTranslateStreamCorpus pins every frame of the five translated-stream
// pairs. It is the oracle BenchmarkTranslateStream reuses: a codec change that
// alters a field fails here even when it looks faster.
func TestTranslateStreamCorpus(t *testing.T) {
	for _, tc := range translateCorpus() {
		t.Run(tc.name, func(t *testing.T) {
			body, flushes := runTranslate(t, tc)
			if got := canonicalFrames(t, body, tc.sourceHasCreated); got != tc.want {
				t.Fatalf("frames changed\n got: %s\nwant: %s\nraw: %s", got, tc.want, body)
			}
			if flushes == 0 {
				t.Fatal("translated stream never flushed a frame")
			}
			if tc.sourceHasCreated && tc.target == ir.Chat &&
				!strings.Contains(body, fmt.Sprintf(`"created":%d`, corpusCreated)) {
				t.Fatalf("Chat target dropped the source creation timestamp: %s", body)
			}
		})
	}
}

// flushBuffer counts flushes; the encoders flush once per emitted frame.
type flushBuffer struct {
	buf     bytes.Buffer
	flushes int
}

func (b *flushBuffer) Write(p []byte) (int, error) { return b.buf.Write(p) }
func (b *flushBuffer) Flush()                      { b.flushes++ }

// BenchmarkTranslateStream reports per-frame allocations and bytes for each
// translated pair over the corpus fixture. The golden assertion runs once
// before the timer starts, so a run cannot quote a number for output the
// correctness test would reject.
func BenchmarkTranslateStream(b *testing.B) {
	for _, tc := range translateCorpus() {
		b.Run(tc.name, func(b *testing.B) {
			input := tc.input()
			var out flushBuffer
			err := TranslateStreamBetween(tc.target, tc.source, &out, strings.NewReader(input), &out, nil,
				StreamOptions{IncludeUsage: true, FallbackModel: "corpus-model"})
			if err != nil {
				b.Fatal(err)
			}
			if got := canonicalFrames(b, out.buf.String(), tc.sourceHasCreated); got != tc.want {
				b.Fatalf("frames changed\n got: %s\nwant: %s", got, tc.want)
			}
			frames := strings.Count(out.buf.String(), "\n\n")
			b.ReportAllocs()
			b.SetBytes(int64(len(input)))
			b.ResetTimer()
			for b.Loop() {
				out.buf.Reset()
				out.flushes = 0
				if err := TranslateStreamBetween(tc.target, tc.source, &out, strings.NewReader(input), &out, nil,
					StreamOptions{IncludeUsage: true, FallbackModel: "corpus-model"}); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(frames), "frames/op")
		})
	}
}

// corpusBigChatResponse is one non-streaming Chat completion whose answer text
// is 1 MiB, the payload size a long answer reaches in production.
func corpusBigChatResponse() []byte {
	content := strings.Repeat("x", 1<<20)
	body := `{"id":"chatcmpl-big","object":"chat.completion","created":1700000000,"model":"corpus-model",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"` + content + `"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":22,"total_tokens":33}}`
	return []byte(body)
}

// BenchmarkTranslateResponseOneMiB measures the non-stream Chat->Responses pair
// at 1 MiB. Its oracle is the decoded answer text: embedding a 1 MiB golden
// would prove nothing more than this equality already does.
func BenchmarkTranslateResponseOneMiB(b *testing.B) {
	body := corpusBigChatResponse()
	out, _, err := TranslateResponseBetween(ir.Responses, ir.Chat, body)
	if err != nil {
		b.Fatal(err)
	}
	if got := decodedResponsesText(b, out); len(got) != 1<<20 {
		b.Fatalf("translated answer text = %d bytes, want %d", len(got), 1<<20)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := TranslateResponseBetween(ir.Responses, ir.Chat, body); err != nil {
			b.Fatal(err)
		}
	}
}

// decodedResponsesText returns every output_text value in a Responses document.
func decodedResponsesText(b *testing.B, body []byte) string {
	b.Helper()
	var doc struct {
		Output []struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		b.Fatalf("translated Responses document does not decode: %v", err)
	}
	var sb strings.Builder
	for _, item := range doc.Output {
		for _, part := range item.Content {
			if part.Type == "output_text" {
				sb.WriteString(part.Text)
			}
		}
	}
	return sb.String()
}
