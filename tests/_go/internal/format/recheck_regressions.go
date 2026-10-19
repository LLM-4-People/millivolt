package format

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// signalBuffer records translator output and closes done when the terminal
// [DONE] frame is written, so the tests wait on a channel instead of polling.
type signalBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{}
	once sync.Once
}

func newSignalBuffer() *signalBuffer { return &signalBuffer{done: make(chan struct{})} }

func (b *signalBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	if bytes.Contains(p, []byte("data: [DONE]")) {
		b.once.Do(func() { close(b.done) })
	}
	b.mu.Unlock()
	return n, err
}

func (b *signalBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAnthropicErrorEventTerminatesWithoutEOF pins PRO-6: a valid provider
// error event closes the translated stream with exactly one [DONE] immediately,
// even while the upstream connection stays open.
func TestAnthropicErrorEventTerminatesWithoutEOF(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	out := newSignalBuffer()
	returned := make(chan error, 1)
	go func() { returned <- StreamToOpenAI(out, pr, nil, nil) }()
	for _, frame := range []string{
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"m\"}}\n\n",
		"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"late\"}}\n\n",
	} {
		if _, err := io.WriteString(pw, frame); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-out.done:
	case <-time.After(2 * time.Second):
		t.Fatalf("terminal error did not emit [DONE] while the upstream stayed open: %q", out.String())
	}
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("translator did not return after its terminal error event")
	}
	if n := strings.Count(out.String(), "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] count = %d, want 1: %q", n, out.String())
	}
	if n := strings.Count(out.String(), `"error"`); n != 1 {
		t.Fatalf("error frame count = %d, want 1: %q", n, out.String())
	}
}

// TestAnthropicMessageStopTerminatesWithoutEOF is the PRO-6 control.
func TestAnthropicMessageStopTerminatesWithoutEOF(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	out := newSignalBuffer()
	returned := make(chan error, 1)
	go func() { returned <- StreamToOpenAI(out, pr, nil, nil) }()
	for _, frame := range []string{
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"m\"}}\n\n",
		"data: {\"type\":\"message_stop\"}\n\n",
	} {
		if _, err := io.WriteString(pw, frame); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-out.done:
	case <-time.After(2 * time.Second):
		t.Fatal("message_stop did not emit [DONE] while the upstream stayed open")
	}
	select {
	case err := <-returned:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("translator did not return after message_stop")
	}
	if n := strings.Count(out.String(), "data: [DONE]"); n != 1 {
		t.Fatalf("[DONE] count = %d, want 1", n)
	}
}

// TestAnthropicRejectsNonObjectToolInput pins PRO-7: Anthropic's tool_use.input
// is an object, so non-object OpenAI arguments are rejected locally while the
// documented empty/null normalization still produces an object.
func TestAnthropicRejectsNonObjectToolInput(t *testing.T) {
	request := func(args string) []byte {
		return []byte(`{"model":"m","messages":[{"role":"user","content":"q"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function",` +
			`"function":{"name":"lookup","arguments":` + args + `}}]}]}`)
	}
	for _, args := range []string{`""`, `"null"`, `"{}"`, `"{\"k\":\"v\"}"`} {
		if _, err := TranslateRequest(request(args), 128, ""); err != nil {
			t.Fatalf("arguments %s rejected: %v", args, err)
		}
	}
	for _, args := range []string{`"[]"`, `"\"text\""`, `"1"`, `"true"`} {
		if _, err := TranslateRequest(request(args), 128, ""); err == nil {
			t.Fatalf("non-object arguments %s were forwarded", args)
		}
	}
}

// TestParseMcpArgsAlwaysEmitsJSONObject pins PRO-4: a zero-argument Cursor tool
// call carries "{}", never the invalid empty string, while real maps are
// unchanged.
func TestParseMcpArgsAlwaysEmitsJSONObject(t *testing.T) {
	noArgs := appendString(nil, fMcpArgsName, "get_time")
	noArgs = appendString(noArgs, fMcpArgsToolCallID, "call-1")
	call := parseMcpArgs(&protoField{wire: wireBytes, raw: noArgs})
	if call.Name != "get_time" || call.Args != "{}" {
		t.Fatalf("no-argument call = name %q args %q", call.Name, call.Args)
	}

	withArgs := appendString(nil, fMcpArgsName, "lookup")
	withArgs = appendString(withArgs, fMcpArgsToolCallID, "call-2")
	withArgs = appendMessage(withArgs, fMcpArgsArgs, mcpArgsEntry("k", mcpArgValueString("v")))
	call = parseMcpArgs(&protoField{wire: wireBytes, raw: withArgs})
	if call.Args != `{"k":"v"}` {
		t.Fatalf("argument map = %q", call.Args)
	}
}

// TestAnthropicEventTypeIsBlockScoped pins ADV-D: a bare event block and an
// event-after-data block cannot leak their type into the next data-only block,
// whose payload type is the fallback.
func TestAnthropicEventTypeIsBlockScoped(t *testing.T) {
	for _, tc := range []struct {
		name           string
		eventAfterData bool
	}{
		{"bare event block", false},
		{"event after data", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := []string{`data: {"type":"message_start","message":{"id":"m","model":"m"}}`}
			if tc.eventAfterData {
				blocks = append(blocks, `event: message_start`)
			}
			blocks = append(blocks,
				``,
				`event: message_stop`,
				``,
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"must survive"}}`,
				``,
				`data: {"type":"message_stop"}`,
				``,
			)
			var out strings.Builder
			if err := StreamToOpenAI(&out, strings.NewReader(strings.Join(blocks, "\n")), nil, nil); err != nil {
				t.Fatalf("StreamToOpenAI = %v", err)
			}
			got := out.String()
			if !strings.Contains(got, `"content":"must survive"`) {
				t.Fatalf("data-only block after an event-only block was misrouted: %q", got)
			}
			if n := strings.Count(got, "data: [DONE]"); n != 1 {
				t.Fatalf("[DONE] count = %d, want 1: %q", n, got)
			}
		})
	}
}
