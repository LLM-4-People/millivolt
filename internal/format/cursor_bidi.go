package format

// Bidirectional Cursor agent.v1.AgentService/Run pump.
//
// Unlike a unary Connect call, Run is a full-duplex HTTP/2 stream: after the
// client sends AgentClientMessage{run_request}, the server writes requests
// BACK on the same stream and blocks until the client answers - a KV blob pull
// (kv_server_message.get_blob_args → kv_client_message.get_blob_result) and a
// request-context handshake (exec_server_message.request_context_args →
// exec_client_message.request_context_result) - before it streams any model
// output. A client that writes one envelope and only reads (the unary mistake)
// stalls or errors. This file implements that duplex exchange.
//
// Field numbers come from cursor2api's vendored proto/agent.proto, cross-
// confirmed against cliproxy-cursor-plugin's generated descriptor and
// cursor-bridge's wire-capture encoder. They are the stable wire contract, not
// user-tunable.

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// agent.v1 bidi field numbers beyond the unary set in cursor.go.
const (
	// AgentClientMessage oneof (client -> server); run_request=1 is in cursor.go.
	fAgentClientMessageExecClientMessage  = 2 // ExecClientMessage
	fAgentClientMessageKvClientMessage    = 3 // KvClientMessage
	fAgentClientMessageConversationAction = 4 // ConversationAction
	fAgentClientMessageClientHeartbeat    = 7 // ClientHeartbeat (empty)

	// AgentServerMessage oneof (server -> client); interaction_update=1 is in cursor.go.
	fAgentServerMessageExecServerMessage = 2 // ExecServerMessage
	fAgentServerMessageKvServerMessage   = 4 // KvServerMessage

	// KvServerMessage / KvClientMessage shared fields.
	fKvMessageID           = 1 // uint32 correlation id (echo back)
	fKvServerGetBlobArgs   = 2 // GetBlobArgs
	fKvServerSetBlobArgs   = 3 // SetBlobArgs
	fKvClientGetBlobResult = 2 // GetBlobResult
	fKvClientSetBlobResult = 3 // SetBlobResult
	fGetBlobArgsBlobID     = 1 // bytes (sha256 digest)
	fSetBlobArgsBlobID     = 1 // bytes
	fSetBlobArgsBlobData   = 2 // bytes
	fGetBlobResultBlobData = 1 // bytes (absent = not found)

	// ExecServerMessage / ExecClientMessage shared fields.
	fExecMessageID              = 1  // uint32 correlation id (echo back)
	fExecMessageExecID          = 15 // string attachable-exec id (echo if present)
	fExecServerRequestContext   = 10 // RequestContextArgs
	fExecClientRequestContext   = 10 // RequestContextResult
	fRequestContextResultSucc   = 1  // RequestContextSuccess
	fRequestContextSuccessInner = 1  // RequestContext (may be empty)
	fExecServerMcpArgs          = 11 // McpArgs - the blocking tool-call request
	fExecClientMcpResult        = 11 // McpResult - the tool result reply

	// McpArgs (the server's tool-call request, ExecServerMessage field 11).
	fMcpArgsName       = 1 // string tool name
	fMcpArgsArgs       = 2 // map<string, bytes(Value)> arguments
	fMcpArgsToolCallID = 3 // string OpenAI-style tool_call id
	fMcpArgsProviderID = 4 // string (e.g. "openai")
	fMcpArgsToolName   = 5 // string tool name (alt)
	fMcpArgsServerID   = 9 // string MCP server id

	// McpToolDefinition (client-declared tools, in run-request mcp_tools field 4
	// and request_context tools field 7).
	fMcpToolsList              = 1 // repeated McpToolDefinition (McpTools.mcp_tools)
	fMcpToolDefName            = 1 // string
	fMcpToolDefDescription     = 2 // string
	fMcpToolDefInputSchemaJSON = 6 // string (JSON schema; field 3 bytes is unused)
	fMcpToolDefProviderID      = 4 // string ("openai")
	fMcpToolDefToolName        = 5 // string (dup of name)
	// RequestContext.tools (repeated McpToolDefinition).
	fRequestContextTools = 7

	// InteractionUpdate tool-call events (informational; the blocking exec
	// mcp_args above is authoritative for caller tools).
	fInteractionUpdateToolCallStarted   = 2  // ToolCallStartedUpdate
	fInteractionUpdateToolCallCompleted = 3  // ToolCallCompletedUpdate
	fInteractionUpdatePartialToolCall   = 7  // PartialToolCallUpdate
	fInteractionUpdateToolCallDelta     = 15 // ToolCallDeltaUpdate

	// google.protobuf.Value (shim) - McpArgs.args values are serialized Values.
	fValueNull   = 1 // NullValue (varint)
	fValueNumber = 2 // double (fixed64, wire type 1)
	fValueString = 3 // string
	fValueBool   = 4 // bool (varint)
	fValueStruct = 5 // Struct
	fValueList   = 6 // ListValue
)

// cursorBlobStoreBytesMax bounds the retained blob bytes of one run's KV blob
// store. History is one JSON blob per conversation turn plus server-pushed
// blobs; 64 MiB comfortably covers a long text/tool conversation while keeping
// a single parked run's blob retention bounded. Internal safety guardrail, not
// user-tunable.
const cursorBlobStoreBytesMax = 64 << 20 // 64 MiB

// cursorBlobStoreEntriesMax bounds the number of blobs retained in one run's
// KV blob store. The byte cap does not charge zero-length values, so an
// upstream sending unique empty set_blob ids could otherwise grow the map
// without bound; 65,536 entries cover a long conversation plus pushes while
// keeping the map bounded. Internal safety guardrail, not user-tunable.
const cursorBlobStoreEntriesMax = 65_536

// cursorBlobIDLen is the exact retained blob-key length: the raw sha256 digest
// the Cursor schema documents for GetBlobArgs.blob_id (and history digests use
// the same shape). A malformed id is rejected at put/get rather than retained
// as an unbounded map key. Internal wire-contract constant.
const cursorBlobIDLen = sha256.Size

var (
	// errCursorBlobBytes is the stable fail-closed reason for exceeding the
	// per-run KV blob byte cap. Callers propagate it: a history set that cannot
	// fit rejects the request, and a server push that cannot fit tears the run
	// down.
	errCursorBlobBytes = errors.New("cursor: kv blob byte limit exceeded")
	// errCursorBlobEntries is the stable fail-closed reason for exceeding the
	// per-run KV blob entry cap. A replacement of an existing id is always
	// allowed; only a new id can hit this bound.
	errCursorBlobEntries = errors.New("cursor: kv blob entry limit exceeded")
	// errCursorBlobID is the stable fail-closed reason for a blob id that is
	// not the documented 32-byte sha256 shape.
	errCursorBlobID = errors.New("cursor: invalid kv blob id")
)

// cloneRetainedBytes detaches b from its backing array with cap == len, so a
// sub-slice of a large upstream frame cannot pin the whole frame in retained
// state. The store and pending map own the returned value; callers must not
// rely on aliasing to the source.
func cloneRetainedBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// KVBlobStore holds the history blobs the server may pull mid-stream. Blobs
// are keyed by their raw sha256 digest bytes (what GetBlobArgs.blob_id carries).
// The store owns every retained value: callers hand over ownership and must not
// mutate a stored blob afterwards. History blobs are freshly marshaled and are
// handed over as-is; server-pushed sub-slices are cloned by the handler before
// put, so a tiny push cannot retain a whole frame.
type KVBlobStore struct {
	mu    sync.Mutex
	m     map[string][]byte
	bytes int64 // exact sum of retained blob lengths, guarded by mu
	// retention is the parked-run store's aggregate blob observer. It is
	// installed only while the owning run is parked and is called under mu
	// with the candidate byte/entry totals before a put commits.
	retention func(bytes int64, entries int) error
}

func newKVBlobStore() *KVBlobStore { return &KVBlobStore{m: map[string][]byte{}} }

// SetRetentionLimit installs the aggregate blob observer used while the owning
// run is parked. The initial call reports the store's current retained bytes
// and entries, so an over-budget candidate is refused before it is parked; a
// later rejected put leaves the map and byte accounting unchanged.
func (s *KVBlobStore) SetRetentionLimit(limit func(bytes int64, entries int) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit == nil {
		return errors.New("cursor: blob retention limit requires an observer")
	}
	if s.retention != nil {
		return errors.New("cursor: blob retention limit already installed")
	}
	if err := limit(s.bytes, len(s.m)); err != nil {
		return err
	}
	s.retention = limit
	return nil
}

// ClearRetentionLimit releases the aggregate observer and reports zero, so the
// parked-run store drops this run's blob bytes and entries exactly once. It is
// idempotent.
func (s *KVBlobStore) ClearRetentionLimit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retention != nil {
		_ = s.retention(0, 0)
		s.retention = nil
	}
}

// put stores data under id. It is the store's single insertion owner: a new id
// is rejected when the map is already at cursorBlobStoreEntriesMax, and a new
// or replacement value that would exceed cursorBlobStoreBytesMax is rejected.
// Both checks leave the map and byte accounting unchanged. A replacement of an
// existing id is always admitted by the entry cap and subtracts the old value
// before the byte check, so same-size replacement always fits.
func (s *KVBlobStore) put(id, data []byte) error {
	if len(id) != cursorBlobIDLen {
		return fmt.Errorf("%w (id %d bytes, want %d)", errCursorBlobID, len(id), cursorBlobIDLen)
	}
	key := string(id)
	size := int64(len(data))
	s.mu.Lock()
	defer s.mu.Unlock()
	old, exists := s.m[key]
	if !exists && len(s.m) >= cursorBlobStoreEntriesMax {
		return fmt.Errorf("%w (limit %d)", errCursorBlobEntries, cursorBlobStoreEntriesMax)
	}
	base := s.bytes
	if exists {
		base -= int64(len(old))
	}
	if size > cursorBlobStoreBytesMax-base {
		return fmt.Errorf("%w (blob %d bytes, retained %d, limit %d)",
			errCursorBlobBytes, size, base, cursorBlobStoreBytesMax)
	}
	entries := len(s.m)
	if !exists {
		entries++
	}
	if s.retention != nil {
		if err := s.retention(base+size, entries); err != nil {
			return err
		}
	}
	s.m[key] = data
	s.bytes = base + size
	return nil
}

func (s *KVBlobStore) get(id []byte) ([]byte, bool) {
	if len(id) != cursorBlobIDLen {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[string(id)]
	return b, ok
}

// RetainedBytes returns the exact sum of stored blob lengths. The parked-run
// store uses it to enforce its aggregate budget without scanning blob values.
func (s *KVBlobStore) RetainedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// RetainedEntries returns the number of stored blobs in O(1). The parked-run
// store uses it to enforce its aggregate entry budget without scanning the map.
func (s *KVBlobStore) RetainedEntries() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// len returns the number of cached blobs (for testing).
func (s *KVBlobStore) len() int { return s.RetainedEntries() }

// streamWriter serializes envelope writes onto the request body so the
// read-loop's callback replies and the heartbeat ticker never interleave bytes.
type streamWriter struct {
	mu  sync.Mutex
	w   io.Writer
	err error // sticky write error (e.g. the pipe/conn closed)
}

// writeFramed writes a complete envelope (flags byte + length + payload) in a
// single critical section. The frame header is written separately so a large
// payload is never copied again into a header buffer; bytes on the wire are
// identical to the previous header+payload concatenation. Once a write fails,
// subsequent writes are no-ops returning the stored error (the stream is dead).
func (s *streamWriter) writeFramed(payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	var hdr [connectFrameHeader]byte
	// hdr[0] flags = 0 (uncompressed data frame).
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if err := writeAll(s.w, hdr[:]); err != nil {
		s.err = err
		return err
	}
	if len(payload) > 0 {
		if err := writeAll(s.w, payload); err != nil {
			s.err = err
			return err
		}
	}
	return nil
}

// writeAll writes p in full. A short nil-error write is an io.Writer contract
// violation; treating it as success would put a truncated frame on the wire.
func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

// encodeClientHeartbeat builds AgentClientMessage{ client_heartbeat=7 {} }.
func encodeClientHeartbeat() []byte {
	return appendMessage(nil, fAgentClientMessageClientHeartbeat, nil)
}

// encodeSteerUserMessage builds
// AgentClientMessage{ conversation_action=4 ConversationAction{ user_message_action=1 ... } }
// - a user message injected into an ALREADY-OPEN run so the in-flight turn sees
// it at its next step (Cursor's "steer"). This is how the real client delivers
// a message the user typed mid-turn, instead of abandoning the turn.
func encodeSteerUserMessage(text string) []byte {
	return appendMessage(nil, fAgentClientMessageConversationAction, encodeUserMessageAction(text))
}

// encodeKvGetBlobResult builds
// AgentClientMessage{ kv_client_message=3 KvClientMessage{ id, get_blob_result=2 GetBlobResult{ blob_data? } } }.
// A nil/empty blobData omits the field, signaling not-found to the server. The
// output is sized exactly, so the blob is copied into the envelope once.
func encodeKvGetBlobResult(id uint64, blobData []byte) []byte {
	var resLen int
	if len(blobData) > 0 {
		resLen = bytesFieldLen(fGetBlobResultBlobData, len(blobData))
	}
	kvLen := uint32FieldLen(fKvMessageID, uint32(id)) + bytesFieldLen(fKvClientGetBlobResult, resLen)
	out := make([]byte, 0, bytesFieldLen(fAgentClientMessageKvClientMessage, kvLen))
	out = appendTag(out, fAgentClientMessageKvClientMessage, wireBytes)
	out = appendVarint(out, uint64(kvLen))
	out = appendUint32(out, fKvMessageID, uint32(id))
	out = appendTag(out, fKvClientGetBlobResult, wireBytes)
	out = appendVarint(out, uint64(resLen))
	if len(blobData) > 0 {
		out = appendBytes(out, fGetBlobResultBlobData, blobData)
	}
	return out
}

// encodeKvSetBlobAck builds AgentClientMessage{ kv_client_message=3 { id, set_blob_result=3 {} } }.
func encodeKvSetBlobAck(id uint64) []byte {
	var kv []byte
	kv = appendUint32(kv, fKvMessageID, uint32(id))
	kv = appendMessage(kv, fKvClientSetBlobResult, nil)
	return appendMessage(nil, fAgentClientMessageKvClientMessage, kv)
}

// execClientBodyLen is the exact encoded size of the inner ExecClientMessage
// (id, optional exec_id and the reply payload) shared by the size and append
// owners, so the allocated capacity can never drift from the bytes written.
func execClientBodyLen(id uint64, execID []byte, field, payloadLen int) int {
	body := uint32FieldLen(fExecMessageID, uint32(id))
	if len(execID) > 0 {
		body += bytesFieldLen(fExecMessageExecID, len(execID))
	}
	return body + bytesFieldLen(field, payloadLen)
}

// execClientEnvelopeLen returns the exact encoded size of one exec reply:
// AgentClientMessage{ exec_client_message=2 ExecClientMessage{ id, exec_id?, field payload } }.
func execClientEnvelopeLen(id uint64, execID []byte, field, payloadLen int) int {
	return bytesFieldLen(fAgentClientMessageExecClientMessage, execClientBodyLen(id, execID, field, payloadLen))
}

// appendExecClientHeader appends the exact-size exec reply envelope headers up
// to (not including) the payload bytes. Callers allocate execClientEnvelopeLen
// bytes of capacity and append exactly payloadLen payload bytes afterwards, so
// the reply is built once instead of being wrapped through several copies.
func appendExecClientHeader(dst []byte, id uint64, execID []byte, field, payloadLen int) []byte {
	dst = appendTag(dst, fAgentClientMessageExecClientMessage, wireBytes)
	dst = appendVarint(dst, uint64(execClientBodyLen(id, execID, field, payloadLen)))
	dst = appendUint32(dst, fExecMessageID, uint32(id))
	if len(execID) > 0 {
		dst = appendBytes(dst, fExecMessageExecID, execID)
	}
	dst = appendTag(dst, field, wireBytes)
	return appendVarint(dst, uint64(payloadLen))
}

// encodeMcpResult builds the reply that delivers a real tool result into a
// parked turn:
// AgentClientMessage{ exec_client_message=2 { id, exec_id?, mcp_result=11 McpResult } }.
// Success:  mcp_result{ success=1 { content=1 { text=1 { text=1 result } }, is_error=2 false } }
// Error:    mcp_result{ error=2 { error=1 result } }
// The nested envelope is sized exactly and the result is copied once.
func encodeMcpResult(id uint64, execID []byte, result string, isError bool) []byte {
	textLen := stringFieldLen(1, result)
	var mcpResultLen, itemLen, successLen int
	if isError {
		mcpResultLen = bytesFieldLen(2, textLen)
	} else {
		itemLen = bytesFieldLen(1, textLen)
		successLen = bytesFieldLen(1, itemLen)
		mcpResultLen = bytesFieldLen(1, successLen)
	}
	out := make([]byte, 0, execClientEnvelopeLen(id, execID, fExecClientMcpResult, mcpResultLen))
	out = appendExecClientHeader(out, id, execID, fExecClientMcpResult, mcpResultLen)
	if isError {
		out = appendTag(out, 2, wireBytes)
		out = appendVarint(out, uint64(textLen))
		out = appendString(out, 1, result)
		return out
	}
	out = appendTag(out, 1, wireBytes)
	out = appendVarint(out, uint64(successLen))
	out = appendTag(out, 1, wireBytes)
	out = appendVarint(out, uint64(itemLen))
	out = appendTag(out, 1, wireBytes)
	out = appendVarint(out, uint64(textLen))
	return appendString(out, 1, result)
}

// encodeRequestContextResult builds
// AgentClientMessage{ exec_client_message=2 ExecClientMessage{ id, exec_id?, request_context_result=10 { success=1 { request_context=1 {} } } } }.
// The RequestContext is empty (a text bridge has no workspace/rules/tools to
// advertise); the proven-minimal reply is success{ request_context{} }.
func encodeRequestContextResult(id uint64, execID []byte) []byte {
	successLen := bytesFieldLen(fRequestContextSuccessInner, 0)
	resultLen := bytesFieldLen(fRequestContextResultSucc, successLen)
	out := make([]byte, 0, execClientEnvelopeLen(id, execID, fExecClientRequestContext, resultLen))
	out = appendExecClientHeader(out, id, execID, fExecClientRequestContext, resultLen)
	out = appendTag(out, fRequestContextResultSucc, wireBytes)
	out = appendVarint(out, uint64(successLen))
	out = appendTag(out, fRequestContextSuccessInner, wireBytes)
	return appendVarint(out, 0)
}

// handleKvServerMessage answers a server's KV get/set request on the request stream.
func handleKvServerMessage(sw *streamWriter, kvField *protoField, blobs *KVBlobStore) error {
	kv, err := subFields(kvField)
	if err != nil {
		return fmt.Errorf("cursor: decode KvServerMessage: %w", err)
	}
	var id uint64
	if f := firstField(kv, fKvMessageID); f != nil {
		id = f.n
	}
	if g := firstField(kv, fKvServerGetBlobArgs); g != nil {
		args, err := subFields(g)
		if err != nil {
			return fmt.Errorf("cursor: decode GetBlobArgs: %w", err)
		}
		var data []byte
		if b := firstField(args, fGetBlobArgsBlobID); b != nil {
			if blob, ok := blobs.get(b.raw); ok {
				data = blob
			}
		}
		// Absent blob_data = not found.
		return sw.writeFramed(encodeKvGetBlobResult(id, data))
	}
	if s := firstField(kv, fKvServerSetBlobArgs); s != nil {
		// Server pushing a blob for us to cache; ack it (and cache for later turns).
		args, err := subFields(s)
		if err != nil {
			return fmt.Errorf("cursor: decode SetBlobArgs: %w", err)
		}
		bid := firstField(args, fSetBlobArgsBlobID)
		bdata := firstField(args, fSetBlobArgsBlobData)
		if bid != nil && bdata != nil {
			// The store retains the value past this frame; detach the sub-slice
			// so a tiny push cannot pin the whole upstream frame.
			if err := blobs.put(bid.raw, cloneRetainedBytes(bdata.raw)); err != nil {
				return fmt.Errorf("cursor: cache SetBlobArgs blob: %w", err)
			}
		}
		return sw.writeFramed(encodeKvSetBlobAck(id))
	}
	return nil
}

// handleExecServerMessage handles a server exec request. The request-context
// handshake is answered in-stream. A blocking MCP tool call (mcp_args) is
// parsed into outCall (when non-nil) and NOT answered - the proxy surfaces it
// to the OpenAI client instead, ending the turn. Cursor's built-in tools
// (shell/read/write/…) are suppressed via the x-cursor-agent-allowed-tools
// header, so they should not arrive; any that do are left unanswered.
func handleExecServerMessage(sw *streamWriter, exField *protoField, outCall *CursorToolCall) error {
	ex, err := subFields(exField)
	if err != nil {
		return fmt.Errorf("cursor: decode ExecServerMessage: %w", err)
	}
	var id uint64
	if f := firstField(ex, fExecMessageID); f != nil {
		id = f.n
	}
	var execID []byte
	if f := firstField(ex, fExecMessageExecID); f != nil {
		execID = f.raw
	}
	// Request-context handshake: answer with a minimal empty RequestContext.
	if firstField(ex, fExecServerRequestContext) != nil {
		return sw.writeFramed(encodeRequestContextResult(id, execID))
	}
	// A blocking MCP (client-declared) tool call: parse it out for the caller,
	// capturing the correlation ids so the caller can release the turn.
	if mcp := firstField(ex, fExecServerMcpArgs); mcp != nil {
		if outCall != nil {
			*outCall = parseMcpArgs(mcp)
			outCall.execID = id
			// The pending map retains the id past this frame; detach it from
			// the frame's backing array.
			outCall.execIDStr = cloneRetainedBytes(execID)
		}
		// Deliberately not answered in-stream: the proxy ends the turn and hands
		// the call to the OpenAI client, which executes the tool and re-sends
		// the result. (An in-stream mcp_result reply would require the proxy to
		// execute the tool itself, which is out of scope.)
		return nil
	}
	// Any other exec request (shell/read/write/…) is a Cursor built-in tool.
	// Built-ins are suppressed via the allowed-tools header; if one still
	// arrives, leave it unanswered so the server times it out and continues.
	return nil
}

// parseMcpArgs decodes an McpArgs message into a CursorToolCall. The args map
// values are serialized google.protobuf.Value messages (not raw JSON).
func parseMcpArgs(f *protoField) CursorToolCall {
	var call CursorToolCall
	args, err := subFields(f)
	if err != nil {
		return call
	}
	argv := map[string]any{}
	// One field budget for the whole args map: nested Value containers and
	// every entry share the frame allowance instead of resetting it per entry.
	budget := newProtoFieldBudget()
	for _, a := range args {
		switch a.num {
		case fMcpArgsName:
			call.Name = string(a.raw)
		case fMcpArgsToolName:
			if call.Name == "" {
				call.Name = string(a.raw)
			}
		case fMcpArgsToolCallID:
			call.CallID = string(a.raw)
		case fMcpArgsArgs:
			// map<string, bytes(Value)> entry: field 1 = key (string), field 2 = value (Value).
			if k, v, ok := decodeStringValueMapEntry(a.raw, budget); ok {
				argv[k] = v
			}
		}
	}
	// The wire tool_call_id can carry embedded newlines/whitespace; strip them so
	// the OpenAI tool_call id is a clean single-token string.
	call.CallID = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			return -1
		}
		return r
	}, call.CallID)
	if call.CallID == "" {
		call.CallID = "call_" + UUID4()
	}
	if len(argv) > 0 {
		if b, err := json.Marshal(argv); err == nil {
			call.Args = string(b)
		}
	}
	return call
}

// cursorBlobsForHistory builds the kv blob store for a translated request.
// The history blobs are sha256-keyed; the server pulls them by digest.
// sha256Digest in cursor.go is the single owner of the digest. Digest/blob
// pairing and order are preserved; an over-cap set returns an error instead of
// silently dropping history, so the caller fails the request rather than
// starting a run with incomplete context.
func cursorBlobsForHistory(digests, blobs [][]byte) (*KVBlobStore, error) {
	s := newKVBlobStore()
	for i := range blobs {
		if i >= len(digests) {
			continue
		}
		if err := s.put(digests[i], blobs[i]); err != nil {
			return nil, fmt.Errorf("cursor: history blob %d: %w", i, err)
		}
	}
	return s, nil
}
