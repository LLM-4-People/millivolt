package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/jsonnum"
	"github.com/LLM-4-People/millivolt/internal/metrics"

	"golang.org/x/net/publicsuffix"
)

// requestReadHintMax bounds speculative allocation from an untrusted
// Content-Length. Larger/unknown bodies still grow through the limited reader.
// This is an allocation guardrail, not a second request-size limit.
const requestReadHintMax = 1 << 20

// setResolvedAuth stamps the extracted upstream key into the target's
// configured auth header (name + prefix); an empty key stamps nothing. The
// one owner shared by the generic upstream request builder, the metadata
// fetchers, and the cursor identity headers. t.authHeader always carries the
// resolved header (Authorization unless the client names one), so there is no
// protocol-specific fallback, and buildUpstreamRequest additionally strips the
// client's forwarded credential before stamping.
func setResolvedAuth(h http.Header, t *target, key string) {
	if key != "" {
		h.Set(t.authHeader, t.authPrefix+key)
	}
}

func (s *Server) buildUpstreamRequest(ctx context.Context, r *http.Request, t *target, key string, body []byte) (*http.Request, error) {
	path := t.path
	if path == "" {
		// EscapedPath preserves the client's percent-encoding: Path alone
		// would turn %2F, %3F and %23 into path separators or query/fragment
		// delimiters before the upstream sees them.
		path = r.URL.EscapedPath()
		if path == "" {
			path = "/"
		}
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u, err := upstreamURL(t.baseURL, path, t.query, r.URL.RawQuery)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// NewRequest re-parses its URL string, where a literal "#" in RawQuery
	// becomes a fragment and truncates the query. Keep the structurally built
	// URL the transport will serialize instead.
	req.URL = u

	// Forward client headers, dropping proxy control, hop-by-hop, and the
	// upstream auth header (re-added below from the extracted key). Transport
	// mode headers (Accept-Encoding, Range) never pass through: the shared
	// transport must own gzip negotiation and auto-decompression, and the relay
	// always forwards a complete body, so a caller-supplied value would
	// desynchronize the framing. The final assembly strips them again after
	// every configured source.
	//
	// RFC 7230 §6.1: a header nominated by the Connection header is itself
	// hop-by-hop and must not be forwarded. The static isHopByHop list alone
	// would let a client smuggle an arbitrary header upstream by nominating it
	// (e.g. "Connection: X-Secret" + "X-Secret: …"), so strip nominated tokens too.
	nominated := connectionTokens(r.Header)
	for k, vs := range r.Header {
		// Inbound names are already canonical, so the direct predicates avoid
		// three lowercasing allocations per forwarded header.
		if isProxyControlHeaderCanonical(k) || isHopByHopCanonical(k) || nominated[k] || isTransportModeHeader(k) {
			continue
		}
		req.Header[k] = append([]string(nil), vs...)
	}

	// Place the upstream key into the provider's auth header. The strip is
	// passthrough-specific: forwarded client headers may carry a stale
	// credential, while the metadata and cursor builders start from fresh
	// header maps.
	req.Header.Del("Authorization")
	req.Header.Del(t.authHeader)
	setResolvedAuth(req.Header, t, key)

	// Provider-configured headers (e.g. mimicking a first-party client's wire
	// fingerprint) override client-forwarded values; the explicit per-request
	// X-Proxy-Headers injection map below still wins over them, and the
	// resolved request_overrides merge applies LAST of all - the operator's
	// config is the point of an override, so it wins over every other source.
	s.applyProviderHeaders(req.Header, t.provider)

	// Extra headers the app asked us to add (or override), filtered.
	for k, vs := range t.extraHeader {
		if isHopByHop(k) || isProxyControlHeader(k) || isTransportModeHeader(k) {
			continue
		}
		req.Header.Del(k)
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	// Scoped request overrides: the resolved merge for this request closes
	// the chain (its set/replace entries, then its removals), with the
	// credential safety belt inside applyOverrideHeaders.
	applyOverrideHeaders(req.Header, t.override, t.authHeader)
	// Transport-mode headers are invariant regardless of which source tried to
	// set them: the shared transport's own Accept-Encoding enables
	// auto-decompression, and Range would request a partial body the relay
	// cannot forward as a completion.
	stripTransportModeHeaders(req.Header)

	return req, nil
}

// applyProviderHeaders sets the provider's configured upstream headers
// (config providers.<label>.headers) on an upstream request. Template
// placeholders in values expand per request: {{uuid4}} mints a fresh UUID v4
// (the cursor x-request-id idiom), {{platform}} renders the rust-style
// "os; arch" pair of the machine the proxy runs on, and
// {{opencode-msg-id}}/{{opencode-ses-id}} mint fresh identifiers in the
// opencode CLI's wire format (the zen idiom). Every configured header
// is set, never deleted: empty values are rejected at config load
// (config.ValidHeaderValue plus the mapping's non-empty rule), so there is no
// delete feature - deny by default.
func (s *Server) applyProviderHeaders(h http.Header, provider string) {
	ov, ok := s.cfg().Providers[provider]
	if !ok || len(ov.Headers) == 0 {
		return
	}
	for name, value := range ov.Headers {
		h.Set(name, expandHeaderTemplates(value))
	}
}

// expandHeaderTemplates expands the per-request placeholders in a configured
// header value. Unknown {{...}} sequences pass through verbatim - only the
// documented placeholders are special.
func expandHeaderTemplates(v string) string {
	if !strings.Contains(v, "{{") {
		return v
	}
	v = strings.ReplaceAll(v, "{{uuid4}}", providerformat.UUID4())
	v = strings.ReplaceAll(v, "{{platform}}", rustPlatform())
	v = strings.ReplaceAll(v, "{{opencode-msg-id}}", opencodeID("msg_"))
	v = strings.ReplaceAll(v, "{{opencode-ses-id}}", opencodeID("ses_"))
	return v
}

// opencodeIDChars is the opencode CLI's identifier alphabet: 62 characters
// the CLI picks its random tail from (byte % 62 over the same set).
const opencodeIDChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var (
	opencodeIDmu      sync.Mutex
	opencodeIDlastMS  int64
	opencodeIDcounter int64
)

// opencodeID mints an identifier in the opencode CLI's wire format: prefix
// plus 26 characters - 12 lowercase hex encoding the low 48 bits of
// (timestamp_ms * 4096 + counter), then 14 base62 characters of
// cryptographically random bytes. This ports the CLI's identifier generator
// (packages/schema identifier.ts) faithfully; upstream gates have been
// observed to validate the shape (12 hex + 14 base62), not the timestamp.
// The counter shares the CLI's same-millisecond monotonicity, so ids minted
// within one process never collide.
func opencodeID(prefix string) string {
	opencodeIDmu.Lock()
	ms := time.Now().UnixMilli()
	if ms != opencodeIDlastMS {
		opencodeIDlastMS = ms
		opencodeIDcounter = 0
	}
	opencodeIDcounter++
	value := uint64(ms)*0x1000 + uint64(opencodeIDcounter)
	opencodeIDmu.Unlock()
	tail := make([]byte, 14)
	// crypto/rand.Read cannot fail on the supported platforms; a zero tail
	// still matches the validated shape if it ever did.
	_, _ = rand.Read(tail)
	chars := make([]byte, 14)
	for i, b := range tail {
		chars[i] = opencodeIDChars[int(b)%62]
	}
	return prefix + fmt.Sprintf("%012x", value&(1<<48-1)) + string(chars)
}

// rustPlatform renders the platform pair the grok-build CLI puts in its
// User-Agent ("({os}; {arch})", rust std::env::consts spellings) for THIS
// process. Go's GOARCH names differ from rust's for two common platforms and
// are mapped so the string reads exactly like the CLI's.
func rustPlatform() string {
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "x86_64"
	case "arm64":
		arch = "aarch64"
	}
	return runtime.GOOS + "; " + arch
}

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
	// the body already carries every name, one struct decode proves the
	// document needs no rewrite. The general path would materialize every
	// top-level value as json.RawMessage (a body-sized copy) and decode every
	// tool entry again only to return the original bytes.
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

// chatMessagesPresence records whether the top-level messages member is a JSON
// array without copying it. The enclosing document decode validates syntax, so
// the one shared predicate can inspect the first token: null, an object, a
// string or a scalar is not the documented chat-completions messages array.
type chatMessagesPresence struct {
	array bool
}

func (m *chatMessagesPresence) UnmarshalJSON(raw []byte) error {
	m.array = chatMessagesArray(raw)
	return nil
}

// chatMessagesArray is the single chat-body predicate shared by the no-copy
// fast path and the general rewrite path. Only a top-level JSON array is the
// documented messages member; every other shape leaves the body unrewritten,
// so the two paths can never disagree about whether a body is chat-shaped.
func chatMessagesArray(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '['
}

// ensureToolsSatisfied reports whether the ensured-tools actor would leave the
// body unchanged: no top-level messages array (the actor skips non-chat
// bodies) or every configured name already present in the tools array. One
// struct decode replaces the general path's raw-value map plus per-entry
// decode; any shape it cannot prove satisfied falls back to that path, so the
// skip semantics stay exactly ensureToolsInBody's.
func ensureToolsSatisfied(body []byte, ensureTools []string) bool {
	var doc struct {
		Messages chatMessagesPresence `json:"messages"`
		Tools    []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return false
	}
	if !doc.Messages.array {
		return true
	}
	for _, want := range ensureTools {
		found := false
		for i := range doc.Tools {
			if doc.Tools[i].Function.Name == want {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
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
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			// Not an array: not a chat tools field the stubs belong in.
			return false, ""
		}
		clientTools = len(entries)
		for _, entry := range entries {
			var fn struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if json.Unmarshal(entry, &fn) == nil && fn.Function.Name != "" {
				have[fn.Function.Name] = true
			}
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

// resolveRequestTokenCap owns the three wire spellings the metadata decoder
// accepts: the Chat pair (max_completion_tokens over max_tokens) first, then the
// Responses max_output_tokens spelling. Every metadata decode path calls it, so
// the hostile-cap trust boundary cannot see a different ceiling per path.
func resolveRequestTokenCap(maxCompletion, maxTokens, maxOutput *int) *int {
	if v := providerformat.MergeTokenCaps(maxCompletion, maxTokens); v != nil {
		return v
	}
	return maxOutput
}

// applyTokenCaps stamps the effective token ceiling onto the record through
// MergeTokenCaps' precedence (max_completion_tokens over max_tokens over
// max_output_tokens; none present stamps nil). The occurrence scan rejects a
// duplicate cap spelling, and a malformed spelling leaves the ceiling unset
// here; ServeHTTP's tokenCapViolation rejects either before the request can
// proceed. Both cap re-reads share it: restampReqMaxTokens re-reads a rewritten
// body, and parseLLMRequest re-reads a body whose parameter block failed its
// strict decode.
func applyTokenCaps(body []byte, rec *metrics.Record) {
	caps, err := scanTokenCaps(body)
	if err != nil {
		return
	}
	rec.ReqMaxTokens = resolveRequestTokenCap(
		tokenCapOptional(caps.MaxCompTokens, "max_completion_tokens"),
		tokenCapOptional(caps.MaxTokens, "max_tokens"),
		tokenCapOptional(caps.MaxOutputTokens, "max_output_tokens"),
	)
}

// tokenCapOptional decodes one cap member for the request record. A missing or
// malformed spelling contributes nothing here; ServeHTTP's tokenCapViolation is
// the trust boundary that rejects a malformed cap before the request proceeds.
func tokenCapOptional(raw json.RawMessage, field string) *int {
	v, err := tokenCapValue(raw, field)
	if err != nil {
		return nil
	}
	return v
}

// restampReqMaxTokens re-reads the effective token ceiling from a rewritten
// body onto the record, so the hostile-cap boundary, the scheduler
// reservation and the recorded request parameters all describe the bytes
// actually sent upstream. Shares the max_completion_tokens-wins precedence
// with the first decode; a caps decode that still fails (the client's own
// cap field carried a non-number) keeps the first decode's value rather
// than fabricating one.
func restampReqMaxTokens(body []byte, rec *metrics.Record) {
	applyTokenCaps(body, rec)
}

// joinUpstreamURL joins a base URL and a request path without duplicating a
// shared leading path segment. Clients typically request "/v1/chat/completions"
// while operators variously set the base to "https://host" or
// "https://host/v1" - naive concatenation would yield ".../v1/v1/..." and 404.
// If the base's last path segment equals the request path's first segment, the
// duplicate is dropped. This is purely a path-join convenience; it never strips
// segments that differ.
func joinUpstreamURL(baseURL, path string) string {
	base := strings.TrimRight(baseURL, "/")
	bu, err := url.Parse(base)
	if err != nil {
		return base + path
	}
	baseSegs := strings.Split(strings.Trim(bu.Path, "/"), "/")
	pathSegs := strings.Split(strings.Trim(path, "/"), "/")
	// Drop the request path's first segment when it duplicates the base's last
	// non-empty segment (e.g. base ".../v1" + path "/v1/chat/completions").
	if len(baseSegs) > 0 && baseSegs[0] != "" && len(pathSegs) > 0 &&
		baseSegs[len(baseSegs)-1] == pathSegs[0] {
		path = "/" + strings.Join(pathSegs[1:], "/")
	}
	return base + path
}

// upstreamURL builds the request target structurally. The path is already an
// escaped path (client request or validated X-Proxy-Path) and is never
// re-parsed as part of a concatenated URL, so an encoded ? or # cannot become
// a query or fragment delimiter.
func upstreamURL(baseURL, escapedPath, queryOverride, clientQuery string) (*url.URL, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid base URL %q: %w", clientEcho(baseURL), err)
	}
	joined := joinUpstreamEscapedPath(base.EscapedPath(), escapedPath)
	decoded, err := url.PathUnescape(joined)
	if err != nil {
		return nil, fmt.Errorf("invalid request path: %w", err)
	}
	u := *base
	u.Path = decoded
	u.RawPath = joined
	// Query override takes precedence; otherwise pass the client's query along.
	if queryOverride != "" {
		u.RawQuery = queryOverride
	} else {
		u.RawQuery = clientQuery
	}
	return &u, nil
}

// joinUpstreamEscapedPath joins two escaped paths and drops the request path's
// first segment when it duplicates the base's last non-empty segment. Segment
// comparison uses the decoded spelling so an encoded character still matches;
// joining uses the escaped spelling so %2F never becomes a separator. Exactly
// one leading separator is consumed: every remaining empty segment (a doubled
// slash at the start, inside or at the end) is part of the client's path shape
// and is preserved.
func joinUpstreamEscapedPath(basePath, escapedPath string) string {
	base := strings.TrimRight(basePath, "/")
	if !strings.HasPrefix(escapedPath, "/") {
		escapedPath = "/" + escapedPath
	}
	var baseSegs []string
	if base != "" {
		baseSegs = strings.Split(strings.Trim(base, "/"), "/")
	}
	pathSegs := strings.Split(escapedPath[1:], "/")
	if len(baseSegs) > 0 && len(pathSegs) > 0 && pathSegs[0] != "" &&
		decodedPathSegment(baseSegs[len(baseSegs)-1]) == decodedPathSegment(pathSegs[0]) {
		pathSegs = pathSegs[1:]
	}
	return "/" + strings.Join(append(baseSegs, pathSegs...), "/")
}

func decodedPathSegment(segment string) string {
	decoded, err := url.PathUnescape(segment)
	if err != nil {
		return segment
	}
	return decoded
}

// maxBaseURLHostBytes is the DNS presentation limit for one hostname (RFC
// 1035, without a trailing dot). It bounds the authority a client can claim
// through X-Proxy-Base-URL before providerFromURL derives a retained label.
const maxBaseURLHostBytes = 253

// validProxyPathHeader accepts one escaped path override. Literal ? and #
// would be reinterpreted as query/fragment delimiters, and controls or spaces
// cannot travel in a request target; percent escapes must be well formed.
func validProxyPathHeader(p string) bool {
	if strings.IndexFunc(p, func(r rune) bool {
		return r <= 0x20 || r == 0x7f || r == '?' || r == '#'
	}) >= 0 {
		return false
	}
	_, err := url.PathUnescape(p)
	return err == nil
}

// validProxyQueryHeader accepts one raw query override. # would start a
// fragment and controls or spaces are invalid in a request target; a literal ?
// and well-formed percent escapes are query data.
func validProxyQueryHeader(q string) bool {
	return strings.IndexFunc(q, func(r rune) bool {
		return r <= 0x20 || r == 0x7f || r == '#'
	}) < 0
}

// clientEchoMax bounds a client-supplied value echoed inside a rejection
// message. Safety guardrail: a header-sized value must never turn a 400 into a
// multi-megabyte JSON body through quoting and HTML escaping. Normal routing
// values stay byte-identical.
const clientEchoMax = 256

// clientEcho prepares a client-supplied value for %q in a rejection message.
// The shared retained-text truncator cuts on a UTF-8 boundary and marks the
// cut with retainedTruncationSuffix.
func clientEcho(value string) string {
	return retainedText(value, clientEchoMax)
}

// resolveTarget parses routing headers into an upstream target. Only
// X-Proxy-Base-URL is mandatory.
func resolveTarget(r *http.Request, cfg *config.Config) (*target, error) {
	baseURL := strings.TrimSpace(r.Header.Get(hdrBaseURL))
	if baseURL == "" {
		return nil, fmt.Errorf("missing %s header", hdrBaseURL)
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errInvalidHeader(hdrBaseURL, baseURL)
	}
	// "http://:8080" parses with a non-empty Host but no hostname, and Go's
	// transport dials the local host for that authority. A base URL without a
	// host is malformed input, never a local-target shorthand; reject it here
	// rather than allowing a request to reach a loopback service.
	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("invalid %s %q: a hostname is required", hdrBaseURL, clientEcho(baseURL))
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported %s scheme %q", hdrBaseURL, clientEcho(parsed.Scheme))
	}
	// The base must be exactly scheme://host[:port][/path] - no userinfo
	// (credentials would ride to the upstream and leak into error echoes), no
	// query or fragment (they collide with the path/query join). A bare
	// trailing "#" parses to an empty Fragment but is still a fragment marker;
	// a percent-encoded %23 remains path data. Deny by default.
	if parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(baseURL, "#") {
		return nil, fmt.Errorf("invalid %s %q: userinfo, query, and fragment are not allowed in a base URL", hdrBaseURL, clientEcho(baseURL))
	}
	// A client controls this authority, and providerFromURL falls back to the
	// raw hostname for single-label and unknown-suffix hosts. Reject a
	// non-DNS-length hostname or an implausible port at the routing boundary so
	// neither the retained provider label nor an upstream dial ever carries
	// header-sized data. Normal domains, IPs, ports and operator aliases pass
	// unchanged.
	if len(parsed.Hostname()) > maxBaseURLHostBytes || !config.ValidURLPort(parsed.Port()) {
		return nil, errInvalidHeader(hdrBaseURL, baseURL)
	}

	t := &target{
		baseURL:    strings.TrimRight(baseURL, "/"),
		authHeader: "Authorization",
		authPrefix: "Bearer ",
		// The provider label is derived from the base URL, never supplied by
		// the client, so a given upstream always maps to the same identity in
		// the dashboard regardless of what each client calls it.
		provider: providerFromURL(parsed),
	}
	if h := strings.TrimSpace(r.Header.Get(hdrAuthHeader)); h != "" {
		if !isValidAuthHeader(h) {
			return nil, fmt.Errorf("invalid %s header %q", hdrAuthHeader, clientEcho(h))
		}
		t.authHeader = h
	}
	if p := r.Header.Get(hdrAuthPrefix); p != "" || headerPresent(r, hdrAuthPrefix) {
		// Explicitly allow an empty prefix (e.g. raw key with no "Bearer ").
		// The prefix is concatenated into the upstream auth header value, so it
		// must be a valid header value - reject invalid bytes at the boundary
		// rather than fail later as an opaque transport error.
		if !config.ValidHeaderValue(p) {
			return nil, fmt.Errorf("invalid %s value", hdrAuthPrefix)
		}
		t.authPrefix = p
	}
	if p := strings.TrimSpace(r.Header.Get(hdrPath)); p != "" {
		if !validProxyPathHeader(p) {
			return nil, fmt.Errorf("invalid %s header", hdrPath)
		}
		t.path = p
	}
	if q := strings.TrimSpace(r.Header.Get(hdrQuery)); q != "" {
		if !validProxyQueryHeader(q) {
			return nil, fmt.Errorf("invalid %s header", hdrQuery)
		}
		t.query = q
	}
	if f := strings.TrimSpace(r.Header.Get(hdrFormat)); f != "" {
		// Validate at the boundary so an unsupported format is rejected
		// consistently (400), not only when a translation happens to run.
		if !validOutboundFormat(f) {
			return nil, fmt.Errorf("unsupported %s %q", hdrFormat, clientEcho(f))
		}
		t.format = f
	}
	inbound, err := inboundProtocol(r)
	if err != nil {
		return nil, err
	}
	t.inbound = inbound
	t.outbound = outboundProtocol(t.format)
	t.translate = t.inbound != "" && t.outbound != "" && t.inbound != t.outbound
	if t.format == "cursor" {
		// Cursor is an adapter that consumes an OpenAI Chat body and speaks
		// its own Connect protocol. Any other declared client protocol would
		// need a response shape the bridge does not emit, so reject it at the
		// routing boundary instead of forwarding the wrong envelope.
		if t.inbound != "" && t.inbound != ir.Chat {
			return nil, fmt.Errorf("cursor target requires an OpenAI Chat client protocol")
		}
		t.translate = false
	}
	if t.inbound != "" && t.outbound != "" && t.path == "" && (t.translate || pathProtocol(r.URL.Path) != "") {
		// A translated request reaches the target protocol's own endpoint: the
		// client's path names the inbound protocol, so forwarding it verbatim
		// would address the wrong wire. A same-protocol request on one of the
		// three known endpoints normalizes to the canonical spelling of that
		// same endpoint. An unknown path on a same-protocol passthrough is
		// preserved: the client chose an upstream route the proxy does not own.
		t.path = protocolPath(t.outbound)
	}
	if ms := r.Header.Get(hdrTimeout); ms != "" {
		d, err := time.ParseDuration(ms + "ms")
		if err != nil || d <= 0 {
			return nil, errInvalidHeader(hdrTimeout, ms)
		}
		t.timeout = d
	}
	if raw := r.Header.Get(hdrHeaders); raw != "" {
		var m map[string][]string
		if err := adminjson.Unmarshal([]byte(raw), &m); err != nil {
			// The strict decoder owns its diagnostics; this boundary bounds the
			// echo so a header-sized duplicate/invalid key cannot amplify the 400.
			return nil, fmt.Errorf("invalid %s header: %s", hdrHeaders, clientEcho(err.Error()))
		}
		names := make([]string, 0, len(m))
		for name := range m {
			names = append(names, name)
		}
		if err := config.ValidateHeaderNames(names); err != nil {
			// The shared validator owns the grammar and its diagnostic wording;
			// this boundary owns the echo bound, so a header-sized invalid name
			// cannot amplify the 400 body. Normal names keep the exact wording.
			return nil, fmt.Errorf("invalid %s header: %s", hdrHeaders, clientEcho(err.Error()))
		}
		// Validate names/values at the boundary so a malformed header is a
		// clean 400 here, not a late transport write failure surfaced as a 502.
		for k, vs := range m {
			if !isValidAuthHeader(k) {
				return nil, fmt.Errorf("invalid %s header name %q", hdrHeaders, clientEcho(k))
			}
			for _, v := range vs {
				if !config.ValidHeaderValue(v) {
					return nil, fmt.Errorf("invalid %s value for header %q", hdrHeaders, clientEcho(k))
				}
			}
		}
		t.extraHeader = http.Header(m)
	}

	if len(cfg.AllowedBaseURLs) > 0 && !allowed(t.baseURL, cfg.AllowedBaseURLs) {
		return nil, fmt.Errorf("base URL %q not in allowed list", clientEcho(t.baseURL))
	}
	// provider_aliases re-key a derived label the operator has merged into a
	// canonical one. Applied here - the single choke point every
	// provider-scoped consumer flows through (record stamp, throttles, pause
	// holds, per-provider field maps, dashboard grouping) - so a merged label
	// is one entity everywhere at once.
	if canon := strings.TrimSpace(cfg.ProviderAliases[t.provider]); canon != "" {
		t.provider = canon
	}
	return t, nil
}

// providerFromURL derives the canonical provider label from the upstream base
// URL: the registrable domain (effective TLD + 1 label per the public suffix
// list). EVERY subdomain collapses to the root domain - api.openai.com →
// "openai.com"; api2.cursor.sh / api5.cursor.sh → "cursor.sh";
// inference.coralbricks.ai → "coralbricks.ai"; hyper.charm.land → "charm.land"
// - so all of a provider's gateway hosts group under one label. IPs (v4/v6)
// keep the authority with port so distinct self-hosted endpoints stay
// distinguishable; hosts without a derivable registrable domain (localhost,
// intranet names, bare suffixes) keep their hostname.
func providerFromURL(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "unknown"
	}
	if ip := net.ParseIP(host); ip != nil {
		// IPs have no meaningful service name; keep the authority (with port)
		// so distinct self-hosted endpoints stay distinguishable.
		return u.Host
	}
	if domain, err := publicsuffix.EffectiveTLDPlusOne(host); err == nil {
		return domain
	}
	return host
}

func allowed(base string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(base, p) && (len(base) == len(p) ||
			base[len(p)] == '/' || base[len(p)] == '?' || base[len(p)] == ':') {
			return true
		}
	}
	return false
}

// isValidAuthHeader rejects header names that could be used to inject
// dangerous headers into upstream requests. Must match header-name grammar.
func isValidAuthHeader(h string) bool {
	if !config.ValidHeaderName(h) {
		return false
	}
	if isHopByHop(h) || isProxyControlHeader(h) || isTransportModeHeader(h) ||
		strings.EqualFold(h, "host") || strings.EqualFold(h, "content-length") {
		return false
	}
	return true
}

// isTransportModeHeader reports whether a header selects a transport-level
// response mode the proxy must own. Accept-Encoding controls gzip negotiation
// and net/http's automatic decompression; Range asks for a partial body. A
// supplied value from any source would make the relay forward bytes whose
// framing no longer matches the status and headers it emits.
func isTransportModeHeader(h string) bool {
	return strings.EqualFold(h, "Accept-Encoding") || strings.EqualFold(h, "Range")
}

// stripTransportModeHeaders removes the transport-mode headers after every
// header source has run. The shared transport then adds its own
// Accept-Encoding, auto-decompresses gzip upstream bodies, and never sends a
// partial-range request.
func stripTransportModeHeaders(h http.Header) {
	h.Del("Accept-Encoding")
	h.Del("Range")
}

// remoteIP returns the client's IP (host part of RemoteAddr), honoring
// X-Forwarded-For when the proxy sits behind another proxy. The returned value
// is bounded at this owner so every caller and consumer shares the same cap.
func remoteIP(r *http.Request) string {
	ip := ""
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ip = strings.TrimSpace(xff)
		if i := strings.Index(xff, ","); i > 0 {
			ip = strings.TrimSpace(xff[:i])
		}
	} else if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = host
	} else {
		ip = r.RemoteAddr
	}
	return retainedText(ip, retainedClientIPMax)
}

// clientLang extracts the SDK language/runtime from x-stainless-* headers or
// the user agent. No hardcoded list of known languages: it derives from
// whatever headers are present. The returned value is bounded at this owner so
// every caller and consumer shares the same cap.
func clientLang(r *http.Request) string {
	lang := ""
	if v := strings.TrimSpace(r.Header.Get("X-Stainless-Lang")); v != "" {
		lang = v
	} else if v := strings.TrimSpace(r.Header.Get("X-Stainless-Runtime")); v != "" {
		lang = v
	} else {
		// Fall back to the user-agent's product token (e.g.
		// "openai-python/1.55" → "openai-python").
		ua := r.UserAgent()
		if i := strings.Index(ua, "/"); i > 0 {
			lang = ua[:i]
		} else if i := strings.Index(ua, " "); i > 0 {
			lang = ua[:i]
		} else {
			lang = ua
		}
	}
	return retainedText(lang, retainedClientLangMax)
}

// classifyClient derives the recorded client identity from the request
// headers. An explicit X-Proxy-Client label wins, except when it merely
// repeats the user agent's product name while the agent reports more detail
// ("opencode" beside "opencode/latest/2.0.26/cli"): the fuller user-agent
// identity is then the true one. Without a label the order is x-stainless-lang
// (OpenAI SDKs), then the user agent's product token, else "unknown". The
// returned value is bounded at this owner so every caller and consumer shares
// the same cap.
func classifyClient(r *http.Request) string {
	ua := strings.TrimSpace(r.UserAgent())
	if i := strings.Index(ua, " "); i > 0 {
		ua = ua[:i]
	}
	client := ""
	// Explicit override from the app.
	if v := strings.TrimSpace(r.Header.Get(hdrClient)); v != "" {
		client = v
		if ua != "" && ua != v {
			name := ua
			if i := strings.Index(name, "/"); i > 0 {
				name = name[:i]
			}
			if v == name {
				client = ua
			}
		}
	} else if v := strings.TrimSpace(r.Header.Get("X-Stainless-Lang")); v != "" {
		// OpenAI-compatible SDKs set x-stainless-lang (e.g. "python", "js", "go").
		client = v
		if pkg := strings.TrimSpace(r.Header.Get("X-Stainless-Package-Version")); pkg != "" {
			client = v + " " + pkg
		}
	} else if ua != "" {
		client = ua
	} else {
		client = "unknown"
	}
	return retainedText(client, retainedClientMax)
}

// clientMetaMaxLen caps a captured client-meta token. Safety guardrail: these
// are short SDK identity strings, never payloads.
const clientMetaMaxLen = 64

// clipClientMeta bounds one captured client-meta token through the shared
// rune-safe truncator. Invalid UTF-8 is coerced first so the retained identity
// is always valid text; normal short values pass through byte-identically.
func clipClientMeta(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return retainedText(strings.ToValidUTF8(s, "\uFFFD"), clientMetaMaxLen)
}

// fillClientMeta copies allowlisted client identity (X-Stainless-*) and
// proxy-routing config (format / timeout / per-request max-concurrency)
// onto the record. Lowest choke point: one call from ServeHTTP after the
// Record exists. Never copies keys or message content.
func fillClientMeta(r *http.Request, t *target, rec *metrics.Record) {
	if rec == nil || r == nil {
		return
	}
	m := metrics.ClientMeta{
		OS:         clipClientMeta(r.Header.Get("X-Stainless-OS")),
		Arch:       clipClientMeta(r.Header.Get("X-Stainless-Arch")),
		Runtime:    clipClientMeta(r.Header.Get("X-Stainless-Runtime")),
		RuntimeVer: clipClientMeta(r.Header.Get("X-Stainless-Runtime-Version")),
		PkgVer:     clipClientMeta(r.Header.Get("X-Stainless-Package-Version")),
	}
	if t != nil {
		if !isOpenAIWire(t.format) {
			m.Format = clipClientMeta(t.format)
		}
		if t.timeout > 0 {
			m.TimeoutMs = int(t.timeout / time.Millisecond)
		}
	}
	if v := strings.TrimSpace(r.Header.Get(hdrMaxConcurrency)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			m.MaxConc = n
		}
	}
	rec.ClientMeta = m
}

// extractKey returns the upstream API key from the request, preferring an
// explicit X-Proxy-Key header and falling back to the Authorization header.
func extractKey(r *http.Request) string {
	if k := r.Header.Get(hdrKey); k != "" {
		return k
	}
	auth := r.Header.Get("Authorization")
	// Auth-scheme is case-insensitive per RFC 7235 §2.1.
	const bearer = "bearer "
	if len(auth) > len(bearer) && strings.EqualFold(auth[:len(bearer)], bearer) {
		return strings.TrimSpace(auth[len(bearer):])
	}
	return strings.TrimSpace(auth)
}

// headerPresent reports whether the header is set at all (even to "").
func headerPresent(r *http.Request, name string) bool {
	_, ok := r.Header[textproto.CanonicalMIMEHeaderKey(name)]
	return ok
}

// proxyControlHeaderNames is the authoritative strip set for the routing
// boundary: every client-consumed routing/control header name, built from
// the named constants plus the documented defensive entries. Two derived
// lookups share this one list: a canonical-name set for parsed inbound
// headers (allocation-free on the hot path) and a case-folded set for
// operator-supplied names. A new routing header is one named constant plus
// one line here; the routing-header drift test fails when the two halves
// diverge.
var proxyControlHeaderNames = []string{
	hdrBaseURL, hdrAuthHeader, hdrAuthPrefix, hdrPath, hdrQuery,
	hdrHeaders, hdrKey, hdrRefreshToken, hdrProvider, hdrAccessToken,
	hdrTimeout, hdrFormat, hdrInFormat, hdrMaxConcurrency, hdrSession,
	hdrParentSession, hdrClient,
	hdrLimitConcurrency, hdrLimitRequests, hdrLimitTokens,
	// authorization and cookie are defensive strips without routing
	// constants: the upstream key rides only the provider's auth header,
	// rebuilt from the extracted key in buildUpstreamRequest, so the
	// client's raw Authorization (which may carry the operator-plane
	// credential) must never pass through; a client's cookies are this
	// proxy origin's browser state, never provider credentials.
	"authorization", "cookie",
}

var (
	proxyControlHeaders       = canonicalHeaderSet(proxyControlHeaderNames)
	proxyControlHeadersFolded = foldedHeaderSet(proxyControlHeaderNames)
)

func canonicalHeaderSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, name := range names {
		m[http.CanonicalHeaderKey(name)] = true
	}
	return m
}

func foldedHeaderSet(names []string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, name := range names {
		m[strings.ToLower(name)] = true
	}
	return m
}

func isProxyControlHeader(h string) bool {
	return proxyControlHeadersFolded[strings.ToLower(h)]
}

// isProxyControlHeaderCanonical is the allocation-free lookup for header maps
// net/http has already canonicalized.
func isProxyControlHeaderCanonical(h string) bool {
	return proxyControlHeaders[h]
}

func copyResponseHeaders(dst, src http.Header) {
	nominated := connectionTokens(src)
	for k, vs := range src {
		// Drop hop-by-hop headers, Content-Length (so the client gets chunked
		// encoding), Content-Encoding (we decompress upstream bodies, so
		// forwarding it would cause clients to double-decompress), and
		// Set-Cookie (an upstream cookie must never enter the browser's
		// millivolt-origin jar, where it could shadow operator-plane state).
		if isHopByHopCanonical(k) || nominated[k] || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Content-Encoding") || strings.EqualFold(k, "Set-Cookie") {
			continue
		}
		// X-Proxy-Warnings is proxy-owned: it describes translation losses.
		// Never let an upstream header of that name reach the client or
		// overwrite the warning the translation path already stamped.
		if strings.EqualFold(k, hdrWarnings) {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

func isHopByHop(h string) bool {
	return isHopByHopCanonical(http.CanonicalHeaderKey(strings.ToLower(h)))
}

// isHopByHopCanonical is the allocation-free form for canonical header names.
func isHopByHopCanonical(h string) bool {
	switch h {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}

// connectionTokens returns the canonical header names nominated by the
// request's Connection header. Per RFC 7230 §6.1 those nominated headers are
// themselves hop-by-hop and must not be forwarded upstream. nil when the
// header is absent, which is the common HTTP/1 no-nomination and every
// HTTP/2 case; a nil map lookup is valid.
func connectionTokens(h http.Header) map[string]bool {
	values := h.Values("Connection")
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]bool, len(values))
	for _, v := range values {
		for _, tok := range strings.Split(v, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				out[http.CanonicalHeaderKey(tok)] = true
			}
		}
	}
	return out
}

func isEventStream(ct string) bool {
	return strings.HasPrefix(strings.ToLower(ct), "text/event-stream")
}

// The audit identity belongs to this proxy, not the client. Reused client
// request IDs must not collapse pending lifecycles or replace durable rows.
// The original X-Request-Id still forwards unchanged in buildUpstreamRequest.
// requestIDEncoding is base32 with an ASCII-ordered alphabet ("0".."9",
// "A".."V"). RFC 4648's alphabet places digits after letters, so its encoded
// strings are not lexicographically ordered; this one is, which is what makes
// the time prefix a right-edge B-tree append.
var requestIDEncoding = base32.NewEncoding("0123456789ABCDEFGHIJKLMNOPQRSTUV").WithPadding(base32.NoPadding)

// requestID returns a 26-character opaque identifier whose leading six bytes
// are the request time. SQLite's TEXT primary key is a B-tree: random IDs make
// every insert seek and dirty a different leaf, while a time prefix appends at
// the right edge and keeps the hot leaf cached. Ten random bytes retain 80 bits
// of uniqueness within a millisecond; the format and length are unchanged from
// the previous fully random value, and IDs stay opaque to every consumer.
func requestID() string {
	var b [16]byte
	ms := time.Now().UnixMilli()
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	if _, err := rand.Read(b[6:]); err != nil {
		// crypto/rand failure is a platform fault; fall back to the random
		// portion only so the request still gets an ID (uniqueness retained).
		for i := 6; i < len(b); i++ {
			b[i] = byte(time.Now().UnixNano() >> (i * 3))
		}
	}
	return requestIDEncoding.EncodeToString(b[:])
}

func hashKey(key string) string {
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// headerIntValue parses the first non-empty of the named headers as an
// integer. Single canonical owner for "read a provider's integer header"
// (processing-time and rate-limit headers share it): the first present name
// decides - an unparseable value does not fall through to later names. ok
// reports whether a value was read.
func headerIntValue(h http.Header, names ...string) (val int, ok bool) {
	for _, name := range names {
		if v := h.Get(name); v != "" {
			n, err := strconv.Atoi(v)
			if err == nil {
				return n, true
			}
			return 0, false
		}
	}
	return 0, false
}

// disconnectWriter wraps a downstream ResponseWriter so a write error records
// the client as gone (markClientGone: client_disconnected + 499 only while no
// error outcome is decided - matching the accounting the analyzer streaming
// paths do) and lets io.Copy abort on the failed write. Its Write also
// accumulates the accepted bytes into the record's ResponseBytes, so the
// raw-copy relay fallbacks (the io.Copy sites in relay.go) report
// response_bytes exactly like the explicit write loops.
type disconnectWriter struct {
	w   http.ResponseWriter
	rec *metrics.Record
}

func (d disconnectWriter) Write(p []byte) (int, error) {
	n, err := writeCounted(d.w, d.rec, p)
	if err != nil {
		markClientGone(d.rec)
	}
	return n, err
}

// firstNonEmpty returns the first non-empty string from the args.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// typeInvalidRequestError is the OpenAI error type the request trust
// boundaries stamp on client-side rejections (routing headers, body decode,
// token caps). One constant owns the spelling across every 400 surface.
const typeInvalidRequestError = "invalid_request_error"

// typeAPIError is OpenAI's generic error type: the unclassified default for
// upstream failures that carried no structured error envelope, and the type
// the proxy's own 502 surfaces (translation, metadata, degenerate
// completions) stamp. One constant owns the spelling across every 502
// surface.
const typeAPIError = "api_error"

// typeUpstreamUnreachable is the OpenAI error type for a transport-level
// upstream failure: no HTTP response at all (connect refused/reset, DNS,
// timeout awaiting headers). Shared by the send-failure epilogues and both
// in-band relay variants so the classification cannot drift.
const typeUpstreamUnreachable = "upstream_unreachable"

// upstreamErrorFields resolves the error type/message to surface for an
// upstream 4xx/5xx whose captured envelope carried neither: the
// unclassified default is OpenAI's api_error type plus the bare HTTP status.
// One owner for the defaulting the three upstream-error surfaces (the cursor
// bidi 4xx/5xx, the committed-SSE in-band error, and the pacer's in-band
// 4xx/5xx) previously hand-rolled independently.
func upstreamErrorFields(rec *metrics.Record, status int) (typ, msg string) {
	typ, msg = rec.ErrorType, rec.ErrorMsg
	if typ == "" {
		typ = typeAPIError
	}
	if msg == "" {
		msg = fmt.Sprintf("upstream HTTP %d", status)
	}
	return typ, msg
}

// errInvalidHeader is the shared rejection error for a client-supplied
// routing or limit header whose value failed its parse: "invalid <header>
// <value>". One owner of the wording and of the bounded echo across the
// routing-header and X-Proxy-Limit-* trust boundaries.
func errInvalidHeader(name, value string) error {
	return fmt.Errorf("invalid %s %q", name, clientEcho(value))
}

func errJSON(typ, msg string) string {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": typ},
	})
	return string(b)
}

// errJSONCode is errJSON plus a structured code: the degenerate-outcome 502s
// carry the canonical class (empty_completion/…) so clients can branch and
// retry on it exactly like any provider error code.
func errJSONCode(typ, msg, code string) string {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": code},
	})
	return string(b)
}

// requestBodyReadInit is the initial capacity of the growable request-body
// scratch. It is a read granularity, not a request-size limit: the scratch
// grows only as bytes actually arrive.
const requestBodyReadInit = 64 << 10

// requestBodyScratchMax bounds the one request-body scratch retained for reuse
// between requests. A body may grow the scratch past this bound (up to
// max_request_bytes); the oversized scratch is then dropped instead of
// retained, so the pool can never pin an operator-sized body. Internal
// allocation guardrail, not a request-size limit.
const requestBodyScratchMax = 32 << 20

// requestBodyScratchPool parks at most one growable read scratch. A body with
// no declared length has to grow as it arrives; a fresh growable buffer per
// request would pay a doubling sequence (about two body copies) every time.
// Reusing the last scratch leaves the steady-state allocation at the one
// exact buffer returned to the caller. The mutex and the single slot bound the
// retained memory: concurrent readers simply allocate their own scratch and
// cannot share bytes. A single scratch also means a later body of a different
// size reuses whatever capacity was proven necessary before.
var requestBodyScratchPool struct {
	mu  sync.Mutex
	buf []byte
}

func takeRequestBodyScratch(limit int) []byte {
	requestBodyScratchPool.mu.Lock()
	buf := requestBodyScratchPool.buf
	requestBodyScratchPool.buf = nil
	requestBodyScratchPool.mu.Unlock()
	if cap(buf) == 0 {
		return make([]byte, 0, min(requestBodyReadInit, limit))
	}
	return buf[:0]
}

func releaseRequestBodyScratch(buf []byte) {
	if cap(buf) == 0 || cap(buf) > requestBodyScratchMax {
		return
	}
	requestBodyScratchPool.mu.Lock()
	if requestBodyScratchPool.buf == nil {
		requestBodyScratchPool.buf = buf[:0]
	}
	requestBodyScratchPool.mu.Unlock()
}

// readRequest preserves the exact body for forwarding/retries and extracts
// routing + observability metadata once. Direct callers use the 1 MiB
// speculative guardrail; the proxy passes the admitted upload size so a
// declared body can be read once in place instead of prefix + copy.
func readRequest(r *http.Request, maxBytes int64, preview bool) (body []byte, rec *metrics.Record, err error) {
	return readRequestHint(r, maxBytes, preview, requestReadHintMax)
}

func readRequestHint(r *http.Request, maxBytes int64, preview bool, hintMax int64) (body []byte, rec *metrics.Record, err error) {
	rec = new(metrics.Record)
	if r.Body == nil {
		rec.Start = time.Now()
		return nil, rec, nil
	}
	defer r.Body.Close()
	if hintMax <= 0 {
		hintMax = requestReadHintMax
	}
	body, err = readRequestBody(r.Body, r.ContentLength, maxBytes, hintMax)
	if err != nil {
		return nil, nil, err
	}
	if int64(len(body)) > maxBytes {
		return nil, nil, &http.MaxBytesError{Limit: maxBytes}
	}
	// The inbound body size: one assignment on the bytes already in hand -
	// no second read, no timing change (Start below still excludes upload).
	// Stamped before the metadata decode so invalid-JSON passthrough
	// requests carry their body size too.
	rec.RequestBytes = int64(len(body))
	// Request timing excludes upload, but includes the unified metadata decode
	// and any later format translation. This is the sole Start owner.
	rec.Start = time.Now()
	if err := parseLLMRequest(body, rec, preview); err != nil {
		return nil, nil, err
	}
	return body, rec, nil
}

// readRequestBody reads at most maxBytes+1 bytes. A declared length that fits
// the caller's admitted hint is read directly into the one body allocation
// that is returned; every other shape (chunked, a zero/understated length, or
// a declared length past the speculative hint) grows through the bounded
// scratch and is copied once into an exact-size result. The returned slice may
// exceed maxBytes by one byte; the caller owns the typed limit error.
func readRequestBody(body io.Reader, contentLength, maxBytes, hintMax int64) ([]byte, error) {
	limit := maxBytes + 1
	reader := io.LimitReader(body, limit)
	hint := min(contentLength, limit, hintMax)
	switch {
	case contentLength < 0:
		// Unknown length (chunked): grow as the bytes arrive.
		return readGrowingRequestBody(reader, maxBytes, limit)
	case hint > 0:
		if contentLength <= hintMax {
			// The declared length fits the admitted allocation: read it in
			// place, with no growth or right-sizing copy. A short body is
			// accepted exactly as before.
			buf := make([]byte, int(hint))
			n, err := io.ReadFull(reader, buf)
			buf = buf[:n]
			if err == io.ErrUnexpectedEOF || err == io.EOF {
				err = nil
			}
			if err != nil {
				return nil, err
			}
			// A body longer than a short/understated Content-Length still
			// reads to the limit, as it always has; the tail probe costs
			// nothing when the reader is already at EOF.
			return readRequestBodyTail(reader, buf, maxBytes, limit)
		}
		// The declared length exceeds the speculative hint. Do not trust it
		// for an allocation; grow from the bytes that actually arrive.
		return readGrowingRequestBody(reader, maxBytes, limit)
	default:
		// A zero or absent declared length is still read to the limit: a
		// client may understate it.
		return readGrowingRequestBody(reader, maxBytes, limit)
	}
}

// readRequestBodyTail appends any bytes past the already-read prefix. A
// one-byte probe answers the normal end-of-body case without an allocation;
// only a genuinely longer body enters the grower, which starts from the
// already-read prefix rather than re-reading it.
func readRequestBodyTail(reader io.Reader, body []byte, maxBytes, limit int64) ([]byte, error) {
	var probe [1]byte
	for {
		n, err := reader.Read(probe[:])
		if n > 0 {
			if err != nil && err != io.EOF {
				return nil, err
			}
			buf := takeRequestBodyScratch(int(limit))
			need := len(body) + n
			if need > cap(buf) {
				buf = growRequestBodyCap(buf, need, int(limit))
			}
			buf = append(buf, body...)
			buf = append(buf, probe[:n]...)
			return finishGrowingRequestBody(buf, reader, maxBytes, limit)
		}
		if err != nil {
			if err == io.EOF {
				return body, nil
			}
			return nil, err
		}
	}
}

// readGrowingRequestBody buffers a body whose length is unknown. It reads
// through the bounded scratch, then copies once into the exact-size result
// returned to the caller.
func readGrowingRequestBody(reader io.Reader, maxBytes, limit int64) ([]byte, error) {
	return finishGrowingRequestBody(takeRequestBodyScratch(int(limit)), reader, maxBytes, limit)
}

func finishGrowingRequestBody(buf []byte, reader io.Reader, maxBytes, limit int64) ([]byte, error) {
	buf, err := growRequestBody(buf, reader, int(limit))
	if err != nil {
		releaseRequestBodyScratch(buf)
		return nil, err
	}
	if int64(len(buf)) > maxBytes {
		// The caller rejects with MaxBytesError and never reads these bytes.
		// Returning the scratch avoids a pointless exact-size copy; it is not
		// pooled because the caller still holds the slice until it returns.
		return buf, nil
	}
	body := make([]byte, len(buf))
	copy(body, buf)
	releaseRequestBodyScratch(buf)
	return body, nil
}

// growRequestBody appends every remaining byte from reader to buf until EOF or
// the limit is reached. Each growth allocates one new buffer, unlike
// bytes.Buffer's growSlice (a make plus an append copy, so two body-sized
// allocations per step), and a full buffer is probed with one byte before
// growing so an exact-size body leaves the scratch at that exact capacity
// instead of triggering a pointless final doubling.
func growRequestBody(buf []byte, reader io.Reader, limit int) ([]byte, error) {
	for {
		if len(buf) < cap(buf) {
			n, err := reader.Read(buf[len(buf):cap(buf)])
			buf = buf[:len(buf)+n]
			if err != nil {
				if err == io.EOF {
					return buf, nil
				}
				return nil, err
			}
			continue
		}
		if cap(buf) >= limit {
			// The limit reader is exhausted; a larger body is rejected by
			// the caller, which needs no more bytes.
			return buf, nil
		}
		var probe [1]byte
		n, err := reader.Read(probe[:])
		if n > 0 {
			buf = growRequestBodyCap(buf, len(buf)+1, limit)
			buf = append(buf, probe[0])
		}
		if err != nil {
			if err == io.EOF {
				return buf, nil
			}
			return nil, err
		}
	}
}

// growRequestBodyCap returns buf with room for at least need bytes. Growth
// doubles (the best worst-case total for an unknown length) but never exceeds
// the caller's limit and never falls below the initial read granularity.
func growRequestBodyCap(buf []byte, need, limit int) []byte {
	if need <= cap(buf) {
		return buf
	}
	nextCap := cap(buf) * 2
	if nextCap < need {
		nextCap = need
	}
	if nextCap < requestBodyReadInit {
		nextCap = min(requestBodyReadInit, limit)
	}
	if nextCap > limit {
		nextCap = limit
	}
	if nextCap <= cap(buf) {
		return buf
	}
	grown := make([]byte, len(buf), nextCap)
	copy(grown, buf)
	return grown
}

// discardedJSON accepts every syntactically valid JSON shape as a no-op. The
// enclosing decoder owns syntax validation, and the metadata key scan only
// locates values with the shared lexical scanner, so nothing is materialized
// or re-decoded merely to be discarded.
type discardedJSON struct{}

func (*discardedJSON) UnmarshalJSON([]byte) error { return nil }

// maxRequestOutputTokens bounds a client-supplied max_tokens /
// max_completion_tokens at the trust boundary. Mainstream model output caps
// top out around 128k tokens; anything beyond 1,000,000 (the same ceiling
// internal/config validates for anthropic_default_max_tokens) is hostile or
// malformed. Unbounded values would let one request wrap the Acquire-time
// token estimate negative or debit a provider token bucket by the full
// attacker-chosen amount.
const maxRequestOutputTokens = 1_000_000

// hostileTokenCap reports whether a decoded token cap exceeds the
// maxRequestOutputTokens bound. Both ServeHTTP checks share it: the check
// on the original body's decode and the check on the fresh record a
// translated body re-decodes into.
func hostileTokenCap(v *int) bool {
	return v != nil && *v > maxRequestOutputTokens
}

// tokenCapMembers holds the first raw occurrence of each accepted token-ceiling
// spelling. It is the occurrence scan's container, not an independently decoded
// record: encoding/json folds member names case-insensitively and keeps the
// last duplicate, while the relay sends the body byte-for-byte, so a second
// occurrence could describe a different value to the record and the wire.
type tokenCapMembers struct {
	MaxTokens       json.RawMessage `json:"max_tokens"`
	MaxCompTokens   json.RawMessage `json:"max_completion_tokens"`
	MaxOutputTokens json.RawMessage `json:"max_output_tokens"`
}

// tokenCapPresent reports whether a raw cap member was supplied. JSON null is
// the schema's explicit absent value.
func tokenCapPresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// scanTokenCaps walks the top-level object once and collects the first raw
// value of each accepted token-ceiling spelling. A second occurrence of any
// member that folds to a cap name fails instead of letting the record and
// reservation validate one value while the relay sends every occurrence
// upstream. Invalid JSON returns an empty scan so the existing passthrough
// path is preserved.
func scanTokenCaps(body []byte) (tokenCapMembers, error) {
	var caps tokenCapMembers
	var seen [3]bool
	i := metrics.SkipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return caps, nil
	}
	i = metrics.SkipSpace(body, i+1)
	for i < len(body) && body[i] != '}' {
		keyStart := i
		end := skipJSONString(body, i)
		if end < 0 {
			return tokenCapMembers{}, nil
		}
		key, ok := decodeRequestKey(body[keyStart:end])
		if !ok {
			return tokenCapMembers{}, nil
		}
		i = metrics.SkipSpace(body, end)
		if i >= len(body) || body[i] != ':' {
			return tokenCapMembers{}, nil
		}
		i = metrics.SkipSpace(body, i+1)
		if i >= len(body) {
			return tokenCapMembers{}, nil
		}
		valueStart := i
		next := skipJSONValue(body, i)
		if next < 0 {
			return tokenCapMembers{}, nil
		}
		switch {
		case key.equal("max_tokens"):
			if seen[0] {
				return tokenCapMembers{}, errors.New("max_tokens appears more than once")
			}
			seen[0] = true
			caps.MaxTokens = json.RawMessage(body[valueStart:next])
		case key.equal("max_completion_tokens"):
			if seen[1] {
				return tokenCapMembers{}, errors.New("max_completion_tokens appears more than once")
			}
			seen[1] = true
			caps.MaxCompTokens = json.RawMessage(body[valueStart:next])
		case key.equal("max_output_tokens"):
			if seen[2] {
				return tokenCapMembers{}, errors.New("max_output_tokens appears more than once")
			}
			seen[2] = true
			caps.MaxOutputTokens = json.RawMessage(body[valueStart:next])
		}
		i = metrics.SkipSpace(body, next)
		if i < len(body) && body[i] == ',' {
			i = metrics.SkipSpace(body, i+1)
			continue
		}
		if i < len(body) && body[i] == '}' {
			break
		}
		return tokenCapMembers{}, nil
	}
	return caps, nil
}

// tokenCapValue validates one raw token-ceiling member. An absent or JSON null
// member returns nil with no error. A supplied member must be a JSON number
// whose value is integral and no greater than maxRequestOutputTokens; the
// returned int is that exact value. The wire schema requires an integer, so a
// fractional, overflowing, out-of-range or non-number value fails closed
// instead of slipping past a typed decode that rejected the whole object.
func tokenCapValue(raw json.RawMessage, field string) (*int, error) {
	if !tokenCapPresent(raw) {
		return nil, nil
	}
	n, err := jsonnum.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s %s", field, err)
	}
	if n > maxRequestOutputTokens {
		return nil, fmt.Errorf("%s must not exceed %d", field, maxRequestOutputTokens)
	}
	return &n, nil
}

// conflictingTokenCapSpellings reports a body that mixes the Responses
// max_output_tokens spelling with a Chat max_tokens/max_completion_tokens
// spelling. Presence is decided on the occurrence scan, so an unrepresentable
// spelling cannot hide the mix. The wire families are distinct, so accepting
// both would make the recorded ceiling depend on decode precedence rather than
// the client's wire.
func conflictingTokenCapSpellings(body []byte) bool {
	caps, err := scanTokenCaps(body)
	if err != nil {
		return false
	}
	return tokenCapPresent(caps.MaxOutputTokens) &&
		(tokenCapPresent(caps.MaxTokens) || tokenCapPresent(caps.MaxCompTokens))
}

// tokenCapViolation returns the client-facing 400 reason for the first token
// ceiling spelling that is hostile, malformed or duplicated, or the empty
// string when every occurrence is valid. It checks every occurrence the body
// carries, not only the one that wins the record precedence: a Responses body
// can carry both max_tokens and max_output_tokens, and checking only the winner
// let the other spelling reach the upstream unbounded. The body is authoritative
// because it carries the client's actual field names; the record's effective
// ceiling is a fallback for a body whose cap fields did not decode.
func tokenCapViolation(body []byte, rec *metrics.Record) string {
	caps, err := scanTokenCaps(body)
	if err != nil {
		return err.Error()
	}
	for _, cap := range []struct {
		name string
		raw  json.RawMessage
	}{
		{"max_completion_tokens", caps.MaxCompTokens},
		{"max_tokens", caps.MaxTokens},
		{"max_output_tokens", caps.MaxOutputTokens},
	} {
		if _, err := tokenCapValue(cap.raw, cap.name); err != nil {
			return err.Error()
		}
	}
	if rec != nil && hostileTokenCap(rec.ReqMaxTokens) {
		return fmt.Sprintf("output token cap must not exceed %d", maxRequestOutputTokens)
	}
	return ""
}

// writeHostileTokenCap rejects a hostile or malformed token cap with the shared
// 400 envelope. Both ServeHTTP enforcement sites use it, so the two trust
// boundaries can never drift in status or type.
func writeHostileTokenCap(w http.ResponseWriter, inbound ir.Protocol, reason string) {
	writeProtocolHTTPError(w, inbound, http.StatusBadRequest, typeInvalidRequestError, reason)
}

type requestRouting struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

type requestMessage[C any] struct {
	Role    string `json:"role"`
	Content C      `json:"content"`
}

type requestParameters struct {
	Temperature    *float64          `json:"temperature"`
	TopP           *float64          `json:"top_p"`
	Tools          []discardedJSON   `json:"tools"`
	ToolChoice     requestToolChoice `json:"tool_choice"`
	StreamOptions  *struct{}         `json:"stream_options"`
	N              json.RawMessage   `json:"n"`
	Stop           json.RawMessage   `json:"stop"`
	StopSequences  json.RawMessage   `json:"stop_sequences"`
	Logprobs       bool              `json:"logprobs"`
	PresencePen    *float64          `json:"presence_penalty"`
	FrequencyPen   *float64          `json:"frequency_penalty"`
	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format"`
	// Text and Include are the native Responses spellings for the response
	// format/verbosity pair and the logprob opt-in. They are raw so a valid
	// but unusual shape cannot trigger a typed-decode failure that erases the
	// rest of the record.
	Text *struct {
		Format *struct {
			Type string `json:"type"`
		} `json:"format"`
		Verbosity string `json:"verbosity"`
	} `json:"text"`
	Include         json.RawMessage          `json:"include"`
	Seed            json.RawMessage          `json:"seed"`
	ParallelTools   *bool                    `json:"parallel_tool_calls"`
	LogitBias       map[string]discardedJSON `json:"logit_bias"`
	TopLogprobs     json.RawMessage          `json:"top_logprobs"`
	ServiceTier     string                   `json:"service_tier"`
	Thinking        json.RawMessage          `json:"thinking"`
	Metadata        requestMetadataKeys      `json:"metadata"`
	ReasoningEffort string                   `json:"reasoning_effort"`
	Verbosity       string                   `json:"verbosity"`
	Reasoning       json.RawMessage          `json:"reasoning"`
	OutputConfig    *struct {
		Effort string `json:"effort"`
		Format *struct {
			Type string `json:"type"`
		} `json:"format"`
	} `json:"output_config"`
	Speed string `json:"speed"`
	// System carries the system prompt for Anthropic-shaped bodies (OpenAI
	// puts it in a role:"system"/"developer" message instead). Content only.
	System   requestContent                   `json:"system"`
	Messages []requestMessage[requestContent] `json:"messages"`
}

// requestToolChoice captures the recorded shape of a tool_choice value during
// the single document decode. The string shorthand is kept verbatim. A Chat
// object contributes function.name with the "tool:" prefix; an Anthropic object
// contributes its type (any becomes the Chat required spelling, tool becomes
// tool:name, auto and none stay themselves) and its disable_parallel_tool_use
// flag. Every other or malformed shape records nothing. A typed decode visits
// only the known members and skips unknown or deeply nested ones instead of
// materializing the whole value as a map[string]any per node. Each top-level
// occurrence resets the field, so a duplicate tool_choice key keeps JSON's
// last-wins semantics.
type requestToolChoice struct {
	Value           string
	DisableParallel bool
	DisableSet      bool
}

func (t *requestToolChoice) UnmarshalJSON(raw []byte) error {
	*t = requestToolChoice{}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			t.Value = s
		}
		return nil
	}
	var choice struct {
		// Raw members keep JSON's last-wins semantics for duplicate keys and
		// let one malformed member reset the whole record instead of failing
		// the request.
		Function        json.RawMessage `json:"function"`
		Type            string          `json:"type"`
		Name            string          `json:"name"`
		DisableParallel json.RawMessage `json:"disable_parallel_tool_use"`
	}
	if json.Unmarshal(trimmed, &choice) != nil {
		return nil
	}
	if raw := bytes.TrimSpace(choice.DisableParallel); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
		var disable bool
		if json.Unmarshal(raw, &disable) != nil {
			return nil
		}
		t.DisableSet = true
		t.DisableParallel = disable
	}
	if raw := bytes.TrimSpace(choice.Function); len(raw) > 0 {
		if bytes.Equal(raw, []byte("null")) {
			return nil
		}
		var function struct {
			Name *string `json:"name"`
		}
		if json.Unmarshal(raw, &function) == nil && function.Name != nil {
			t.Value = "tool:" + *function.Name
		}
		return nil
	}
	switch choice.Type {
	case "any":
		t.Value = "required"
	case "tool", "function":
		// Chat/Anthropic spell the forced tool as type "tool" with name, and
		// native Responses spells it as type "function" with name.
		if choice.Name != "" {
			t.Value = "tool:" + choice.Name
		}
	case "auto", "none":
		t.Value = choice.Type
	}
	return nil
}

// requestStopCount records how many stop sequences a raw stop field carries.
// A string is one sequence and an array contributes its length; null, an
// object and every other shape record zero. The scan is lexical: counting an
// accepted array never materializes a []string, and an over-bound array stops
// at requestMetadataMaxElements+1.
func requestStopCount(raw json.RawMessage) int {
	i := metrics.SkipSpace(raw, 0)
	if i >= len(raw) {
		return 0
	}
	switch raw[i] {
	case '"':
		if metrics.SkipJSONString(raw, i) == i+2 {
			return 0
		}
		return 1
	case '[':
		n, _, ok := metrics.CountJSONArrayBytes(raw, i, requestMetadataMaxElements)
		if ok {
			return n
		}
	}
	return 0
}

// requestIncludeLogprobs reports whether a raw Responses include array names
// the output-text logprob option. The scan is lexical: it never materializes
// the array, and a malformed or non-array value records nothing.
func requestIncludeLogprobs(raw json.RawMessage) bool {
	i := metrics.SkipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return false
	}
	const field = `"message.output_text.logprobs"`
	i = metrics.SkipSpace(raw, i+1)
	for i < len(raw) && raw[i] != ']' {
		if raw[i] == '"' {
			end := skipJSONString(raw, i)
			if end < 0 {
				return false
			}
			if string(raw[i:end]) == field {
				return true
			}
			i = metrics.SkipSpace(raw, end)
		} else {
			next := skipJSONValue(raw, i)
			if next < 0 {
				return false
			}
			i = metrics.SkipSpace(raw, next)
		}
		if i < len(raw) && raw[i] == ',' {
			i = metrics.SkipSpace(raw, i+1)
		}
	}
	return false
}

// requestMetadataKeysMax bounds the distinct top-level metadata keys counted
// per request. Metadata is a small client note map; counts below the cap are
// exact, and a count at the cap means the map held at least that many keys.
// Internal safety guardrail, not user-tunable.
const requestMetadataKeysMax = 1024

// requestMetadataKeys counts a metadata object's distinct top-level keys
// without materializing a per-key value map. The enclosing document decode
// validates JSON syntax before this runs, and this scan only locates values
// with the shared lexical scanner before passing them to the no-op
// discardedJSON, so no value is decoded. Duplicate metadata members merge like
// the previous map field, and null clears the accumulated keys. The recorded
// count saturates at requestMetadataKeysMax: a value at the cap means "at
// least this many keys", never that a larger map was fully counted.
type requestMetadataKeys struct {
	keys map[string]struct{}
}

func (m *requestMetadataKeys) UnmarshalJSON(raw []byte) error {
	i := metrics.SkipSpace(raw, 0)
	if i >= len(raw) {
		return nil
	}
	if raw[i] == 'n' {
		// JSON null replaces the previous map value, as map decoding did.
		m.keys = nil
		return nil
	}
	if raw[i] != '{' {
		// Preserve the standard decoder's type error for a non-object value so
		// the split-decode boundary is unchanged.
		var discarded map[string]discardedJSON
		return json.Unmarshal(raw, &discarded)
	}
	if m.keys == nil {
		m.keys = make(map[string]struct{}, 8)
	}
	i = metrics.SkipSpace(raw, i+1)
	for i < len(raw) && raw[i] != '}' {
		keyEnd := skipJSONString(raw, i)
		if keyEnd < 0 {
			return fmt.Errorf("metadata key is not a JSON string")
		}
		if len(m.keys) < requestMetadataKeysMax {
			var key string
			if err := json.Unmarshal(raw[i:keyEnd], &key); err != nil {
				return err
			}
			m.keys[key] = struct{}{}
		}
		i = metrics.SkipSpace(raw, keyEnd)
		if i >= len(raw) || raw[i] != ':' {
			return fmt.Errorf("metadata member has no value")
		}
		i = metrics.SkipSpace(raw, i+1)
		if i >= len(raw) {
			return fmt.Errorf("metadata member has no value")
		}
		valueEnd := skipJSONValue(raw, i)
		if valueEnd < 0 {
			return fmt.Errorf("metadata value is unterminated")
		}
		var discarded discardedJSON
		if err := discarded.UnmarshalJSON(raw[i:valueEnd]); err != nil {
			return err
		}
		i = metrics.SkipSpace(raw, valueEnd)
		if i < len(raw) && raw[i] == ',' {
			i = metrics.SkipSpace(raw, i+1)
			continue
		}
		break
	}
	return nil
}

func (m *requestMetadataKeys) count() int { return len(m.keys) }

// parseLLMRequest is the single document decode for routing and metadata.
// Keep the two independent trust boundaries on type errors: a malformed
// metadata-only field must not erase valid model/stream routing, and vice
// versa. Only that exceptional path needs independent decoding; malformed
// JSON still passes through unchanged with no partially extracted metadata.
// requestMetadataMaxElements bounds the decoded top-level collection sizes and
// the distinct top-level member count before the single metadata decode and
// before the rewrite engine materializes every top-level value. A 32 MiB body
// can otherwise expand into hundreds of MiB of empty objects or raw values;
// this is an internal DoS guardrail, not a wire-format limit, and 10k
// messages/tools/logit_bias/stop entries or top-level keys is far above a
// normal request. The wire codecs use the same ceiling for their own decode
// preflight, so the proxy can never reject a document the translator would
// accept (or the reverse).
const requestMetadataMaxElements = providerformat.WireCollectionMax

// requestContentPartsMax bounds the aggregate content-part arrays over every
// message, the top-level system content and every nested Responses input
// content/output array. The per-array metrics.ContentPartMax keeps one message
// from expanding a typed slice, but thousands of messages at that per-array cap
// still compose a request-sized slice of empty parts. It reuses the format
// owner so the preflight and the translated codec accept the same aggregate.
// Internal DoS guardrail, not a wire-format or user-tunable limit; text length
// never counts against it.
const requestContentPartsMax = providerformat.WireContentPartsMax

// requestAnnotationsMax bounds the aggregate annotation members over every
// Responses content part. Each annotation becomes a typed citation and a
// possible warning, so per-array ContentPartMax alone lets thousands of parts
// multiply. It reuses the format owner's aggregate so the proxy preflight and
// the codec reject the same document class.
const requestAnnotationsMax = providerformat.WireNestedCollectionMax

// requestAnnotationArrayMax bounds one Responses annotation array. It matches
// the codec's per-array check in checkResponsesContent, so a single oversized
// array fails in the preflight instead of after translation begins.
const requestAnnotationArrayMax = metrics.ContentPartMax

// requestToolCallsMax bounds the aggregate tool_calls array members over every
// message. Each member expands into a typed tool-call value and, on the
// translated Anthropic or Cursor wire, a content block/history part. A 300 KiB
// body of empty calls therefore materializes six figures of typed values and
// over a hundred MiB of transient allocation. Internal DoS guardrail, not a
// wire-format or user-tunable limit; real requests carry a handful of calls
// per turn.
const requestToolCallsMax = providerformat.WireToolCallsMax

// boundRequestMetadata is a lexical preflight over the top-level document. It
// rejects only a valid JSON document whose messages/tools/stop array,
// logit_bias object, distinct top-level member count, aggregate content parts,
// or aggregate tool calls exceed their bounds. Invalid JSON returns nil so the
// existing passthrough behavior is preserved; collection members are skipped
// with the shared byte scanner. One small distinct-key set per top-level object
// records only the keys it sees; escaped and non-ASCII keys decode once per
// member and that result feeds both the distinct set and field comparisons.
func boundRequestMetadata(body []byte) error {
	i := metrics.SkipSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return nil
	}
	i = metrics.SkipSpace(body, i+1)
	if i < len(body) && body[i] == '}' {
		return nil
	}
	// logit_bias is a map field: duplicate top-level objects merge in
	// encoding/json, so distinct keys accumulate across every occurrence.
	// Slice fields overwrite for the final value, but every duplicate
	// occurrence still reaches its decoder, so each is counted below.
	var logitSeen map[string]struct{}
	logitOver := false
	// metadata is also a map field. Duplicate metadata objects merge in
	// encoding/json, so distinct keys accumulate across every occurrence. A
	// single occurrence needs only a raw member count: distinct keys cannot
	// exceed raw members, and the typed decode caps its retained keys at the
	// same ceiling. The distinct-key set is built only once a second object
	// makes merge semantics relevant, so a hostile single object never
	// retains a decoded key map.
	var metadataSeen map[string]struct{}
	metadataObjects := 0
	metadataFirstStart := 0
	metadataFirstOver := false
	metadataOver := false
	// memberSeen counts distinct top-level keys because the rewrite engine and
	// metadata decode both keep one entry per distinct key; duplicate members
	// must stay cheap and accepted. Encoding/json retains the last duplicate
	// value, so raw member count is not the decoded map size.
	var memberSeen map[string]struct{}
	// contentParts accumulates message content-part array lengths, the
	// top-level system content and every nested Responses input content/output
	// part. annotations accumulates the annotation members inside each of
	// those parts. toolCalls accumulates every message tool_calls member. All
	// stop growing at the first over-budget array, so the rejection costs one
	// bounded prefix scan.
	contentParts := 0
	annotationParts := 0
	toolCalls := 0
	// stream_options is a RawMessage field: duplicate top-level occurrences
	// overwrite, so only the last object reaches the typed decode. Retain its
	// span and bound that occurrence after the scan, matching the codec.
	streamOptionsStart, streamOptionsEnd := -1, -1
	streamOptionsSeen := false
	for i < len(body) {
		keyStart := i
		end := skipJSONString(body, i)
		if end < 0 {
			return nil
		}
		key, ok := decodeRequestKey(body[keyStart:end])
		if !ok {
			return nil
		}
		if memberSeen == nil {
			memberSeen = make(map[string]struct{})
		}
		n := countDistinctJSONKey(memberSeen, key, requestMetadataMaxElements)
		if n > requestMetadataMaxElements {
			return requestMetadataOverflow(body, fmt.Errorf("request top-level members exceed %d", requestMetadataMaxElements))
		}
		i = metrics.SkipSpace(body, end)
		if i >= len(body) || body[i] != ':' {
			return nil
		}
		i = metrics.SkipSpace(body, i+1)
		if i >= len(body) {
			return nil
		}
		collection := key.collection()
		switch collection {
		case "messages":
			n, next, ok := metrics.CountJSONArrayBytes(body, i, requestMetadataMaxElements)
			if !ok {
				return nil
			}
			if n > requestMetadataMaxElements {
				return requestMetadataOverflow(body, fmt.Errorf("request %s exceeds %d elements", collection, requestMetadataMaxElements))
			}
			if n > 0 {
				parts, ok := countRequestMessagesContentParts(body, i, requestContentPartsMax-contentParts)
				if !ok {
					return nil
				}
				if parts > requestContentPartsMax-contentParts {
					return requestMetadataOverflow(body, fmt.Errorf("request content parts exceed %d", requestContentPartsMax))
				}
				contentParts += parts
				calls, ok := countRequestMessagesToolCalls(body, i, requestToolCallsMax-toolCalls)
				if !ok {
					return nil
				}
				if calls > requestToolCallsMax-toolCalls {
					return requestMetadataOverflow(body, fmt.Errorf("request tool calls exceed %d", requestToolCallsMax))
				}
				toolCalls += calls
			}
			i = next
		case "tools", "functions", "modalities":
			n, next, ok := metrics.CountJSONArrayBytes(body, i, requestMetadataMaxElements)
			if !ok {
				return nil
			}
			if n > requestMetadataMaxElements {
				return requestMetadataOverflow(body, fmt.Errorf("request %s exceeds %d elements", collection, requestMetadataMaxElements))
			}
			i = next
		case "input":
			// Responses input is an item array whose nested content/output
			// arrays are decoded by every translated request. Count both the
			// items and their parts/annotations here so the preflight rejects
			// an amplification body before the codec materializes it.
			n, next, ok := metrics.CountJSONArrayBytes(body, i, requestMetadataMaxElements)
			if !ok {
				return nil
			}
			if n > requestMetadataMaxElements {
				return requestMetadataOverflow(body, fmt.Errorf("request %s exceeds %d elements", collection, requestMetadataMaxElements))
			}
			if n > 0 {
				parts, annotations, nestedNext, annotationArrayOver, nestedOK := countResponsesInputNested(
					body, i, requestContentPartsMax-contentParts, requestAnnotationsMax-annotationParts)
				if !nestedOK {
					return nil
				}
				if annotationArrayOver {
					return requestMetadataOverflow(body, fmt.Errorf("request annotation array exceeds %d", requestAnnotationArrayMax))
				}
				if parts > requestContentPartsMax-contentParts {
					return requestMetadataOverflow(body, fmt.Errorf("request content parts exceed %d", requestContentPartsMax))
				}
				if annotations > requestAnnotationsMax-annotationParts {
					return requestMetadataOverflow(body, fmt.Errorf("request annotations exceed %d", requestAnnotationsMax))
				}
				contentParts += parts
				annotationParts += annotations
				next = nestedNext
			}
			i = next
		case "metadata":
			next := skipJSONValue(body, i)
			if next < 0 {
				return nil
			}
			trimmed := metrics.SkipSpace(body, i)
			switch {
			case trimmed < next && body[trimmed] == 'n':
				// encoding/json sets the map field to nil for null, so every
				// key merged before this member is discarded.
				clear(metadataSeen)
				metadataSeen = nil
				metadataObjects = 0
				metadataFirstStart = 0
				metadataFirstOver, metadataOver = false, false
			case trimmed < next && body[trimmed] == '{':
				metadataObjects++
				rawOK := metrics.ObjectWithinLimit(body[i:next], requestMetadataMaxElements)
				if metadataObjects == 1 {
					metadataFirstStart = i
					metadataFirstOver = !rawOK
				} else if !metadataOver {
					if metadataSeen == nil {
						// Merge semantics now need the first object's
						// distinct keys; it was counted but not decoded.
						metadataSeen = make(map[string]struct{}, 8)
						n, _, ok := countJSONObjectKeys(body, metadataFirstStart, requestMetadataMaxElements, metadataSeen)
						if !ok {
							return nil
						}
						if n > requestMetadataMaxElements {
							metadataOver = true
						}
					}
					if !metadataOver {
						n, _, ok := countJSONObjectKeys(body, i, requestMetadataMaxElements, metadataSeen)
						if !ok {
							return nil
						}
						if n > requestMetadataMaxElements {
							metadataOver = true
						}
					}
				}
			}
			i = next
		case "stream_options":
			next := skipJSONValue(body, i)
			if next < 0 {
				return nil
			}
			streamOptionsStart, streamOptionsEnd = i, next
			streamOptionsSeen = true
			i = next
		case "stop":
			// stop is decoded into []string only to count it. A string is one
			// value and every non-array shape is tolerated by the metadata
			// decoder, so only an array is bounded here.
			n, next, ok := metrics.CountJSONArrayBytes(body, i, requestMetadataMaxElements)
			if !ok {
				return nil
			}
			if n > requestMetadataMaxElements {
				return requestMetadataOverflow(body, fmt.Errorf("request stop exceeds %d values", requestMetadataMaxElements))
			}
			i = next
		case "logit_bias":
			if logitOver {
				next := skipJSONValue(body, i)
				if next < 0 {
					return nil
				}
				i = next
				break
			}
			if logitSeen == nil {
				logitSeen = make(map[string]struct{})
			}
			n, next, ok := countJSONObjectKeys(body, i, requestMetadataMaxElements, logitSeen)
			if !ok {
				return nil
			}
			if n > requestMetadataMaxElements {
				logitOver = true
			}
			i = next
		default:
			if key.equal("system") {
				// Anthropic bodies carry a top-level system field whose array
				// shape composes through the same requestContent decoder as a
				// message. Count it against the aggregate budget; a string or
				// other shape contributes no parts.
				n, next, ok := metrics.CountJSONArrayBytes(body, i, requestContentPartsMax-contentParts)
				if !ok {
					return nil
				}
				if n > requestContentPartsMax-contentParts {
					return requestMetadataOverflow(body, fmt.Errorf("request content parts exceed %d", requestContentPartsMax))
				}
				contentParts += n
				i = next
				break
			}
			next := skipJSONValue(body, i)
			if next < 0 {
				return nil
			}
			i = next
		}
		i = metrics.SkipSpace(body, i)
		if i >= len(body) {
			return nil
		}
		if body[i] == '}' {
			break
		}
		if body[i] != ',' {
			return nil
		}
		i = metrics.SkipSpace(body, i+1)
	}
	if logitOver {
		return requestMetadataOverflow(body, fmt.Errorf("request logit_bias exceeds %d entries", requestMetadataMaxElements))
	}
	if metadataOver || (metadataObjects == 1 && metadataFirstOver) {
		return requestMetadataOverflow(body, fmt.Errorf("request metadata exceeds %d members", requestMetadataMaxElements))
	}
	if streamOptionsSeen &&
		!metrics.ObjectWithinLimit(body[streamOptionsStart:streamOptionsEnd], requestMetadataMaxElements) {
		return requestMetadataOverflow(body, fmt.Errorf("request stream_options exceeds %d members", requestMetadataMaxElements))
	}
	return nil
}

// requestKey is one raw JSON object key decoded at most once per member. The
// common plain ASCII spelling keeps only the source slice, so its comparisons
// and duplicate lookups allocate nothing; text holds the canonical string
// when the source needs JSON unescaping, invalid UTF-8 replacement or Unicode
// case folding. One top-level member used to decode an escaped key once for
// the distinct-key set and again for every candidate field name.
type requestKey struct {
	raw     []byte
	text    string
	textSet bool
}

// decodeRequestKey validates one raw JSON string key and decodes it at most
// once. ok is false on an undecodable key, which leaves the document on the
// existing malformed-JSON passthrough path.
func decodeRequestKey(raw []byte) (requestKey, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return requestKey{}, false
	}
	inner := raw[1 : len(raw)-1]
	if bytes.IndexByte(inner, '\\') < 0 && utf8.Valid(inner) {
		ascii := true
		for _, c := range inner {
			if c >= utf8.RuneSelf {
				ascii = false
				break
			}
		}
		if ascii {
			return requestKey{raw: inner}, true
		}
		// Valid non-ASCII UTF-8 needs no unescaping, but struct-field
		// matching still folds Unicode, so retain one canonical string.
		return requestKey{raw: inner, text: string(inner), textSet: true}, true
	}
	var decoded string
	if json.Unmarshal(raw, &decoded) != nil {
		return requestKey{}, false
	}
	return requestKey{raw: inner, text: decoded, textSet: true}, true
}

// equal reports whether the key names the given struct field. encoding/json
// matches struct fields case-insensitively, while map keys stay exact.
func (k requestKey) equal(name string) bool {
	if k.textSet {
		return strings.EqualFold(k.text, name)
	}
	return asciiKeyEqual(k.raw, name)
}

// collection returns the canonical bounded collection name for this top-level
// key, or "" when the key is not one of them. encoding/json matches struct
// fields case-insensitively, so "Messages"/"MESSAGES" reach messages; it does
// not fold '-' or '_', so "Mes-sages" stays unknown.
func (k requestKey) collection() string {
	switch {
	case k.equal("messages"):
		return "messages"
	case k.equal("tools"):
		return "tools"
	case k.equal("functions"):
		return "functions"
	case k.equal("input"):
		return "input"
	case k.equal("metadata"):
		return "metadata"
	case k.equal("stream_options"):
		return "stream_options"
	case k.equal("logit_bias"):
		return "logit_bias"
	case k.equal("stop"), k.equal("stop_sequences"):
		return "stop"
	case k.equal("modalities"):
		return "modalities"
	}
	return ""
}

// requestKeyEqual reports whether one raw JSON object key names the given
// struct field. It serves the nested key scans that decode one key per call;
// the top-level preflight decodes once and reuses the requestKey instead.
func requestKeyEqual(raw []byte, name string) bool {
	key, ok := decodeRequestKey(raw)
	return ok && key.equal(name)
}

// countDistinctJSONKey records one decoded top-level object key in seen and
// returns the distinct count. Plain ASCII keys allocate only when a new key
// is inserted; duplicate lookups stay allocation-free. Escaped, non-ASCII or
// invalid UTF-8 keys reuse the member's one canonical decode, so they compare
// with the exact string the rewrite map will hold. Once max distinct keys are
// seen, a new key reports max+1 without recording it, so the caller can reject
// without growing the set.
func countDistinctJSONKey(seen map[string]struct{}, key requestKey, max int) int {
	if key.textSet {
		if _, exists := seen[key.text]; exists {
			return len(seen)
		}
		if len(seen) >= max {
			return max + 1
		}
		seen[key.text] = struct{}{}
		return len(seen)
	}
	if _, exists := seen[string(key.raw)]; exists {
		return len(seen)
	}
	if len(seen) >= max {
		return max + 1
	}
	seen[string(key.raw)] = struct{}{}
	return len(seen)
}

// countRequestMessagesContentParts counts the aggregate content-part array
// lengths over the message objects of the messages array at start, including
// every duplicate content occurrence in a message. budget is the remaining
// aggregate allowance; a count above it means the aggregate bound is exceeded.
// Non-object elements and non-array content contribute zero, matching the
// typed decoder's shape tolerance. ok is false on malformed input so the
// document keeps its passthrough behavior.
func countRequestMessagesContentParts(data []byte, start, budget int) (int, bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) || data[i] != '[' {
		return 0, true
	}
	i = metrics.SkipSpace(data, i+1)
	total := 0
	for i < len(data) && data[i] != ']' {
		n, next, ok := countRequestMessageContentParts(data, i, budget-total)
		if !ok {
			return 0, false
		}
		if n > budget-total {
			return total + n, true
		}
		total += n
		i = metrics.SkipSpace(data, next)
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			if i >= len(data) || data[i] == ']' {
				return 0, false
			}
			continue
		}
		if i < len(data) && data[i] == ']' {
			break
		}
		return 0, false
	}
	if i >= len(data) || data[i] != ']' {
		return 0, false
	}
	return total, true
}

// countRequestMessageContentParts counts one message object's content arrays.
// Duplicate content members each reach requestContent.UnmarshalJSON before
// encoding/json's last-wins field assignment, so every occurrence is charged
// against the budget; only a non-array occurrence contributes zero. Counting
// stops at the first occurrence that crosses the remaining budget, so the
// rejection costs one bounded prefix scan.
func countRequestMessageContentParts(data []byte, start, budget int) (int, int, bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) {
		return 0, 0, false
	}
	if data[i] != '{' {
		next := metrics.SkipJSONValue(data, i)
		return 0, next, next >= 0
	}
	i = metrics.SkipSpace(data, i+1)
	parts := 0
	for i < len(data) && data[i] != '}' {
		end := skipJSONString(data, i)
		if end < 0 {
			return 0, 0, false
		}
		key := data[i:end]
		i = metrics.SkipSpace(data, end)
		if i >= len(data) || data[i] != ':' {
			return 0, 0, false
		}
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) {
			return 0, 0, false
		}
		if requestKeyEqual(key, "content") {
			n, next, ok := metrics.CountJSONArrayBytes(data, i, budget-parts)
			if !ok {
				return 0, 0, false
			}
			parts += n
			if parts > budget {
				return parts, 0, true
			}
			i = next
		} else {
			next := skipJSONValue(data, i)
			if next < 0 {
				return 0, 0, false
			}
			i = next
		}
		i = metrics.SkipSpace(data, i)
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			continue
		}
		if i < len(data) && data[i] == '}' {
			break
		}
		return 0, 0, false
	}
	if i >= len(data) || data[i] != '}' {
		return 0, 0, false
	}
	return parts, i + 1, true
}

// countResponsesInputNested counts the aggregate content-part and annotation
// members of one Responses input array without materializing it. Every item
// object's content and output arrays contribute parts; every part object's
// annotations array contributes annotation members. budget is the remaining
// aggregate allowance; a count above it means the bound is exceeded. Duplicate
// members are each charged because every occurrence reaches the decoder.
// Non-object items and non-array values contribute zero, matching the typed
// decoder's shape tolerance. annotationArrayOver means one annotation array
// exceeded requestAnnotationArrayMax. ok is false on malformed input so the
// document keeps its passthrough behavior.
func countResponsesInputNested(data []byte, start, partBudget, annotationBudget int) (parts, annotations, next int, annotationArrayOver bool, ok bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) {
		return 0, 0, 0, false, false
	}
	if data[i] != '[' {
		next := skipJSONValue(data, i)
		return 0, 0, next, false, next >= 0
	}
	i = metrics.SkipSpace(data, i+1)
	for i < len(data) && data[i] != ']' {
		if data[i] == '{' {
			itemParts, itemAnnotations, itemNext, itemArrayOver, itemOK := countResponsesInputItemNested(data, i, partBudget-parts, annotationBudget-annotations)
			if !itemOK {
				return 0, 0, 0, false, false
			}
			parts += itemParts
			annotations += itemAnnotations
			if itemArrayOver {
				return parts, annotations, 0, true, true
			}
			if parts > partBudget || annotations > annotationBudget {
				return parts, annotations, 0, false, true
			}
			i = metrics.SkipSpace(data, itemNext)
		} else {
			itemNext := skipJSONValue(data, i)
			if itemNext < 0 {
				return 0, 0, 0, false, false
			}
			i = metrics.SkipSpace(data, itemNext)
		}
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			if i >= len(data) || data[i] == ']' {
				return 0, 0, 0, false, false
			}
			continue
		}
		if i < len(data) && data[i] == ']' {
			break
		}
		return 0, 0, 0, false, false
	}
	if i >= len(data) || data[i] != ']' {
		return 0, 0, 0, false, false
	}
	return parts, annotations, i + 1, false, true
}

// countResponsesInputItemNested counts one item object's content and output
// arrays. A string output or content value contributes no parts.
func countResponsesInputItemNested(data []byte, start, partBudget, annotationBudget int) (parts, annotations, next int, annotationArrayOver bool, ok bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) || data[i] != '{' {
		return 0, 0, 0, false, false
	}
	i = metrics.SkipSpace(data, i+1)
	for i < len(data) && data[i] != '}' {
		end := skipJSONString(data, i)
		if end < 0 {
			return 0, 0, 0, false, false
		}
		key := data[i:end]
		i = metrics.SkipSpace(data, end)
		if i >= len(data) || data[i] != ':' {
			return 0, 0, 0, false, false
		}
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) {
			return 0, 0, 0, false, false
		}
		if requestKeyEqual(key, "content") || requestKeyEqual(key, "output") {
			partCount, annotationCount, partNext, partArrayOver, partOK := countResponsesPartArray(data, i, partBudget-parts, annotationBudget-annotations)
			if !partOK {
				return 0, 0, 0, false, false
			}
			parts += partCount
			annotations += annotationCount
			if partArrayOver {
				return parts, annotations, 0, true, true
			}
			if parts > partBudget || annotations > annotationBudget {
				return parts, annotations, 0, false, true
			}
			i = metrics.SkipSpace(data, partNext)
		} else {
			valueNext := skipJSONValue(data, i)
			if valueNext < 0 {
				return 0, 0, 0, false, false
			}
			i = metrics.SkipSpace(data, valueNext)
		}
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			continue
		}
		if i < len(data) && data[i] == '}' {
			break
		}
		return 0, 0, 0, false, false
	}
	if i >= len(data) || data[i] != '}' {
		return 0, 0, 0, false, false
	}
	return parts, annotations, i + 1, false, true
}

// countResponsesPartArray counts one content-part array's members and the
// annotation members inside each part object. A string value contributes no
// parts, matching function_call_output's string-or-array contract.
func countResponsesPartArray(data []byte, start, partBudget, annotationBudget int) (parts, annotations, next int, annotationArrayOver bool, ok bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) {
		return 0, 0, 0, false, false
	}
	if data[i] != '[' {
		next := skipJSONValue(data, i)
		return 0, 0, next, false, next >= 0
	}
	i = metrics.SkipSpace(data, i+1)
	for i < len(data) && data[i] != ']' {
		parts++
		if parts > partBudget {
			return parts, annotations, 0, false, true
		}
		if data[i] == '{' {
			annotationCount, partNext, partArrayOver, partOK := countResponsesPartAnnotations(data, i, annotationBudget-annotations)
			if !partOK {
				return 0, 0, 0, false, false
			}
			annotations += annotationCount
			if partArrayOver {
				return parts, annotations, 0, true, true
			}
			if annotations > annotationBudget {
				return parts, annotations, 0, false, true
			}
			i = metrics.SkipSpace(data, partNext)
		} else {
			partNext := skipJSONValue(data, i)
			if partNext < 0 {
				return 0, 0, 0, false, false
			}
			i = metrics.SkipSpace(data, partNext)
		}
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			if i >= len(data) || data[i] == ']' {
				return 0, 0, 0, false, false
			}
			continue
		}
		if i < len(data) && data[i] == ']' {
			break
		}
		return 0, 0, 0, false, false
	}
	if i >= len(data) || data[i] != ']' {
		return 0, 0, 0, false, false
	}
	return parts, annotations, i + 1, false, true
}

// countResponsesPartAnnotations counts one part object's annotations array.
// Duplicate annotations members are each charged for the same reason as
// duplicate content members. One array above requestAnnotationArrayMax sets
// annotationArrayOver before the aggregate check.
func countResponsesPartAnnotations(data []byte, start, budget int) (total, next int, annotationArrayOver bool, ok bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) || data[i] != '{' {
		return 0, 0, false, false
	}
	i = metrics.SkipSpace(data, i+1)
	for i < len(data) && data[i] != '}' {
		end := skipJSONString(data, i)
		if end < 0 {
			return 0, 0, false, false
		}
		key := data[i:end]
		i = metrics.SkipSpace(data, end)
		if i >= len(data) || data[i] != ':' {
			return 0, 0, false, false
		}
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) {
			return 0, 0, false, false
		}
		if requestKeyEqual(key, "annotations") {
			n, valueNext, countOK := metrics.CountJSONArrayBytes(data, i, requestAnnotationArrayMax)
			if !countOK {
				return 0, 0, false, false
			}
			if n > requestAnnotationArrayMax {
				return total + n, 0, true, true
			}
			total += n
			if total > budget {
				return total, 0, false, true
			}
			i = metrics.SkipSpace(data, valueNext)
		} else {
			valueNext := skipJSONValue(data, i)
			if valueNext < 0 {
				return 0, 0, false, false
			}
			i = metrics.SkipSpace(data, valueNext)
		}
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			continue
		}
		if i < len(data) && data[i] == '}' {
			break
		}
		return 0, 0, false, false
	}
	if i >= len(data) || data[i] != '}' {
		return 0, 0, false, false
	}
	return total, i + 1, false, true
}

// countRequestMessagesToolCalls counts the aggregate tool_calls array members
// over the message objects of the messages array at start. budget is the
// remaining aggregate allowance; a count above it means the aggregate bound is
// exceeded. Non-object elements and non-array values contribute zero, matching
// the typed decoder's shape tolerance. ok is false on malformed input so the
// document keeps its passthrough behavior.
func countRequestMessagesToolCalls(data []byte, start, budget int) (int, bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) || data[i] != '[' {
		return 0, true
	}
	i = metrics.SkipSpace(data, i+1)
	total := 0
	for i < len(data) && data[i] != ']' {
		n, next, ok := countRequestMessageToolCalls(data, i, budget-total)
		if !ok {
			return 0, false
		}
		if n > budget-total {
			return total + n, true
		}
		total += n
		i = metrics.SkipSpace(data, next)
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			if i >= len(data) || data[i] == ']' {
				return 0, false
			}
			continue
		}
		if i < len(data) && data[i] == ']' {
			break
		}
		return 0, false
	}
	if i >= len(data) || data[i] != ']' {
		return 0, false
	}
	return total, true
}

// countRequestMessageToolCalls counts one message object's tool_calls array
// members. Every occurrence accumulates: encoding/json decodes each duplicate
// tool_calls member into a fresh typed slice before the field's last-wins
// assignment, so each one allocates even though only the final slice survives.
// Once the budget is crossed, counting stops and the over-count is returned so
// the caller rejects before any translation decoder materializes the calls.
func countRequestMessageToolCalls(data []byte, start, budget int) (int, int, bool) {
	i := metrics.SkipSpace(data, start)
	if i >= len(data) {
		return 0, 0, false
	}
	if data[i] != '{' {
		next := metrics.SkipJSONValue(data, i)
		return 0, next, next >= 0
	}
	i = metrics.SkipSpace(data, i+1)
	total := 0
	for i < len(data) && data[i] != '}' {
		end := skipJSONString(data, i)
		if end < 0 {
			return 0, 0, false
		}
		key := data[i:end]
		i = metrics.SkipSpace(data, end)
		if i >= len(data) || data[i] != ':' {
			return 0, 0, false
		}
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) {
			return 0, 0, false
		}
		if requestKeyEqual(key, "tool_calls") {
			n, next, ok := metrics.CountJSONArrayBytes(data, i, budget-total)
			if !ok {
				return 0, 0, false
			}
			total += n
			if total > budget {
				return total, 0, true
			}
			i = next
		} else {
			next := metrics.SkipJSONValue(data, i)
			if next < 0 {
				return 0, 0, false
			}
			i = next
		}
		i = metrics.SkipSpace(data, i)
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			continue
		}
		if i < len(data) && data[i] == '}' {
			break
		}
		return 0, 0, false
	}
	if i >= len(data) || data[i] != '}' {
		return 0, 0, false
	}
	return total, i + 1, true
}

func asciiKeyEqual(raw []byte, name string) bool {
	if len(raw) != len(name) {
		return false
	}
	for i, c := range raw {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		w := name[i]
		if w >= 'A' && w <= 'Z' {
			w += 'a' - 'A'
		}
		if c != w {
			return false
		}
	}
	return true
}

// countJSONObjectKeys accumulates the distinct keys of one JSON map value
// into seen. encoding/json reuses the non-nil map field across duplicate
// top-level map members (metadata, logit_bias), so distinct keys, not raw
// members, are the decoded size. Duplicate members of one object therefore do
// not consume the bound. Plain ASCII keys decode from their source slice and
// allocate only the map key; escaped or non-ASCII keys decode once through the
// shared requestKey owner. Once seen exceeds max, the map is cleared and the
// rest of the object is skipped without decoding keys; the caller preserves
// the overflow state and skips later occurrences entirely.
func countJSONObjectKeys(data []byte, i, max int, seen map[string]struct{}) (int, int, bool) {
	i = metrics.SkipSpace(data, i)
	if i >= len(data) {
		return 0, 0, false
	}
	if data[i] != '{' {
		// encoding/json sets a map field to nil when the value is null, so a
		// null between duplicate objects resets the accumulated map rather
		// than leaving the previous keys counted. Other non-object values are
		// type errors handled by the main decode; leave the accumulator alone.
		if len(data)-i >= 4 && bytes.Equal(data[i:i+4], []byte("null")) {
			clear(seen)
			return 0, i + 4, true
		}
		next := skipJSONValue(data, i)
		return len(seen), next, next >= 0
	}
	i = metrics.SkipSpace(data, i+1)
	over := false
	for i < len(data) && data[i] != '}' {
		keyStart := i
		end := skipJSONString(data, i)
		if end < 0 {
			return 0, 0, false
		}
		if !over {
			key, ok := decodeRequestKey(data[keyStart:end])
			if !ok {
				return 0, 0, false
			}
			if countDistinctJSONKey(seen, key, max) > max {
				over = true
				clear(seen)
			}
		}
		i = metrics.SkipSpace(data, end)
		if i >= len(data) || data[i] != ':' {
			return 0, 0, false
		}
		i = metrics.SkipSpace(data, i+1)
		if i >= len(data) {
			return 0, 0, false
		}
		next := skipJSONValue(data, i)
		if next <= i {
			return 0, 0, false
		}
		i = metrics.SkipSpace(data, next)
		if i < len(data) && data[i] == ',' {
			i = metrics.SkipSpace(data, i+1)
			continue
		}
		if i < len(data) && data[i] == '}' {
			break
		}
		return 0, 0, false
	}
	if i >= len(data) || data[i] != '}' {
		return 0, 0, false
	}
	if over {
		return max + 1, i + 1, true
	}
	return len(seen), i + 1, true
}

// requestMetadataOverflow rejects only a fully valid JSON document. The
// lexical counter can have seen an over-cap prefix before invalid trailing
// bytes; malformed input must keep its unchanged passthrough contract, so
// validation happens only on the rejection path.
func requestMetadataOverflow(body []byte, err error) error {
	if !json.Valid(body) {
		return nil
	}
	return err
}

// requestStringExpansionMax is the raw-byte headroom allowed for decoded JSON
// string bytes beyond the request's own size. Valid UTF-8 and JSON escapes
// decode to no more bytes than their spelling, so only raw invalid UTF-8 can
// consume this allowance: encoding/json replaces each invalid byte with
// U+FFFD (three bytes). 4 KiB keeps the documented drop-not-reject small-value
// cases bounded (a tracked identity is at most 512 bytes) while rejecting the
// body-sized replacement expansion. Internal safety guardrail, not
// user-tunable.
const requestStringExpansionMax = 4 << 10

// requestStringExpansionExceeded reports whether a syntactically valid JSON
// document's decoded strings would exceed the body size by more than
// requestStringExpansionMax. Without this preflight a body-sized string of raw
// invalid UTF-8 triples across the metadata decode and translation in memory
// before any configured size bound sees the result. The scan is lexical and
// allocation-free. Malformed documents return false to keep the established
// passthrough contract; encoding/json validates their syntax before decoding
// any string, so they cannot expand either.
func requestStringExpansionExceeded(body []byte) bool {
	// Ordinary requests are valid UTF-8, so every JSON string's decoded form
	// is no larger than its raw spelling and no string scan is needed. Only a
	// body carrying raw invalid bytes can expand.
	if utf8.Valid(body) {
		return false
	}
	decoded, ok := decodedJSONStringBytes(body)
	if !ok || decoded <= len(body)+requestStringExpansionMax {
		return false
	}
	return json.Valid(body)
}

// decodedJSONStringBytes sums the decoded UTF-8 byte length of every JSON
// string in data. Escapes are bounded by their raw spelling: a two-byte escape
// decodes to one byte, and a six-byte \uXXXX escape is counted as at most
// three, so a valid document's decoded strings never exceed its raw size
// unless raw invalid UTF-8 is present. An invalid byte is counted as its
// three-byte U+FFFD replacement.
func decodedJSONStringBytes(data []byte) (int, bool) {
	total := 0
	for i := 0; i < len(data); {
		if data[i] != '"' {
			i++
			continue
		}
		end := metrics.SkipJSONString(data, i)
		if end < 0 {
			return 0, false
		}
		n, ok := decodedJSONStringLength(data[i:end])
		if !ok {
			return 0, false
		}
		total += n
		i = end
	}
	return total, true
}

// decodedJSONStringLength returns the decoded UTF-8 byte length of one raw
// JSON string span. It never allocates and accepts malformed escapes only far
// enough to keep the caller on its passthrough path.
func decodedJSONStringLength(raw []byte) (int, bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return 0, false
	}
	s := raw[1 : len(raw)-1]
	n := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c == '\\' {
			i++
			if i >= len(s) {
				return 0, false
			}
			if s[i] == 'u' {
				if i+4 >= len(s) {
					return 0, false
				}
				// Six raw bytes decode to at most three UTF-8 bytes; a
				// surrogate pair is two escapes (twelve raw bytes) and
				// decodes to four, still bounded by its spelling.
				n += 3
				i += 5
				continue
			}
			n++
			i++
			continue
		}
		if c < utf8.RuneSelf {
			n++
			i++
			continue
		}
		r, size := utf8.DecodeRune(s[i:])
		if r == utf8.RuneError && size == 1 {
			n += utf8.RuneLen(utf8.RuneError)
			i++
			continue
		}
		n += size
		i += size
	}
	return n, true
}

func parseLLMRequest(body []byte, rec *metrics.Record, preview bool) error {
	// The common minimal body is fully recognized by a lexical pre-pass. Every
	// other shape, including preview capture, keeps the typed decode below
	// unchanged.
	if !preview && parseLLMRequestFast(body, rec) {
		return nil
	}
	return parseLLMRequestSlow(body, rec, preview, true)
}

// parseGeneratedLLMRequest decodes the proxy's own translated request body.
// It passes bound=false, so parseLLMRequestSlow skips ALL of
// boundRequestMetadata: the messages/tools/stop array limits, the logit_bias,
// metadata and distinct top-level-member counts, the aggregate content-part
// and annotation counts, and the 1,024 aggregate tool-call bound. That is
// deliberate: translation legitimately changes composition (one tool result
// becomes a content block, media parts split, lifted system fragments add
// members), and re-applying the client-facing aggregate would reject a body
// the codec just built. The broad skip is safe because the source body was
// already bounded by parseLLMRequest, the codec validates the IR and its own
// wire collections, and the translated document is capped by
// max_request_bytes before this decode runs. UTF-8 expansion, the typed
// metadata decode and the token-cap recheck remain active.
func parseGeneratedLLMRequest(body []byte, rec *metrics.Record, preview bool) error {
	if !preview && parseLLMRequestFast(body, rec) {
		return nil
	}
	return parseLLMRequestSlow(body, rec, preview, false)
}

func parseLLMRequestSlow(body []byte, rec *metrics.Record, preview, bound bool) error {
	// Reject replacement expansion before the first typed decode so raw
	// invalid UTF-8 cannot multiply the request in memory.
	if requestStringExpansionExceeded(body) {
		return fmt.Errorf("request body contains invalid UTF-8 that expands beyond the request size")
	}
	if bound {
		if err := boundRequestMetadata(body); err != nil {
			return err
		}
	}
	var req struct {
		requestRouting
		requestParameters
	}
	if err := json.Unmarshal(body, &req); err != nil {
		var typeErr *json.UnmarshalTypeError
		if !errors.As(err, &typeErr) {
			return nil
		}
		req.requestRouting = requestRouting{}
		if json.Unmarshal(body, &req.requestRouting) != nil {
			req.requestRouting = requestRouting{}
		}
		req.requestParameters = requestParameters{}
		if json.Unmarshal(body, &req.requestParameters) != nil {
			// The cap is the one extracted field the trust boundary rejects on
			// (hostileTokenCap reads rec.ReqMaxTokens): a type error anywhere
			// else in the parameters block - including tool parameters carried
			// as raw JSON by the Anthropic translator - must not blind that
			// check. Recover the cap alone with applyTokenCaps' minimal decode
			// of the same body, applying the same max_completion_tokens-wins
			// precedence as the clean merge below (a cap pair that still does
			// not decode leaves the record's cap untouched).
			applyTokenCaps(body, rec)
			rec.Model, rec.Stream = retainedText(req.Model, retainedLabelMax), req.Stream
			return nil
		}
	}
	rec.Model, rec.Stream = retainedText(req.Model, retainedLabelMax), req.Stream
	// The occurrence scan owns the cap value, so the record describes the same
	// occurrence the trust boundary validated. Prefer max_completion_tokens,
	// matching the Anthropic translator's pick.
	applyTokenCaps(body, rec)
	// Integral spellings (1.0, 1e1) are accepted by every codec and relayed
	// verbatim, but a typed int decode fails them and erased the rest of the
	// record with it. Carry these three members as raw JSON and decode them
	// with the shared exact parser after the merged decode; a malformed
	// spelling records nothing here (the wire codec owns its own validation)
	// without discarding the other fields.
	n, errN := jsonnum.OptionalInt(req.N, "n")
	seed, errSeed := jsonnum.OptionalInt64(req.Seed, "seed")
	topLogprobs, errTop := jsonnum.OptionalInt(req.TopLogprobs, "top_logprobs")
	if errN == nil {
		rec.ReqN = n
	}
	if errSeed == nil {
		rec.ReqSeed = seed
	}
	if errTop == nil {
		rec.ReqTopLogprobs = topLogprobs
	}
	rec.ReqTemperature = req.Temperature
	rec.ReqTopP = req.TopP
	rec.ReqToolsCount = len(req.Tools)
	rec.ReqPresencePen = req.PresencePen
	rec.ReqFrequencyPen = req.FrequencyPen
	rec.ReqLogprobs = req.Logprobs || requestIncludeLogprobs(req.Include)
	if req.StreamOptions != nil {
		rec.ReqStreamOpts = true
	}
	rec.ReqToolChoice = req.ToolChoice.Value
	if len(req.Stop) > 0 {
		rec.ReqStop = requestStopCount(req.Stop)
	} else if len(req.StopSequences) > 0 {
		rec.ReqStop = requestStopCount(req.StopSequences)
	}
	// Additional non-content params: capture shape only, never values that
	// could carry content (logit_bias keys are token ids, metadata is counted
	// not copied).
	if req.ResponseFormat != nil {
		rec.ReqResponseFormat = req.ResponseFormat.Type
	} else if req.OutputConfig != nil && req.OutputConfig.Format != nil {
		rec.ReqResponseFormat = req.OutputConfig.Format.Type
	} else if req.Text != nil && req.Text.Format != nil {
		rec.ReqResponseFormat = req.Text.Format.Type
	}
	if req.ParallelTools != nil {
		rec.ReqParallelTools = req.ParallelTools
	} else if req.ToolChoice.DisableSet {
		parallel := !req.ToolChoice.DisableParallel
		rec.ReqParallelTools = &parallel
	}
	if req.ServiceTier != "" {
		rec.ReqServiceTier = req.ServiceTier
	} else {
		rec.ReqServiceTier = req.Speed
	}
	rec.ReqLogitBias = len(req.LogitBias)
	rec.ReqMetadataKeys = req.Metadata.count()
	if len(req.Thinking) > 0 {
		// Any non-null "thinking" object means extended thinking was requested.
		rec.ReqThinking = string(req.Thinking) != "null"
	}
	if tok := configToken(req.ReasoningEffort); tok != "" {
		rec.ReqReasoningEffort = tok
	} else {
		var effort string
		if req.OutputConfig != nil {
			effort = configToken(req.OutputConfig.Effort)
		}
		if effort == "" && len(req.Reasoning) > 0 {
			var obj struct {
				Effort string `json:"effort"`
			}
			if json.Unmarshal(req.Reasoning, &obj) == nil {
				effort = configToken(obj.Effort)
			}
		}
		rec.ReqReasoningEffort = effort
	}
	rec.ReqVerbosity = configToken(req.Verbosity)
	if rec.ReqVerbosity == "" && req.Text != nil {
		rec.ReqVerbosity = configToken(req.Text.Verbosity)
	}
	// Prompt composition: per-role character sizes, multimodal counts, and the
	// role of the last turn (what the model is actually responding to). Sizes
	// count text characters only - never the text itself, and never inline
	// base64 payloads (an image part contributes its text siblings, not its
	// data). System size comes from a role:"system"/"developer" message
	// (OpenAI) and/or a top-level "system" field (Anthropic).
	rec.CharsSystem = req.System.chars
	rec.LastTurnRole = ""
	for _, m := range req.Messages {
		n := m.Content.chars
		switch m.Role {
		case "user":
			rec.TurnsUser++
			rec.CharsUser += n
		case "assistant":
			rec.TurnsAssistant++
			rec.CharsAssistant += n
		case "tool", "function":
			rec.TurnsTool++
			rec.CharsTool += n
		case "system", "developer":
			rec.CharsSystem += n
		}
		rec.Images += m.Content.images
		rec.Attachments += m.Content.attachments
		if m.Role != "" {
			rec.LastTurnRole = m.Role
		}
	}
	if preview && len(req.Messages) > 0 {
		// Content is never retained on the default path. Opt-in previews use
		// the same message shape with a bounded string-only content decoder.
		var captured struct {
			Messages []requestMessage[requestPreview] `json:"messages"`
		}
		if json.Unmarshal(body, &captured) != nil {
			return nil
		}
		for i := len(captured.Messages) - 1; i >= 0; i-- {
			if captured.Messages[i].Role == "user" {
				rec.PromptPreview = string(captured.Messages[i].Content)
				break
			}
		}
	}
	return nil
}

// configToken accepts a short alphanumeric request-config token (effort,
// verbosity, …). Deny by default: anything with spaces, punctuation other
// than _/-, or a long payload is dropped rather than stored.
func configToken(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 32 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return ""
		}
	}
	return s
}

// requestContent measures one message content field, retaining only its text
// character count plus the number of image and non-image attachment parts.
// Content may be a plain string or an array of typed parts; the part "type"
// discriminator is matched by substring so both OpenAI ("image_url",
// "input_image", "input_audio", "file") and Anthropic ("image", "document",
// "tool_result") shapes - and future input_* variants - bucket consistently.
// Text is not retained, and base64 payloads are never decoded or retained.
type requestContent struct {
	chars, images, attachments int
}

func (c *requestContent) UnmarshalJSON(content []byte) error {
	*c = requestContent{}
	if len(content) > 0 && content[0] == '"' {
		var size requestStringSize
		if size.UnmarshalJSON(content) == nil {
			c.chars = int(size)
		}
		return nil
	}
	if i := metrics.SkipSpace(content, 0); i < len(content) && content[i] == '[' {
		if n, _, ok := metrics.CountJSONArrayBytes(content, i, metrics.ContentPartMax); !ok || n > metrics.ContentPartMax {
			return nil // best-effort composition, never a routing/type failure
		}
	}
	var parts []struct {
		Type string            `json:"type"`
		Text requestStringSize `json:"text"`
	}
	if json.Unmarshal(content, &parts) != nil {
		return nil // content is best-effort, never a routing/type failure
	}
	for _, p := range parts {
		switch {
		case strings.Contains(p.Type, "image"):
			c.images++
		case p.Type == "text" || strings.HasPrefix(p.Type, "input_text"):
			c.chars += int(p.Text)
		case p.Type != "" && p.Type != "thinking" && p.Type != "redacted_thinking" && p.Type != "tool_use":
			// Any other non-text part is an attachment: file/document/audio/
			// video or an embedded tool result. Model-authored reasoning and
			// tool-call blocks are excluded - they are not user attachments.
			c.attachments++
		}
	}
	return nil
}

// requestStringBytes only borrows bytes during UnmarshalJSON. The enclosing
// encoding/json call already validated quoting/control bytes. Invalid UTF-8
// stays on the standard decoder's Unicode replacement path.
func requestStringBytes(raw []byte) ([]byte, bool) {
	if len(raw) < 2 || raw[0] != '"' {
		return nil, false
	}
	s := raw[1 : len(raw)-1]
	return s, utf8.Valid(s)
}

type requestStringSize int

func (n *requestStringSize) UnmarshalJSON(raw []byte) error {
	if bytes.Equal(raw, []byte("null")) {
		return nil // a repeated null leaves a string field unchanged
	}
	if s, ok := requestStringBytes(raw); ok {
		size := len(s)
		for {
			i := bytes.IndexByte(s, '\\')
			if i < 0 {
				*n = requestStringSize(size)
				return nil
			}
			if s[i+1] == 'u' {
				break // the standard decoder owns Unicode/surrogate semantics
			}
			// Every other validated JSON escape is two bytes representing one
			// ASCII byte (newline, quote, slash, ...); no string allocation.
			size--
			s = s[i+2:]
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	*n = requestStringSize(len(s))
	return nil
}

type requestPreview string

func (p *requestPreview) UnmarshalJSON(raw []byte) error {
	*p = ""
	if s, ok := requestStringBytes(raw); ok && bytes.IndexByte(s, '\\') < 0 {
		// A few bytes beyond the cap let TruncatePreview own rune boundaries
		// and the ellipsis without allocating an entire large prompt string.
		s = s[:min(len(s), metrics.PreviewMaxBytes+utf8.UTFMax)]
		*p = requestPreview(metrics.TruncatePreview(string(s)))
	} else if len(raw) > 0 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			*p = requestPreview(metrics.TruncatePreview(s))
		}
	}
	return nil
}
