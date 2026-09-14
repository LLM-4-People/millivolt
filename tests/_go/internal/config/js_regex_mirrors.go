package config

// Cross-language regex mirrors: the dashboard's PROV_PATH_RE and
// PROV_HEADER_RE (chrome.js) must accept and reject exactly what the Go
// owners (provPathRE, ValidHeaderName) do. The JS literals are extracted
// from the real chrome.js source and compiled with Go's regexp engine, so
// one shared table drives both sides of each pair: any verdict change on
// either side reddens here. The extraction itself fails loudly if a future
// pattern uses JS-only syntax or an unescaped slash, instead of silently
// comparing nothing.

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// chromeJSSource locates the repository root from this file's compile-time
// source path (the same self-location rule the web test suite applies;
// test overlays keep the physical path, so the walk finds the root's
// go.mod) and reads the dashboard's chrome.js from it.
func chromeJSSource(t *testing.T) []byte {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok || file == "" {
		t.Skip("cannot locate own source path (trimpath build) - chrome.js regex mirrors unchecked")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			b, err := os.ReadFile(filepath.Join(dir, "internal", "web", "static", "js", "chrome.js"))
			if err != nil {
				t.Fatalf("read chrome.js: %v", err)
			}
			return b
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("no go.mod above " + file + " - chrome.js regex mirrors unchecked")
		}
		dir = parent
	}
}

// jsRegexLiteral extracts the pattern text of a `const NAME = /pattern/;`
// declaration. Both mirrored patterns are slash-free; a future pattern
// carrying one fails here loudly rather than half-matching.
func jsRegexLiteral(t *testing.T, src []byte, name string) string {
	t.Helper()
	decl := "const " + name + " = /"
	start := strings.Index(string(src), decl)
	if start < 0 {
		t.Fatalf("chrome.js missing const %s = /", name)
	}
	rest := string(src[start+len(decl):])
	end := strings.Index(rest, "/")
	if end < 0 {
		t.Fatalf("chrome.js %s pattern literal is not closed", name)
	}
	if next := rest[end+1 : end+2]; next != ";" && next != "" && next != "\n" {
		t.Fatalf("chrome.js %s carries regex flags; the mirror only handles flagless literals", name)
	}
	return rest[:end]
}

// TestProvPathGrammarAndJSRegexAgree pins the dotted JSON path grammar: the
// Go owner (provPathRE, shared by cost_keys, usage_keys and models_keys
// validation) and the dashboard's PROV_PATH_RE must render the same
// verdict on every crafted path, so the Settings editor can never accept a
// path the server would reject (or refuse one it accepts).
func TestProvPathGrammarAndJSRegexAgree(t *testing.T) {
	js := jsRegexLiteral(t, chromeJSSource(t), "PROV_PATH_RE")
	jsRE, err := regexp.Compile(js)
	if err != nil {
		t.Fatalf("the extracted PROV_PATH_RE %q is not a Go-compilable pattern: %v", js, err)
	}
	accept := []string{"usage", "data.usage", "a.b.c", "x_1.y-2", "items.0.name", "0.1"}
	reject := []string{"", ".", "a.", ".a", "a..b", "a b", "a.b c", "a|b", "café", "a/b", "a[0]", "a,b"}
	for _, s := range accept {
		if !provPathRE.MatchString(s) {
			t.Errorf("provPathRE(%q) = false, want true", s)
		}
		if !jsRE.MatchString(s) {
			t.Errorf("chrome.js PROV_PATH_RE(%q) = false, want true (Go accepts)", s)
		}
	}
	for _, s := range reject {
		if provPathRE.MatchString(s) {
			t.Errorf("provPathRE(%q) = true, want false", s)
		}
		if jsRE.MatchString(s) {
			t.Errorf("chrome.js PROV_PATH_RE(%q) = true, want false (Go rejects)", s)
		}
	}
}

// TestValidHeaderNameGrammarAndJSRegexAgree pins the RFC 7230 field-name
// token rule: the Go owner (ValidHeaderName, the shared gate for provider
// header maps and routing controls) and the dashboard's PROV_HEADER_RE
// must agree on every crafted name, so the Settings editor denies exactly
// the names the server denies.
func TestValidHeaderNameGrammarAndJSRegexAgree(t *testing.T) {
	js := jsRegexLiteral(t, chromeJSSource(t), "PROV_HEADER_RE")
	jsRE, err := regexp.Compile(js)
	if err != nil {
		t.Fatalf("the extracted PROV_HEADER_RE %q is not a Go-compilable pattern: %v", js, err)
	}
	accept := []string{"a", "X-Request-Id", "x_a", "0", "!#$%&'*+-.^_`|~", "A0-~"}
	reject := []string{"", "X Header", "X:Header", "h\u00e9ader", "a\u007fb", "a\u0000b", "a\rb", "(x)", `"x"`, "x/y"}
	for _, s := range accept {
		if !ValidHeaderName(s) {
			t.Errorf("ValidHeaderName(%q) = false, want true", s)
		}
		if !jsRE.MatchString(s) {
			t.Errorf("chrome.js PROV_HEADER_RE(%q) = false, want true (Go accepts)", s)
		}
	}
	for _, s := range reject {
		if ValidHeaderName(s) {
			t.Errorf("ValidHeaderName(%q) = true, want false", s)
		}
		if jsRE.MatchString(s) {
			t.Errorf("chrome.js PROV_HEADER_RE(%q) = true, want false (Go rejects)", s)
		}
	}
}
