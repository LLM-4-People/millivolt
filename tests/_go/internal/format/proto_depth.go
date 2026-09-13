package format

import (
	"testing"
)

// nestValueList wraps value bytes as one more google.protobuf.Value
// list_value level: Value{list_value: ListValue{values: [inner]}}, so the
// wire form is field 6 (list_value) containing a ListValue whose field 1
// (values) entry is the inner Value. Each wrapper adds one recursion level.
func nestValueList(inner []byte) []byte {
	list := append(appendVarint([]byte{0x0a}, uint64(len(inner))), inner...) // ListValue.values = inner
	return append(appendVarint([]byte{0x32}, uint64(len(list))), list...)    // Value.list_value = list
}

// deepValueFrame builds a Value nested `levels` list_value wrappers deep
// around a string_value("deep"), then wraps it as a map<string, bytes(Value)>
// entry (field 1 key, field 2 value) exactly like the mcp_args wire.
func deepValueFrame(t *testing.T, levels int) []byte {
	t.Helper()
	value := []byte{0x1a, 0x04, 'd', 'e', 'e', 'p'} // Value.string_value = "deep"
	for i := 0; i < levels; i++ {
		value = nestValueList(value)
	}
	entry := append([]byte{0x0a, 0x03, 'a', 'r', 'g'}, 0x12) // field 1 = "arg"
	entry = appendVarint(entry, uint64(len(value)))
	return append(entry, value...) // field 2 = Value bytes
}

// TestProtoValueDepthBound pins the fail-closed depth bound on the
// google.protobuf.Value shim. Before the bound, the decode trio recursed
// once per wire level with no limit: the wire costs ~4-5 bytes per level, so
// a single crafted frame under the connect frame cap encoded millions of
// levels and exhausted the goroutine stack - a fatal, unrecoverable process
// crash from one upstream frame (the in-process test asserts the CAP
// behavior, not the crash). A frame nested past the bound must fail closed
// (ok=false, so the mcp_args entry is dropped) and a frame within the bound
// must still decode to the correct nested value.
func TestProtoValueDepthBound(t *testing.T) {
	// Within the bound: 64 levels decode to the correctly nested value.
	if _, v, ok := decodeStringValueMapEntry(deepValueFrame(t, 64)); !ok {
		t.Fatal("a 64-level Value frame failed to decode")
	} else {
		for i := 0; i < 64; i++ {
			list, isList := v.([]any)
			if !isList || len(list) != 1 {
				t.Fatalf("level %d unwraps to %T, want a one-element list", 64-i, v)
			}
			v = list[0]
		}
		if v != "deep" {
			t.Fatalf("innermost value = %v, want \"deep\"", v)
		}
	}
	// Past the bound: fail closed, never decode, never recurse.
	if _, _, ok := decodeStringValueMapEntry(deepValueFrame(t, 400)); ok {
		t.Fatal("a 400-level Value frame decoded; the depth bound must fail closed")
	}
	if _, ok := decodeProtoValue(deepValueFrameRaw(t, 400), 0); ok {
		t.Fatal("decodeProtoValue must reject a too-deep frame")
	}
}

// deepValueFrameRaw is the raw Value bytes of a `levels`-deep frame (without
// the map-entry wrapper), for the direct decodeProtoValue door.
func deepValueFrameRaw(t *testing.T, levels int) []byte {
	t.Helper()
	value := []byte{0x1a, 0x04, 'd', 'e', 'e', 'p'}
	for i := 0; i < levels; i++ {
		value = nestValueList(value)
	}
	return value
}
