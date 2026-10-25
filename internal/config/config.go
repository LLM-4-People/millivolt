package config

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// provPathRE is the dotted JSON path shape every provider field-map value
// must match (object segments, optionally indexed by numeric array
// segments). Shared by cost_keys, usage_keys, and models_keys validation so
// all three reject garbage at the load boundary with one rule.
var provPathRE = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

// canonicalUsageFieldSet is the lookup-set form of
// metrics.CanonicalUsageFields - the single owner of the canonical usage
// field names (served to the Settings UI dropdown, consumed by ParseUsage).
// usage_keys keys are checked against it so the load boundary accepts
// exactly what the UI offers and what ParseUsage reads; a typo'd canonical
// name would otherwise be a silent runtime no-op.
var canonicalUsageFieldSet = func() map[string]bool {
	set := make(map[string]bool, len(metrics.CanonicalUsageFields))
	for _, f := range metrics.CanonicalUsageFields {
		set[f] = true
	}
	return set
}()

// checkProvPath rejects a provider field-map entry whose value is not a
// dotted JSON path (provPathRE). label and slot locate the entry in the
// error ("providers.<label>.<slot>"); name is the map key the path belongs
// to, empty for the slice-shaped cost_keys.
func checkProvPath(label, slot, name, path string) error {
	if !provPathRE.MatchString(path) {
		if name == "" {
			return fmt.Errorf("providers.%s.%s: %q is not a dotted JSON path", label, slot, path)
		}
		return fmt.Errorf("providers.%s.%s: %s: %q is not a dotted JSON path", label, slot, name, path)
	}
	return nil
}

// sortedKeys returns m's keys in sorted order, for error-reporting loops over
// Go maps: map iteration order is randomized, so an unsorted loop reports a
// nondeterministic first error when several entries are invalid. Every loop in
// Validate that can return an error iterates through this.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// canonicalProviderLabel is the one provider-label policy shared by the
// providers map and provider_aliases: the runtime derives a label from the
// upstream host (lowercase, no surrounding whitespace), so a configured label
// only ever matches in that canonical form. Anything else is rejected by
// validateProviderLabel rather than kept as a silent no-op.
func canonicalProviderLabel(label string) string {
	return strings.ToLower(strings.TrimSpace(label))
}

// validateProviderLabel checks a configured provider label against the one
// canonical form. A label containing whitespace cannot be the host-derived
// value the request path records, so it is denied just like a mixed-case
// spelling. normalizeProviderAliases calls it after canonicalization;
// Validate calls it directly because provider labels are rejected, not
// rewritten.
func validateProviderLabel(label string) error {
	if label == "" {
		return errors.New("must be non-empty")
	}
	if strings.ContainsAny(label, " \t\r\n") {
		return fmt.Errorf("%q must not contain whitespace", label)
	}
	if label != strings.ToLower(label) {
		return fmt.Errorf("%q must be lowercase", label)
	}
	return nil
}

// normalizeProviderAliases is the one normalizer for provider_aliases. Both
// sides are canonicalized before the self/chain checks, and a collision created
// by that canonicalization is rejected instead of letting map iteration choose
// a winner. It is idempotent, so Apply and the shared Validate boundary may
// both call it without changing a valid map.
func normalizeProviderAliases(aliases map[string]string) (map[string]string, error) {
	if aliases == nil {
		return nil, nil
	}
	out := make(map[string]string, len(aliases))
	seen := make(map[string]string, len(aliases))
	for _, rawFrom := range sortedKeys(aliases) {
		from := canonicalProviderLabel(rawFrom)
		to := canonicalProviderLabel(aliases[rawFrom])
		if err := validateProviderLabel(from); err != nil {
			return nil, fmt.Errorf("alias label %q: %w", from, err)
		}
		if err := validateProviderLabel(to); err != nil {
			return nil, fmt.Errorf("%s: target label: %w", from, err)
		}
		if first, duplicate := seen[from]; duplicate {
			return nil, fmt.Errorf("%q and %q: labels collide after normalization to %q", first, rawFrom, from)
		}
		seen[from] = rawFrom
		out[from] = to
	}
	return out, nil
}

// forbiddenOverrideHeaders is the name set of the request-overrides header
// trust boundary, keyed lowercase (HTTP header names are case-insensitive,
// so the check is too). Credential headers are denied because the proxy
// injects the upstream credential itself; protocol and framing headers are
// denied because the proxy and the HTTP transport own the wire. The value
// is the denial reason used by the error wording. The x-proxy- control
// prefix is denied as a whole and lives in the prefix check inside
// ForbiddenOverrideHeader, not in this set. The proxy's runtime strip maps
// serve a different consumer and stay untouched.
var forbiddenOverrideHeaders = map[string]string{
	"authorization":       "credential",
	"cookie":              "credential",
	"proxy-authorization": "credential",
	"host":                "protocol",
	"content-type":        "protocol",
	"content-length":      "protocol",
	"transfer-encoding":   "protocol",
	"te":                  "protocol",
	"connection":          "protocol",
	"keep-alive":          "protocol",
	"proxy-authenticate":  "protocol",
	"proxy-connection":    "protocol",
	"trailer":             "protocol",
	"upgrade":             "protocol",
}

// ForbiddenOverrideHeader owns the request-overrides header grammar: it
// reports whether a set or removed header name is credential-owned,
// protocol-owned, or carries the proxy's x-proxy- control prefix, along
// with the reason the rejection message explains. Both lists of a rule
// share the grammar: deleting a credential or framing header is as
// breaking as rewriting it.
func ForbiddenOverrideHeader(name string) (reason string, forbidden bool) {
	lower := strings.ToLower(name)
	if reason = forbiddenOverrideHeaders[lower]; reason != "" {
		return reason, true
	}
	if strings.HasPrefix(lower, "x-proxy-") {
		return "control", true
	}
	return "", false
}

// validateRequestOverrides is the load-boundary gate for the whole
// request-overrides list (called by Validate, i.e. by YAML load AND the
// Settings POST). It normalizes safe input before judging it - scope
// fields, header names and header values are trimmed, header-name case is
// canonicalized, empty collections are dropped - and then enforces the
// rules: a scope and an action per rule, the header token grammar and the
// forbidden-owner set, one canonical name never both set and removed, the
// body band, and the length cap. A rule list that passes is canonical:
// every stored name is in HTTP canonical case, and every collection that
// survives is non-empty.
func validateRequestOverrides(c *Config) error {
	if len(c.RequestOverrides) > RequestOverridesMax {
		return fmt.Errorf("request_overrides: %d rules exceeds the cap of %d", len(c.RequestOverrides), RequestOverridesMax)
	}
	scopes := make(map[[3]string]int, len(c.RequestOverrides))
	for i := range c.RequestOverrides {
		r := &c.RequestOverrides[i]
		r.Client = strings.TrimSpace(r.Client)
		r.Provider = strings.TrimSpace(r.Provider)
		r.Model = strings.TrimSpace(r.Model)
		if r.Client == "" && r.Provider == "" && r.Model == "" {
			return fmt.Errorf("request_overrides[%d]: no scope set - give the rule a client, provider or model, or remove the rule", i)
		}
		if len(r.Headers) == 0 && len(r.RemoveHeaders) == 0 && overrideBodyEmpty(r.Body) {
			return fmt.Errorf("request_overrides[%d]: no action set - give the rule a headers entry, a remove_headers entry or a body value, or remove the rule", i)
		}
		if err := normalizeOverrideHeaders(i, r); err != nil {
			return err
		}
		if r.Body != nil {
			for _, f := range [...]struct {
				name  string
				value *int
			}{
				{"max_tokens", r.Body.MaxTokens},
				{"max_completion_tokens", r.Body.MaxCompletionTokens},
			} {
				if f.value != nil && (*f.value < 1 || *f.value > 1_000_000) {
					return errRange(fmt.Sprintf("request_overrides[%d].body.%s", i, f.name), "1", "1000000", strconv.Itoa(*f.value))
				}
			}
			if overrideBodyEmpty(r.Body) {
				r.Body = nil
			}
		}
		key := [3]string{r.Client, r.Provider, r.Model}
		if first, dup := scopes[key]; dup {
			return fmt.Errorf("request_overrides[%d]: duplicate scope with request_overrides[%d] (client %q, provider %q, model %q); merge the rules or change one scope", i, first, r.Client, r.Provider, r.Model)
		}
		scopes[key] = i
	}
	return nil
}

// normalizeOverrideHeaders validates and canonicalizes one rule's two
// header lists. Names are trimmed, checked against the RFC 7230 token
// grammar and the forbidden-owner set, and rewritten in HTTP canonical
// case; values are trimmed and must be non-empty single-line header
// values. HTTP names are case-insensitive, so "user-agent" and
// "User-Agent" are one header: only a true duplicate (the same canonical
// name twice in one list, or in both lists of one rule) is rejected, and
// an empty collection is normalized away. rule is the rule's index for
// the error messages.
func normalizeOverrideHeaders(rule int, r *RequestOverride) error {
	headers := make(map[string]string, len(r.Headers))
	for _, name := range sortedKeys(r.Headers) {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return fmt.Errorf("request_overrides[%d].headers: header name must not be empty or only whitespace", rule)
		}
		if !ValidHeaderName(trimmed) {
			return fmt.Errorf("request_overrides[%d].headers: %q is not a valid header name (RFC 7230 token)", rule, trimmed)
		}
		if reason, forbidden := ForbiddenOverrideHeader(trimmed); forbidden {
			return forbiddenOverrideHeaderError(rule, "headers", trimmed, reason)
		}
		canon := http.CanonicalHeaderKey(trimmed)
		if _, dup := headers[canon]; dup {
			return fmt.Errorf("request_overrides[%d].headers: duplicate HTTP header name %q", rule, trimmed)
		}
		value := strings.TrimSpace(r.Headers[name])
		if value == "" || !ValidHeaderValue(value) {
			return fmt.Errorf("request_overrides[%d].headers: %s: value must be a non-empty single-line header value", rule, canon)
		}
		headers[canon] = value
	}
	remove := make([]string, 0, len(r.RemoveHeaders))
	seen := make(map[string]bool, len(r.Headers)+len(r.RemoveHeaders))
	for _, name := range r.RemoveHeaders {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			return fmt.Errorf("request_overrides[%d].remove_headers: header name must not be empty or only whitespace", rule)
		}
		if !ValidHeaderName(trimmed) {
			return fmt.Errorf("request_overrides[%d].remove_headers: %q is not a valid header name (RFC 7230 token)", rule, trimmed)
		}
		if reason, forbidden := ForbiddenOverrideHeader(trimmed); forbidden {
			return forbiddenOverrideHeaderError(rule, "remove_headers", trimmed, reason)
		}
		canon := http.CanonicalHeaderKey(trimmed)
		if seen[canon] {
			return fmt.Errorf("request_overrides[%d].remove_headers: duplicate HTTP header name %q", rule, trimmed)
		}
		seen[canon] = true
		remove = append(remove, canon)
	}
	for canon := range headers {
		if seen[canon] {
			return fmt.Errorf("request_overrides[%d]: %q is both set in headers and removed in remove_headers; keep exactly one action per header", rule, canon)
		}
	}
	if len(headers) == 0 {
		r.Headers = nil
	} else {
		r.Headers = headers
	}
	if len(remove) == 0 {
		r.RemoveHeaders = nil
	} else {
		r.RemoveHeaders = remove
	}
	return nil
}

// forbiddenOverrideHeaderError is the operator wording for a denied header
// name: each reason explains who owns the header, so the fix is obvious
// from the message alone.
func forbiddenOverrideHeaderError(rule int, field, name, reason string) error {
	switch reason {
	case "credential":
		return fmt.Errorf("request_overrides[%d].%s: %q is credential-owned and cannot be overridden; the proxy injects the provider key itself", rule, field, name)
	case "protocol":
		return fmt.Errorf("request_overrides[%d].%s: %q is protocol-owned and cannot be overridden; the proxy and the HTTP transport manage it", rule, field, name)
	default: // "control": the x-proxy- prefix
		return fmt.Errorf("request_overrides[%d].%s: %q carries the x-proxy- control prefix and cannot be overridden; x-proxy- headers are the proxy's own request channel", rule, field, name)
	}
}

// validateSubConversations is the load-boundary gate for the whole
// sub-conversations list (called by Validate, i.e. by YAML load AND the
// Settings POST). It normalizes safe input before judging it - the client
// and every param name are trimmed - and then enforces the rules: one
// entry per classified client (duplicates rejected, exact spellings only,
// no case folding), 1..4 params per entry, the one JSON-key-segment param
// grammar, and the entry cap. strip is a plain bool, never normalized. A
// list that passes is canonical: every stored name is trimmed.
func validateSubConversations(c *Config) error {
	if len(c.SubConversations) > SubConversationsMax {
		return fmt.Errorf("sub_conversations: %d entries exceeds the cap of %d", len(c.SubConversations), SubConversationsMax)
	}
	clients := make(map[string]int, len(c.SubConversations))
	for i := range c.SubConversations {
		s := &c.SubConversations[i]
		s.Client = strings.TrimSpace(s.Client)
		if s.Client == "" {
			return fmt.Errorf("sub_conversations[%d]: client must not be empty or only whitespace - name the classified client this entry tracks, or remove the entry", i)
		}
		if len(s.Params) == 0 {
			return fmt.Errorf("sub_conversations[%d]: no param set - give the entry a request body field to track, or remove the entry", i)
		}
		if len(s.Params) > SubConversationParamsMax {
			return fmt.Errorf("sub_conversations[%d].params: %d params exceeds the cap of %d", i, len(s.Params), SubConversationParamsMax)
		}
		seen := make(map[string]bool, len(s.Params))
		for j := range s.Params {
			s.Params[j] = strings.TrimSpace(s.Params[j])
			if err := checkSubConversationParam(i, j, s.Params[j]); err != nil {
				return err
			}
			if seen[s.Params[j]] {
				return fmt.Errorf("sub_conversations[%d].params: duplicate %q", i, s.Params[j])
			}
			seen[s.Params[j]] = true
		}
		if first, dup := clients[s.Client]; dup {
			return fmt.Errorf("sub_conversations[%d]: duplicate client %q with sub_conversations[%d]; merge the entries or change one client", i, s.Client, first)
		}
		clients[s.Client] = i
	}
	return nil
}

// checkSubConversationParam is the single owner of the tracked-param name
// grammar: one JSON object key segment - non-empty, at most
// subConversationParamMaxBytes bytes, printable ASCII only, with no quote
// and no backslash. Deliberately NOT provPathRE (that owns dotted response
// paths) and deliberately without a regex or case folding: the names are
// exact wire field names, and legal-but-unusual spellings (a literal dot,
// an interior space) stay legal because JSON allows them. entry and param
// locate the value in the error.
func checkSubConversationParam(entry, param int, name string) error {
	key := fmt.Sprintf("sub_conversations[%d].params[%d]", entry, param)
	if name == "" {
		return fmt.Errorf("%s: must not be empty or only whitespace", key)
	}
	for i := 0; i < len(name); i++ {
		b := name[i]
		if b < 0x20 || b > 0x7e || b == '"' || b == '\\' {
			return fmt.Errorf("%s: %q is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)", key, name)
		}
	}
	if len(name) > subConversationParamMaxBytes {
		return errRange(key, "1", strconv.Itoa(subConversationParamMaxBytes)+" bytes", strconv.Itoa(len(name)))
	}
	return nil
}

// checkYAMLType is the YAML type gate. Ranges and cross-field rules live in
// Validate, which keepYAMLKeys runs on the merged overlay. Schema Kind is
// the type owner; yaml.v3 must not coerce floats, nulls, or 1.1 bool words.
func checkYAMLType(field Field, v any) error {
	switch field.Kind {
	case KindInt:
		switch v.(type) {
		case int, int64, uint64:
		default:
			return fmt.Errorf("%s: must be an integer", field.Key)
		}
	case KindBytes:
		switch v.(type) {
		case int, int64, uint64, string, ByteSize:
		default:
			return fmt.Errorf("%s: must be a byte size or integer", field.Key)
		}
		if _, err := parseByteSize(v); err != nil {
			return fmt.Errorf("%s: %w", field.Key, err)
		}
	case KindDuration:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s: must be a duration string", field.Key)
		}
	case KindBool:
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: must be a boolean", field.Key)
		}
	case KindString:
		if _, ok := v.(string); !ok {
			return fmt.Errorf("%s: must be a string", field.Key)
		}
	case KindStrings:
		switch items := v.(type) {
		case []string:
		case []any:
			for _, item := range items {
				if _, ok := item.(string); !ok {
					return fmt.Errorf("%s: every item must be a string", field.Key)
				}
			}
		default:
			return fmt.Errorf("%s: must be a list of strings", field.Key)
		}
	case KindAliases:
		switch m := v.(type) {
		case map[string]string:
		case map[string]any:
			for _, item := range m {
				if _, ok := item.(string); !ok {
					return fmt.Errorf("%s: every value must be a string", field.Key)
				}
			}
		default:
			return fmt.Errorf("%s: must be a map of strings", field.Key)
		}
	case KindProviders:
		m, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: must be a map", field.Key)
		}
		for _, item := range m {
			if _, ok := item.(map[string]any); !ok {
				return fmt.Errorf("%s: each provider must be a map", field.Key)
			}
		}
	case KindModelRules:
		switch items := v.(type) {
		case []ModelRule:
		case []any:
			for _, item := range items {
				if _, ok := item.(map[string]any); !ok {
					return fmt.Errorf("%s: every item must be a map", field.Key)
				}
			}
		default:
			return fmt.Errorf("%s: must be a list", field.Key)
		}
	case KindRequestOverrides:
		switch items := v.(type) {
		case []RequestOverride:
		case []any:
			for _, item := range items {
				if _, ok := item.(map[string]any); !ok {
					return fmt.Errorf("%s: every item must be a map", field.Key)
				}
			}
		default:
			return fmt.Errorf("%s: must be a list", field.Key)
		}
	case KindSubConversations:
		switch items := v.(type) {
		case []SubConversation:
		case []any:
			for _, item := range items {
				if _, ok := item.(map[string]any); !ok {
					return fmt.Errorf("%s: every item must be a map", field.Key)
				}
			}
		default:
			return fmt.Errorf("%s: must be a list", field.Key)
		}
	default:
		return fmt.Errorf("%s: unsupported kind %q", field.Key, field.Kind)
	}
	return nil
}
