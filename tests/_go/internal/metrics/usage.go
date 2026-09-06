package metrics

import (
	"encoding/json"
	"math"
	"testing"
)

func TestExtractCost(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		override []string
		want     float64
		wantOK   bool
	}{
		// hyper (charm): cost is a NESTED object - the scalar is at usage.cost.usd.
		{"hyper nested cost.usd", `{"usage":{"prompt_tokens":90,"completion_tokens":38,"cost":{"usd":0.001038,"hypercredits":0.02076},"remaining":{"hypercredits":6909.5}}}`, nil, 0.001038, true},
		// OpenRouter: usage.cost as a number.
		{"openrouter usage.cost", `{"usage":{"prompt_tokens":10,"completion_tokens":5,"cost":0.00123}}`, nil, 0.00123, true},
		// Cost serialized as a string (several providers do this).
		{"string cost", `{"usage":{"cost":"0.0042"}}`, nil, 0.0042, true},
		// total_cost variant.
		{"usage.total_cost", `{"usage":{"total_cost":0.5}}`, nil, 0.5, true},
		// Top-level cost.
		{"top-level cost", `{"cost":0.007,"usage":{"prompt_tokens":1}}`, nil, 0.007, true},
		// No cost field anywhere → not found (never invent a number).
		{"no cost field", `{"usage":{"prompt_tokens":10,"completion_tokens":5}}`, nil, 0, false},
		// Invalid JSON → not found, no panic.
		{"invalid json", `{"usage":`, nil, 0, false},
		// Per-provider override key path wins / unlocks a nonstandard key.
		{"override custom key", `{"usage":{"custom_charge":0.009}}`, []string{"usage.custom_charge"}, 0.009, true},
		// Override that doesn't resolve falls through to built-in candidates.
		{"override misses, builtin hits", `{"usage":{"cost":0.003}}`, []string{"usage.nonexistent"}, 0.003, true},
		// Negative cost is rejected (not a valid charge).
		{"negative rejected", `{"usage":{"cost":-1}}`, nil, 0, false},
		// xAI reports cost as integer USD ticks (1e10 ticks == $1) - scaled,
		// never relayed raw. Real shape observed live on grok-4.6.
		{"xai cost_in_usd_ticks", `{"usage":{"prompt_tokens":637,"completion_tokens":10,"cost_in_usd_ticks":1421200}}`, nil, 0.00014212, true},
		// Plain-USD keys win over the scaled-tick fallback when both appear.
		{"ticks lose to plain usd", `{"usage":{"cost_in_usd_ticks":1421200,"cost":0.5}}`, nil, 0.5, true},
		// Non-finite cost is rejected: strconv.ParseFloat accepts "Inf"/
		// "Infinity"/"NaN" from strings, but a non-finite cost would break
		// JSON serialization of the whole dashboard snapshot.
		{"+Inf string rejected", `{"usage":{"cost":"+Inf"}}`, nil, 0, false},
		{"Infinity string rejected", `{"usage":{"cost":"Infinity"}}`, nil, 0, false},
		{"NaN string rejected", `{"usage":{"cost":"NaN"}}`, nil, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ExtractCost([]byte(c.body), c.override)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && got != c.want {
				t.Errorf("cost = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParseUsage(t *testing.T) {
	cases := []struct {
		name      string
		usage     string
		overrides map[string]string
		want      Usage
	}{
		{"openai + hyper (prompt/completion + cache + reasoning)",
			`{"prompt_tokens":90,"completion_tokens":38,"total_tokens":128,"prompt_tokens_details":{"cached_tokens":60},"completion_tokens_details":{"reasoning_tokens":24}}`,
			nil,
			Usage{InputTokens: 90, OutputTokens: 38, TotalTokens: 128, CacheReadTokens: 60, ReasoningTokens: 24}},
		{"anthropic (input/output + cache_read_input)",
			`{"input_tokens":50,"output_tokens":20,"cache_read_input_tokens":10,"cache_creation_input_tokens":5}`,
			nil,
			Usage{InputTokens: 50, OutputTokens: 20, CacheReadTokens: 10, CacheWrite: 5}},
		{"per-provider override key path",
			`{"custom_in":7,"prompt_tokens":99}`,
			map[string]string{"input_tokens": "custom_in"},
			Usage{InputTokens: 7}},
		{"empty usage object",
			`{}`, nil, Usage{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var obj any
			if err := json.Unmarshal([]byte(c.usage), &obj); err != nil {
				t.Fatal(err)
			}
			got := ParseUsage(obj, c.overrides)
			if got != c.want {
				t.Errorf("ParseUsage = %+v, want %+v", got, c.want)
			}
		})
	}
}

// ExtractCostFromValue must also work on a bare "usage" object (the SSE
// analyzer captures just the usage fragment, not the whole body).
func TestExtractCostFromBareUsage(t *testing.T) {
	body := []byte(`{"prompt_tokens":10,"completion_tokens":5,"cost":0.0021}`)
	// The bare usage object has no "usage." prefix; ExtractCostFromValue must
	// still find "usage.cost" by retrying relative to the root.
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	got, ok := ExtractCostFromValue(root, nil)
	if !ok || got != 0.0021 {
		t.Errorf("cost = %v ok=%v, want 0.0021 true from bare usage object", got, ok)
	}
}

// xAI's streaming responses carry the usage in a final choices:[] chunk, and
// the SSE analyzer hands ExtractCostFromValue just that usage fragment. The
// ticks key must therefore resolve against the bare object too (real live
// capture: 1370200 ticks == $0.00013702).
func TestExtractCostXTicksFromBareUsage(t *testing.T) {
	body := []byte(`{"prompt_tokens":637,"completion_tokens":8,"cached_tokens":512,"cost_in_usd_ticks":1370200}`)
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	got, ok := ExtractCostFromValue(root, nil)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if math.Abs(got-0.00013702) > 1e-12 {
		t.Errorf("cost = %v, want 0.00013702 (1370200 ticks scaled)", got)
	}
	if got >= 1370200 {
		t.Errorf("cost = %v - raw ticks leaked through unscaled", got)
	}
}

// CanonicalUsageFields is served to the Settings UI as the canonical-field
// dropdown for usage_keys maps. ParseUsage must consume EXACTLY this set -
// a field in the list that ParseUsage ignores would render a mapping that
// silently does nothing; a ParseUsage field missing from the list could never
// be mapped from the UI.
func TestCanonicalUsageFieldsMatchesParseUsage(t *testing.T) {
	if len(CanonicalUsageFields) == 0 {
		t.Fatal("CanonicalUsageFields empty")
	}
	seen := map[string]bool{}
	for _, f := range CanonicalUsageFields {
		if seen[f] {
			t.Errorf("duplicate canonical field %q", f)
		}
		seen[f] = true
		// ParseUsage must route the override for this canonical field to the
		// matching Usage field: feed a usage object where every canonical
		// field's override path points at the same custom key.
		u := ParseUsage(map[string]any{"custom": float64(7)}, map[string]string{f: "custom"})
		got := map[string]int64{
			"input_tokens":       u.InputTokens,
			"output_tokens":      u.OutputTokens,
			"total_tokens":       u.TotalTokens,
			"cache_read_tokens":  u.CacheReadTokens,
			"cache_write_tokens": u.CacheWrite,
			"reasoning_tokens":   u.ReasoningTokens,
		}
		if got[f] != 7 {
			t.Errorf("ParseUsage override %q did not land (got %d)", f, got[f])
		}
	}
}
