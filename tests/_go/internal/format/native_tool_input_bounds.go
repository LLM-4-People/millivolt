package format

// Regression tests for the aggregate retained-initial budget on open Anthropic
// tool_use blocks. Each content_block_start may carry up to one SSE line
// (sseLineBufMax) of input JSON that the translator retains until the block
// stops; 1024 open blocks used to retain ~1 GiB. The budget fails the
// translation closed before an open start can push the retained total over
// its bound, while ordinary starts, deltas and stop-released blocks are
// unchanged.

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// nativeToolStartLine renders one content_block_start event line whose input
// is the given JSON value.
func nativeToolStartLine(index int, input string) string {
	if input == "" {
		return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"t%d","name":"n"}}`, index, index)
	}
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"t%d","name":"n","input":%s}}`, index, index, input)
}

// TestNativeToolInitialRetentionBound drives 128 open blocks of 819 KiB input
// each: enough to retain over 100 MiB before the fix. The translator must fail
// with the stable budget error and stay well under the hard allocation
// ceiling, without writing the over-budget start.
func TestNativeToolInitialRetentionBound(t *testing.T) {
	const (
		blocks = 128
		input  = 819 * 1024
		// The retained budget plus one decoded line, the scanner's reusable
		// buffer and the emitted start frames stay far below this ceiling; the
		// unbounded path allocated ~219 MiB for the same fixture.
		allocMax = 16 << 20
	)
	value := strings.Repeat("x", input)
	var stream strings.Builder
	stream.Grow(blocks * (input + 160))
	stream.WriteString("data: " + `{"type":"message_start","message":{"id":"m","model":"m"}}` + "\n\n")
	for i := 0; i < blocks; i++ {
		stream.WriteString("data: " + nativeToolStartLine(i, `"`+value+`"`) + "\n\n")
	}
	raw := stream.String()

	var out strings.Builder
	var err error
	alloc := allocDelta(func() {
		err = StreamToOpenAI(&out, strings.NewReader(raw), nil, nil)
	})
	if !errors.Is(err, errNativeToolInitialBytes) {
		t.Fatalf("retained tool input error = %v, want %v", err, errNativeToolInitialBytes)
	}
	if out.Len() > 1<<20 {
		t.Fatalf("over-budget stream emitted %d bytes, want a bounded prefix", out.Len())
	}
	if alloc > allocMax {
		t.Fatalf("%d-block open stream allocated %d bytes, want <= %d", blocks, alloc, allocMax)
	}
	t.Logf("tool initial over-bound: input=%d blocks=%d alloc=%d", stream.Len(), blocks, alloc)
}

// TestNativeToolInitialBudgetPreservesNormalStream is the accepted control:
// a small inline start still emits its arguments at stop, streamed deltas
// still pass through, and a stopped block releases its retained bytes so the
// next block at the same index starts with a fresh budget.
func TestNativeToolInitialBudgetPreservesNormalStream(t *testing.T) {
	stream := "data: " + `{"type":"message_start","message":{"id":"m","model":"m"}}` + "\n\n" +
		"data: " + nativeToolStartLine(0, `{"city":"NY"}`) + "\n\n" +
		"data: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
		"data: " + nativeToolStartLine(0, "") + "\n\n" +
		"data: " + `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}` + "\n\n" +
		"data: " + `{"type":"content_block_stop","index":0}` + "\n\n" +
		"data: " + `{"type":"message_stop"}` + "\n\n"
	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream), nil, nil); err != nil {
		t.Fatalf("normal tool stream failed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"arguments":"{\"city\":\"NY\"}"`) {
		t.Fatalf("inline start arguments missing from %s", got)
	}
	if !strings.Contains(got, `"arguments":"{\"x\":"`) {
		t.Fatalf("streamed delta arguments missing from %s", got)
	}
}

// TestNativeToolInitialBudgetReleasesStoppedBlocks proves the budget tracks
// only open blocks: six sequential 900 KiB starts exceed the total budget in
// aggregate, but each block's initial is released at its stop.
func TestNativeToolInitialBudgetReleasesStoppedBlocks(t *testing.T) {
	const blocks = 6
	value := strings.Repeat("x", 900*1024)
	var stream strings.Builder
	stream.Grow(blocks * (len(value) + 200))
	stream.WriteString("data: " + `{"type":"message_start","message":{"id":"m","model":"m"}}` + "\n\n")
	for i := 0; i < blocks; i++ {
		stream.WriteString("data: " + nativeToolStartLine(i, `"`+value+`"`) + "\n\n")
		stream.WriteString("data: " + fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i) + "\n\n")
	}
	stream.WriteString("data: " + `{"type":"message_stop"}` + "\n\n")
	var out strings.Builder
	if err := StreamToOpenAI(&out, strings.NewReader(stream.String()), nil, nil); err != nil {
		t.Fatalf("sequential stopped blocks exhausted the open-block budget: %v", err)
	}
	if n := strings.Count(out.String(), `"arguments":"\"`); n != blocks {
		t.Fatalf("emitted argument payloads = %d, want %d", n, blocks)
	}
}
