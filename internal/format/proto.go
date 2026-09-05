package format

// Minimal, allocation-conscious Protocol Buffers wire-format primitives.
//
// The Cursor upstream speaks Connect-RPC carrying protobuf messages. Rather
// than pull in a protobuf runtime (and a .proto codegen step) for a handful of
// fields, we hand-encode/decode the exact wire format. Only the two wire types
// the Cursor schema uses are implemented: varint (0) and length-delimited (2).
//
// These are internal, non-tunable implementation details.

import (
	"fmt"
	"math"
)

const (
	wireVarint = 0 // int32/int64/bool/enum/uint32/uint64
	wireBytes  = 2 // string/bytes/embedded message
)

// appendVarint encodes v as a base-128 varint (little-endian groups, 7 bits
// per byte, continuation bit set on all but the last).
func appendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// appendTag writes the field key: (fieldNumber << 3) | wireType.
func appendTag(dst []byte, field, wire int) []byte {
	return appendVarint(dst, uint64(field<<3|wire))
}

// appendString writes a length-delimited string field. Empty strings are
// skipped (proto3 omits zero values, and an empty length-delimited field is
// both wasteful and, for some servers, a parse distinction).
func appendString(dst []byte, field int, s string) []byte {
	if s == "" {
		return dst
	}
	dst = appendTag(dst, field, wireBytes)
	dst = appendVarint(dst, uint64(len(s)))
	return append(dst, s...)
}

// appendBytes writes a length-delimited bytes field (empty allowed: a repeated
// history entry may legitimately be an empty JSON blob placeholder).
func appendBytes(dst []byte, field int, b []byte) []byte {
	dst = appendTag(dst, field, wireBytes)
	dst = appendVarint(dst, uint64(len(b)))
	return append(dst, b...)
}

// appendMessage writes an embedded message field (length-delimited).
func appendMessage(dst []byte, field int, msg []byte) []byte {
	return appendBytes(dst, field, msg)
}

// appendBool writes a varint bool field, omitting false (proto3 zero value).
func appendBool(dst []byte, field int, b bool) []byte {
	if !b {
		return dst
	}
	dst = appendTag(dst, field, wireVarint)
	return append(dst, 1)
}

// appendInt32 writes a varint int32 field, omitting zero (proto3 zero value).
func appendInt32(dst []byte, field int, v int32) []byte {
	if v == 0 {
		return dst
	}
	dst = appendTag(dst, field, wireVarint)
	return appendVarint(dst, uint64(uint32(v)))
}

// appendUint32 encodes a uint32 field as a varint without narrowing the value
// through int32 (correlation ids from the server are uint64; a truncating
// cast would echo a wrong id and break the reply correlation).
func appendUint32(dst []byte, field int, v uint32) []byte {
	if v == 0 {
		return dst
	}
	dst = appendTag(dst, field, wireVarint)
	return appendVarint(dst, uint64(v))
}

// protoField is one decoded field: its number, wire type, and payload.
// For wireBytes, raw holds the length-delimited bytes (string/bytes/message).
// For wireVarint, num holds the decoded varint value.
type protoField struct {
	num  int
	wire int
	raw  []byte
	n    uint64
}

// parseProtoFields decodes a protobuf message into its top-level fields.
// Unknown fields are tolerated (returned) so callers can ignore them; only a
// structurally malformed buffer (truncated varint, overrun length) errors.
// The returned slices alias into msg - callers must not retain them past msg's
// lifetime, which is fine for our single-pass decode.
func parseProtoFields(msg []byte) ([]protoField, error) {
	var fields []protoField
	i := 0
	for i < len(msg) {
		key, n := consumeVarint(msg[i:])
		if n <= 0 {
			return nil, fmt.Errorf("protobuf: bad field key at offset %d", i)
		}
		i += n
		num := int(key >> 3)
		wire := int(key & 0x7)
		f := protoField{num: num, wire: wire}
		switch wire {
		case wireVarint:
			v, m := consumeVarint(msg[i:])
			if m <= 0 {
				return nil, fmt.Errorf("protobuf: bad varint field %d at offset %d", num, i)
			}
			i += m
			f.n = v
		case wireBytes:
			l, m := consumeVarint(msg[i:])
			if m <= 0 {
				return nil, fmt.Errorf("protobuf: bad length field %d at offset %d", num, i)
			}
			i += m
			if l > uint64(len(msg)-i) {
				return nil, fmt.Errorf("protobuf: field %d length %d overruns buffer", num, l)
			}
			f.raw = msg[i : i+int(l)]
			i += int(l)
		default:
			// We only need varint + length-delimited for the Cursor schema; a
			// fixed32/64/group field means we're misaligned or the schema grew
			// a type we don't model. Fail closed rather than mis-parse.
			return nil, fmt.Errorf("protobuf: unsupported wire type %d for field %d", wire, num)
		}
		fields = append(fields, f)
	}
	return fields, nil
}

// consumeVarint reads a base-128 varint from b, returning the value and the
// number of bytes consumed (<=0 on malformed/truncated input).
func consumeVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		c := b[i]
		if c < 0x80 {
			// The 10th byte may only carry 1 bit (values fit in 64 bits).
			if i == 9 && c > 1 {
				return 0, -1
			}
			return v | uint64(c)<<uint(7*i), i + 1
		}
		v |= uint64(c&0x7f) << uint(7*i)
	}
	return 0, -1
}

// firstField returns the first field with the given number, or nil.
func firstField(fields []protoField, num int) *protoField {
	for i := range fields {
		if fields[i].num == num {
			return &fields[i]
		}
	}
	return nil
}

// subFields parses an embedded message field's bytes into its fields.
func subFields(f *protoField) ([]protoField, error) {
	if f == nil || f.wire != wireBytes {
		return nil, fmt.Errorf("protobuf: field is not an embedded message")
	}
	return parseProtoFields(f.raw)
}

// decodeStringValueMapEntry decodes a map<string, bytes> entry (field 1 = key
// string, field 2 = value bytes), then decodes the value bytes as a
// google.protobuf.Value shim. Returns the key and the decoded Go value.
func decodeStringValueMapEntry(entry []byte) (string, any, bool) {
	fields, err := parseProtoFields(entry)
	if err != nil {
		return "", nil, false
	}
	var key string
	var valBytes []byte
	for _, f := range fields {
		if f.num == 1 {
			key = string(f.raw)
		} else if f.num == 2 {
			valBytes = f.raw
		}
	}
	if key == "" {
		return "", nil, false
	}
	v, ok := decodeProtoValue(valBytes)
	return key, v, ok
}

// decodeProtoValue decodes the google.protobuf.Value shim:
// oneof kind { null=1, number=2 (fixed64), string=3, bool=4, struct=5, list=6 }.
// Falls back to the raw bytes as a UTF-8 string when it isn't a Value envelope
// (some servers send plain text). Returns ok=false only on a structurally
// malformed buffer.
func decodeProtoValue(b []byte) (any, bool) {
	if len(b) == 0 {
		return nil, true
	}
	fields, err := parseProtoFieldsLenient(b)
	if err != nil {
		// Not a Value envelope - treat as raw UTF-8 text (cursor2api does this).
		return string(b), true
	}
	for _, f := range fields {
		switch f.num {
		case 3: // string_value
			return string(f.raw), true
		case 4: // bool_value
			return f.n != 0, true
		case 2: // number_value (fixed64) - our parser only does varint/bytes, so
			// a fixed64 lands in raw as the 8 little-endian bytes. Best-effort.
			if len(f.raw) == 8 {
				bits := uint64(f.raw[0]) | uint64(f.raw[1])<<8 | uint64(f.raw[2])<<16 | uint64(f.raw[3])<<24 |
					uint64(f.raw[4])<<32 | uint64(f.raw[5])<<40 | uint64(f.raw[6])<<48 | uint64(f.raw[7])<<56
				return math.Float64frombits(bits), true
			}
			return f.raw, true
		case 5: // struct_value: map<string, Value>
			if m, ok := decodeProtoStruct(f.raw); ok {
				return m, true
			}
			return nil, true
		case 6: // list_value: repeated Value
			if arr, ok := decodeProtoList(f.raw); ok {
				return arr, true
			}
			return nil, true
		case 1: // null_value
			return nil, true
		}
	}
	return string(b), true
}

// decodeProtoStruct decodes a Struct (map<string, Value> at field 1).
func decodeProtoStruct(b []byte) (map[string]any, bool) {
	fields, err := parseProtoFields(b)
	if err != nil {
		return nil, false
	}
	out := map[string]any{}
	for _, f := range fields {
		if f.num != 1 {
			continue
		}
		// each entry: map<string, Value> with field 1 = key, field 2 = Value
		entry, err := parseProtoFields(f.raw)
		if err != nil {
			continue
		}
		var k string
		var vb []byte
		for _, e := range entry {
			if e.num == 1 {
				k = string(e.raw)
			} else if e.num == 2 {
				vb = e.raw
			}
		}
		if k != "" {
			v, _ := decodeProtoValue(vb)
			out[k] = v
		}
	}
	return out, true
}

// decodeProtoList decodes a ListValue (repeated Value at field 1).
func decodeProtoList(b []byte) ([]any, bool) {
	fields, err := parseProtoFields(b)
	if err != nil {
		return nil, false
	}
	var out []any
	for _, f := range fields {
		if f.num != 1 {
			continue
		}
		v, _ := decodeProtoValue(f.raw)
		out = append(out, v)
	}
	return out, true
}

// parseProtoFieldsLenient is parseProtoFields but tolerates a fixed64 (wire 1)
// field by reading 8 bytes into raw, so google.protobuf.Value number fields
// decode. All other wire types behave as parseProtoFields (fail closed).
func parseProtoFieldsLenient(msg []byte) ([]protoField, error) {
	var fields []protoField
	i := 0
	for i < len(msg) {
		key, n := consumeVarint(msg[i:])
		if n <= 0 {
			return nil, fmt.Errorf("protobuf: bad field key at offset %d", i)
		}
		i += n
		num := int(key >> 3)
		wire := int(key & 0x7)
		f := protoField{num: num, wire: wire}
		switch wire {
		case wireVarint:
			v, m := consumeVarint(msg[i:])
			if m <= 0 {
				return nil, fmt.Errorf("protobuf: bad varint field %d", num)
			}
			i += m
			f.n = v
		case 1: // fixed64 (google.protobuf.Value number_value)
			if len(msg)-i < 8 {
				return nil, fmt.Errorf("protobuf: truncated fixed64 field %d", num)
			}
			f.raw = msg[i : i+8]
			i += 8
		case wireBytes:
			l, m := consumeVarint(msg[i:])
			if m <= 0 {
				return nil, fmt.Errorf("protobuf: bad length field %d", num)
			}
			i += m
			if l > uint64(len(msg)-i) {
				return nil, fmt.Errorf("protobuf: field %d length %d overruns buffer", num, l)
			}
			f.raw = msg[i : i+int(l)]
			i += int(l)
		case 5: // fixed32
			if len(msg)-i < 4 {
				return nil, fmt.Errorf("protobuf: truncated fixed32 field %d", num)
			}
			f.raw = msg[i : i+4]
			i += 4
		default:
			return nil, fmt.Errorf("protobuf: unsupported wire type %d for field %d", wire, num)
		}
		fields = append(fields, f)
	}
	return fields, nil
}
