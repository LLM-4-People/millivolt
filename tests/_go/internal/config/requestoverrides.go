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

// RequestOverridesMax pins the literal cap value: the dashboard editor
// mirrors the number in its row count and add gate, and the rejection
// text carries it to the operator.
func TestRequestOverridesMaxPinned(t *testing.T) {
	if RequestOverridesMax != 64 {
		t.Fatalf("RequestOverridesMax = %d, want 64 (the dashboard editor mirror)", RequestOverridesMax)
	}
}

// TestRequestOverridesValidationRows walks every rejection class with the
// exact operator-facing wording: each error must name the rule index, the
// field, the offending value and the fix (the usability contract).
func TestRequestOverridesValidationRows(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{"no scope set", func(c *Config) {
			c.RequestOverrides = []RequestOverride{{Headers: map[string]string{"x-a": "v"}}}
		}, `request_overrides[0]: no scope set - give the rule a client, provider or model, or remove the rule`},
		{"scope only whitespace", func(c *Config) {
			c.RequestOverrides = []RequestOverride{{Client: "  ", Provider: "\t", Model: " ", Headers: map[string]string{"x-a": "v"}}}
		}, `request_overrides[0]: no scope set - give the rule a client, provider or model, or remove the rule`},
		{"no action set", func(c *Config) {
			c.RequestOverrides = []RequestOverride{{Client: "opencode"}}
		}, `request_overrides[0]: no action set - give the rule a headers entry, a remove_headers entry or a body value, or remove the rule`},
		{"empty body is no action", func(c *Config) {
			c.RequestOverrides = []RequestOverride{{Client: "opencode", Body: &OverrideBody{}}}
		}, `request_overrides[0]: no action set - give the rule a headers entry, a remove_headers entry or a body value, or remove the rule`},
		{"set and remove the same header", func(c *Config) {
			c.RequestOverrides = []RequestOverride{{Client: "opencode", Headers: map[string]string{"X-A": "v"}, RemoveHeaders: []string{"x-a"}}}
		}, `request_overrides[0]: "X-A" is both set in headers and removed in remove_headers; keep exactly one action per header`},
		{"duplicate in remove list", func(c *Config) {
			c.RequestOverrides = []RequestOverride{{Client: "opencode", RemoveHeaders: []string{"X-A", "x-a"}}}
		}, `request_overrides[0].remove_headers: duplicate HTTP header name "x-a"`},
		{"duplicate in headers map (case variant)", func(c *Config) {
			c.RequestOverrides = []RequestOverride{{Client: "opencode", Headers: map[string]string{"x-a": "1", "X-A": "2"}}}
		}, `request_overrides[0].headers: duplicate HTTP header name "x-a"`},
		{"invalid header name", func(c *Config) {
			c.RequestOverrides = []RequestOverride{{Client: "opencode", Headers: map[string]string{"bad name": "v"}}}
		}, `request_overrides[0].headers: "bad name" is not a valid header name (RFC 7230 token)`},
		{"invalid remove name", func(c *Config) {
			c.RequestOverrides = []RequestOverride{{Client: "opencode", RemoveHeaders: []string{"bad name"}}}
		}, `request_overrides[0].remove_headers: "bad name" is not a valid header name (RFC 7230 token)`},
		{"whitespace-only header name", func(c *Config) {
			c.RequestOverrides = []RequestOverride{{Client: "opencode", Headers: map[string]string{"   ": "v"}}}
		}, `request_overrides[0].headers: header name must not be empty or only whitespace`},
		{"whitespace-only remove name", func(c *Config) {
			c.RequestOverrides = []RequestOverride{{Client: "opencode", RemoveHeaders: []string{"  "}}}
		}, `request_overrides[0].remove_headers: header name must not be empty or only whitespace`},
		{"empty header value", func(c *Config) {
			c.RequestOverrides = []RequestOverride{{Client: "opencode", Headers: map[string]string{"x-a": " "}}}
		}, `request_overrides[0].headers: X-A: value must be a non-empty single-line header value`},
		{"newline in header value", func(c *Config) {
			c.RequestOverrides = []RequestOverride{{Client: "opencode", Headers: map[string]string{"x-a": "one\ntwo"}}}
		}, `request_overrides[0].headers: X-A: value must be a non-empty single-line header value`},
		{"duplicate scope triple", func(c *Config) {
			c.RequestOverrides = []RequestOverride{
				{Client: "opencode", Provider: "api.example", Model: "glm-5.3", Headers: map[string]string{"x-a": "1"}},
				{Client: "opencode", Provider: "api.example", Model: "glm-5.3", RemoveHeaders: []string{"x-b"}},
			}
		}, `request_overrides[1]: duplicate scope with request_overrides[0] (client "opencode", provider "api.example", model "glm-5.3"); merge the rules or change one scope`},
		{"body max_tokens zero", func(c *Config) {
			zero := 0
			c.RequestOverrides = []RequestOverride{{Client: "opencode", Body: &OverrideBody{MaxTokens: &zero}}}
		}, `request_overrides[0].body.max_tokens: must be 1..1000000, got 0`},
		{"body max_tokens negative", func(c *Config) {
			neg := -5
			c.RequestOverrides = []RequestOverride{{Client: "opencode", Body: &OverrideBody{MaxTokens: &neg}}}
		}, `request_overrides[0].body.max_tokens: must be 1..1000000, got -5`},
		{"body max_completion_tokens too big", func(c *Config) {
			big := 1_000_001
			c.RequestOverrides = []RequestOverride{{Client: "opencode", Body: &OverrideBody{MaxCompletionTokens: &big}}}
		}, `request_overrides[0].body.max_completion_tokens: must be 1..1000000, got 1000001`},
		{"cap exceeded", func(c *Config) {
			c.RequestOverrides = make([]RequestOverride, RequestOverridesMax+1)
			for i := range c.RequestOverrides {
				c.RequestOverrides[i] = RequestOverride{Client: "client-" + strconv.Itoa(i), Headers: map[string]string{"x-a": "v"}}
			}
		}, `request_overrides: 65 rules exceeds the cap of 64`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate accepted an invalid request_overrides list: %+v", c.RequestOverrides)
			}
			if err.Error() != tc.wantErr {
				t.Fatalf("Validate error = %q\nwant %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestRequestOverridesForbiddenHeaders walks the whole forbidden-owner set
// in both lists: credential headers, protocol headers and the x-proxy-
// control prefix, each with its ownership reason and case-insensitively
// (HTTP names are case-insensitive, so "AUTHORIZATION" is as denied as
// "authorization").
func TestRequestOverridesForbiddenHeaders(t *testing.T) {
	credential := []string{"authorization", "cookie", "proxy-authorization"}
	protocol := []string{"host", "content-type", "content-length", "transfer-encoding",
		"te", "connection", "keep-alive", "proxy-authenticate", "proxy-connection", "trailer", "upgrade"}
	for _, name := range credential {
		for _, spelling := range []string{name, strings.ToUpper(name)} {
			for _, field := range []string{"headers", "remove_headers"} {
				c := Default()
				if field == "headers" {
					c.RequestOverrides = []RequestOverride{{Client: "opencode", Headers: map[string]string{spelling: "v"}}}
				} else {
					c.RequestOverrides = []RequestOverride{{Client: "opencode", RemoveHeaders: []string{spelling}}}
				}
				err := c.Validate()
				if err == nil {
					t.Fatalf("%s: %s accepted forbidden header %q", field, name, spelling)
				}
				want := `request_overrides[0].` + field + `: "` + spelling + `" is credential-owned and cannot be overridden; the proxy injects the provider key itself`
				if err.Error() != want {
					t.Fatalf("%s %q: error = %q\nwant %q", field, spelling, err.Error(), want)
				}
			}
		}
	}
	for _, name := range protocol {
		for _, field := range []string{"headers", "remove_headers"} {
			c := Default()
			if field == "headers" {
				c.RequestOverrides = []RequestOverride{{Client: "opencode", Headers: map[string]string{name: "v"}}}
			} else {
				c.RequestOverrides = []RequestOverride{{Client: "opencode", RemoveHeaders: []string{name}}}
			}
			err := c.Validate()
			if err == nil {
				t.Fatalf("%s: protocol header %q accepted", field, name)
			}
			if !strings.Contains(err.Error(), `is protocol-owned and cannot be overridden; the proxy and the HTTP transport manage it`) {
				t.Fatalf("%s %q: error = %q, want the protocol-owned wording", field, name, err)
			}
		}
	}
	for _, spelling := range []string{"x-proxy-format", "X-Proxy-Headers", "x-proxy-anything"} {
		for _, field := range []string{"headers", "remove_headers"} {
			c := Default()
			if field == "headers" {
				c.RequestOverrides = []RequestOverride{{Client: "opencode", Headers: map[string]string{spelling: "v"}}}
			} else {
				c.RequestOverrides = []RequestOverride{{Client: "opencode", RemoveHeaders: []string{spelling}}}
			}
			err := c.Validate()
			if err == nil {
				t.Fatalf("%s: x-proxy- prefix header %q accepted", field, spelling)
			}
			if !strings.Contains(err.Error(), `carries the x-proxy- control prefix and cannot be overridden`) {
				t.Fatalf("%s %q: error = %q, want the x-proxy- control-prefix wording", field, spelling, err)
			}
		}
	}
	// ForbiddenOverrideHeader is the one grammar owner: an allowed name
	// passes it directly in any case spelling, a denied one reports why.
	for _, ok := range []string{"user-agent", "USER-AGENT", "x-lab-mode", "accept-encoding", "anthropic-version"} {
		if reason, forbidden := ForbiddenOverrideHeader(ok); forbidden {
			t.Fatalf("ForbiddenOverrideHeader(%q) denied an allowed name (reason %q)", ok, reason)
		}
	}
	for _, name := range credential {
		if reason, forbidden := ForbiddenOverrideHeader(name); !forbidden || reason != "credential" {
			t.Fatalf("ForbiddenOverrideHeader(%q) = (%q, %v), want (credential, true)", name, reason, forbidden)
		}
	}
	for _, name := range protocol {
		if reason, forbidden := ForbiddenOverrideHeader(name); !forbidden || reason != "protocol" {
			t.Fatalf("ForbiddenOverrideHeader(%q) = (%q, %v), want (protocol, true)", name, reason, forbidden)
		}
	}
	if reason, forbidden := ForbiddenOverrideHeader("X-Proxy-Format"); !forbidden || reason != "control" {
		t.Fatalf("ForbiddenOverrideHeader(X-Proxy-Format) = (%q, %v), want (control, true)", reason, forbidden)
	}
}

// TestRequestOverridesNormalize pins the normalization-before-judging
// contract: a valid list comes back canonical - scope fields trimmed,
// header names and values trimmed with names in HTTP canonical case, the
// remove list canonicalized in list order, and empty collections plus an
// all-unset body dropped to nil.
func TestRequestOverridesNormalize(t *testing.T) {
	tokens := 4096
	c := Default()
	c.RequestOverrides = []RequestOverride{
		{
			Client:        "  opencode  ",
			Provider:      "\tapi.example\n",
			Model:         " glm-5.3 ",
			Headers:       map[string]string{"  user-agent  ": "  my-shell/1.0  ", "x-b": "v"},
			RemoveHeaders: []string{"  x-old  ", "x-c"},
			Body:          &OverrideBody{MaxCompletionTokens: &tokens},
		},
		{
			Client:        "second",
			Headers:       map[string]string{},
			RemoveHeaders: []string{"  x-c "},
			Body:          &OverrideBody{},
		},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate(valid overrides) = %v, want nil", err)
	}
	r := c.RequestOverrides[0]
	if r.Client != "opencode" || r.Provider != "api.example" || r.Model != "glm-5.3" {
		t.Fatalf("scope not trimmed: %q/%q/%q", r.Client, r.Provider, r.Model)
	}
	if len(r.Headers) != 2 || r.Headers["User-Agent"] != "my-shell/1.0" || r.Headers["X-B"] != "v" {
		t.Fatalf("headers not trimmed/canonicalized: %#v", r.Headers)
	}
	if len(r.RemoveHeaders) != 2 || r.RemoveHeaders[0] != "X-Old" || r.RemoveHeaders[1] != "X-C" {
		t.Fatalf("remove_headers not trimmed/canonicalized/in order: %#v", r.RemoveHeaders)
	}
	if r.Body == nil || r.Body.MaxCompletionTokens == nil || *r.Body.MaxCompletionTokens != tokens || r.Body.MaxTokens != nil {
		t.Fatalf("set body field lost by normalization: %#v", r.Body)
	}
	second := c.RequestOverrides[1]
	if second.Headers != nil {
		t.Fatalf("empty headers map not normalized to nil: %#v", second.Headers)
	}
	if second.Body != nil {
		t.Fatalf("all-unset body not normalized to nil: %#v", second.Body)
	}
	if len(second.RemoveHeaders) != 1 || second.RemoveHeaders[0] != "X-C" {
		t.Fatalf("second rule remove_headers not canonicalized: %#v", second.RemoveHeaders)
	}
}

// TestRequestOverridesBodyBand pins the body band boundaries: 1 and 1000000
// load on both fields, and the out-of-band values are rejected (the
// maxRequestOutputTokens band shared with the request boundary).
func TestRequestOverridesBodyBand(t *testing.T) {
	for _, v := range []int{1, 1_000_000} {
		c := Default()
		c.RequestOverrides = []RequestOverride{{Client: "opencode", Body: &OverrideBody{MaxTokens: &v, MaxCompletionTokens: &v}}}
		if err := c.Validate(); err != nil {
			t.Fatalf("body value %d rejected: %v", v, err)
		}
	}
}

// TestRequestOverridesCapBoundary: the cap itself is a valid length.
func TestRequestOverridesCapBoundary(t *testing.T) {
	c := Default()
	c.RequestOverrides = make([]RequestOverride, RequestOverridesMax)
	for i := range c.RequestOverrides {
		c.RequestOverrides[i] = RequestOverride{Client: "client-" + strconv.Itoa(i), Headers: map[string]string{"x-a": "v"}}
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("%d distinct rules rejected: %v", RequestOverridesMax, err)
	}
}

// TestRequestOverridesYAMLRoundTrip: a canonical rule list survives
// WriteFile/LoadFile byte-for-value, and the written document renders the
// documented shape (all six sub-keys, canonical header names, body
// fields).
func TestRequestOverridesYAMLRoundTrip(t *testing.T) {
	tokens := 2048
	one := 1
	c := Default()
	c.RequestOverrides = []RequestOverride{
		{
			Client:        "opencode",
			Provider:      "api.example",
			Model:         "glm-5.3",
			Headers:       map[string]string{"  user-agent  ": "  my-shell/1.0  ", "x-b": "v"},
			RemoveHeaders: []string{"  x-old  "},
			Body:          &OverrideBody{MaxCompletionTokens: &tokens},
		},
		{Provider: "api.example", Body: &OverrideBody{MaxTokens: &one}},
		{Model: "glm-5.3", RemoveHeaders: []string{"x-gone"}},
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
		"request_overrides:\n",
		`  - client: "opencode"` + "\n",
		`      "User-Agent": "my-shell/1.0"` + "\n",
		"      - \"X-Old\"\n",
		"      max_completion_tokens: 2048\n",
		"    headers: {}\n",
		"    remove_headers: []\n",
		"      max_tokens: 1\n",
		"    body: {}\n",
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("written YAML omits %q:\n%s", want, body)
		}
	}
	got, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.RequestOverrides, c.RequestOverrides) {
		t.Fatalf("round-tripped rules = %#v\nwant %#v", got.RequestOverrides, c.RequestOverrides)
	}
}

// TestRequestOverridesSettingsPost pins the Settings POST path end to end:
// the JSON-decoded sheet shape coerces into the typed field, the round
// trip through Map() (what GET serves, the editor echoes back) re-applies
// to an identical config, and an unknown nested field is rejected by the
// strict decoder instead of silently dropped.
func TestRequestOverridesSettingsPost(t *testing.T) {
	raw := `{"request_overrides":[{"client":"opencode","provider":"","model":"","headers":{"x-lab-mode":"aggressive"},"remove_headers":["x-old"],"body":{"max_tokens":4096,"max_completion_tokens":null}}]}`
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
	rs := c.RequestOverrides
	if len(rs) != 1 || rs[0].Client != "opencode" || rs[0].Provider != "" || rs[0].Model != "" {
		t.Fatalf("scope not coerced: %#v", rs)
	}
	if rs[0].Headers["X-Lab-Mode"] != "aggressive" || len(rs[0].Headers) != 1 {
		t.Fatalf("headers not coerced/canonicalized: %#v", rs[0].Headers)
	}
	if len(rs[0].RemoveHeaders) != 1 || rs[0].RemoveHeaders[0] != "X-Old" {
		t.Fatalf("remove_headers not coerced/canonicalized: %#v", rs[0].RemoveHeaders)
	}
	if rs[0].Body == nil || rs[0].Body.MaxTokens == nil || *rs[0].Body.MaxTokens != 4096 || rs[0].Body.MaxCompletionTokens != nil {
		t.Fatalf("body not coerced: %#v", rs[0].Body)
	}

	// Round trip: Map() exports JSON-friendly values; marshaling them and
	// applying to a fresh config reproduces the same validated state.
	exported, err := json.Marshal(c.Map()["request_overrides"])
	if err != nil {
		t.Fatal(err)
	}
	var echoed map[string]any
	if err := json.Unmarshal([]byte(`{"request_overrides":`+string(exported)+`}`), &echoed); err != nil {
		t.Fatal(err)
	}
	fresh := Default()
	if err := fresh.Apply(echoed); err != nil {
		t.Fatalf("Apply(echoed Map) = %v, want nil", err)
	}
	if err := fresh.Validate(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Map()["request_overrides"], c.Map()["request_overrides"]) {
		t.Fatalf("Map round trip drifted:\n got %#v\nwant %#v", fresh.Map()["request_overrides"], c.Map()["request_overrides"])
	}

	// Unknown nested fields never pass the strict decoder.
	bad := map[string]any{"request_overrides": []any{map[string]any{"clientt": "opencode"}}}
	err = c.Apply(bad)
	if err == nil {
		t.Fatal("Apply(unknown nested field) = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), `unknown field "clientt"`) {
		t.Fatalf("unknown-field error = %q, want it to name clientt", err)
	}
	// A non-list shape is rejected with the shape wording.
	err = c.Apply(map[string]any{"request_overrides": 5})
	if err == nil || !strings.Contains(err.Error(), "want [{client,") {
		t.Fatalf("scalar Apply error = %v, want the documented rule shape wording", err)
	}
	// An explicit empty list and an explicit null both clear the field.
	if err := c.Apply(map[string]any{"request_overrides": []any{}}); err != nil {
		t.Fatal(err)
	}
	if len(c.RequestOverrides) != 0 {
		t.Fatalf("explicit empty list not applied: %#v", c.RequestOverrides)
	}
	if err := c.Apply(map[string]any{"request_overrides": nil}); err != nil {
		t.Fatal(err)
	}
	if c.RequestOverrides != nil {
		t.Fatalf("explicit null not applied: %#v", c.RequestOverrides)
	}
}

// TestRequestOverridesMapIsJSONArray pins the Map export shape: an empty
// list exports as [] (never null), matching the KindStrings/Providers
// precedent (TestMapEmptySlicesAreJSONArrays) so the settings editor never
// has to handle null, and the exported rules are deep copies that cannot
// alias the live config.
func TestRequestOverridesMapIsJSONArray(t *testing.T) {
	b, err := json.Marshal(Default().Map()["request_overrides"])
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Fatalf("request_overrides JSON = %s, want [] (not null)", b)
	}
	c := Default()
	c.RequestOverrides = []RequestOverride{{Client: "opencode", Headers: map[string]string{"x-a": "1"}}}
	exported, ok := c.Map()["request_overrides"].([]RequestOverride)
	if !ok || len(exported) != 1 {
		t.Fatalf("Map export = %#v, want the typed rule list", c.Map()["request_overrides"])
	}
	exported[0].Client = "mutated"
	exported[0].Headers["x-a"] = "mutated"
	if c.RequestOverrides[0].Client != "opencode" || c.RequestOverrides[0].Headers["x-a"] != "1" {
		t.Fatal("Map export shares rule state with the live config")
	}
}

// TestRequestOverridesSchemaPins: the registry entry is the documentation
// surface - exact-leaf matching, list order and later-rule precedence, the
// forbidden headers, the cursor body skip and the canonical provider label
// all read from the schema help, and the new category exists with it.
func TestRequestOverridesSchemaPins(t *testing.T) {
	f := FieldByKey("request_overrides")
	if f == nil {
		t.Fatal("request_overrides missing from Schema()")
	}
	if f.Kind != KindRequestOverrides {
		t.Fatalf("kind = %q, want %q", f.Kind, KindRequestOverrides)
	}
	if f.Category != "overrides" {
		t.Fatalf("category = %q, want overrides", f.Category)
	}
	if !f.HotReload {
		t.Fatal("request_overrides must hot-reload")
	}
	for _, phrase := range []string{
		"exactly",          // exact-leaf matching
		"provider_aliases", // canonical provider labels
		"list order",       // rule ordering
		"later rule wins",  // precedence
		"x-proxy-",         // forbidden control prefix
		"authorization",    // forbidden credential header
		"cursor",           // cursor body skip
	} {
		if !strings.Contains(f.Help, phrase) {
			t.Errorf("schema help omits %q: %s", phrase, f.Help)
		}
	}
	var cat *Category
	for i := range Categories() {
		if Categories()[i].ID == "overrides" {
			cat = &Categories()[i]
		}
	}
	if cat == nil {
		t.Fatal("overrides category missing from Categories()")
	}
	if cat.Label == "" || cat.Help == "" {
		t.Fatalf("overrides category must carry a label and help: %+v", cat)
	}
	line := f.TypeLine(Default().Map()["request_overrides"])
	if !strings.Contains(line, "{client, provider, model, headers, remove_headers, body}") {
		t.Errorf("TypeLine = %q, want the documented rule shape", line)
	}
	if !strings.Contains(line, "default []") {
		t.Errorf("TypeLine = %q, want the empty default documented", line)
	}
}

// TestCloneRequestOverridesIndependent: Clone deep-copies the rule list,
// its nested collections and the body pointers, so a reload snapshot can
// never alias a prior snapshot.
func TestCloneRequestOverridesIndependent(t *testing.T) {
	tokens := 4096
	c := Default()
	c.RequestOverrides = []RequestOverride{{
		Client:        "opencode",
		Headers:       map[string]string{"x-a": "1"},
		RemoveHeaders: []string{"x-old"},
		Body:          &OverrideBody{MaxTokens: &tokens},
	}}
	clone := c.Clone()
	if len(clone.RequestOverrides) != 1 {
		t.Fatalf("clone dropped the rules: %#v", clone.RequestOverrides)
	}
	r := clone.RequestOverrides[0]
	r.Client = "mutated"
	r.Headers["x-a"] = "mutated"
	r.RemoveHeaders[0] = "mutated"
	*r.Body.MaxTokens = 0
	if c.RequestOverrides[0].Client != "opencode" ||
		c.RequestOverrides[0].Headers["x-a"] != "1" ||
		c.RequestOverrides[0].RemoveHeaders[0] != "x-old" ||
		*c.RequestOverrides[0].Body.MaxTokens != 4096 {
		t.Fatalf("clone shares rule state with the original: %+v", c.RequestOverrides[0])
	}
	// A nil list clones to nil (absent stays absent).
	if Default().Clone().RequestOverrides != nil {
		t.Fatal("nil RequestOverrides must clone to nil")
	}
}

// TestRequestOverridesLoadFile: valid rules load and normalize; a rule the
// gate rejects drops only that key (defaults stay, boot succeeds), and the
// YAML type gate rejects scalar and non-map item shapes.
func TestRequestOverridesLoadFile(t *testing.T) {
	valid := `request_overrides:
  - client: opencode
    headers:
      x-lab-mode: aggressive
    remove_headers:
      - x-old
    body:
      max_tokens: 4096
`
	c := loadFileOK(t, valid)
	if len(c.RequestOverrides) != 1 {
		t.Fatalf("valid rules not applied: %#v", c.RequestOverrides)
	}
	r := c.RequestOverrides[0]
	if r.Client != "opencode" || r.Headers["X-Lab-Mode"] != "aggressive" ||
		len(r.RemoveHeaders) != 1 || r.RemoveHeaders[0] != "X-Old" ||
		r.Body == nil || r.Body.MaxTokens == nil || *r.Body.MaxTokens != 4096 {
		t.Fatalf("loaded rule not normalized: %#v", r)
	}

	for _, tc := range []struct{ name, yaml string }{
		{"no scope", "request_overrides:\n  - headers:\n      x-a: v\n"},
		{"forbidden header", "request_overrides:\n  - client: x\n    headers:\n      authorization: secret\n"},
		{"body out of band", "request_overrides:\n  - client: x\n    body:\n        max_tokens: 0\n"},
		{"unknown nested field leaves no rule", "request_overrides:\n  - bogus: y\n"},
		{"scalar value", "request_overrides: 5\n"},
		{"item not a map", "request_overrides:\n  - x\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertOverlaySkipped(t, tc.yaml, "request_overrides")
		})
	}

	// The rendered file round-trips through the schema round-trip test's
	// writer as well; here pin that a written non-empty list reloads
	// non-empty (the presence probe never swallows a written list).
	path := filepath.Join(t.TempDir(), "written.yaml")
	if err := WriteFile(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RequestOverrides) != 1 {
		t.Fatalf("written rules lost on reload: %#v", got.RequestOverrides)
	}
}
