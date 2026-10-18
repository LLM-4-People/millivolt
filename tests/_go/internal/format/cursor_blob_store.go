package format

// Bounded per-run KV blob retention and server-pushed set_blob propagation.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testBlobID returns a valid raw 32-byte sha256 blob key for label, the only
// shape KVBlobStore.put accepts.
func testBlobID(label string) []byte { return sha256Digest([]byte(label)) }

// fillKVBlobEntries fills the store with unique zero-length blobs to the exact
// per-run entry cap.
func fillKVBlobEntries(t *testing.T, s *KVBlobStore) {
	t.Helper()
	for i := 0; i < cursorBlobStoreEntriesMax; i++ {
		if err := s.put(testBlobID("entry-"+strconv.Itoa(i)), nil); err != nil {
			t.Fatalf("entry %d of %d was rejected: %v", i+1, cursorBlobStoreEntriesMax, err)
		}
	}
}

// cursorKvSetBlobMessage builds AgentServerMessage{kv_server_message=4{
// id, set_blob_args=3{blob_id=1, blob_data=2}}}, the same shape the upstream
// uses to push a blob into a run's cache.
func cursorKvSetBlobMessage(t *testing.T, id uint32, blobID, data []byte) []byte {
	t.Helper()
	args := appendBytes(nil, fSetBlobArgsBlobID, blobID)
	args = appendBytes(args, fSetBlobArgsBlobData, data)
	kv := appendUint32(nil, fKvMessageID, id)
	kv = appendMessage(kv, fKvServerSetBlobArgs, args)
	return appendMessage(nil, fAgentServerMessageKvServerMessage, kv)
}

func cursorKvSetBlobField(t *testing.T, msg []byte) *protoField {
	t.Helper()
	asm, err := parseProtoFields(msg)
	if err != nil {
		t.Fatalf("decode AgentServerMessage: %v", err)
	}
	kv := firstField(asm, fAgentServerMessageKvServerMessage)
	if kv == nil {
		t.Fatal("kv_server_message field missing")
	}
	return kv
}

// TestKVBlobStorePerRunByteCap fills the store to the exact cap, proves the
// next new blob is rejected without changing retention, and that get still
// returns the stored bytes.
func TestKVBlobStorePerRunByteCap(t *testing.T) {
	s := newKVBlobStore()
	full := make([]byte, cursorBlobStoreBytesMax)
	if err := s.put(testBlobID("fill"), full); err != nil {
		t.Fatalf("filling to the exact cap was rejected: %v", err)
	}
	if got := s.RetainedBytes(); got != cursorBlobStoreBytesMax {
		t.Fatalf("retained bytes = %d, want exactly %d", got, cursorBlobStoreBytesMax)
	}
	got, ok := s.get(testBlobID("fill"))
	if !ok || !bytes.Equal(got, full) {
		t.Fatal("get did not return the stored bytes")
	}

	if err := s.put(testBlobID("overflow"), []byte("x")); !errors.Is(err, errCursorBlobBytes) {
		t.Fatalf("over-cap insert error = %v, want %v", err, errCursorBlobBytes)
	}
	if got := s.RetainedBytes(); got != cursorBlobStoreBytesMax {
		t.Fatalf("rejected insert changed retained bytes: got %d, want %d", got, cursorBlobStoreBytesMax)
	}
	if s.len() != 1 {
		t.Fatalf("rejected insert changed the map: %d entries, want 1", s.len())
	}
	if _, ok := s.get(testBlobID("overflow")); ok {
		t.Fatal("rejected insert stored a partial value")
	}
}

// TestKVBlobStorePerRunEntryCap fills the map to the exact entry cap with
// unique zero-length blobs, then proves a new id is rejected while replacing an
// existing id at the cap remains allowed and the rejected insert changes
// neither count nor bytes.
func TestKVBlobStorePerRunEntryCap(t *testing.T) {
	s := newKVBlobStore()
	fillKVBlobEntries(t, s)
	if got := s.RetainedEntries(); got != cursorBlobStoreEntriesMax {
		t.Fatalf("retained entries = %d, want exactly %d", got, cursorBlobStoreEntriesMax)
	}
	if got := s.RetainedBytes(); got != 0 {
		t.Fatalf("zero-length entries retained %d bytes, want 0", got)
	}

	if err := s.put(testBlobID("overflow"), nil); !errors.Is(err, errCursorBlobEntries) {
		t.Fatalf("over-cap insert error = %v, want %v", err, errCursorBlobEntries)
	}
	if got := s.RetainedEntries(); got != cursorBlobStoreEntriesMax {
		t.Fatalf("rejected insert changed entries: got %d, want %d", got, cursorBlobStoreEntriesMax)
	}
	if got := s.RetainedBytes(); got != 0 {
		t.Fatalf("rejected insert changed bytes: got %d, want 0", got)
	}
	if _, ok := s.get(testBlobID("overflow")); ok {
		t.Fatal("rejected insert stored an entry")
	}

	payload := []byte("replacement")
	if err := s.put(testBlobID("entry-0"), payload); err != nil {
		t.Fatalf("replacement of an existing id at the entry cap was rejected: %v", err)
	}
	if got := s.RetainedEntries(); got != cursorBlobStoreEntriesMax {
		t.Fatalf("replacement changed entries: got %d, want %d", got, cursorBlobStoreEntriesMax)
	}
	if got, want := s.RetainedBytes(), int64(len(payload)); got != want {
		t.Fatalf("replacement retained bytes = %d, want %d", got, want)
	}
	got, ok := s.get(testBlobID("entry-0"))
	if !ok || !bytes.Equal(got, payload) {
		t.Fatal("replacement did not store the payload")
	}

	// A rejected new id at the cap must leave the replacement's bytes intact.
	if err := s.put(testBlobID("overflow-2"), nil); !errors.Is(err, errCursorBlobEntries) {
		t.Fatalf("over-cap insert error = %v, want %v", err, errCursorBlobEntries)
	}
	if got, want := s.RetainedBytes(), int64(len(payload)); got != want {
		t.Fatalf("rejected insert changed replacement bytes: got %d, want %d", got, want)
	}
	if got := s.RetainedEntries(); got != cursorBlobStoreEntriesMax {
		t.Fatalf("rejected insert changed entries: got %d, want %d", got, cursorBlobStoreEntriesMax)
	}
}

// TestKVBlobStoreReplacementByteAccounting proves duplicate replacement is
// charged by delta, not by adding the full new size, and that a replacement
// which would overrun the cap fails without corrupting the existing entry or
// its accounting.
func TestKVBlobStoreReplacementByteAccounting(t *testing.T) {
	s := newKVBlobStore()
	original := bytes.Repeat([]byte("a"), 1<<10)
	if err := s.put(testBlobID("dup"), original); err != nil {
		t.Fatalf("initial insert was rejected: %v", err)
	}
	if got, want := s.RetainedBytes(), int64(len(original)); got != want {
		t.Fatalf("initial retained bytes = %d, want %d", got, want)
	}

	larger := bytes.Repeat([]byte("b"), 2<<20)
	if err := s.put(testBlobID("dup"), larger); err != nil {
		t.Fatalf("larger replacement was rejected: %v", err)
	}
	if got, want := s.RetainedBytes(), int64(len(larger)); got != want {
		t.Fatalf("larger replacement retained bytes = %d, want exactly %d", got, want)
	}

	smaller := bytes.Repeat([]byte("c"), 1<<9)
	if err := s.put(testBlobID("dup"), smaller); err != nil {
		t.Fatalf("smaller replacement was rejected: %v", err)
	}
	if got, want := s.RetainedBytes(), int64(len(smaller)); got != want {
		t.Fatalf("smaller replacement retained bytes = %d, want exactly %d", got, want)
	}

	// Fill the remaining cap so replacing dup can only fit if the old entry's
	// bytes were released before the check.
	filler := make([]byte, cursorBlobStoreBytesMax-s.RetainedBytes())
	if err := s.put(testBlobID("filler"), filler); err != nil {
		t.Fatalf("filling the remaining cap was rejected: %v", err)
	}
	if got := s.RetainedBytes(); got != cursorBlobStoreBytesMax {
		t.Fatalf("filled retained bytes = %d, want exactly %d", got, cursorBlobStoreBytesMax)
	}

	if err := s.put(testBlobID("dup"), bytes.Repeat([]byte("d"), len(smaller)+1)); !errors.Is(err, errCursorBlobBytes) {
		t.Fatalf("over-cap replacement error = %v, want %v", err, errCursorBlobBytes)
	}
	if got := s.RetainedBytes(); got != cursorBlobStoreBytesMax {
		t.Fatalf("failed replacement changed retained bytes: got %d, want %d", got, cursorBlobStoreBytesMax)
	}
	got, ok := s.get(testBlobID("dup"))
	if !ok || !bytes.Equal(got, smaller) {
		t.Fatal("failed replacement lost or changed the existing entry")
	}
	if s.len() != 2 {
		t.Fatalf("failed replacement changed the map: %d entries, want 2", s.len())
	}
}

// TestCursorHistoryOverCapFailsTranslation proves an over-cap history set is an
// error, not a request with silently dropped history.
func TestCursorHistoryOverCapFailsTranslation(t *testing.T) {
	huge := strings.Repeat("x", cursorBlobStoreBytesMax+1)
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"assistant","content":"` + huge + `"},` +
		`{"role":"user","content":"next"}]}`)

	_, _, _, err := TranslateCursorRunRequestWithConversation(body, "")
	if !errors.Is(err, errCursorBlobBytes) {
		t.Fatalf("translation error = %v, want %v", err, errCursorBlobBytes)
	}
}

// TestCursorHistoryOverEntryCapFailsTranslation proves an over-entry-cap
// history set is an error, not a request with silently dropped history.
func TestCursorHistoryOverEntryCapFailsTranslation(t *testing.T) {
	var body strings.Builder
	body.WriteString(`{"model":"m","messages":[`)
	for i := 0; i <= cursorBlobStoreEntriesMax; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		body.WriteString(`{"role":"assistant","content":"a`)
		body.WriteString(strconv.Itoa(i))
		body.WriteString(`"}`)
	}
	body.WriteString(`,{"role":"user","content":"next"}]}`)

	_, _, _, err := TranslateCursorRunRequestWithConversation([]byte(body.String()), "")
	if !errors.Is(err, errCursorBlobEntries) {
		t.Fatalf("translation error = %v, want %v", err, errCursorBlobEntries)
	}
}

// TestCursorHistoryBlobsRoundTrip proves a normal history set resolves
// byte-for-byte by digest in construction order.
func TestCursorHistoryBlobsRoundTrip(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":"q1"},` +
		`{"role":"assistant","content":"a1"},` +
		`{"role":"user","content":"q2"}]}`)
	msg, blobs, _, err := TranslateCursorRunRequestWithConversation(body, "")
	if err != nil {
		t.Fatal(err)
	}
	if blobs == nil {
		t.Fatal("history blob store missing")
	}
	asm, err := parseProtoFields(msg)
	if err != nil {
		t.Fatal(err)
	}
	rr, err := subFields(firstField(asm, fAgentClientMessageRunRequest))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := subFields(firstField(rr, fRunRequestConversationState))
	if err != nil {
		t.Fatal(err)
	}

	expected, err := json.Marshal(map[string]any{"role": "system", "content": "sys"})
	if err != nil {
		t.Fatal(err)
	}
	want := [][]byte{expected}
	for _, obj := range []map[string]any{
		{"role": "user", "content": "q1"},
		{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "a1"}}},
	} {
		b, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, b)
	}

	var digests [][]byte
	for _, f := range cs {
		if f.num == fConversationStateRootPromptMessagesJSON {
			digests = append(digests, f.raw)
		}
	}
	if len(digests) != len(want) || blobs.len() != len(want) {
		t.Fatalf("history size = %d digests / %d blobs, want %d", len(digests), blobs.len(), len(want))
	}
	for i, digest := range digests {
		got, ok := blobs.get(digest)
		if !ok {
			t.Fatalf("digest %d did not resolve", i)
		}
		if !bytes.Equal(got, want[i]) {
			t.Fatalf("blob %d = %s, want %s", i, got, want[i])
		}
		if !bytes.Equal(digest, sha256Digest(want[i])) {
			t.Fatalf("digest %d is not the sha256 of its blob", i)
		}
	}
}

// TestCursorSetBlobHandlerByteCap proves a server-pushed blob over the per-run
// cap returns the error without storing a partial value or acknowledging it,
// while a normal push still stores and acks.
func TestCursorSetBlobHandlerByteCap(t *testing.T) {
	t.Run("over cap", func(t *testing.T) {
		s := newKVBlobStore()
		if err := s.put(testBlobID("fill"), make([]byte, cursorBlobStoreBytesMax)); err != nil {
			t.Fatalf("filling to the exact cap was rejected: %v", err)
		}
		blobID := testBlobID("pushed")
		var out bytes.Buffer
		err := handleKvServerMessage(&streamWriter{w: &out}, cursorKvSetBlobField(t, cursorKvSetBlobMessage(t, 7, blobID, []byte("x"))), s)
		if !errors.Is(err, errCursorBlobBytes) {
			t.Fatalf("handler error = %v, want %v", err, errCursorBlobBytes)
		}
		if _, ok := s.get(blobID); ok {
			t.Fatal("rejected set_blob stored a partial value")
		}
		if got := s.RetainedBytes(); got != cursorBlobStoreBytesMax {
			t.Fatalf("rejected set_blob changed retained bytes: got %d, want %d", got, cursorBlobStoreBytesMax)
		}
		if out.Len() != 0 {
			t.Fatal("handler acknowledged a set_blob it did not store")
		}
	})

	t.Run("normal set", func(t *testing.T) {
		s := newKVBlobStore()
		blobID, data := testBlobID("pushed"), []byte("payload")
		var out bytes.Buffer
		err := handleKvServerMessage(&streamWriter{w: &out}, cursorKvSetBlobField(t, cursorKvSetBlobMessage(t, 8, blobID, data)), s)
		if err != nil {
			t.Fatalf("normal set_blob failed: %v", err)
		}
		got, ok := s.get(blobID)
		if !ok || !bytes.Equal(got, data) {
			t.Fatal("normal set_blob did not store the payload")
		}
		if got, want := s.RetainedBytes(), int64(len(data)); got != want {
			t.Fatalf("normal set_blob retained bytes = %d, want %d", got, want)
		}
		if out.Len() == 0 {
			t.Fatal("normal set_blob was not acknowledged")
		}
	})
}

// TestCursorSetBlobHandlerEntryCap proves a server-pushed new id over the
// per-run entry cap returns the error without storing or acknowledging it,
// while an empty blob below the cap still stores and acks.
func TestCursorSetBlobHandlerEntryCap(t *testing.T) {
	t.Run("over cap", func(t *testing.T) {
		s := newKVBlobStore()
		fillKVBlobEntries(t, s)
		blobID := testBlobID("pushed-empty")
		var out bytes.Buffer
		err := handleKvServerMessage(&streamWriter{w: &out}, cursorKvSetBlobField(t, cursorKvSetBlobMessage(t, 10, blobID, nil)), s)
		if !errors.Is(err, errCursorBlobEntries) {
			t.Fatalf("handler error = %v, want %v", err, errCursorBlobEntries)
		}
		if _, ok := s.get(blobID); ok {
			t.Fatal("rejected set_blob stored a partial value")
		}
		if got := s.RetainedEntries(); got != cursorBlobStoreEntriesMax {
			t.Fatalf("rejected set_blob changed entries: got %d, want %d", got, cursorBlobStoreEntriesMax)
		}
		if got := s.RetainedBytes(); got != 0 {
			t.Fatalf("rejected set_blob changed bytes: got %d, want 0", got)
		}
		if out.Len() != 0 {
			t.Fatal("handler acknowledged a set_blob it did not store")
		}
	})

	t.Run("normal empty set", func(t *testing.T) {
		s := newKVBlobStore()
		blobID := testBlobID("pushed-empty")
		var out bytes.Buffer
		err := handleKvServerMessage(&streamWriter{w: &out}, cursorKvSetBlobField(t, cursorKvSetBlobMessage(t, 11, blobID, nil)), s)
		if err != nil {
			t.Fatalf("normal empty set_blob failed: %v", err)
		}
		got, ok := s.get(blobID)
		if !ok || len(got) != 0 {
			t.Fatal("normal empty set_blob did not store the empty payload")
		}
		if got := s.RetainedEntries(); got != 1 {
			t.Fatalf("normal empty set_blob entries = %d, want 1", got)
		}
		if got := s.RetainedBytes(); got != 0 {
			t.Fatalf("normal empty set_blob bytes = %d, want 0", got)
		}
		if out.Len() == 0 {
			t.Fatal("normal empty set_blob was not acknowledged")
		}
	})
}

// TestCursorSetBlobOverCapFailsPump carries the handler error through the
// existing pump error path: the run ends TurnErrored and the blob is absent.
func TestCursorSetBlobOverCapFailsPump(t *testing.T) {
	s := newKVBlobStore()
	if err := s.put(testBlobID("fill"), make([]byte, cursorBlobStoreBytesMax)); err != nil {
		t.Fatalf("filling to the exact cap was rejected: %v", err)
	}
	blobID := testBlobID("pushed")
	frames := env(t, cursorKvSetBlobMessage(t, 9, blobID, []byte("x")))
	r := NewCursorRun(io.Discard, bytes.NewReader(frames), s, nil, time.Hour)
	t.Cleanup(r.Close)
	r.pump()

	res := r.RunTurn(t.Context(), nil)
	if res.Outcome != TurnErrored || !errors.Is(res.Err, errCursorBlobBytes) {
		t.Fatalf("driver result = %+v, want TurnErrored with %v", res, errCursorBlobBytes)
	}
	if _, ok := s.get(blobID); ok {
		t.Fatal("over-cap set_blob was stored")
	}
}

// TestKVBlobStoreRejectsInvalidIDShape pins the documented sha256 key shape at
// both store doors: only a 32-byte id is admitted or looked up, so a malformed
// or oversized upstream id can never become an unbounded map key.
func TestKVBlobStoreRejectsInvalidIDShape(t *testing.T) {
	s := newKVBlobStore()
	for _, n := range []int{0, 1, 31, 33, 64} {
		id := make([]byte, n)
		if err := s.put(id, []byte("x")); !errors.Is(err, errCursorBlobID) {
			t.Fatalf("%d-byte id put error = %v, want %v", n, err, errCursorBlobID)
		}
		if _, ok := s.get(id); ok {
			t.Fatalf("%d-byte id resolved in the store; the shape must be rejected", n)
		}
	}
	if s.RetainedEntries() != 0 || s.RetainedBytes() != 0 {
		t.Fatalf("rejected ids changed retention: entries=%d bytes=%d", s.RetainedEntries(), s.RetainedBytes())
	}
	valid := make([]byte, cursorBlobIDLen)
	if err := s.put(valid, []byte("x")); err != nil {
		t.Fatalf("valid %d-byte id was rejected: %v", cursorBlobIDLen, err)
	}
	if got, ok := s.get(valid); !ok || !bytes.Equal(got, []byte("x")) {
		t.Fatal("valid id did not round-trip")
	}
}

// TestCursorSetBlobDetachesRetainedValue proves a one-byte server-pushed blob
// carried in a multi-megabyte frame retains only its own bytes: the store owns
// a detached, exact-capacity copy and cannot pin the frame.
func TestCursorSetBlobDetachesRetainedValue(t *testing.T) {
	s := newKVBlobStore()
	blobID := testBlobID("pushed")
	small := cursorKvSetBlobMessage(t, 1, blobID, []byte("x"))
	msg := append(small, appendBytes(nil, 99, make([]byte, 4<<20))...)
	var out bytes.Buffer
	if err := handleKvServerMessage(&streamWriter{w: &out}, cursorKvSetBlobField(t, msg), s); err != nil {
		t.Fatalf("normal set_blob failed: %v", err)
	}
	got, ok := s.get(blobID)
	if !ok || !bytes.Equal(got, []byte("x")) {
		t.Fatal("set_blob did not retain its value")
	}
	if cap(got) != len(got) {
		t.Fatalf("stored blob len=%d cap=%d, want cap==len (detached from the frame)", len(got), cap(got))
	}
	if retained := s.RetainedBytes(); retained != 1 {
		t.Fatalf("retained bytes = %d, want the 1-byte value only", retained)
	}
}

// TestCursorExecDetachesRetainedID proves a one-byte exec id carried in a
// multi-megabyte frame is detached before the pending map retains it, and that
// pending accounting charges only the real fields.
func TestCursorExecDetachesRetainedID(t *testing.T) {
	ex := appendUint32(nil, fExecMessageID, 1)
	ex = appendBytes(ex, fExecMessageExecID, []byte("e"))
	ex = appendMessage(ex, fExecServerMcpArgs, appendString(nil, fMcpArgsName, "tool"))
	ex = append(ex, appendBytes(nil, 99, make([]byte, 4<<20))...)
	payload := appendMessage(nil, fAgentServerMessageExecServerMessage, ex)
	fields, err := parseProtoFields(payload)
	if err != nil {
		t.Fatalf("decode AgentServerMessage: %v", err)
	}
	asm := firstField(fields, fAgentServerMessageExecServerMessage)
	var call CursorToolCall
	if err := handleExecServerMessage(&streamWriter{w: io.Discard}, asm, &call); err != nil {
		t.Fatalf("handle exec message: %v", err)
	}
	if string(call.execIDStr) != "e" || cap(call.execIDStr) != len(call.execIDStr) {
		t.Fatalf("exec id len=%d cap=%d, want a detached 1-byte value", len(call.execIDStr), cap(call.execIDStr))
	}
	r := NewCursorRun(io.Discard, bytes.NewReader(nil), nil, nil, time.Hour)
	t.Cleanup(r.Close)
	if !r.addPending(call) {
		t.Fatal("normal pending call was rejected")
	}
	r.mu.Lock()
	got := r.pending[call.CallID]
	pendingBytes := r.pendingBytes
	r.mu.Unlock()
	if cap(got.execIDStr) != len(got.execIDStr) {
		t.Fatalf("pending exec id len=%d cap=%d, want a detached value", len(got.execIDStr), cap(got.execIDStr))
	}
	if want := cursorRunToolCallSize(call); pendingBytes != want {
		t.Fatalf("pending bytes = %d, want exactly the retained field size %d", pendingBytes, want)
	}
}

// TestCursorSetBlobOverEntryCapFailsPump carries the entry-cap error through
// the same pump path: the run ends TurnErrored and the new id is absent.
func TestCursorSetBlobOverEntryCapFailsPump(t *testing.T) {
	s := newKVBlobStore()
	fillKVBlobEntries(t, s)
	blobID := testBlobID("pushed-empty")
	frames := env(t, cursorKvSetBlobMessage(t, 12, blobID, nil))
	r := NewCursorRun(io.Discard, bytes.NewReader(frames), s, nil, time.Hour)
	t.Cleanup(r.Close)
	r.pump()

	res := r.RunTurn(t.Context(), nil)
	if res.Outcome != TurnErrored || !errors.Is(res.Err, errCursorBlobEntries) {
		t.Fatalf("driver result = %+v, want TurnErrored with %v", res, errCursorBlobEntries)
	}
	if _, ok := s.get(blobID); ok {
		t.Fatal("over-entry-cap set_blob was stored")
	}
	if got := s.RetainedEntries(); got != cursorBlobStoreEntriesMax {
		t.Fatalf("over-entry-cap set_blob changed entries: got %d, want %d", got, cursorBlobStoreEntriesMax)
	}
}
