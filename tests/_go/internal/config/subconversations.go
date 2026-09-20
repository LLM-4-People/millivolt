package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// TestSubConversationsCapsPinned pins the literal cap values: the dashboard
// editor mirrors the entry cap in its row count and add gate, the params cap
// in its per-entry add gate, and the param byte bound in its live grammar
// validation, and the rejection texts carry all three to the operator.
func TestSubConversationsCapsPinned(t *testing.T) {
	if SubConversationsMax != 16 {
		t.Fatalf("SubConversationsMax = %d, want 16 (the dashboard editor mirror)", SubConversationsMax)
	}
	if SubConversationParamsMax != 4 {
		t.Fatalf("SubConversationParamsMax = %d, want 4 (the dashboard editor mirror)", SubConversationParamsMax)
	}
	if subConversationParamMaxBytes != 64 {
		t.Fatalf("subConversationParamMaxBytes = %d, want 64 (the editor grammar mirror)", subConversationParamMaxBytes)
	}
}

// TestSubConversationsValidationRows walks every rejection class with the
// exact operator-facing wording: each error must name the entry (and param)
// index, the offending value and the fix (the usability contract).
func TestSubConversationsValidationRows(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{"client empty", func(c *Config) {
			c.SubConversations = []SubConversation{{Params: []string{"promptCacheKey"}}}
		}, `sub_conversations[0]: client must not be empty or only whitespace - name the classified client this entry tracks, or remove the entry`},
		{"client only whitespace", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "  \t ", Params: []string{"promptCacheKey"}}}
		}, `sub_conversations[0]: client must not be empty or only whitespace - name the classified client this entry tracks, or remove the entry`},
		{"no param set", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "opencode"}}
		}, `sub_conversations[0]: no param set - give the entry a request body field to track, or remove the entry`},
		{"empty params list is no param", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{}}}
		}, `sub_conversations[0]: no param set - give the entry a request body field to track, or remove the entry`},
		{"params cap", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{"a", "b", "c", "d", "e"}}}
		}, `sub_conversations[0].params: 5 params exceeds the cap of 4`},
		{"entries cap", func(c *Config) {
			c.SubConversations = make([]SubConversation, SubConversationsMax+1)
			for i := range c.SubConversations {
				c.SubConversations[i] = SubConversation{Client: "client-" + strconv.Itoa(i), Params: []string{"promptCacheKey"}}
			}
		}, `sub_conversations: 17 entries exceeds the cap of 16`},
		{"duplicate clients", func(c *Config) {
			c.SubConversations = []SubConversation{
				{Client: "opencode", Params: []string{"promptCacheKey"}},
				{Client: "opencode", Params: []string{"sessionKey"}, Strip: true},
			}
		}, `sub_conversations[1]: duplicate client "opencode" with sub_conversations[0]; merge the entries or change one client`},
		{"duplicate clients exact spelling only", func(c *Config) {
			// No case folding: "OpenCode" is a different (equally exact) client.
			c.SubConversations = []SubConversation{
				{Client: "opencode", Params: []string{"promptCacheKey"}},
				{Client: "OpenCode", Params: []string{"sessionKey"}},
			}
		}, ""},
		{"duplicate params within one entry", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{"promptCacheKey", "sessionKey", "promptCacheKey"}}}
		}, `sub_conversations[0].params: duplicate "promptCacheKey"`},
		{"param only whitespace", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{"  "}}}
		}, `sub_conversations[0].params[0]: must not be empty or only whitespace`},
		{"param too long", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{strings.Repeat("a", 65)}}}
		}, `sub_conversations[0].params[0]: must be 1..64 bytes, got 65`},
		{"param quote", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{"ba\"d"}}}
		}, `sub_conversations[0].params[0]: "ba\"d" is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)`},
		{"param backslash", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{"ba\\ck"}}}
		}, `sub_conversations[0].params[0]: "ba\\ck" is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)`},
		{"param control byte tab", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{"one\ttwo"}}}
		}, `sub_conversations[0].params[0]: "one\ttwo" is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)`},
		{"param control byte newline", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{"one\ntwo"}}}
		}, `sub_conversations[0].params[0]: "one\ntwo" is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)`},
		{"param nul byte", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{"x\x00y"}}}
		}, `sub_conversations[0].params[0]: "x\x00y" is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)`},
		{"param del byte", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{"del\x7fbyte"}}}
		}, `sub_conversations[0].params[0]: "del\x7fbyte" is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)`},
		{"param non-ascii", func(c *Config) {
			c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{"café"}}}
		}, `sub_conversations[0].params[0]: "café" is not a JSON key segment (printable ASCII only, no quote, backslash or control bytes)`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(c)
			err := c.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate rejected a valid list: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted an invalid sub_conversations list: %+v", c.SubConversations)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("Validate error = %q\nwant %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestSubConversationsNormalize pins the normalization-before-judging
// contract: a valid list comes back canonical - the client and every param
// name trimmed, order preserved, first-present-wins order intact - and
// strip stays a plain bool defaulting to false (forward unchanged).
func TestSubConversationsNormalize(t *testing.T) {
	c := Default()
	c.SubConversations = []SubConversation{
		{Client: "  opencode  ", Params: []string{"  promptCacheKey ", "sessionKey"}, Strip: true},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate(valid entries) = %v, want nil", err)
	}
	s := c.SubConversations[0]
	if s.Client != "opencode" {
		t.Fatalf("client not trimmed: %q", s.Client)
	}
	if len(s.Params) != 2 || s.Params[0] != "promptCacheKey" || s.Params[1] != "sessionKey" {
		t.Fatalf("params not trimmed/in order: %#v", s.Params)
	}
	if !s.Strip {
		t.Fatal("strip not preserved")
	}
	// The strip default is forward (false), never a silent rewrite.
	c2 := Default()
	c2.SubConversations = []SubConversation{{Client: "opencode", Params: []string{"promptCacheKey"}}}
	if err := c2.Validate(); err != nil {
		t.Fatalf("Validate(stripless entry) = %v, want nil", err)
	}
	if c2.SubConversations[0].Strip {
		t.Fatal("strip must default to false (the tracked field is forwarded)")
	}
}

// TestSubConversationsParamGrammarBoundaries pins the one JSON-key-segment
// grammar: 64 bytes pass and 65 fail, control bytes (tab, newline, NUL,
// DEL) fail, quote and backslash fail, non-ASCII fails, and legal-but-unusual
// JSON spellings (a literal dot, an interior space) pass because the rule is
// exactly the stated grammar, deliberately not provPathRE (dotted response
// paths) and deliberately not a regex.
func TestSubConversationsParamGrammarBoundaries(t *testing.T) {
	ok64 := strings.Repeat("a", 64)
	for _, name := range []string{"promptCacheKey", "session_id", "x-convo", ok64, "a.b", "spaced key"} {
		c := Default()
		c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{name}}}
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(param %q) = %v, want accepted", name, err)
		}
	}
	for _, name := range []string{
		strings.Repeat("a", 65),
		"one\ttwo",
		"one\ntwo",
		"x\x00y",
		"\x01ctl",
		"del\x7fbyte",
		"café",
		"ba\"d",
		"ba\\ck",
	} {
		c := Default()
		c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{name}}}
		if err := c.Validate(); err == nil {
			t.Errorf("Validate(param %q) = nil, want rejected", name)
		}
	}
	// Leading and trailing whitespace trim away to a valid name.
	c := Default()
	c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{"  padded  "}}}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate(padded param) = %v, want trimmed to valid", err)
	}
	if c.SubConversations[0].Params[0] != "padded" {
		t.Fatalf("param not trimmed: %q", c.SubConversations[0].Params[0])
	}
}

// TestSubConversationsCapBoundary: the caps themselves are valid lengths -
// 16 distinct-client entries, 4 params each.
func TestSubConversationsCapBoundary(t *testing.T) {
	c := Default()
	c.SubConversations = make([]SubConversation, SubConversationsMax)
	for i := range c.SubConversations {
		c.SubConversations[i] = SubConversation{
			Client: "client-" + strconv.Itoa(i),
			Params: []string{"p0", "p1", "p2", "p3"},
		}
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("%d distinct entries with %d params rejected: %v", SubConversationsMax, SubConversationParamsMax, err)
	}
}

// TestSubConversationsYAMLRoundTrip: a canonical entry list survives
// WriteFile/LoadFile byte-for-value, and the written document renders the
// documented shape (client, the params list, strip always explicit).
func TestSubConversationsYAMLRoundTrip(t *testing.T) {
	c := Default()
	c.SubConversations = []SubConversation{
		{Client: "  opencode  ", Params: []string{"  promptCacheKey ", "sessionKey"}, Strip: true},
		{Client: "llm-shell", Params: []string{"conversationId"}},
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := WriteFile(path, c); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"sub_conversations:\n",
		`  - client: "opencode"` + "\n",
		"    params:\n",
		"      - \"promptCacheKey\"\n",
		"      - \"sessionKey\"\n",
		"    strip: true\n",
		`  - client: "llm-shell"` + "\n",
		"      - \"conversationId\"\n",
		"    strip: false\n",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("written YAML omits %q:\n%s", want, body)
		}
	}
	got, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.SubConversations, c.SubConversations) {
		t.Fatalf("round-tripped entries = %#v\nwant %#v", got.SubConversations, c.SubConversations)
	}
}

// TestSubConversationsSettingsPost pins the Settings POST path end to end:
// the JSON-decoded sheet shape coerces into the typed field, the round
// trip through Map() (what GET serves, the editor echoes back) re-applies
// to an identical config, and an unknown nested field is rejected by the
// strict decoder instead of silently dropped.
func TestSubConversationsSettingsPost(t *testing.T) {
	raw := `{"sub_conversations":[{"client":"opencode","params":["promptCacheKey","sessionKey"],"strip":true}]}`
	var values map[string]any
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		t.Fatal(err)
	}
	c := Default()
	if err := c.Apply(values); err != nil {
		t.Fatalf("Apply(settings shape) = %v, want nil", err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate(applied settings) = %v, want nil", err)
	}
	ss := c.SubConversations
	if len(ss) != 1 || ss[0].Client != "opencode" || !ss[0].Strip {
		t.Fatalf("entry not coerced: %#v", ss)
	}
	if len(ss[0].Params) != 2 || ss[0].Params[0] != "promptCacheKey" || ss[0].Params[1] != "sessionKey" {
		t.Fatalf("params not coerced: %#v", ss[0].Params)
	}

	// Round trip: Map() exports JSON-friendly values; marshaling them and
	// applying to a fresh config reproduces the same validated state.
	exported, err := json.Marshal(c.Map()["sub_conversations"])
	if err != nil {
		t.Fatal(err)
	}
	var echoed map[string]any
	if err := json.Unmarshal([]byte(`{"sub_conversations":`+string(exported)+`}`), &echoed); err != nil {
		t.Fatal(err)
	}
	fresh := Default()
	if err := fresh.Apply(echoed); err != nil {
		t.Fatalf("Apply(echoed Map) = %v, want nil", err)
	}
	if err := fresh.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Map()["sub_conversations"], c.Map()["sub_conversations"]) {
		t.Fatalf("Map round trip drifted:\n got %#v\nwant %#v", fresh.Map()["sub_conversations"], c.Map()["sub_conversations"])
	}

	// Unknown nested fields never pass the strict decoder.
	bad := map[string]any{"sub_conversations": []any{map[string]any{"clientt": "opencode"}}}
	err = c.Apply(bad)
	if err == nil {
		t.Fatal("Apply(unknown nested field) = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), `unknown field "clientt"`) {
		t.Fatalf("unknown-field error = %q, want it to name clientt", err)
	}
	// A non-list shape is rejected with the shape wording.
	err = c.Apply(map[string]any{"sub_conversations": 5})
	if err == nil || !strings.Contains(err.Error(), "want [{client,") {
		t.Fatalf("scalar Apply error = %v, want the documented entry shape wording", err)
	}
	// An explicit empty list and an explicit null both clear the field.
	if err := c.Apply(map[string]any{"sub_conversations": []any{}}); err != nil {
		t.Fatal(err)
	}
	if len(c.SubConversations) != 0 {
		t.Fatalf("explicit empty list not applied: %#v", c.SubConversations)
	}
	if err := c.Apply(map[string]any{"sub_conversations": nil}); err != nil {
		t.Fatal(err)
	}
	if c.SubConversations != nil {
		t.Fatalf("explicit null not applied: %#v", c.SubConversations)
	}
}

// TestSubConversationsMapIsJSONArray pins the Map export shape: an empty
// list exports as [] (never null), matching the KindStrings/Providers
// precedent (TestMapEmptySlicesAreJSONArrays) so the settings editor never
// has to handle null, and the exported entries are deep copies that cannot
// alias the live config.
func TestSubConversationsMapIsJSONArray(t *testing.T) {
	b, err := json.Marshal(Default().Map()["sub_conversations"])
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Fatalf("sub_conversations JSON = %s, want [] (not null)", b)
	}
	c := Default()
	c.SubConversations = []SubConversation{{Client: "opencode", Params: []string{"promptCacheKey"}, Strip: true}}
	exported, ok := c.Map()["sub_conversations"].([]SubConversation)
	if !ok || len(exported) != 1 {
		t.Fatalf("Map export = %#v, want the typed entry list", c.Map()["sub_conversations"])
	}
	exported[0].Client = "mutated"
	exported[0].Params[0] = "mutated"
	if c.SubConversations[0].Client != "opencode" || c.SubConversations[0].Params[0] != "promptCacheKey" {
		t.Fatal("Map export shares entry state with the live config")
	}
}

// TestSubConversationsSchemaPins: the registry entry is the documentation
// surface - the caps, the exact-name semantics, first-present-wins, the k:
// identity and its header precedence, strip, translated-target injection
// and the drop-not-reject bound all read from the schema help, and the
// entry lives in the conversation category.
func TestSubConversationsSchemaPins(t *testing.T) {
	f := FieldByKey("sub_conversations")
	if f == nil {
		t.Fatal("sub_conversations missing from Schema()")
	}
	if f.Kind != KindSubConversations {
		t.Fatalf("kind = %q, want %q", f.Kind, KindSubConversations)
	}
	if f.Category != "conversation" {
		t.Fatalf("category = %q, want conversation", f.Category)
	}
	if !f.HotReload {
		t.Fatal("sub_conversations must hot-reload")
	}
	for _, phrase := range []string{
		"exactly",          // exact client names, no wildcard, no case folding
		"1..4",             // params per entry
		"first present",    // first-present-wins
		"dropped",          // drop-not-reject bound
		"k:",               // the tracked identity namespace
		"X-Proxy-Session",  // identity precedence
		"prompt_cache_key", // translated Anthropic injection
		"strip",            // strip semantics
		"64 bytes",         // param name byte bound
		"16 entries",       // entry cap
		"4 params",         // param cap
	} {
		if !strings.Contains(f.Help, phrase) {
			t.Errorf("schema help omits %q: %s", phrase, f.Help)
		}
	}
	line := f.TypeLine(Default().Map()["sub_conversations"])
	if !strings.Contains(line, "{client, params, strip}") {
		t.Errorf("TypeLine = %q, want the documented entry shape", line)
	}
	if !strings.Contains(line, "default []") {
		t.Errorf("TypeLine = %q, want the empty default documented", line)
	}
}

// TestCloneSubConversationsIndependent: Clone deep-copies the entry list
// and its nested params, so a reload snapshot can never alias a prior
// snapshot.
func TestCloneSubConversationsIndependent(t *testing.T) {
	c := Default()
	c.SubConversations = []SubConversation{{
		Client: "opencode",
		Params: []string{"promptCacheKey"},
		Strip:  true,
	}}
	clone := c.Clone()
	if len(clone.SubConversations) != 1 {
		t.Fatalf("clone dropped the entries: %#v", clone.SubConversations)
	}
	s := clone.SubConversations[0]
	s.Client = "mutated"
	s.Params[0] = "mutated"
	s.Strip = false
	if c.SubConversations[0].Client != "opencode" ||
		c.SubConversations[0].Params[0] != "promptCacheKey" ||
		!c.SubConversations[0].Strip {
		t.Fatalf("clone shares entry state with the original: %+v", c.SubConversations[0])
	}
	// A nil list clones to nil (absent stays absent).
	if Default().Clone().SubConversations != nil {
		t.Fatal("nil SubConversations must clone to nil")
	}
}

// TestSubConversationsLoadFile: valid entries load and normalize; an entry
// the gate rejects drops only that key (defaults stay, boot succeeds), and
// the YAML type gate rejects scalar and non-map item shapes.
func TestSubConversationsLoadFile(t *testing.T) {
	valid := `sub_conversations:
  - client: opencode
    params:
      - promptCacheKey
    strip: true
`
	c := loadFileOK(t, valid)
	if len(c.SubConversations) != 1 {
		t.Fatalf("valid entries not applied: %#v", c.SubConversations)
	}
	s := c.SubConversations[0]
	if s.Client != "opencode" || len(s.Params) != 1 || s.Params[0] != "promptCacheKey" || !s.Strip {
		t.Fatalf("loaded entry not normalized: %#v", s)
	}

	for _, tc := range []struct{ name, yaml string }{
		{"no client", "sub_conversations:\n  - params:\n      - promptCacheKey\n"},
		{"empty client", "sub_conversations:\n  - client: \"\"\n    params:\n      - promptCacheKey\n"},
		{"no param", "sub_conversations:\n  - client: opencode\n"},
		{"duplicate client", "sub_conversations:\n  - client: opencode\n    params:\n      - promptCacheKey\n  - client: opencode\n    params:\n      - sessionKey\n"},
		{"bad param grammar", "sub_conversations:\n  - client: opencode\n    params:\n      - \"ba\\td\"\n"},
		{"params cap", "sub_conversations:\n  - client: opencode\n    params: [a, b, c, d, e]\n"},
		{"unknown nested field leaves no entry", "sub_conversations:\n  - bogus: y\n"},
		{"scalar value", "sub_conversations: 5\n"},
		{"item not a map", "sub_conversations:\n  - x\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertOverlaySkipped(t, tc.yaml, "sub_conversations")
		})
	}

	// A written non-empty list reloads non-empty (the presence probe never
	// swallows a written list).
	path := filepath.Join(t.TempDir(), "written.yaml")
	if err := WriteFile(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.SubConversations) != 1 {
		t.Fatalf("written entries lost on reload: %#v", got.SubConversations)
	}
}

// TestExampleSubConversationsExemplar pins the shipped compatibility
// exemplar: Example() (the compatibility-profile home, not Default()) is
// the only place the opencode entry is enabled, with strip because the
// operator's upstream rejects the field with a 400.
func TestExampleSubConversationsExemplar(t *testing.T) {
	if Default().SubConversations != nil {
		t.Fatal("Default() must leave sub_conversations nil (the only default site)")
	}
	ex := Example()
	if len(ex.SubConversations) != 1 {
		t.Fatalf("Example() sub_conversations = %#v, want exactly the opencode exemplar", ex.SubConversations)
	}
	s := ex.SubConversations[0]
	if s.Client != "opencode" || len(s.Params) != 1 || s.Params[0] != "promptCacheKey" || !s.Strip {
		t.Fatalf("opencode exemplar = %#v, want client opencode, params [promptCacheKey], strip true", s)
	}
	if err := ex.Validate(); err != nil {
		t.Fatalf("shipped exemplar invalid: %v", err)
	}
}
