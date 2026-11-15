package format

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// Target tool vocabularies. Chat function names are 1-64 characters of
// [a-zA-Z0-9_-]; Anthropic tool names are 1-128 of the same class, and
// Anthropic tool_use ids / tool_use_id references are non-empty of that class.
// A cross-protocol encoder validates the whole request before emitting a wire
// the target would reject.
var (
	// openAIIdentifierPattern is the shared OpenAI identifier class: Chat
	// function names and the json_schema response_format name both document
	// 1-64 characters of this set.
	openAIIdentifierPattern  = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	anthropicToolNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
	anthropicToolIDPattern   = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

func toolVocabularyError(target, field, value, pattern string) error {
	return fmt.Errorf("%s %s %q does not match %s", target, field, boundedEcho(value), pattern)
}

// droppableProviderTools are the Responses tool types a provider executes
// itself. They are optional to the client's remaining function tools and have
// no cross-protocol equivalent, so a translated request drops one with a
// warning instead of failing the turn. An unknown type still fails closed: a
// tool this proxy cannot classify must not be silently discarded.
var droppableProviderTools = map[string]bool{
	"web_search":                true,
	"web_search_preview":        true,
	"file_search":               true,
	"code_interpreter":          true,
	"image_generation":          true,
	"computer":                  true,
	"computer_use_preview":      true,
	"mcp":                       true,
	"tool_search":               true,
	"programmatic_tool_calling": true,
	// A custom tool carries freeform input with a grammar instead of a JSON
	// schema. Chat has no equivalent, and a client that declared it expects a
	// custom_tool_call item back, which a function call cannot satisfy, so the
	// tool is dropped rather than declared in a shape the client cannot use.
	"custom": true,
}

// toolNameSeparator joins a Responses namespace and its tool name into the flat
// spelling a target without namespace support receives. Two underscores keep
// the result inside the Chat function-name class, and the namer below resolves
// the one ambiguous case where two pairs flatten to the same string.
const toolNameSeparator = "__"

// toolNameMax bounds a flattened name to the shortest target class, the Chat
// function-name limit. A longer pair is truncated and suffixed with a hash so
// the spelling stays deterministic for the same pair.
const toolNameMax = 64

// toolNameHashLen is the number of hex characters appended to a truncated flat
// name, sized to make a collision between two distinct pairs negligible.
const toolNameHashLen = 8

// flatToolName is the preferred flat spelling for one namespace pair. A pair
// whose joined spelling exceeds the target class is truncated and suffixed with
// a hash of the pair, so the encoding is deterministic and collision-resistant.
func flatToolName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	joined := namespace + toolNameSeparator + name
	if len(joined) <= toolNameMax {
		return joined
	}
	sum := sha256.Sum256([]byte(namespace + "\x00" + name))
	digest := hex.EncodeToString(sum[:])[:toolNameHashLen]
	keep := toolNameMax - toolNameHashLen - 1
	if keep < 1 {
		keep = 1
	}
	return truncateToolName(joined, keep) + "_" + digest
}

// truncateToolName cuts a name to at most max characters without splitting a
// multi-byte rune, so a non-ASCII client name still marshals as valid UTF-8.
func truncateToolName(name string, max int) string {
	if len(name) <= max {
		return name
	}
	runes := []rune(name)
	if len(runes) <= max {
		return name
	}
	return string(runes[:max])
}

// toolNamer assigns one unique flat name per declared tool in a request. The
// preferred spelling wins; a later pair that flattens to the same string gets a
// numeric suffix, so every declared tool stays addressable and the returned
// name always resolves back to exactly one tool.
type toolNamer struct {
	used map[string]string
}

func newToolNamer() *toolNamer {
	return &toolNamer{used: map[string]string{}}
}

// claim records one exact spelling as taken without renaming it. A plain tool
// keeps the name the client declared, even when the client declares it twice; a
// later namespaced pair that flattens to that spelling takes a suffix instead.
func (n *toolNamer) claim(name string) string {
	if _, ok := n.used[name]; !ok {
		n.used[name] = "plain\x00" + name
	}
	return name
}

// toolIdentity keys one declared tool by its namespace and declared name.
func toolIdentity(namespace, name string) string { return namespace + "\x00" + name }

// assignToolNames owns the flat spelling of every declared tool in a request.
// Plain tools keep their declared name and are claimed first, so a namespace
// pair that flattens to the same string always takes the suffix no matter which
// array declared it. The returned map resolves the same pair later, for a named
// tool choice or a replayed namespaced call, to the exact spelling the model
// saw; a pair the request did not declare falls back to flatToolName.
func assignToolNames(tools []ir.Tool) map[string]string {
	namer := newToolNamer()
	for _, tool := range tools {
		if tool.Namespace == "" {
			namer.claim(tool.Name)
		}
	}
	assigned := make(map[string]string, len(tools))
	for i := range tools {
		tool := &tools[i]
		if tool.Namespace == "" {
			tool.WireName = tool.Name
			continue
		}
		tool.WireName = namer.assign(tool.Namespace, tool.Name)
		assigned[toolIdentity(tool.Namespace, tool.Name)] = tool.WireName
	}
	return assigned
}

// resolveToolName returns the declared pair's assigned spelling, or the
// preferred flat spelling when this request declared no such pair.
func resolveToolName(assigned map[string]string, namespace, name string) string {
	if wire, ok := assigned[toolIdentity(namespace, name)]; ok {
		return wire
	}
	return flatToolName(namespace, name)
}

// assign returns the unique flat name for one pair. identity identifies the
// pair so a repeat of the same pair returns its first assignment instead of
// taking a new suffix.
func (n *toolNamer) assign(namespace, name string) string {
	base := flatToolName(namespace, name)
	identity := namespace + "\x00" + name
	if got, ok := n.used[base]; ok {
		if got == identity {
			return base
		}
		for i := 2; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			candidate := base
			if len(candidate)+len(suffix) > toolNameMax {
				candidate = truncateToolName(candidate, toolNameMax-len(suffix))
			}
			candidate += suffix
			if got, ok := n.used[candidate]; !ok || got == identity {
				if !ok {
					n.used[candidate] = identity
				}
				return candidate
			}
		}
	}
	n.used[base] = identity
	return base
}

// checkToolCallName enforces the IR's non-empty tool-name invariant at the
// streaming decoder owners. Each target encoder still owns its name-class
// check; this owner keeps an empty streamed name from reaching any target
// wire, where a per-target encoder might only notice it after bytes were
// already emitted.
func checkToolCallName(name string) error {
	if name == "" {
		return errors.New("tool name is required")
	}
	return nil
}

// requestToolIDs is one request-scoped namespace for tool-call ids across every
// message and item. A request decoder pre-reserves every explicit id before it
// decodes any message or item, then allocates synthetic ids from the same set,
// so a generated id can never collide with an explicit one anywhere in the
// request, including a later message. A nil namespace disables the check for
// response decoders, whose stream counterparts already own their own set.
type requestToolIDs struct {
	seen map[string]struct{}
	// preReserved holds the explicit ids claimed by the request pre-scan. The
	// decode pass re-claims its own id from here instead of treating the
	// pre-reservation as a duplicate.
	preReserved map[string]struct{}
}

func newRequestToolIDs() *requestToolIDs {
	return &requestToolIDs{
		seen:        map[string]struct{}{},
		preReserved: map[string]struct{}{},
	}
}

// preReserve claims one explicit id before any message or item is decoded. A
// repeat fails closed here, before a generated id can be mistaken for one.
func (r *requestToolIDs) preReserve(id string) error {
	if r == nil {
		return nil
	}
	if err := reserveToolID(r.seen, id); err != nil {
		return err
	}
	r.preReserved[id] = struct{}{}
	return nil
}

// reserve claims one explicit id during decoding. An id already claimed by the
// request pre-scan belongs to this call and is consumed; any other repeat fails
// closed. An explicit id is never suffixed: that would break the client's
// correlation with the provider's tool result.
func (r *requestToolIDs) reserve(id string) error {
	if r == nil {
		return nil
	}
	if _, ok := r.preReserved[id]; ok {
		delete(r.preReserved, id)
		return nil
	}
	return reserveToolID(r.seen, id)
}

// synthesize claims the first free spelling of base. The suffix starts at 1 so
// a generated call_3_0 becomes call_3_0_1 when an explicit call_3_0 already
// exists.
func (r *requestToolIDs) synthesize(base string) string {
	if r == nil {
		return base
	}
	id := base
	for suffix := 1; ; suffix++ {
		if _, taken := r.seen[id]; !taken {
			break
		}
		id = fmt.Sprintf("%s_%d", base, suffix)
	}
	r.seen[id] = struct{}{}
	return id
}

// reserveToolID records one committed tool-call id and fails closed when the
// same id was already committed. The IR correlates a tool result by id, so two
// target blocks sharing one id are ambiguous. A source id is never suffixed:
// that would break the client's correlation with the provider's tool result.
func reserveToolID(seen map[string]struct{}, id string) error {
	if _, taken := seen[id]; taken {
		return fmt.Errorf("duplicate tool call id %q", boundedEcho(id))
	}
	seen[id] = struct{}{}
	return nil
}

// validateResponseToolIDs enforces the same target-correlation contract on a
// non-streaming response that reserveToolID enforces while streaming: two
// tool-call blocks sharing one id make the client's tool result ambiguous.
// The validator lives at the shared encode boundary so every target protocol
// and every caller of EncodeResponse inherits it.
func validateResponseToolIDs(resp *ir.Response) error {
	if resp == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(resp.Blocks))
	for _, block := range resp.Blocks {
		if block.Type != ir.BlockToolCall || block.ToolCall == nil {
			continue
		}
		if err := reserveToolID(seen, block.ToolCall.ID); err != nil {
			return err
		}
	}
	return nil
}

// validateChatToolVocabulary enforces the Chat target's name class on every
// declared tool, a named tool choice, and every history tool call. Chat tool
// ids are not pattern-constrained, so only the name is checked.
func validateChatToolVocabulary(req *ir.Request) error {
	for _, tool := range req.Tools {
		if name := tool.ModelName(); !openAIIdentifierPattern.MatchString(name) {
			return toolVocabularyError("chat", "tool name", name, "^[a-zA-Z0-9_-]{1,64}$")
		}
	}
	if req.ToolChoice != nil && req.ToolChoice.Mode == ir.ToolChoiceTool &&
		!openAIIdentifierPattern.MatchString(req.ToolChoice.Name) {
		return toolVocabularyError("chat", "tool_choice.name", req.ToolChoice.Name, "^[a-zA-Z0-9_-]{1,64}$")
	}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Type == ir.BlockToolCall {
				if err := validateChatToolCall(block.ToolCall); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateChatToolCall enforces the Chat target's name class on one tool call.
// The request and response encoders share it so a call the target would reject
// fails at the same owner instead of reaching the wire. Chat tool ids have no
// pattern, so only the name is validated.
func validateChatToolCall(tc *ir.ToolCall) error {
	if tc == nil {
		return nil
	}
	if !openAIIdentifierPattern.MatchString(tc.Name) {
		return toolVocabularyError("chat", "tool name", tc.Name, "^[a-zA-Z0-9_-]{1,64}$")
	}
	return nil
}

// validateResponsesToolVocabulary enforces the Responses request-input bound on
// history tool references. Responses FunctionCallOutputItemParam.call_id is
// 1..64 characters; the output FunctionToolCall.call_id has no such bound, so
// this is a request-only rule. Chat has no id length limit, and a longer id
// cannot be shortened without changing the client-visible identity, so the
// request fails closed instead of emitting a wire the target rejects.
func validateResponsesToolVocabulary(req *ir.Request) error {
	for _, message := range req.Messages {
		for _, block := range message.Content {
			switch block.Type {
			case ir.BlockToolCall:
				if block.ToolCall != nil {
					if err := validateResponsesToolReference("tool call id", block.ToolCall.ID); err != nil {
						return err
					}
				}
			case ir.BlockToolResult:
				if block.ToolResult != nil {
					if err := validateResponsesToolReference("tool result call id", block.ToolResult.CallID); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func validateResponsesToolReference(field, id string) error {
	// JSON Schema maxLength counts characters (RFC 8259 code points), not
	// UTF-8 bytes. A 64-rune multibyte id is valid even though it is longer
	// than 64 bytes.
	if n := utf8.RuneCountInString(id); n >= 1 && n <= 64 {
		return nil
	}
	return fmt.Errorf("responses %s %q must be 1..64 characters", field, boundedEcho(id))
}

// validateAnthropicToolCall enforces the Anthropic target's name and id
// classes on one tool call. The request encoders and the response encoders
// share it so a call the target would reject fails at the same owner instead
// of reaching the wire.
func validateAnthropicToolCall(tc *ir.ToolCall) error {
	if tc == nil {
		return nil
	}
	if !anthropicToolNamePattern.MatchString(tc.Name) {
		return toolVocabularyError("anthropic", "tool name", tc.Name, "^[a-zA-Z0-9_-]{1,128}$")
	}
	if !anthropicToolIDPattern.MatchString(tc.ID) {
		return toolVocabularyError("anthropic", "tool_use.id", tc.ID, "^[a-zA-Z0-9_-]+$")
	}
	return nil
}

// validateAnthropicToolResult enforces the Anthropic target's id class on one
// tool result reference.
func validateAnthropicToolResult(tr *ir.ToolResult) error {
	if tr == nil {
		return nil
	}
	if !anthropicToolIDPattern.MatchString(tr.CallID) {
		return toolVocabularyError("anthropic", "tool_use_id", tr.CallID, "^[a-zA-Z0-9_-]+$")
	}
	return nil
}

// validateAnthropicToolVocabulary enforces the Anthropic target's name and id
// classes on declared tools, named choices, tool calls and tool results.
func validateAnthropicToolVocabulary(req *ir.Request) error {
	for _, tool := range req.Tools {
		if name := tool.ModelName(); !anthropicToolNamePattern.MatchString(name) {
			return toolVocabularyError("anthropic", "tool name", name, "^[a-zA-Z0-9_-]{1,128}$")
		}
	}
	if req.ToolChoice != nil && req.ToolChoice.Mode == ir.ToolChoiceTool &&
		!anthropicToolNamePattern.MatchString(req.ToolChoice.Name) {
		return toolVocabularyError("anthropic", "tool_choice.name", req.ToolChoice.Name, "^[a-zA-Z0-9_-]{1,128}$")
	}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			switch block.Type {
			case ir.BlockToolCall:
				if err := validateAnthropicToolCall(block.ToolCall); err != nil {
					return err
				}
			case ir.BlockToolResult:
				if err := validateAnthropicToolResult(block.ToolResult); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateAnthropicResponseToolVocabulary applies the same tool_use vocabulary
// to a decoded assistant response before any target wire is emitted.
func validateAnthropicResponseToolVocabulary(resp *ir.Response) error {
	for _, block := range resp.Blocks {
		if block.Type != ir.BlockToolCall {
			continue
		}
		if err := validateAnthropicToolCall(block.ToolCall); err != nil {
			return err
		}
	}
	return nil
}

// toolAllowedCallers validates the allowed_callers knob shared by the
// Responses and Anthropic tool decoders. The IR and every target can invoke a
// tool only directly, so a supplied list must still permit "direct"; a
// programmatic-only list would otherwise become directly callable when the
// knob is dropped. present reports a non-null value so the caller records the
// loss. A malformed list fails closed instead of silently widening the tool.
func toolAllowedCallers(raw json.RawMessage) (present bool, err error) {
	if !wireValuePresent(raw) {
		return false, nil
	}
	// The outer tools array is already bounded, but one tool can carry this
	// array alone at the request-body cap. Bound it lexically before the
	// typed []string decode materializes it.
	if err := checkWireArray(raw, "allowed_callers", WireCollectionMax); err != nil {
		return true, err
	}
	var callers []string
	if err := json.Unmarshal(raw, &callers); err != nil {
		return true, fmt.Errorf("allowed_callers: %w", err)
	}
	for _, caller := range callers {
		if caller == "direct" {
			return true, nil
		}
	}
	return true, errors.New("allowed_callers excludes direct calls, which the target requires")
}

// validateResponseFormatName enforces the OpenAI identifier class on a
// json_schema response-format name. An empty name is legal: both encoders
// synthesize the documented "response" default. A name the target would
// reject fails closed at the request boundary.
func validateResponseFormatName(target string, rf *ir.ResponseFormat) error {
	if rf == nil || rf.Type != "json_schema" || rf.Name == "" {
		return nil
	}
	if !openAIIdentifierPattern.MatchString(rf.Name) {
		return toolVocabularyError(target, "response_format.name", rf.Name, "^[a-zA-Z0-9_-]{1,64}$")
	}
	return nil
}
