package metrics

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// CostKeys are the built-in JSON key paths checked (in order) for a
// per-request cost that a provider reports in its response. Providers mostly
// all report usage+cost back, just under different key names - this list is
// the auto-detection net, and a per-provider override (config) can prepend a
// provider-specific path. The first path that resolves to a number wins.
var CostKeys = []string{
	"usage.cost.usd",   // hyper (charm): cost is a nested object {usd, hypercredits}
	"usage.cost",       // OpenRouter (scalar)
	"usage.total_cost", // some aggregators
	"usage.cost_usd",   // alt naming
	"usage.charge",     // alt
	"cost",             // top-level scalar
	"total_cost",       // top-level alt
}

// costKeyScales maps JSON key paths to a USD-per-unit multiplier for providers
// that report cost in integer sub-units instead of plain USD. xAI reports
// usage.cost_in_usd_ticks where 10,000,000,000 ticks == $1 (their
// TICKS_IN_USD_CENT = 100,000,000 - docs.x.ai chat/Responses usage schema),
// so the scale is 1e-10. Checked after the plain-USD CostKeys; a value of
// null (Responses API) falls through as "not found" via the normal accept
// guard. The unprefixed spelling mirrors the bare usage object the SSE
// analyzer hands to ExtractCostFromValue.
var costKeyScales = map[string]float64{
	"usage.cost_in_usd_ticks": 1e-10,
	"cost_in_usd_ticks":       1e-10,
}

// DigJSON walks a decoded JSON value by a dot-separated path ("usage.cost")
// and returns the terminal value. Arrays are indexed by numeric path segments.
// The single dotted-path resolver for provider field maps (cost_keys,
// usage_keys, and models_keys all share this shape).
//
// The path is walked in place: each segment is a substring of path, so no
// segment slice is allocated. Splitting the path (as this used to) allocated
// one slice per candidate, about sixteen times per standard usage parse. The
// walk is the compiled form: every path is still declared once, as a string,
// in CostKeys, costKeyScales, usageKeyPaths or a provider override.
func DigJSON(v any, path string) (any, bool) {
	cur := v
	for {
		seg, rest, more := path, "", false
		if i := strings.IndexByte(path, '.'); i >= 0 {
			seg, rest, more = path[:i], path[i+1:], true
		}
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, false
			}
			cur = next
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(node) {
				return nil, false
			}
			cur = node[i]
		default:
			return nil, false
		}
		if !more {
			return cur, true
		}
		path = rest
	}
}

// asFloat coerces a JSON scalar to float64. Accepts JSON numbers and numeric
// strings (several providers serialize cost as a string, e.g. "0.0012").
// Non-finite values (Inf/NaN, which strconv.ParseFloat accepts from strings
// like "Infinity") are rejected: they are never valid cost/usage data, and a
// non-finite float would break JSON serialization of the whole dashboard.
func asFloat(v any) (float64, bool) {
	finite := func(f float64) (float64, bool) {
		if math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, false
		}
		return f, true
	}
	switch n := v.(type) {
	case float64:
		return finite(n)
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return finite(f)
		}
	}
	return 0, false
}

// ExtractCost reads the per-request cost from a raw response body (any JSON
// document). It checks provider-specific key paths first (override), then the
// built-in candidates. Returns (cost, true) when a usable non-negative number
// is found.
func ExtractCost(body []byte, overrideKeys []string) (float64, bool) {
	// Most completions report no cost at all. A generic any-decode would
	// materialize the whole document just to prove the key is absent, so
	// require a plausible cost key before decoding. Escaped keys (\u....)
	// fall back to the decode; the gate is an optimization, not a parser.
	if !hasCostKey(body, overrideKeys) {
		return 0, false
	}
	var root any
	// Best-effort: an unparseable body just means "no cost reported" - the
	// proxy never invents a number and never fails the request over metrics.
	if json.Unmarshal(body, &root) != nil {
		return 0, false
	}
	return ExtractCostFromValue(root, overrideKeys)
}

// hasCostKey reports whether body contains any candidate leaf key as an
// object key. It is deliberately conservative: a false positive only costs
// the decode that used to happen unconditionally; a false negative would lose
// a cost, so any escape sequence in the body falls back to decoding.
func hasCostKey(body []byte, overrideKeys []string) bool {
	for _, path := range overrideKeys {
		if hasJSONLeafKey(body, path) {
			return true
		}
	}
	for _, path := range CostKeys {
		if hasJSONLeafKey(body, path) {
			return true
		}
	}
	for path := range costKeyScales {
		if hasJSONLeafKey(body, path) {
			return true
		}
	}
	return bytes.Contains(body, []byte(`\u`))
}

func hasJSONLeafKey(body []byte, path string) bool {
	leaf := path
	if i := strings.LastIndexByte(leaf, '.'); i >= 0 {
		leaf = leaf[i+1:]
	}
	if leaf == "" {
		return false
	}
	needle := []byte(`"` + leaf + `"`)
	for start := 0; start < len(body); {
		at := bytes.Index(body[start:], needle)
		if at < 0 {
			return false
		}
		i := start + at + len(needle)
		for i < len(body) && (body[i] == ' ' || body[i] == '\t' || body[i] == '\r' || body[i] == '\n') {
			i++
		}
		if i < len(body) && body[i] == ':' {
			return true
		}
		start += at + len(needle)
	}
	return false
}

// CanonicalUsageFields lists the Usage field names allowed as usage_keys keys
// (the provider field-map override in config). It is the single owner served
// to the Settings UI for the canonical-field dropdown; ParseUsage consumes
// exactly these fields - TestCanonicalUsageFieldsMatchesParseUsage enforces
// the lockstep.
var CanonicalUsageFields = []string{
	"input_tokens",
	"output_tokens",
	"total_tokens",
	"cache_read_tokens",
	"cache_write_tokens",
	"reasoning_tokens",
}

// usageKeyPaths maps each canonical Usage field to the JSON key paths (relative
// to the "usage" object) that providers use, checked in order. OpenAI uses
// prompt_tokens/completion_tokens; Anthropic uses input_tokens/output_tokens.
// A per-provider override (config usage_keys) prepends a custom path.
var usageKeyPaths = map[string][]string{
	"input_tokens":  {"prompt_tokens", "input_tokens"},
	"output_tokens": {"completion_tokens", "output_tokens"},
	"total_tokens":  {"total_tokens"},
	"cache_read_tokens": {"prompt_tokens_details.cached_tokens", "input_tokens_details.cached_tokens",
		"cache_read_input_tokens", "cache_read_tokens"},
	"cache_write_tokens": {"prompt_tokens_details.cache_write_tokens", "prompt_tokens_details.created_cache_tokens",
		"input_tokens_details.cache_write_tokens", "cache_creation_input_tokens", "cache_write_tokens"},
	"reasoning_tokens": {"completion_tokens_details.reasoning_tokens", "output_tokens_details.reasoning_tokens",
		"output_tokens_details.thinking_tokens", "reasoning_tokens"},
}

// usageField names one canonical count and its destination.
type usageField struct {
	name string
	dst  *int64
}

// usageFields lists every canonical count in one place so the override-only
// parser and ParseUsage stay in lockstep. It is a value array, not a slice:
// both callers range over it immediately, so a fixed-size value keeps that
// shared owner allocation-free on the per-response path.
func usageFields(u *Usage) [6]usageField {
	return [6]usageField{
		{"input_tokens", &u.InputTokens},
		{"output_tokens", &u.OutputTokens},
		{"total_tokens", &u.TotalTokens},
		{"cache_read_tokens", &u.CacheReadTokens},
		{"cache_write_tokens", &u.CacheWrite},
		{"reasoning_tokens", &u.ReasoningTokens},
	}
}

// usageCount bounds a token count to a sane, in-range non-negative value.
// Token counts are never negative and never near int64 range; a rejected value
// is treated as unreported.
func usageCount(v any) (int64, bool) {
	f, ok := asFloat(v)
	if !ok {
		return 0, false
	}
	// float64(math.MaxInt64) rounds UP to 2^63, which cannot convert
	// to a nonnegative int64. Counts must also be integral.
	if f < 0 || f >= float64(math.MaxInt64) || math.Trunc(f) != f {
		return 0, false
	}
	return int64(f), true
}

// tryUsagePath stores the count at one candidate path when it resolves to an
// accepted value; false falls through to the next candidate.
func tryUsagePath(usageObj any, path string, dst *int64) bool {
	v, ok := DigJSON(usageObj, path)
	if !ok {
		return false
	}
	n, ok := usageCount(v)
	if !ok {
		return false
	}
	*dst = n
	return true
}

// ParseUsage extracts token usage from a decoded "usage" object (already
// unmarshalled to any). It auto-detects OpenAI and Anthropic key names; a
// per-provider override map (canonical field -> custom key path) is checked
// first for the fields it names.
func ParseUsage(usageObj any, overrides map[string]string) Usage {
	var u Usage
	set := func(field string, dst *int64) {
		// provider override path first
		if p := overrides[field]; p != "" {
			if tryUsagePath(usageObj, p, dst) {
				return
			}
		}
		for _, p := range usageKeyPaths[field] {
			if tryUsagePath(usageObj, p, dst) {
				return
			}
		}
	}
	for _, f := range usageFields(&u) {
		set(f.name, f.dst)
	}
	return u
}

// ParseUsageOverrides extracts only the fields a provider override names,
// relative to usageObj. The standard usage document already owns every
// built-in path, so folding built-ins again could clobber a folded total
// (Anthropic's cache-inclusive input). Empty when no override is configured
// or none resolves.
func ParseUsageOverrides(usageObj any, overrides map[string]string) Usage {
	var u Usage
	if len(overrides) == 0 {
		return u
	}
	for _, f := range usageFields(&u) {
		if p := overrides[f.name]; p != "" {
			tryUsagePath(usageObj, p, f.dst)
		}
	}
	return u
}

// ParseUsageBody extracts the top-level usage object from a provider response
// body and parses it with the provider's overrides. The source document is
// authoritative for accounting even when a translated client wire no longer
// carries the provider's custom paths.
func ParseUsageBody(body []byte, usageKeys map[string]string) (Usage, bool) {
	var root struct {
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(body, &root) != nil || !rawValuePresent(root.Usage) {
		return Usage{}, false
	}
	var usage map[string]any
	if json.Unmarshal(root.Usage, &usage) != nil || usage == nil {
		return Usage{}, false
	}
	return ParseUsage(usage, usageKeys), true
}

// ExtractCostFromValue is ExtractCost over an already-decoded JSON value. It
// also tolerates being handed the bare "usage" object (as the SSE analyzer
// captures it): a leading "usage." segment that doesn't resolve is retried
// relative to the current root.
func ExtractCostFromValue(root any, overrideKeys []string) (float64, bool) {
	try := func(path string) (float64, bool) {
		if path == "" {
			return 0, false
		}
		if v, ok := DigJSON(root, path); ok {
			if f, ok := asFloat(v); ok && f >= 0 {
				return f, true
			}
			return 0, false
		}
		// Retry relative to a bare usage object: drop a leading "usage.".
		if strings.HasPrefix(path, "usage.") {
			if v, ok := DigJSON(root, strings.TrimPrefix(path, "usage.")); ok {
				if f, ok := asFloat(v); ok && f >= 0 {
					return f, true
				}
			}
		}
		return 0, false
	}
	for _, path := range overrideKeys {
		if f, ok := try(path); ok {
			return f, true
		}
	}
	for _, path := range CostKeys {
		if f, ok := try(path); ok {
			return f, true
		}
	}
	for path, scale := range costKeyScales {
		if f, ok := try(path); ok {
			return f * scale, true
		}
	}
	return 0, false
}
