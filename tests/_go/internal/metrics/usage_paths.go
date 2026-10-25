package metrics

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// digJSONSplitOracle is the pre-change resolver: split the dotted path into a
// []string, then walk. It is the differential oracle for DigJSON, which now
// walks the path in place to avoid the per-call slice.
func digJSONSplitOracle(v any, path string) (any, bool) {
	cur := v
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
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

// Every built-in candidate path, in one document, plus the shapes that stress
// the segment walk rather than the data: an empty key, a key ending in a dot,
// a leading-dot path, numeric array indexes (valid, out of range, negative,
// zero-padded, non-numeric) and a non-container terminal.
const pathFixtureJSON = `{
  "usage": {
    "prompt_tokens": 11, "input_tokens": 12,
    "completion_tokens": 13, "output_tokens": 14,
    "total_tokens": 27, "cost": 0.5, "total_cost": 0.6,
    "cost_usd": 0.7, "charge": 0.8, "cost_in_usd_ticks": 900,
    "prompt_tokens_details": {"cached_tokens": 2, "cache_write_tokens": 3, "created_cache_tokens": 4},
    "input_tokens_details": {"cached_tokens": 5, "cache_write_tokens": 6},
    "cache_read_input_tokens": 7, "cache_read_tokens": 8,
    "cache_creation_input_tokens": 9, "cache_write_tokens": 10,
    "completion_tokens_details": {"reasoning_tokens": 15},
    "output_tokens_details": {"reasoning_tokens": 16, "thinking_tokens": 17},
    "reasoning_tokens": 18
  },
  "cost": 1.5, "total_cost": 1.6, "cost_in_usd_ticks": 1700,
  "a": {"b": [{"c": 1}, "two"]},
  "": {"a": 2},
  "a.": {"": "tail"},
  "n": null
}`

// TestDigJSONMatchesSplitOracle is the WS2 differential: the in-place walk and
// the slice-splitting walk must agree on every built-in candidate path, every
// provider override shape, and the path edges the provider-field-map validator
// rejects at load (so a rejected path still cannot panic or mislead).
func TestDigJSONMatchesSplitOracle(t *testing.T) {
	var root any
	if err := json.Unmarshal([]byte(pathFixtureJSON), &root); err != nil {
		t.Fatal(err)
	}

	paths := []string{
		// every built-in cost candidate, absolute and relative to a usage object
		"usage.cost.usd", "usage.cost", "usage.total_cost", "usage.cost_usd",
		"usage.charge", "cost", "total_cost", "usage.cost_in_usd_ticks", "cost_in_usd_ticks",
	}
	for _, candidates := range usageKeyPaths {
		paths = append(paths, candidates...)
		for _, c := range candidates {
			paths = append(paths, "usage."+c)
		}
	}
	paths = append(paths,
		// override corpus and edges
		"usage", "usage.prompt_tokens", "a", "a.b", "a.b.0", "a.b.0.c", "a.b.1",
		"a.b.2", "a.b.-1", "a.b.01", "a.b.x", "a.b.0.c.d", "a..b", "a.", ".a",
		".", "", "missing", "a.missing.deep", "n", "n.x",
	)

	roots := []struct {
		name string
		v    any
	}{{"document", root}, {"scalar", 42.0}, {"array", []any{map[string]any{"x": 1.0}}}, {"nil", nil}}
	for _, r := range roots {
		for _, p := range paths {
			wantV, wantOK := digJSONSplitOracle(r.v, p)
			gotV, gotOK := DigJSON(r.v, p)
			if gotOK != wantOK || !reflect.DeepEqual(gotV, wantV) {
				t.Errorf("%s: DigJSON(%q) = (%v, %v), split oracle = (%v, %v)", r.name, p, gotV, gotOK, wantV, wantOK)
			}
		}
	}
}

// TestDigJSONAllocatesNothing pins the compiled-path contract: resolving a
// built-in path costs no allocation whether it hits or misses, and neither
// does a numeric array index.
func TestDigJSONAllocatesNothing(t *testing.T) {
	var root any
	if err := json.Unmarshal([]byte(pathFixtureJSON), &root); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
	}{
		{"single segment", "cost"},
		{"deep built-in", "usage.prompt_tokens_details.cached_tokens"},
		{"array index", "a.b.0.c"},
		{"miss", "usage.nonexistent_field"},
	}
	for _, c := range cases {
		if _, ok := DigJSON(root, c.path); !ok && c.name != "miss" {
			t.Fatalf("%s: fixture path %q does not resolve", c.name, c.path)
		}
		if allocs := testing.AllocsPerRun(200, func() { _, _ = DigJSON(root, c.path) }); allocs != 0 {
			t.Errorf("%s (%q): DigJSON allocates %v per call, want 0", c.name, c.path, allocs)
		}
	}
}

// TestParseUsageAllocatesNothing pins the consumer: one standard OpenAI usage
// object must parse without allocating, with and without a provider override.
// The canonical field list is owned once (usageFields) and ranged as a value,
// so the shared owner costs nothing on the per-response path.
func TestParseUsageAllocatesNothing(t *testing.T) {
	var usage any
	if err := json.Unmarshal([]byte(`{"prompt_tokens":90,"completion_tokens":38,"total_tokens":128,"prompt_tokens_details":{"cached_tokens":60,"cache_write_tokens":4},"completion_tokens_details":{"reasoning_tokens":24}}`), &usage); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name      string
		overrides map[string]string
	}{
		{"no overrides", nil},
		{"provider overrides", map[string]string{"input_tokens": "details.custom_in", "total_tokens": "custom_total"}},
	}
	for _, c := range cases {
		if got := ParseUsage(usage, c.overrides); got.InputTokens != 90 {
			t.Fatalf("%s: ParseUsage did not resolve the built-in path: %+v", c.name, got)
		}
		if allocs := testing.AllocsPerRun(200, func() { _ = ParseUsage(usage, c.overrides) }); allocs != 0 {
			t.Errorf("%s: ParseUsage allocates %v per call, want 0", c.name, allocs)
		}
	}
}
