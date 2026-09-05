package metrics

import (
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
func DigJSON(v any, path string) (any, bool) {
	cur := v
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			// allow indexing into arrays with a numeric segment
			if arr, isArr := cur.([]any); isArr {
				if i, err := strconv.Atoi(seg); err == nil && i >= 0 && i < len(arr) {
					cur = arr[i]
					continue
				}
			}
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
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
	var root any
	// Best-effort: an unparseable body just means "no cost reported" - the
	// proxy never invents a number and never fails the request over metrics.
	if json.Unmarshal(body, &root) != nil {
		return 0, false
	}
	return ExtractCostFromValue(root, overrideKeys)
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
	"input_tokens":       {"prompt_tokens", "input_tokens"},
	"output_tokens":      {"completion_tokens", "output_tokens"},
	"total_tokens":       {"total_tokens"},
	"cache_read_tokens":  {"prompt_tokens_details.cached_tokens", "cache_read_input_tokens", "cache_read_tokens"},
	"cache_write_tokens": {"prompt_tokens_details.cache_write_tokens", "cache_creation_input_tokens", "cache_write_tokens"},
	"reasoning_tokens":   {"completion_tokens_details.reasoning_tokens", "reasoning_tokens"},
}

// ParseUsage extracts token usage from a decoded "usage" object (already
// unmarshalled to any). It auto-detects OpenAI and Anthropic key names; a
// per-provider override map (canonical field -> custom key path) is checked
// first for the fields it names.
func ParseUsage(usageObj any, overrides map[string]string) Usage {
	var u Usage
	// accept bounds a token count to a sane, in-range non-negative value; a
	// rejected value falls through to the next candidate key path rather than
	// being stored. Token counts are never negative and never near int64 range.
	accept := func(f float64) (int64, bool) {
		// float64(math.MaxInt64) rounds UP to 2^63, which cannot convert
		// to a nonnegative int64. Counts must also be integral.
		if f < 0 || f >= float64(math.MaxInt64) || math.Trunc(f) != f {
			return 0, false
		}
		return int64(f), true
	}
	set := func(field string, dst *int64) {
		// provider override path first
		if p, ok := overrides[field]; ok && p != "" {
			if v, ok := DigJSON(usageObj, p); ok {
				if f, ok := asFloat(v); ok {
					if n, ok := accept(f); ok {
						*dst = n
						return
					}
				}
			}
		}
		for _, p := range usageKeyPaths[field] {
			if v, ok := DigJSON(usageObj, p); ok {
				if f, ok := asFloat(v); ok {
					if n, ok := accept(f); ok {
						*dst = n
						return
					}
				}
			}
		}
	}
	set("input_tokens", &u.InputTokens)
	set("output_tokens", &u.OutputTokens)
	set("total_tokens", &u.TotalTokens)
	set("cache_read_tokens", &u.CacheReadTokens)
	set("cache_write_tokens", &u.CacheWrite)
	set("reasoning_tokens", &u.ReasoningTokens)
	return u
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
