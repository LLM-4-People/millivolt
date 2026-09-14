package format

import (
	"encoding/json"
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

// nestValueStruct wraps value bytes as one more google.protobuf.Value
// struct_value level: Value{struct_value: Struct{fields: {"k": inner}}}, so
// the wire form is field 5 (struct_value) containing a Struct whose single
// field-1 map entry carries key "k" (field 1) and the inner Value (field 2).
// Each wrapper adds one recursion level, exactly like nestValueList.
func nestValueStruct(inner []byte) []byte {
	entry := append(appendVarint([]byte{0x0a}, uint64(len("k"))), 'k') // map entry field 1 = "k"
	entry = append(entry, 0x12)                                        // map entry field 2 = inner Value
	entry = appendVarint(entry, uint64(len(inner)))
	entry = append(entry, inner...)
	strct := append(appendVarint([]byte{0x0a}, uint64(len(entry))), entry...) // Struct.fields = entry
	return append(appendVarint([]byte{0x2a}, uint64(len(strct))), strct...)   // Value.struct_value = Struct
}

// deepValueNested builds the raw Value bytes of a `levels`-deep frame
// around string_value("deep"), one wrapper hop per level.
func deepValueNested(t *testing.T, levels int, wrap func([]byte) []byte) []byte {
	t.Helper()
	value := []byte{0x1a, 0x04, 'd', 'e', 'e', 'p'} // Value.string_value = "deep"
	for i := 0; i < levels; i++ {
		value = wrap(value)
	}
	return value
}

// mapValueEntry wraps Value bytes as a map<string, bytes(Value)> entry
// (field 1 key "arg", field 2 value) exactly like the mcp_args wire.
func mapValueEntry(value []byte) []byte {
	entry := append([]byte{0x0a, 0x03, 'a', 'r', 'g'}, 0x12) // field 1 = "arg"
	entry = appendVarint(entry, uint64(len(value)))
	return append(entry, value...) // field 2 = Value bytes
}

// deepValueFrame builds a Value nested `levels` list_value wrappers deep
// around a string_value("deep"), then wraps it as a map<string, bytes(Value)>
// entry exactly like the mcp_args wire.
func deepValueFrame(t *testing.T, levels int) []byte {
	t.Helper()
	return mapValueEntry(deepValueFrameRaw(t, levels))
}

// deepValueFrameRaw is the raw Value bytes of a `levels`-deep list_value
// frame (without the map-entry wrapper), for the direct decodeProtoValue door.
func deepValueFrameRaw(t *testing.T, levels int) []byte {
	t.Helper()
	return deepValueNested(t, levels, nestValueList)
}

// deepStructFrame is deepValueFrame with struct_value wrapper hops, so the
// struct door counts against the same depth budget as the list door.
func deepStructFrame(t *testing.T, levels int) []byte {
	t.Helper()
	return mapValueEntry(deepValueNested(t, levels, nestValueStruct))
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
	// The exact gate boundary: the deepest Value sits at depth == wrapper
	// levels and the gate is depth > protoValueMaxDepth, so exactly 128
	// wrapper levels decode and 129 fail closed. The pair is hardcoded so
	// silent drift of the constant fails here instead of moving with it.
	if protoValueMaxDepth != 128 {
		t.Fatalf("protoValueMaxDepth = %d; the pinned gate is 128 decode / 129 fail closed", protoValueMaxDepth)
	}
	if _, _, ok := decodeStringValueMapEntry(deepValueFrame(t, 128)); !ok {
		t.Fatal("a 128-level Value frame failed to decode; exactly 128 must pass the gate")
	}
	if _, _, ok := decodeStringValueMapEntry(deepValueFrame(t, 129)); ok {
		t.Fatal("a 129-level Value frame decoded; 129 (protoValueMaxDepth+1) must fail closed")
	}
	// struct_value hops count against the same budget.
	if _, _, ok := decodeStringValueMapEntry(deepStructFrame(t, 128)); !ok {
		t.Fatal("a 128-level struct frame failed to decode; struct hops share the 128 budget")
	}
	if _, _, ok := decodeStringValueMapEntry(deepStructFrame(t, 129)); ok {
		t.Fatal("a 129-level struct frame decoded; struct hops must fail closed at 129")
	}
}

// TestProtoStructValueDoor covers the struct_value door (decodeProtoValue
// case 5), which had no test coverage anywhere: one struct level around
// string_value("deep") decodes through the mcp_args entry door to
// map[string]any{"k": "deep"}.
func TestProtoStructValueDoor(t *testing.T) {
	k, v, ok := decodeStringValueMapEntry(deepStructFrame(t, 1))
	if !ok {
		t.Fatal("a struct_value frame failed to decode")
	}
	if k != "arg" {
		t.Errorf("key = %q, want \"arg\"", k)
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		t.Fatalf("struct_value decoded to %T, want map[string]any", v)
	}
	if got := m["k"]; got != "deep" {
		t.Errorf("struct_value[\"k\"] = %#v, want \"deep\"", got)
	}
}

// mcpArgValueString wraps a string as google.protobuf.Value.string_value
// (field 3), the way an mcp_args entry carries it.
func mcpArgValueString(s string) []byte {
	b := appendTag(nil, 3, wireBytes)
	b = appendVarint(b, uint64(len(s)))
	return append(b, s...)
}

// mcpArgValueBool wraps a bool as google.protobuf.Value.bool_value (field 4).
func mcpArgValueBool(v bool) []byte {
	b := appendTag(nil, 4, wireVarint)
	if v {
		return append(b, 1)
	}
	return append(b, 0)
}

// mcpArgsEntry builds one map<string, bytes(Value)> entry exactly like the
// mcp_args wire: field 1 key, field 2 Value bytes. An empty key is the
// documented hostile shape decodeMapEntry must refuse.
func mcpArgsEntry(key string, value []byte) []byte {
	entry := appendTag(nil, 1, wireBytes)
	entry = appendVarint(entry, uint64(len(key)))
	entry = append(entry, key...)
	entry = appendTag(entry, 2, wireBytes)
	entry = appendVarint(entry, uint64(len(value)))
	return append(entry, value...)
}

// TestParseMcpArgsSkipsHostileArgEntries pins the consumer leg of the
// mcp_args map: parseMcpArgs must keep every good entry and silently skip
// hostile ones (an empty-key entry decodes ok=false and is dropped), so one
// malformed entry can neither blank the whole argument map nor smuggle an
// empty-named key into it.
func TestParseMcpArgsSkipsHostileArgEntries(t *testing.T) {
	msg := appendString(nil, fMcpArgsName, "lookup")
	msg = appendString(msg, fMcpArgsToolCallID, "call-9")
	msg = appendMessage(msg, fMcpArgsArgs, mcpArgsEntry("file", mcpArgValueString("main.go")))
	msg = appendMessage(msg, fMcpArgsArgs, mcpArgsEntry("dry_run", mcpArgValueBool(true)))
	msg = appendMessage(msg, fMcpArgsArgs, mcpArgsEntry("", mcpArgValueString("hostile")))
	outer := appendMessage(nil, 1, msg)
	fields, err := parseProtoFields(outer)
	if err != nil {
		t.Fatalf("fixture mcp_args frame: %v", err)
	}
	if len(fields) != 1 {
		t.Fatalf("fixture frame fields = %d, want 1", len(fields))
	}
	call := parseMcpArgs(&fields[0])
	if call.Name != "lookup" || call.CallID != "call-9" {
		t.Fatalf("name = %q callID = %q, want lookup/call-9", call.Name, call.CallID)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Args), &args); err != nil {
		t.Fatalf("Args = %q: %v", call.Args, err)
	}
	if len(args) != 2 || args["file"] != "main.go" || args["dry_run"] != true {
		t.Fatalf("args = %#v, want the two good entries only (hostile entry skipped)", args)
	}
}
