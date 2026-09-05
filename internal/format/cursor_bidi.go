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
	"encoding/binary"
	"encoding/json"
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

// KVBlobStore holds the history blobs the server may pull mid-stream. Blobs
// are keyed by their raw sha256 digest bytes (what GetBlobArgs.blob_id carries).
type KVBlobStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func newKVBlobStore() *KVBlobStore { return &KVBlobStore{m: map[string][]byte{}} }

func (s *KVBlobStore) put(id, data []byte) {
	s.mu.Lock()
	s.m[string(id)] = data
	s.mu.Unlock()
}

func (s *KVBlobStore) get(id []byte) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[string(id)]
	return b, ok
}

// len returns the number of cached blobs (for testing).
func (s *KVBlobStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// streamWriter serializes envelope writes onto the request body so the
// read-loop's callback replies and the heartbeat ticker never interleave bytes.
type streamWriter struct {
	mu  sync.Mutex
	w   io.Writer
	err error // sticky write error (e.g. the pipe/conn closed)
}

// writeFramed writes a complete envelope (flags byte + length + payload) in a
// single critical section. Once a write fails, subsequent writes are no-ops
// returning the stored error (the stream is dead).
func (s *streamWriter) writeFramed(payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	buf := make([]byte, connectFrameHeader+len(payload))
	// buf[0] flags = 0 (uncompressed data frame).
	binary.BigEndian.PutUint32(buf[1:], uint32(len(payload)))
	copy(buf[connectFrameHeader:], payload)
	if _, err := s.w.Write(buf); err != nil {
		s.err = err
		return err
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
// A nil/empty blobData omits the field, signaling not-found to the server.
func encodeKvGetBlobResult(id uint64, blobData []byte) []byte {
	var res []byte
	if len(blobData) > 0 {
		res = appendBytes(res, fGetBlobResultBlobData, blobData)
	}
	var kv []byte
	kv = appendUint32(kv, fKvMessageID, uint32(id))
	kv = appendMessage(kv, fKvClientGetBlobResult, res)
	return appendMessage(nil, fAgentClientMessageKvClientMessage, kv)
}

// encodeKvSetBlobAck builds AgentClientMessage{ kv_client_message=3 { id, set_blob_result=3 {} } }.
func encodeKvSetBlobAck(id uint64) []byte {
	var kv []byte
	kv = appendUint32(kv, fKvMessageID, uint32(id))
	kv = appendMessage(kv, fKvClientSetBlobResult, nil)
	return appendMessage(nil, fAgentClientMessageKvClientMessage, kv)
}

// encodeMcpResult builds the reply that delivers a real tool result into a
// parked turn:
// AgentClientMessage{ exec_client_message=2 { id, exec_id?, mcp_result=11 McpResult } }.
// Success:  mcp_result{ success=1 { content=1 { text=1 { text=1 result } }, is_error=2 false } }
// Error:    mcp_result{ error=2 { error=1 result } }
func encodeMcpResult(id uint64, execID []byte, result string, isError bool) []byte {
	var mcpResult []byte
	if isError {
		var mcpErr []byte
		mcpErr = appendString(mcpErr, 1, result)  // McpError.error = 1
		mcpResult = appendMessage(nil, 2, mcpErr) // McpResult.error = 2
	} else {
		text := appendString(nil, 1, result) // McpTextContent.text = 1
		item := appendMessage(nil, 1, text)  // McpToolResultContentItem.text = 1
		var success []byte
		success = appendMessage(success, 1, item) // McpSuccess.content = 1 (repeated)
		// is_error = 2 stays false (omitted).
		mcpResult = appendMessage(nil, 1, success) // McpResult.success = 1
	}
	var ex []byte
	ex = appendUint32(ex, fExecMessageID, uint32(id))
	if len(execID) > 0 {
		ex = appendBytes(ex, fExecMessageExecID, execID)
	}
	ex = appendMessage(ex, fExecClientMcpResult, mcpResult)
	return appendMessage(nil, fAgentClientMessageExecClientMessage, ex)
}

// encodeRequestContextResult builds
// AgentClientMessage{ exec_client_message=2 ExecClientMessage{ id, exec_id?, request_context_result=10 { success=1 { request_context=1 {} } } } }.
// The RequestContext is empty (a text bridge has no workspace/rules/tools to
// advertise); the proven-minimal reply is success{ request_context{} }.
func encodeRequestContextResult(id uint64, execID []byte) []byte {
	success := appendMessage(nil, fRequestContextSuccessInner, nil) // empty RequestContext{}
	result := appendMessage(nil, fRequestContextResultSucc, success)
	var ex []byte
	ex = appendUint32(ex, fExecMessageID, uint32(id))
	if len(execID) > 0 {
		ex = appendBytes(ex, fExecMessageExecID, execID)
	}
	ex = appendMessage(ex, fExecClientRequestContext, result)
	return appendMessage(nil, fAgentClientMessageExecClientMessage, ex)
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
			blobs.put(bid.raw, bdata.raw)
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
			outCall.execIDStr = execID
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
			if k, v, ok := decodeStringValueMapEntry(a.raw); ok {
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
// sha256Digest in cursor.go is the single owner of the digest.
func cursorBlobsForHistory(digests, blobs [][]byte) *KVBlobStore {
	s := newKVBlobStore()
	for i := range blobs {
		if i < len(digests) {
			s.put(digests[i], blobs[i])
		}
	}
	return s
}
