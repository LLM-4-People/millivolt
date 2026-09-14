package proxy

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// The dashboard's duration dropdowns offer fixed token sets (chrome.js
// PAUSE_DURS/DEBUG_DURS, LIMIT_WINDOWS, FILTER_AGES). These pins read the
// chrome.js source (the TestDashCfgSeedsMatchDefault precedent) and assert
// every token the menu actually offers is accepted by the Go owner of its
// surface, so the menu can never widen to a duration the server would
// reject - adding 45m to a menu without the server accepting it reddens
// here, on the exact source a deploy would ship:
//   - pause/debug hold durations go through the pauseDurations allowlist
//     (shared by HandlePause and HandleDebug), checked through the real
//     handlers;
//   - throttle request/token windows go through parseLimitWindow;
//   - the Clear/Logs age filter's tokens must be canonical Go duration
//     spellings (config.FormatDuration round-trips them) because the client
//     parses them with the Go duration grammar before sending before_ms.

// chromeMenuSource locates the repository root from this file's
// compile-time source path (the same self-location rule the web test suite
// applies; test overlays keep the physical path, so the walk finds the
// root's go.mod) and reads the dashboard's chrome.js from it.
func chromeMenuSource(t *testing.T) []byte {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok || file == "" {
		t.Skip("cannot locate own source path (trimpath build) - chrome.js menu tokens unchecked")
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
			t.Skip("no go.mod above " + file + " - chrome.js menu tokens unchecked")
		}
		dir = parent
	}
}

// menuDeclStatement extracts one `const NAME = ...;` statement's text.
func menuDeclStatement(t *testing.T, src []byte, name string) string {
	t.Helper()
	decl := "const " + name + " = "
	start := strings.Index(string(src), decl)
	if start < 0 {
		t.Fatalf("chrome.js missing const %s", name)
	}
	rest := string(src[start:])
	end := strings.Index(rest, ";\n")
	if end < 0 {
		t.Fatalf("chrome.js %s statement is not terminated", name)
	}
	return rest[:end]
}

// menuDurationTokens collects every duration token a menu statement offers:
// the first element of each ['token', 'label'] pair literal plus every
// durationPairs('a', 'b', ...) argument. Duplicates collapse; an empty
// statement fails loudly.
func menuDurationTokens(t *testing.T, stmt string) []string {
	t.Helper()
	seen := map[string]bool{}
	var toks []string
	add := func(tok string) {
		if !seen[tok] {
			seen[tok] = true
			toks = append(toks, tok)
		}
	}
	// Quote-style agnostic: a pair literal may widen to double quotes,
	// and a mixed widening must not let tokens slip past the pin.
	for _, m := range regexp.MustCompile(`\[['"]([^'"]*)["']`).FindAllStringSubmatch(stmt, -1) {
		add(m[1])
	}
	for _, m := range regexp.MustCompile(`durationPairs\(([^)]*)\)`).FindAllStringSubmatch(stmt, -1) {
		for _, q := range regexp.MustCompile(`['"]([^'"]*)["']`).FindAllStringSubmatch(m[1], -1) {
			add(q[1])
		}
	}
	if len(toks) == 0 {
		t.Fatalf("no duration tokens found in: %s", stmt)
	}
	return toks
}

// quoteJSON renders a duration token (or the empty string) as a JSON string.
func quoteJSON(tok string) string {
	if tok == "" {
		return `""`
	}
	return `"` + tok + `"`
}

// TestMenuDurationTokensAcceptEitherQuoteStyle pins the extractor's
// quote-agnosticism: chrome.js pair literals may widen to double quotes,
// and a mixed widening must not let tokens slip past the server-
// acceptance pins. Single- and double-quoted spellings of the same
// statement must extract identical token lists.
func TestMenuDurationTokensAcceptEitherQuoteStyle(t *testing.T) {
	single := "const X = [['', 'I resume'], durationPairs('15m', '1 hour')];"
	double := `const X = [["", "I resume"], durationPairs("15m", "1 hour")];`
	want := []string{"", "15m", "1 hour"}
	for name, stmt := range map[string]string{"single": single, "double": double} {
		got := menuDurationTokens(t, stmt)
		if len(got) != len(want) {
			t.Fatalf("%s-quoted statement extracted %v, want %v", name, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s-quoted statement extracted %v, want %v", name, got, want)
			}
		}
	}
}

// TestPauseDebugMenuDurationsAreServerAccepted posts every PAUSE_DURS token
// through the real pause and debug handlers. DEBUG_DURS must stay the
// derived list (PAUSE_DURS minus its resume pair), so the pause acceptance
// covers it; an own token list would need its own acceptance rows and
// reddens until it gets them.
func TestPauseDebugMenuDurationsAreServerAccepted(t *testing.T) {
	js := chromeMenuSource(t)
	toks := menuDurationTokens(t, menuDeclStatement(t, js, "PAUSE_DURS"))
	debugStmt := menuDeclStatement(t, js, "DEBUG_DURS")
	if !strings.Contains(debugStmt, "PAUSE_DURS.slice(1)") {
		t.Fatalf("DEBUG_DURS no longer derives from PAUSE_DURS; give any own token its own acceptance rows: %s", debugStmt)
	}
	for _, tok := range toks {
		p := New(config.Default(), metrics.Noop{})
		body := `{"paused":true,"clients":["client-a"],"duration":` + quoteJSON(tok) + `}`
		rec := httptest.NewRecorder()
		p.HandlePause(rec, httptest.NewRequest(http.MethodPost, "/admin/pause", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Errorf("pause duration %q = %d %s, want 200", tok, rec.Code, rec.Body.Bytes())
			continue
		}
		p.removePause(pauseJSON(t, rec)["holds"].([]any)[0].(map[string]any)["id"].(string), time.Time{})

		body = `{"enabled":true,"clients":["client-a"],"duration":` + quoteJSON(tok) + `}`
		rec = httptest.NewRecorder()
		p.HandleDebug(rec, httptest.NewRequest(http.MethodPost, "/admin/debug", strings.NewReader(body)))
		if rec.Code != 200 {
			t.Errorf("debug duration %q = %d %s, want 200", tok, rec.Code, rec.Body.Bytes())
		}
	}
	// A token outside the vocabulary is denied (deny by default): the
	// shared allowlist, not ParseDuration, decides.
	p := New(config.Default(), metrics.Noop{})
	rec := httptest.NewRecorder()
	p.HandlePause(rec, httptest.NewRequest(http.MethodPost, "/admin/pause",
		strings.NewReader(`{"paused":true,"clients":["client-a"],"duration":"30s"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("pause duration 30s = %d, want 400 (not on the menu, not accepted)", rec.Code)
	}
}

// TestLimitWindowMenuTokensAreServerAccepted parses every LIMIT_WINDOWS
// token through the limits owner; a menu window outside the server's
// accepted range reddens.
func TestLimitWindowMenuTokensAreServerAccepted(t *testing.T) {
	toks := menuDurationTokens(t, menuDeclStatement(t, chromeMenuSource(t), "LIMIT_WINDOWS"))
	if len(toks) == 0 {
		t.Fatal("LIMIT_WINDOWS offers no tokens")
	}
	for _, tok := range toks {
		d, err := parseLimitWindow(tok)
		if err != nil {
			t.Errorf("parseLimitWindow(%q): %v, want the Limits menu token accepted", tok, err)
			continue
		}
		if d < minLimitWindow || d > maxLimitWindow {
			t.Errorf("parseLimitWindow(%q) = %v, outside the accepted range", tok, d)
		}
	}
	// Out-of-range windows stay denied: the menu's largest token is 24h, the
	// server's upper bound is the same 24h, and anything beyond is rejected.
	for _, tok := range []string{"25h", "0s", "500ms"} {
		if _, err := parseLimitWindow(tok); err == nil {
			t.Errorf("parseLimitWindow(%q) accepted, want rejected (outside the window range)", tok)
		}
	}
}

// TestFilterAgeMenuTokensAreCanonicalGoDurations round-trips every FILTER_AGES
// token through ParseDuration and FormatDuration: the client parses the age
// with the Go duration grammar before sending before_ms, and a token that
// is not a canonical spelling (or parses to nothing) would silently widen
// or no-op the confirmed deletion preview.
func TestFilterAgeMenuTokensAreCanonicalGoDurations(t *testing.T) {
	toks := menuDurationTokens(t, menuDeclStatement(t, chromeMenuSource(t), "FILTER_AGES"))
	ages := 0
	for _, tok := range toks {
		if tok == "" {
			continue // the empty pair is "no age filter", not a duration
		}
		ages++
		d, err := time.ParseDuration(tok)
		if err != nil {
			t.Errorf("time.ParseDuration(%q): %v, want a Go duration literal", tok, err)
			continue
		}
		if got := config.FormatDuration(d); got != tok {
			t.Errorf("FormatDuration(%q) = %q, want the same canonical spelling", tok, got)
		}
	}
	if ages == 0 {
		t.Fatal("FILTER_AGES offers no age tokens")
	}
}
