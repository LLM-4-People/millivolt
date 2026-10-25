package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// chatMessagesArray is the single chat-body predicate shared by the no-copy
// fast path and the general rewrite path. Only a top-level JSON array is the
// documented messages member; every other shape leaves the body unrewritten,
// so the two paths can never disagree about whether a body is chat-shaped.
func chatMessagesArray(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '['
}

// ensureToolsSatisfied reports whether the ensured-tools actor would leave the
// body unchanged: no exact top-level messages array (the actor skips non-chat
// bodies), a tools value that is not an array (the actor skips it), or every
// configured name already present. One lexical scan finds the exact top-level
// keys with last-occurrence-wins semantics, the same keys the general path's
// map decode sees; a case variant is a different key to both paths. Any shape
// the scan cannot prove unchanged falls back to the general path, which owns
// the rewrite and its fail-closed errors.
func ensureToolsSatisfied(body []byte, ensureTools []string) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return false
	}
	var messages, tools json.RawMessage
	i := metrics.SkipSpace(trimmed, 1)
	for i < len(trimmed) && trimmed[i] != '}' {
		keyStart := i
		keyEnd := skipJSONString(trimmed, i)
		if keyEnd < 0 {
			return false
		}
		var key string
		if json.Unmarshal(trimmed[keyStart:keyEnd], &key) != nil {
			return false
		}
		i = metrics.SkipSpace(trimmed, keyEnd)
		if i >= len(trimmed) || trimmed[i] != ':' {
			return false
		}
		i = metrics.SkipSpace(trimmed, i+1)
		valueStart := i
		valueEnd := skipJSONValue(trimmed, i)
		if valueEnd < 0 {
			return false
		}
		switch key {
		case "messages":
			messages = trimmed[valueStart:valueEnd]
		case "tools":
			tools = trimmed[valueStart:valueEnd]
		}
		i = metrics.SkipSpace(trimmed, valueEnd)
		if i >= len(trimmed) {
			return false
		}
		if trimmed[i] == ',' {
			i = metrics.SkipSpace(trimmed, i+1)
			if i >= len(trimmed) || trimmed[i] == '}' {
				return false
			}
			continue
		}
		if trimmed[i] != '}' {
			return false
		}
	}
	if i >= len(trimmed) || trimmed[i] != '}' || metrics.SkipSpace(trimmed, i+1) != len(trimmed) {
		return false
	}
	if !chatMessagesArray(messages) {
		return true
	}
	if len(tools) == 0 {
		return false
	}
	have, _, array := toolsArrayNames(tools)
	if !array {
		return true
	}
	for _, want := range ensureTools {
		if !have[want] {
			return false
		}
	}
	return true
}

// toolsArrayNames decodes the function names one top-level tools value
// declares. array is false when the value is not a JSON array, which the actor
// treats as a body it must not touch. Malformed entries are ignored exactly
// like the rewrite path ignores them.
func toolsArrayNames(raw json.RawMessage) (names map[string]bool, entries int, array bool) {
	names = make(map[string]bool)
	var decoded []json.RawMessage
	if json.Unmarshal(raw, &decoded) != nil {
		return names, 0, false
	}
	for _, entry := range decoded {
		var fn struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(entry, &fn) == nil && fn.Function.Name != "" {
			names[fn.Function.Name] = true
		}
	}
	return names, len(decoded), true
}

// ensureToolStub is the inert function stub injected for a configured name a
// chat body's tools array lacks. The description tells the model to keep
// away: the stub exists for a first-party client's tool-signature gate, not
// to be called, and a body that carried no tools of its own additionally
// gets tool_choice "none" so the model cannot invoke it at all.
type ensureToolStub struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// ensureToolsInBody is the tools actor of the body-rewrite engine: it adds
// every configured name the body's tools array lacks as an inert stub. Only
// chat-shaped bodies qualify (a top-level messages array), so discovery,
// responses-style and embeddings bodies stay untouched; a tools value that
// does not decode as an array skips the actor rather than corrupting the
// body. When the client supplied no tools of its own, the injected stubs are
// inert only if the model cannot choose them: an absent, null, or auto
// tool_choice becomes none. An explicit none keeps its spelling. A required,
// named, or malformed choice skips injection entirely - forcing none would
// contradict the client, and injecting would let it call an undeclared stub.
// A body that already carries every name is returned unchanged - the
// passthrough invariant survives satisfied signatures.
func ensureToolsInBody(doc map[string]json.RawMessage, ensureTools []string) (changed bool, why string) {
	rawMessages, ok := doc["messages"]
	if !ok || !chatMessagesArray(rawMessages) {
		return false, ""
	}
	have := make(map[string]bool, len(ensureTools))
	clientTools := 0
	if raw, ok := doc["tools"]; ok {
		var array bool
		have, clientTools, array = toolsArrayNames(raw)
		if !array {
			// Not an array: not a chat tools field the stubs belong in.
			return false, ""
		}
	}
	var additions []json.RawMessage
	for _, name := range ensureTools {
		if have[name] {
			continue
		}
		stub := ensureToolStub{Type: "function"}
		stub.Function.Name = name
		stub.Function.Description = "Proxy-injected compatibility stub; never call this tool."
		stub.Function.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		encoded, err := json.Marshal(stub)
		if err != nil {
			return false, fmt.Sprintf("encoding an ensured tool stub for %q failed: %v", name, err)
		}
		additions = append(additions, encoded)
	}
	if len(additions) == 0 {
		return false, ""
	}
	var tools []json.RawMessage
	if clientTools > 0 {
		if raw, ok := doc["tools"]; ok && json.Unmarshal(raw, &tools) == nil {
			tools = append(tools, additions...)
		}
	} else {
		// The injected stubs are the body's only tools. Decide the choice
		// once: force none for an implicit choice, keep an explicit none, and
		// skip injection for anything that would contradict the client or let
		// it select an undeclared stub.
		forceNone := false
		if raw, ok := doc["tool_choice"]; ok {
			var choice any
			if err := json.Unmarshal(raw, &choice); err != nil {
				return false, ""
			}
			switch value := choice.(type) {
			case nil:
				forceNone = true
			case string:
				switch value {
				case "auto":
					forceNone = true
				case "none":
					// Already inert; keep the client's spelling.
				default:
					return false, ""
				}
			default:
				return false, ""
			}
		} else {
			forceNone = true
		}
		tools = additions
		if forceNone {
			doc["tool_choice"] = json.RawMessage(`"none"`)
		}
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return false, fmt.Sprintf("encoding the ensured tools array failed: %v", err)
	}
	doc["tools"] = encoded
	return true, ""
}
