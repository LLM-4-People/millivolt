package format

// OpenAI Chat Completions <-> Cursor agent.v1 (Connect-RPC + protobuf)
// translation. Selected per request via X-Proxy-Format: cursor.
//
// The Cursor backend is a Connect-RPC service (agent.v1.AgentService) carrying
// hand-decoded protobuf messages over HTTP/2. The chat RPC is Run:
//
//	POST /agent.v1.AgentService/Run
//	content-type: application/connect+proto
//	connect-protocol-version: 1
//
// The request body is a single enveloped AgentClientMessage; the response is a
// Connect stream of enveloped AgentServerMessage frames terminated by a JSON
// EndStreamResponse frame. Field numbers below come from the decoded agent.v1
// descriptor (see AGENTS.md / the design notes); they are the stable wire
// contract, not user-tunable, so they live as named constants here.
//
// Scope: OpenAI chat + tool calling over agent.v1 Run. History is sha256
// blobs on the KV channel; client tools are declared as McpTools and
// answered on the parked stream. Multimodal parts are out of scope.
// Fail-closed throughout: a malformed frame or an unrecognized terminal
// body is an error, never a fabricated empty success.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// agent.v1 protobuf field numbers (wire contract - do not change without the
// upstream schema). Only the fields we emit/decode are listed.
const (
	// AgentClientMessage.run_request -> AgentRunRequest
	fAgentClientMessageRunRequest = 1

	// AgentRunRequest fields
	fRunRequestConversationState    = 1  // ConversationStateStructure
	fRunRequestAction               = 2  // ConversationAction
	fRunRequestModelDetails         = 3  // ModelDetails
	fRunRequestMcpTools             = 4  // McpTools (always emitted, may be empty)
	fRunRequestConversationID       = 5  // string
	fRunRequestRequestedModel       = 9  // RequestedModel
	fRunRequestConversationIDMirror = 16 // string (mirrors field 5 on the wire)

	// ConversationStateStructure.root_prompt_messages_json (repeated bytes)
	fConversationStateRootPromptMessagesJSON = 1

	// ConversationAction.user_message_action -> UserMessageAction
	fConversationActionUserMessageAction = 1
	// ConversationAction.resume_action = ResumeAction (sent empty). Used when a
	// continuation's history ends on a tool result / assistant turn - no fresh
	// user text to answer; real clients resume instead of re-answering.
	fConversationActionResumeAction = 2
	// UserMessageAction.user_message -> UserMessage
	fUserMessageActionUserMessage = 1

	// UserMessage fields
	fUserMessageText            = 1 // string
	fUserMessageMessageID       = 2 // string
	fUserMessageSelectedContext = 3 // SelectedContext (empty)
	fUserMessageMode            = 4 // int32

	// ModelDetails.model_id
	fModelDetailsModelID = 1 // string

	// RequestedModel fields
	fRequestedModelModelID    = 1 // string
	fRequestedModelMaxMode    = 2 // bool
	fRequestedModelParameters = 3 // repeated RequestedModel_ModelParameterbytes
	// RequestedModel_ModelParameterbytes fields (all values are strings).
	fModelParameterID    = 1 // string ("effort"|"reasoning"|"thinking"|"fast"|"context")
	fModelParameterValue = 2 // string

	// AgentServerMessage oneof members we decode.
	fAgentServerMessageInteractionUpdate = 1 // InteractionUpdate
	// AgentServerMessage.conversation_checkpoint_update ->
	// ConversationStateStructure. The server pushes this as the conversation
	// state checkpoints; it carries token_details, the ONLY accurate
	// context-window occupancy on the wire.
	fAgentServerMessageConversationCheckpointUpdate = 3

	// InteractionUpdate oneof members we decode (field numbers per the
	// canonical agent.v1 proto). NOTE: there is NO "usage summary" message -
	// an earlier mapping read InteractionUpdate field 14 as a prompt/output/
	// reasoning summary, but field 14 is TurnEndedUpdate (an EMPTY marker that
	// only signals the turn ended). Decoding it as a summary misaligned the
	// wire and produced the runaway multi-million "prompt_tokens" that made
	// clients compact. Per-turn output is TokenDeltaUpdate; per-turn context
	// occupancy is token_details on the checkpoint update.
	fInteractionUpdateTextDelta     = 1  // TextDeltaUpdate
	fInteractionUpdateThinkingDelta = 4  // ThinkingDeltaUpdate
	fInteractionUpdateTokenDelta    = 8  // TokenDeltaUpdate
	fInteractionUpdateTurnEnded     = 14 // TurnEndedUpdate (empty completion marker)

	// TextDeltaUpdate.text / ThinkingDeltaUpdate.text
	fTextDeltaText     = 1 // string
	fThinkingDeltaText = 1 // string

	// TokenDeltaUpdate.tokens - a PER-TURN output increment (answer +
	// reasoning), the only token counter on the InteractionUpdate channel.
	fTokenDeltaTokens = 1 // int32

	// ConversationStateStructure.token_details -> ConversationTokenDetails.
	fConversationStateTokenDetails = 5
	// ConversationTokenDetails.used_tokens - the conversation's CURRENT
	// context-window occupancy (the real prompt size the model is working
	// with). This is the value clients should use for context management.
	fTokenDetailsUsedTokens = 1 // uint32
)

// cursorConnectPath is the Connect-RPC route for the chat RPC. The proxy
// points the upstream here when format=cursor and no explicit X-Proxy-Path is
// given (an explicit path always wins, for forward-compat).
const cursorConnectPath = "/agent.v1.AgentService/Run"

// CursorConnectPath exposes the canonical Connect-RPC route so the proxy layer
// can default the upstream path for format=cursor without duplicating the
// literal.
func CursorConnectPath() string { return cursorConnectPath }

// cursorAssistantBlob builds the structured history blob for an assistant turn.
// Cursor's prompt blobs use a Vercel-AI-SDK message shape: content is an array
// of {type:"text",text} and {type:"tool-call",toolCallId,toolName,args} parts.
// cursorMessageText extracts the text a cursor history blob or action needs:
// plain string content, or an array of {type:"text"} parts joined; other part
// kinds (file/image/audio) degrade to a bracketed placeholder so the message
// never silently vanishes. Array content is what clients send for multipart
// messages; dropping it meant the model never saw those turns.
func cursorMessageText(m openaiMessage) string {
	if s, ok := m.ContentString(); ok {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if json.Unmarshal(m.Content, &parts) != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text", "text/plain":
			sb.WriteString(p.Text)
		default:
			label := p.Type
			if label == "" {
				label = "file"
			}
			if p.Name != "" {
				label += ":" + p.Name
			}
			sb.WriteString("[" + label + "]")
		}
	}
	return sb.String()
}

func cursorAssistantBlob(text string, calls []openaiToolCall) map[string]any {
	var parts []map[string]any
	if text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	for _, c := range calls {
		// args must never be JSON null, a bare string, or a bare scalar: the
		// server maps over it ("Cannot read properties of null (reading
		// 'map')" - a live crash class). cursor2api's parseArgs contract: {}
		// when empty, {_raw: raw} for anything that isn't a JSON object.
		var args any = map[string]any{}
		if raw := strings.TrimSpace(c.Function.Arguments); raw != "" && raw != "null" {
			var parsed any
			if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
				if _, ok := parsed.(map[string]any); ok {
					args = parsed
				} else {
					args = map[string]any{"_raw": raw}
				}
			} else {
				args = map[string]any{"_raw": raw}
			}
		}
		parts = append(parts, map[string]any{
			"type":       "tool-call",
			"toolCallId": c.ID,
			"toolName":   c.Function.Name,
			"args":       args,
		})
	}
	return map[string]any{"role": "assistant", "content": parts}
}

// cursorToolResultBlob builds the structured history blob for a role:"tool"
// result: {role:"tool", id:<toolCallId>, content:[{type:"tool-result",…}]}.
func cursorToolResultBlob(name, toolCallID, content string) map[string]any {
	return map[string]any{
		"role": "tool",
		"id":   toolCallID,
		"content": []map[string]any{{
			"type":       "tool-result",
			"toolCallId": toolCallID,
			"toolName":   name,
			"result":     content,
		}},
	}
}

// TranslateCursorRunRequestWithConversation builds the run_request, reusing
// conversationID when non-empty so turns of one logical conversation link
// server-side (the server keys conversation state and the KV blob channel on
// it). The history blobs use Cursor's structured message shapes (Vercel-AI-SDK
// style {type:"tool-call"}/{type:"tool-result"} parts) so tool call/result
// pairs stay legible to the model - matching what real Cursor clients send.
func TranslateCursorRunRequestWithConversation(openaiBody []byte, conversationID string) (clientMessage []byte, blobs *KVBlobStore, resume bool, err error) {
	return translateCursorRunRequest(openaiBody, conversationID, false)
}

// TranslateCursorRunRequestFreshUser is the one-shot re-ask variant: the same
// request, but the action is a fresh user_message_action carrying the last
// user text even though the history ends on a tool result/assistant turn. This
// is the recovery for a resume-action continuation that returned an empty turn
// (the "void"): the model answers the last question as a fresh turn instead of
// resuming nothing. Requires a non-empty last user message - without text to
// act on there is nothing to re-ask.
func TranslateCursorRunRequestFreshUser(openaiBody []byte) (clientMessage []byte, blobs *KVBlobStore, err error) {
	clientMessage, blobs, _, err = translateCursorRunRequest(openaiBody, "", true)
	return clientMessage, blobs, err
}

// translateCursorRunRequest builds the run_request, reusing conversationID
// when non-empty so turns of one logical conversation link server-side (the
// server keys conversation state and the KV blob channel on it). The history
// blobs use Cursor's structured message shapes (Vercel-AI-SDK style
// {type:"tool-call"}/{type:"tool-result"} parts) so tool call/result pairs
// stay legible to the model - matching what real Cursor clients send.
// forceUserTurn overrides the resume-action decision for the one-shot re-ask.
func translateCursorRunRequest(openaiBody []byte, conversationID string, forceUserTurn bool) (clientMessage []byte, blobs *KVBlobStore, resume bool, err error) {
	var in struct {
		Model    string          `json:"model"`
		Messages []openaiMessage `json:"messages"`
		Stream   bool            `json:"stream"`
		Tools    []openaiTool    `json:"tools"`
	}
	if err := json.Unmarshal(openaiBody, &in); err != nil {
		return nil, nil, false, fmt.Errorf("parse openai body: %w", err)
	}
	if strings.TrimSpace(in.Model) == "" {
		return nil, nil, false, fmt.Errorf("model is required for cursor format")
	}

	// Build structured history blobs (one per prior turn) and find the current
	// user message (the last user turn). System/developer text travels as the
	// FIRST history blob(s) - what every real Cursor client does (the native
	// custom_system_prompt field is allowlist-gated, and no client prepends
	// system text into the action).
	var blobObjs []any // one JSON object per prior turn
	for _, m := range in.Messages {
		if m.Role == "system" || m.Role == "developer" {
			if s := cursorMessageText(m); s != "" {
				blobObjs = append(blobObjs, map[string]any{"role": "system", "content": s})
			}
		}
	}
	lastUser := -1
	for i, m := range in.Messages {
		if m.Role == "user" {
			lastUser = i
		}
	}
	// Continuation without a fresh user text (the history ends on a tool result
	// or assistant turn): real Cursor clients send ConversationAction
	// .resume_action { } (empty ResumeAction) - the model continues from the
	// blobs. Sending the ORIGINAL user question as the action instead makes the
	// model "answer the void" and re-answer the old question (cursor2api's live
	// never-converging loop bug).
	resumeAction := (len(in.Messages) > 0 && in.Messages[len(in.Messages)-1].Role != "user") && !forceUserTurn
	for i, m := range in.Messages {
		if !resumeAction && i == lastUser {
			continue // the current message goes in the action, not history
		}
		s := cursorMessageText(m)
		switch m.Role {
		case "system", "developer":
			// already blobs above (first in order)
		case "assistant":
			// Skip empty assistant messages (content "" with no tool calls):
			// canonical clients omit them rather than shipping content:[].
			if s == "" && len(m.ToolCalls) == 0 {
				continue
			}
			blobObjs = append(blobObjs, cursorAssistantBlob(s, m.ToolCalls))
		case "tool":
			// Emit even when empty: the paired assistant tool-call part is
			// already in history; dropping the result would replay an orphaned
			// call server-side (cursor2api's documented guard).
			blobObjs = append(blobObjs, cursorToolResultBlob(m.Name, m.ToolCallID, s))
		case "user":
			if s == "" {
				continue
			}
			blobObjs = append(blobObjs, map[string]any{"role": "user", "content": s})
		default:
			if s != "" {
				blobObjs = append(blobObjs, map[string]any{"role": "user", "content": s})
			}
		}
	}
	current := ""
	if lastUser >= 0 {
		current = cursorMessageText(in.Messages[lastUser])
	}
	// A trailing "user" with empty text is not a real fresh turn - resume
	// instead of emitting an empty UserMessageAction (the "answer the void"
	// failure class); a request with nothing to act on at all is rejected.
	// A forced re-ask (forceUserTurn) cannot fall back: without real user text
	// there is nothing to re-ask.
	if forceUserTurn && current == "" {
		return nil, nil, false, fmt.Errorf("re-ask requires a last user message with text")
	}
	if current == "" && !resumeAction {
		if len(blobObjs) == 0 {
			return nil, nil, false, fmt.Errorf("message content is required for cursor format")
		}
		resumeAction = true
	}

	// History: one content-addressed blob per prior turn. root_prompt_messages_json
	// carries the sha256 DIGEST of each blob (the id the server pulls); the blob
	// contents are stored in the KV blob store so get_blob pulls resolve.
	var convState []byte
	var digests, contents [][]byte
	for _, obj := range blobObjs {
		blob, err := json.Marshal(obj)
		if err != nil {
			return nil, nil, false, fmt.Errorf("marshal history blob: %w", err)
		}
		d := sha256Digest(blob)
		digests = append(digests, d)
		contents = append(contents, blob)
		convState = appendBytes(convState, fConversationStateRootPromptMessagesJSON, d)
	}
	blobs = cursorBlobsForHistory(digests, contents)

	// UserMessage: text + a UUID message_id + empty selected_context + mode=1
	// (matches the captured real client shape).
	var action []byte
	if resumeAction {
		// ConversationAction.resume_action = 2 (empty ResumeAction - every
		// verified bridge sends it empty).
		action = appendMessage(nil, fConversationActionResumeAction, nil)
	} else {
		action = encodeUserMessageAction(current)
	}

	if conversationID == "" {
		conversationID = UUID4() // fresh per conversation
	}
	requestedModel := cursorRequestedModel(in.Model)

	var runRequest []byte
	runRequest = appendMessage(runRequest, fRunRequestConversationState, convState)
	runRequest = appendMessage(runRequest, fRunRequestAction, action)
	// Client-declared tools (OpenAI function defs) become MCP tool definitions
	// so the model can call them; always emit the (possibly empty) McpTools.
	runRequest = appendMessage(runRequest, fRunRequestMcpTools, cursorMcpTools(in.Tools))
	runRequest = appendString(runRequest, fRunRequestConversationID, conversationID)
	runRequest = appendString(runRequest, fRunRequestConversationIDMirror, conversationID)
	runRequest = appendMessage(runRequest, fRunRequestRequestedModel, requestedModel)

	clientMessage = appendMessage(nil, fAgentClientMessageRunRequest, runRequest)
	return clientMessage, blobs, resumeAction, nil
}

// encodeUserMessageAction builds a ConversationAction.user_message_action for
// the given user text: text + a UUID message_id + empty selected_context +
// mode=1 (matches the captured real client shape). Used both for a fresh run's
// opening action AND to steer a user message into a parked run mid-turn.
func encodeUserMessageAction(text string) []byte {
	userMessage := appendString(nil, fUserMessageText, text)
	userMessage = appendString(userMessage, fUserMessageMessageID, UUID4())
	userMessage = appendMessage(userMessage, fUserMessageSelectedContext, nil)
	userMessage = appendInt32(userMessage, fUserMessageMode, 1)

	userMessageAction := appendMessage(nil, fUserMessageActionUserMessage, userMessage)
	return appendMessage(nil, fConversationActionUserMessageAction, userMessageAction)
}

// CursorTrailingUserText returns the text of the request's LAST message when it
// is a non-empty role:"user" turn - i.e. a message the user typed alongside
// (after) the tool results. The cursor resume path steers this into the parked
// run; without it the resume would forward only the tool results and the user's
// text would never reach the model. Returns "" when the request ends on a
// tool/assistant turn (a pure resume with nothing to steer).
//
// It reuses the SAME canonical text extraction as the fresh-run action
// (cursorMessageText): string content, or text parts joined, with non-text
// parts rendered as [type]/[type:name] placeholders - so a multimodal user
// message still steers meaningful text. Never parses a parallel/ weaker shape.
func CursorTrailingUserText(openaiBody []byte) string {
	var in struct {
		Messages []openaiMessage `json:"messages"`
	}
	if json.Unmarshal(openaiBody, &in) != nil || len(in.Messages) == 0 {
		return ""
	}
	last := in.Messages[len(in.Messages)-1]
	if last.Role != "user" {
		return ""
	}
	return strings.TrimSpace(cursorMessageText(last))
}

// cursorMcpTools converts OpenAI function-tool definitions into Cursor's
// McpTools message (McpTools.mcp_tools = repeated McpToolDefinition). The JSON
// schema goes in input_schema_json (field 6); name/tool_name/provider follow
// the captured real-client shape.
func cursorMcpTools(tools []openaiTool) []byte {
	var mcpTools []byte
	for _, t := range tools {
		if t.Function.Name == "" {
			continue
		}
		var def []byte
		def = appendString(def, fMcpToolDefName, t.Function.Name)
		def = appendString(def, fMcpToolDefDescription, t.Function.Description)
		def = appendString(def, fMcpToolDefProviderID, "openai")
		def = appendString(def, fMcpToolDefToolName, t.Function.Name)
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = []byte(`{"type":"object","properties":{}}`)
		}
		def = appendString(def, fMcpToolDefInputSchemaJSON, string(schema))
		mcpTools = appendMessage(mcpTools, fMcpToolsList, def)
	}
	return mcpTools
}

// sha256Digest returns the raw 32-byte sha256 digest of b.
func sha256Digest(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// cursorRequestedModel builds the RequestedModel for a usable-model id,
// decomposing a FUSED display id into its base wire model_id plus the full
// parameter set the real client sends. The wire contract (verified against
// cursor-bridge's byte-exact captures of the real CLI) is:
//
//	model_id   = base wire id (tier/fast/thinking suffixes stripped)
//	max_mode   = 1 only for max-mode variants (the "-…-fast" catalog ids)
//	parameters = thinking? + context + level + fast  (family-ordered)
//
// The full parameter list - including the context tier and an explicit
// fast=false - is required: a partial set is rejected with failed_precondition,
// and a fused id as model_id is rejected with not_found. The context tier is
// per-model-family wire data (a property of the Cursor model spec, not a
// user-facing price table); the level parameter name is per-family (effort for
// Claude/Grok/Gemini/Composer, reasoning for GPT/Kimi/GLM). Ids with no tier
// (default/auto/claude-4-sonnet) pass through with no parameters.
func cursorRequestedModel(modelID string) []byte {
	base, params, maxMode := cursorSplitModelID(modelID)

	var rm []byte
	rm = appendString(rm, fRequestedModelModelID, base)
	if maxMode {
		rm = appendBool(rm, fRequestedModelMaxMode, true)
	}
	for _, p := range params {
		var pb []byte
		pb = appendString(pb, fModelParameterID, p[0])
		pb = appendString(pb, fModelParameterValue, p[1])
		rm = appendMessage(rm, fRequestedModelParameters, pb)
	}
	return rm
}

// cursorSplitModelID decomposes a fused usable-model id into (base model_id,
// ordered [param,value] pairs, maxMode). Decomposition is driven by the id's
// own tokens plus the per-family wire constants - no static per-model table.
func cursorSplitModelID(modelID string) (string, [][2]string, bool) {
	id := strings.ToLower(modelID)

	// Split -thinking FIRST: fused ids put thinking either as a terminal bare
	// suffix (claude-4.6-opus-max-thinking) or before a level; the remaining
	// left side carries the tier/fast suffixes.
	fast := strings.HasSuffix(id, "-fast")
	thinking := strings.Contains(id, "-thinking-") || strings.HasSuffix(id, "-thinking")
	rest := id
	if fast {
		rest = strings.TrimSuffix(rest, "-fast")
	}
	if strings.HasSuffix(rest, "-thinking") {
		rest = strings.TrimSuffix(rest, "-thinking")
	}
	tier := ""
	for _, t := range []string{"extra-high", "xhigh", "minimal", "medium", "none", "high", "low", "max"} {
		if strings.HasSuffix(rest, "-"+t) {
			tier = t
			rest = strings.TrimSuffix(rest, "-"+t)
			break
		}
	}
	if tier == "" && !thinking && !fast {
		// Not a tiered id - but still normalize base-id quirks (cursor- prefix,
		// auto→default, dotted versions) since those appear on plain ids too.
		return cursorNormalizeBaseID(modelID), nil, false
	}
	// A "-thinking" bridge between base and tier comes off before normalization.
	rest = strings.TrimSuffix(rest, "-thinking")
	base := cursorNormalizeBaseID(rest)

	// Per-family wire constants (from the live catalog; family-level, not a
	// per-model table): which param carries the level, and which families
	// declare an explicit fast parameter.
	reasoningFamily := strings.HasPrefix(base, "gpt-") || strings.HasPrefix(base, "kimi-") || strings.HasPrefix(base, "glm-")
	levelParam := "effort"
	if reasoningFamily {
		levelParam = "reasoning"
	}
	// Catalog-declared fast families: grok, gpt*, composer, claude opus
	// 4.7/4.8/5. glm/kimi declare reasoning only - an unsolicited fast param
	// risks failed_precondition.
	fastFamily := strings.HasPrefix(base, "grok-") || strings.HasPrefix(base, "gpt-") ||
		strings.HasPrefix(base, "composer-") ||
		strings.Contains(base, "opus-4-7") || strings.Contains(base, "opus-4-8") || strings.Contains(base, "opus-5")

	var params [][2]string
	if thinking {
		params = append(params, [2]string{"thinking", "true"})
	}
	if ctx := cursorContextTier(id); ctx != "" {
		params = append(params, [2]string{"context", ctx})
	}
	if tier != "" {
		params = append(params, [2]string{levelParam, tier})
	}
	if fast {
		params = append(params, [2]string{"fast", "true"})
	} else if fastFamily {
		params = append(params, [2]string{"fast", "false"})
	}
	return base, params, fast && tier == "max"
}

// cursorNormalizeBaseID maps a catalog base id to the wire model_id, handling
// the known naming quirks (verified against cursor-bridge's model-map.json):
// the "cursor-" provider prefix is dropped (cursor-grok-4.6 → grok-4.6), the
// "auto" picker maps to "default", and Claude 4.x dotted versions move the
// version after the family (claude-4.5-sonnet → claude-sonnet-4-5). Claude 5+
// ids (claude-sonnet-5, claude-fable-5) and other families are already wire
// ids and pass through.
func cursorNormalizeBaseID(base string) string {
	b := strings.ToLower(base)
	b = strings.TrimPrefix(b, "cursor-")
	if b == "auto" {
		return "default"
	}
	// claude-4.5-sonnet → claude-sonnet-4-5 (dotted 4.x version moves after the
	// family name, dots become dashes). Matches "claude-<maj>.<min>-<name>".
	if strings.HasPrefix(b, "claude-") {
		rest := strings.TrimPrefix(b, "claude-")
		// e.g. "4.5-sonnet", "4.6-opus"
		if i := strings.Index(rest, "-"); i > 0 {
			ver := rest[:i]    // "4.5"
			name := rest[i+1:] // "sonnet"
			if strings.Contains(ver, ".") {
				return "claude-" + name + "-" + strings.ReplaceAll(ver, ".", "-")
			}
		}
	}
	return b
}

// CursorModelBase returns the canonical base model id of a Cursor model id for
// recording and display: the fused display id with the thinking level, tier,
// and -fast suffixes stripped and base-id quirks normalized
// (claude-4.5-sonnet-thinking → claude-sonnet-4-5), so a Cursor model groups
// with the same model from other providers. It shares cursorSplitModelID's
// decomposition (single owner), so the recorded name always equals the wire
// model_id. Ids the decomposition leaves untouched pass through lowercased.
func CursorModelBase(modelID string) string {
	base, _, _ := cursorSplitModelID(modelID)
	return base
}

// cursorContextTier returns the context-window tier string ("300k"/"272k"/
// "200k") for a model family, or "" when the family carries no context param.
// This is per-model-family wire data from Cursor's model spec (the same value
// the real CLI sends), not a user-facing price/limit table.
func cursorContextTier(lowerID string) string {
	switch {
	case strings.Contains(lowerID, "claude-sonnet-5"),
		strings.Contains(lowerID, "claude-fable-5"),
		strings.Contains(lowerID, "claude-opus-5"),
		strings.Contains(lowerID, "claude-opus-4-7"),
		strings.Contains(lowerID, "claude-opus-4-8"):
		return "300k"
	case strings.HasPrefix(lowerID, "gpt-5.4"),
		strings.HasPrefix(lowerID, "gpt-5.5"),
		strings.HasPrefix(lowerID, "gpt-5.6"):
		return "272k"
	case strings.Contains(lowerID, "claude-sonnet-4"),
		strings.Contains(lowerID, "claude-4-5"),
		strings.Contains(lowerID, "claude-4.5"),
		strings.Contains(lowerID, "claude-opus-4-6"),
		strings.Contains(lowerID, "claude-4-6"),
		strings.Contains(lowerID, "claude-4.6"):
		return "200k"
	}
	return ""
}

// UUID4 returns a random RFC 4122 version-4 UUID. One owner for Cursor
// message/conversation ids (the real client uses randomUUID()) and the
// proxy's X-Request-Id / cursor record ids.
func UUID4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a time-derived value; uniqueness within a process is enough.
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		copy(b[:], sum[:16])
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
