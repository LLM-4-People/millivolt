package proxy

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

// --- scoped request overrides (config request_overrides) ---

// resolvedOverride is the per-request merge of every request_overrides rule
// whose scope matched the request (client, provider, recorded model; an
// empty scope field is a wildcard). resolveRequestOverrides owns the ordered
// walk: for any one header name the LAST matching action wins - a later
// set/replace beats an earlier removal and vice versa - and for the body
// fields a later rule's value wins per field. A nil *resolvedOverride (the
// feature-off default: no rules configured, or nothing matched) must leave
// the request path byte-identical.
type resolvedOverride struct {
	// headers are the merged set/replace entries keyed by canonical header
	// name (config validation canonicalizes case at the load boundary).
	headers map[string]string
	// removes are the merged removal names, sorted so application is
	// deterministic. A name whose last action was a set is absent here, and
	// one whose last action was a removal is absent from headers.
	removes []string
	// body holds the merged token ceilings; nil when no matching rule names
	// a body field. It is stamped onto a Chat or Anthropic client body
	// before translation (overrideBodyInbound).
	body *config.OverrideBody
}

// resolveRequestOverrides walks the configured request_overrides rules in
// list order and merges every rule whose non-empty scope fields all equal
// the request's values exactly: the classified client, the canonical
// provider label (post provider_aliases), and the recorded model id. No
// canonicalization, no regex, no broadening - exact leaf only. No rules or
// no match returns nil: the feature-off path costs one slice-length check,
// while the configured-but-unmatched path allocates the removed map and
// walks up to config.RequestOverridesMax rules before returning nil.
func resolveRequestOverrides(rules []config.RequestOverride, client, provider, model string) *resolvedOverride {
	if len(rules) == 0 {
		return nil
	}
	var ov resolvedOverride
	// removed tracks the names whose last action was a removal, so a later
	// rule's set can win a name an earlier rule removed. Allocated only when a
	// matching rule actually removes something; the configured-but-unmatched
	// path stays allocation-free.
	var removed map[string]bool
	matched := false
	for i := range rules {
		r := &rules[i]
		if (r.Client != "" && r.Client != client) ||
			(r.Provider != "" && r.Provider != provider) ||
			(r.Model != "" && r.Model != model) {
			continue
		}
		matched = true
		for name, value := range r.Headers {
			if removed != nil {
				delete(removed, name)
			}
			if ov.headers == nil {
				ov.headers = make(map[string]string, len(r.Headers))
			}
			ov.headers[name] = value
		}
		for _, name := range r.RemoveHeaders {
			delete(ov.headers, name)
			if removed == nil {
				removed = make(map[string]bool)
			}
			removed[name] = true
		}
		if r.Body != nil {
			if ov.body == nil {
				ov.body = &config.OverrideBody{}
			}
			if r.Body.MaxTokens != nil {
				v := *r.Body.MaxTokens
				ov.body.MaxTokens = &v
			}
			if r.Body.MaxCompletionTokens != nil {
				v := *r.Body.MaxCompletionTokens
				ov.body.MaxCompletionTokens = &v
			}
		}
	}
	if !matched {
		return nil
	}
	if len(removed) > 0 {
		ov.removes = slices.Sorted(maps.Keys(removed))
	}
	return &ov
}

// overrideBodyInbound reports whether the request-overrides body fields can
// edit a body of this inbound wire shape: Chat and Anthropic both carry a
// token ceiling at the top level, while a Responses body spells it
// max_output_tokens and a Cursor run has no such field. The gate reads the
// CLIENT body because the rewrite runs before translation.
func overrideBodyInbound(p ir.Protocol) bool {
	return p == ir.Chat || p == ir.Anthropic
}

// applyOverrideHeaders applies the resolved request-overrides merge to an
// upstream request's headers: every merged set/replace entry first, then the
// merged removals. Both send owners stamp it as the operator's last word on
// the upstream wire - as the final stage of buildUpstreamRequest (after
// client-forwarded headers, the provider map, and X-Proxy-Headers), and in
// setCursorIdentity after the provider identity map. Runtime safety belt: a
// header whose name is the target's auth header, or Authorization, is
// silently skipped in both directions - the upstream credential stays at
// its owner even if a future config grammar slips one past validation.
func applyOverrideHeaders(h http.Header, ov *resolvedOverride, authHeader string) {
	if ov == nil {
		return
	}
	for name, value := range ov.headers {
		if overrideHeaderAuthOwned(name, authHeader) {
			continue
		}
		h.Set(name, value)
	}
	for _, name := range ov.removes {
		if overrideHeaderAuthOwned(name, authHeader) {
			continue
		}
		h.Del(name)
	}
}

// overrideHeaderAuthOwned reports whether an override header name is the
// credential slot for this target: literal Authorization, or the target's
// configured auth header (X-Proxy-Auth-Header, e.g. Anthropic's x-api-key).
// Case-insensitive, like HTTP header names.
func overrideHeaderAuthOwned(name, authHeader string) bool {
	return strings.EqualFold(name, "Authorization") ||
		(authHeader != "" && strings.EqualFold(name, authHeader))
}

// overrideRequestBody rewrites an OpenAI-wire request body through the
// single body-rewrite engine and returns the new bytes: the whole document
// decodes as raw JSON values once, the named token ceilings are set, the
// named strip fields are deleted, the ensured tool names are injected where
// missing, and the document re-marshals with every other field's value
// preserved verbatim - the re-encode sorts keys, compacts whitespace and
// HTML-escapes <, > and & inside preserved values, so the bytes are not
// preserved, only the values. ob may be nil (no ceiling actions), strip may
// be nil (no strip actions) and ensureTools may be nil (no tool actions);
// when no action changes the document the ORIGINAL bytes come back
// unchanged - no rewrite happened. Fail closed - why carries the
// operator-actionable reason and the caller relays the ORIGINAL bytes
// unchanged - when the body is not a JSON object or the rewritten result
// would exceed the request byte budget.
func overrideRequestBody(body []byte, ob *config.OverrideBody, strip []string, ensureTools []string, maxBytes int64, inbound ir.Protocol) (out []byte, changed bool, why string) {
	// Narrow fast path: when ensured tools are the only configured action and
	// the body already carries every name, one bounded lexical scan of the
	// top-level keys plus the tools decode proves the document needs no
	// rewrite. The general path would materialize every top-level value as
	// json.RawMessage (a body-sized copy) and decode every tool entry again
	// only to return the original bytes.
	if ob == nil && len(strip) == 0 && len(ensureTools) > 0 && ensureToolsSatisfied(body, ensureTools) {
		return body, false, ""
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false, fmt.Sprintf("the request body is not a JSON object: %v", err)
	}
	if doc == nil {
		// Valid JSON null: not an object, so no field can be set on it.
		return nil, false, "the request body is not a JSON object"
	}
	changed = false
	if ob != nil {
		if inbound == ir.Anthropic {
			// Anthropic has one token field. Both override spellings address
			// it, with the same precedence the Chat pair resolves in
			// MergeTokenCaps so an operator can use either name.
			cap := ob.MaxCompletionTokens
			if cap == nil {
				cap = ob.MaxTokens
			}
			if cap != nil {
				doc["max_tokens"] = json.RawMessage(strconv.Itoa(*cap))
				changed = true
			}
		} else {
			if ob.MaxTokens != nil {
				doc["max_tokens"] = json.RawMessage(strconv.Itoa(*ob.MaxTokens))
				changed = true
			}
			if ob.MaxCompletionTokens != nil {
				doc["max_completion_tokens"] = json.RawMessage(strconv.Itoa(*ob.MaxCompletionTokens))
				changed = true
			}
		}
	}
	for _, name := range strip {
		if _, ok := doc[name]; ok {
			delete(doc, name)
			changed = true
		}
	}
	if len(ensureTools) > 0 {
		if ensureToolsChanged, whyTools := ensureToolsInBody(doc, ensureTools); whyTools != "" {
			return nil, false, whyTools
		} else if ensureToolsChanged {
			changed = true
		}
	}
	if !changed {
		return body, false, ""
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false, fmt.Sprintf("re-encoding the rewritten body failed: %v", err)
	}
	if int64(len(out)) > maxBytes {
		return nil, false, fmt.Sprintf("the rewritten body would exceed max_request_bytes (%d bytes)", maxBytes)
	}
	return out, true, ""
}
