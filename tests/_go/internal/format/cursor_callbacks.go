package format

// Exact-size Cursor callback envelopes: the reply encoders size the nested
// protobuf message first and write the payload once, and the frame writer
// emits the 5-byte header separately instead of copying the payload into a
// second full-size buffer.

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// legacyWrapExecClient is the previous wrapExecClient implementation, kept as
// the byte-for-byte reference for the exact-size encoder.
func legacyWrapExecClient(id uint64, execID []byte, field int, payload []byte) []byte {
	var ex []byte
	ex = appendUint32(ex, fExecMessageID, uint32(id))
	if len(execID) > 0 {
		ex = appendBytes(ex, fExecMessageExecID, execID)
	}
	ex = appendMessage(ex, field, payload)
	return appendMessage(nil, fAgentClientMessageExecClientMessage, ex)
}

func legacyEncodeKvGetBlobResult(id uint64, blobData []byte) []byte {
	var res []byte
	if len(blobData) > 0 {
		res = appendBytes(res, fGetBlobResultBlobData, blobData)
	}
	var kv []byte
	kv = appendUint32(kv, fKvMessageID, uint32(id))
	kv = appendMessage(kv, fKvClientGetBlobResult, res)
	return appendMessage(nil, fAgentClientMessageKvClientMessage, kv)
}

func legacyEncodeMcpResult(id uint64, execID []byte, result string, isError bool) []byte {
	var mcpResult []byte
	if isError {
		var mcpErr []byte
		mcpErr = appendString(mcpErr, 1, result)
		mcpResult = appendMessage(nil, 2, mcpErr)
	} else {
		text := appendString(nil, 1, result)
		item := appendMessage(nil, 1, text)
		var success []byte
		success = appendMessage(success, 1, item)
		mcpResult = appendMessage(nil, 1, success)
	}
	return legacyWrapExecClient(id, execID, fExecClientMcpResult, mcpResult)
}

func legacyEncodeRequestContextResult(id uint64, execID []byte) []byte {
	success := appendMessage(nil, fRequestContextSuccessInner, nil)
	result := appendMessage(nil, fRequestContextResultSucc, success)
	return legacyWrapExecClient(id, execID, fExecClientRequestContext, result)
}

// legacyFrame is the previous writeFramed allocation: one header+payload copy.
func legacyFrame(payload []byte) []byte {
	buf := make([]byte, connectFrameHeader+len(payload))
	binary.BigEndian.PutUint32(buf[1:], uint32(len(payload)))
	copy(buf[connectFrameHeader:], payload)
	return buf
}

func captureFramed(t *testing.T, payload []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := (&streamWriter{w: &out}).writeFramed(payload); err != nil {
		t.Fatalf("writeFramed: %v", err)
	}
	return out.Bytes()
}

// TestCursorCallbackWireBytesIdentical pins every callback encoder and the
// frame writer to the previous wire bytes for normal and edge shapes.
func TestCursorCallbackWireBytesIdentical(t *testing.T) {
	id := uint64(0x0102030405060708)
	execID := []byte("exec-9")
	for _, tc := range []struct {
		name      string
		payload   []byte
		legacyPay []byte
	}{
		{"get blob present", encodeKvGetBlobResult(id, []byte("blob")), legacyEncodeKvGetBlobResult(id, []byte("blob"))},
		{"get blob absent", encodeKvGetBlobResult(id, nil), legacyEncodeKvGetBlobResult(id, nil)},
		{"get blob zero id", encodeKvGetBlobResult(0, []byte("x")), legacyEncodeKvGetBlobResult(0, []byte("x"))},
		{"mcp success", encodeMcpResult(id, execID, "result", false), legacyEncodeMcpResult(id, execID, "result", false)},
		{"mcp success empty", encodeMcpResult(id, execID, "", false), legacyEncodeMcpResult(id, execID, "", false)},
		{"mcp error", encodeMcpResult(id, execID, "boom", true), legacyEncodeMcpResult(id, execID, "boom", true)},
		{"mcp error empty", encodeMcpResult(0, nil, "", true), legacyEncodeMcpResult(0, nil, "", true)},
		{"request context", encodeRequestContextResult(id, execID), legacyEncodeRequestContextResult(id, execID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !bytes.Equal(tc.payload, tc.legacyPay) {
				t.Fatalf("payload bytes changed:\n got %x\nwant %x", tc.payload, tc.legacyPay)
			}
			got := captureFramed(t, tc.payload)
			want := legacyFrame(tc.legacyPay)
			if !bytes.Equal(got, want) {
				t.Fatalf("framed bytes changed:\n got %x\nwant %x", got, want)
			}
		})
	}
}

// TestCursorCallbackLargePayloadAllocation proves a 4 MiB blob and a 1 MiB
// tool result produce identical wire bytes with one payload copy instead of
// the old 4-7 transient copies.
func TestCursorCallbackLargePayloadAllocation(t *testing.T) {
	const blobBytes = 4 << 20
	blob := bytes.Repeat([]byte("b"), blobBytes)
	execID := []byte("exec-callback")

	oldBlobAlloc := allocDelta(func() {
		_ = legacyFrame(legacyEncodeKvGetBlobResult(7, blob))
	})
	newBlobAlloc := allocDelta(func() {
		_ = (&streamWriter{w: io.Discard}).writeFramed(encodeKvGetBlobResult(7, blob))
	})
	t.Logf("blob bytes=%d old alloc=%d new alloc=%d", blobBytes, oldBlobAlloc, newBlobAlloc)
	if !bytes.Equal(captureFramed(t, encodeKvGetBlobResult(7, blob)), legacyFrame(legacyEncodeKvGetBlobResult(7, blob))) {
		t.Fatal("large get_blob reply wire bytes changed")
	}
	if newBlobAlloc > uint64(blobBytes)+1<<20 {
		t.Fatalf("large get_blob reply allocated %d bytes, want about one %d-byte payload copy", newBlobAlloc, blobBytes)
	}
	if oldBlobAlloc < 2*newBlobAlloc {
		t.Fatalf("old get_blob path allocated %d bytes versus %d new; fixture no longer exposes the copy amplification", oldBlobAlloc, newBlobAlloc)
	}

	result := string(bytes.Repeat([]byte("r"), 1<<20))
	oldResultAlloc := allocDelta(func() {
		_ = legacyFrame(legacyEncodeMcpResult(9, execID, result, false))
	})
	newResultAlloc := allocDelta(func() {
		_ = (&streamWriter{w: io.Discard}).writeFramed(encodeMcpResult(9, execID, result, false))
	})
	t.Logf("result bytes=%d old alloc=%d new alloc=%d", len(result), oldResultAlloc, newResultAlloc)
	if !bytes.Equal(captureFramed(t, encodeMcpResult(9, execID, result, false)), legacyFrame(legacyEncodeMcpResult(9, execID, result, false))) {
		t.Fatal("large mcp_result reply wire bytes changed")
	}
	if newResultAlloc > uint64(len(result))+1<<20 {
		t.Fatalf("large mcp_result reply allocated %d bytes, want about one %d-byte result copy", newResultAlloc, len(result))
	}
	if oldResultAlloc < 2*newResultAlloc {
		t.Fatalf("old mcp_result path allocated %d bytes versus %d new; fixture no longer exposes the copy amplification", oldResultAlloc, newResultAlloc)
	}
}
