package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// menuDeclStatementErr extracts one `const NAME = ...;` statement's text.
// The cut at the first ";\n" stays the extraction boundary, and it is LOUD:
// every real chrome.js menu statement closes its last pair literal or call
// before that semicolon, so a cut that leaves any other trailing character
// truncated the statement (a `//` comment carrying a semicolon at
// end-of-line, or a ";\n" inside a backtick template literal) and fails
// here by name instead of silently dropping tokens. The residual blind spot
// is only that raw cut: covering it fully would need a real JS parser - out
// of scope for source pins.
func menuDeclStatementErr(src []byte, name string) (string, error) {
	decl := "const " + name + " = "
	start := strings.Index(string(src), decl)
	if start < 0 {
		return "", fmt.Errorf("chrome.js missing const %s", name)
	}
	rest := string(src[start:])
	end := strings.Index(rest, ";\n")
	if end < 0 {
		return "", fmt.Errorf("chrome.js %s statement is not terminated", name)
	}
	stmt := rest[:end]
	trimmed := strings.TrimRight(stmt, " \t\r\n")
	last := byte(0)
	if trimmed != "" {
		last = trimmed[len(trimmed)-1]
	}
	if last != ']' && last != ')' {
		return "", fmt.Errorf("chrome.js %s statement truncated at the first %q cut (comment or template literal carrying it?): %q", name, ";\n", stmt)
	}
	return stmt, nil
}

// menuDeclStatement is the test-facing wrapper around menuDeclStatementErr:
// any extraction failure is a loud test failure, never a silently narrowed
// statement (the menuDurationTokens wrapper's rule).
func menuDeclStatement(t *testing.T, src []byte, name string) string {
	t.Helper()
	stmt, err := menuDeclStatementErr(src, name)
	if err != nil {
		t.Fatal(err)
	}
	return stmt
}

// stripJSComments removes `//` line comments and `/* */` block comments from
// a menu statement before token scanning, writing one space per comment (a
// comment is a token separator in JS; tokens cannot fuse). String literals
// are skipped with the same scanner the token pass uses, so a delimiter
// inside a comment can never phantom-pair with a later token's delimiter,
// and a comment marker inside a label stays data. An unterminated block
// comment is a loud error - the extractor never guesses at a truncated
// statement.
func stripJSComments(stmt string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(stmt); {
		c := stmt[i]
		if isJSStringDelimiter(c) {
			_, after, ok := scanJSString(stmt, i)
			if !ok {
				return "", fmt.Errorf("unterminated string literal at offset %d in: %s", i, stmt)
			}
			b.WriteString(stmt[i:after])
			i = after
			continue
		}
		if c == '/' && i+1 < len(stmt) && stmt[i+1] == '/' {
			if j := strings.IndexByte(stmt[i:], '\n'); j >= 0 {
				b.WriteByte(' ')
				i += j + 1
			} else {
				i = len(stmt)
			}
			continue
		}
		if c == '/' && i+1 < len(stmt) && stmt[i+1] == '*' {
			j := strings.Index(stmt[i:], "*/")
			if j < 0 {
				return "", fmt.Errorf("unterminated block comment at offset %d in: %s", i, stmt)
			}
			b.WriteByte(' ')
			i += j + 2
			continue
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), nil
}

// isJSStringDelimiter reports whether c opens a JS string literal the menu
// statements can use: '...', "..." or `...` (a style widening to backticks
// must keep its tokens pinned, not let them slip past the acceptance checks).
func isJSStringDelimiter(c byte) bool {
	return c == '\'' || c == '"' || c == '`'
}

// scanJSString scans one JS string literal starting at the delimiter s[i].
// It returns the literal's raw content (source bytes between the delimiters,
// escape sequences kept verbatim - unescaping is not the extractor's job;
// tokens carrying escapes simply fail the acceptance surfaces loudly) and the
// index just past the closing delimiter. ok is false when the literal never
// terminates; callers fail loudly rather than guess at a truncated token.
func scanJSString(s string, i int) (content string, next int, ok bool) {
	q := s[i]
	var b strings.Builder
	for j := i + 1; j < len(s); j++ {
		c := s[j]
		if c == '\\' && j+1 < len(s) {
			b.WriteByte(c)
			b.WriteByte(s[j+1])
			j++
			continue
		}
		if c == q {
			return b.String(), j + 1, true
		}
		b.WriteByte(c)
	}
	return "", 0, false
}

// scanParenGroup scans a parenthesized group starting at s[i] == '(', skipping
// string literals so quotes and unbalanced ')' inside tokens cannot cut the
// group short. It returns the group's inner text and the index just past its
// matching ')'.
func scanParenGroup(s string, i int) (inner string, next int, err error) {
	depth := 0
	for j := i; j < len(s); j++ {
		c := s[j]
		if isJSStringDelimiter(c) {
			_, after, ok := scanJSString(s, j)
			if !ok {
				return "", 0, fmt.Errorf("unterminated string literal at offset %d in: %s", j, s)
			}
			j = after - 1
			continue
		}
		if c == '(' {
			depth++
			continue
		}
		if c == ')' {
			depth--
			if depth == 0 {
				return s[i+1 : j], j + 1, nil
			}
		}
	}
	return "", 0, fmt.Errorf("unterminated parenthesized group at offset %d in: %s", i, s)
}

// scanBracketGroup scans from just past a pair literal's first element through
// the pair's closing ']', skipping string literals so labels containing
// brackets or quotes cannot end the pair early.
func scanBracketGroup(s string, i int) (next int, err error) {
	depth := 0
	for j := i; j < len(s); j++ {
		c := s[j]
		if isJSStringDelimiter(c) {
			_, after, ok := scanJSString(s, j)
			if !ok {
				return 0, fmt.Errorf("unterminated string literal at offset %d in: %s", j, s)
			}
			j = after - 1
			continue
		}
		if c == '[' {
			depth++
			continue
		}
		if c == ']' {
			if depth == 0 {
				return j + 1, nil
			}
			depth--
		}
	}
	return 0, fmt.Errorf("unterminated pair literal at offset %d in: %s", i, s)
}

// menuDurationTokensErr collects every duration token a menu statement
// offers: the first element of each ['token', 'label'] pair literal plus
// every string literal inside a durationPairs(...) call, in statement order
// with duplicates collapsed. The whole statement is scanned by the escape-,
// quote- and bracket-aware primitives above, so a widened quote style, a
// backtick literal, an embedded quote or a paren inside a token either
// extracts exactly or reports an error - nothing truncates silently.
func menuDurationTokensErr(stmt string) ([]string, error) {
	stmt, err := stripJSComments(stmt)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var toks []string
	add := func(tok string) {
		if !seen[tok] {
			seen[tok] = true
			toks = append(toks, tok)
		}
	}
	skipSpace := func(s string, i int) int {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		return i
	}
	collect := func(s string) error {
		for i := 0; i < len(s); {
			if isJSStringDelimiter(s[i]) {
				content, next, ok := scanJSString(s, i)
				if !ok {
					return fmt.Errorf("unterminated string literal at offset %d in: %s", i, s)
				}
				add(content)
				i = next
				continue
			}
			i++
		}
		return nil
	}
	i := 0
	for i < len(stmt) {
		if strings.HasPrefix(stmt[i:], "durationPairs") {
			j := skipSpace(stmt, i+len("durationPairs"))
			if j < len(stmt) && stmt[j] == '(' {
				args, after, err := scanParenGroup(stmt, j)
				if err != nil {
					return nil, err
				}
				if err := collect(args); err != nil {
					return nil, err
				}
				i = after
				continue
			}
		}
		if stmt[i] == '[' {
			k := skipSpace(stmt, i+1)
			if k < len(stmt) && isJSStringDelimiter(stmt[k]) {
				content, after, ok := scanJSString(stmt, k)
				if !ok {
					return nil, fmt.Errorf("unterminated string literal at offset %d in: %s", k, stmt)
				}
				add(content)
				next, err := scanBracketGroup(stmt, after)
				if err != nil {
					return nil, err
				}
				i = next
				continue
			}
		}
		i++
	}
	if len(toks) == 0 {
		return nil, fmt.Errorf("no duration tokens found in: %s", stmt)
	}
	return toks, nil
}

// menuDurationTokens is the test-facing wrapper around menuDurationTokensErr:
// any extraction failure is a loud test failure, never a silently narrowed
// token set. An empty statement fails loudly.
func menuDurationTokens(t *testing.T, stmt string) []string {
	t.Helper()
	toks, err := menuDurationTokensErr(stmt)
	if err != nil {
		t.Fatal(err)
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
// robustness: single- and double-quoted spellings of the same statement
// must extract identical token lists, an embedded or escaped quote must not
// truncate its token into plausible fragments, backtick template literals
// extract exactly (a style widening stays pinned), and a paren inside a
// quoted token cannot cut the durationPairs group short. Malformed input
// fails loudly instead of extracting a truncated token.
func TestMenuDurationTokensAcceptEitherQuoteStyle(t *testing.T) {
	single := "const X = [['', 'I resume'], durationPairs('15m', '1 hour')];"
	double := `const X = [["", "I resume"], durationPairs("15m", "1 hour")];`
	rows := []struct {
		name string
		stmt string
		want []string
	}{
		{"single", single, []string{"", "15m", "1 hour"}},
		{"double", double, []string{"", "15m", "1 hour"}},
		{"embedded quote", `const X = durationPairs("15m", "worker's hour");`, []string{"15m", "worker's hour"}},
		{"escaped quote", "const X = durationPairs('15m', 'it\\'s an hour');", []string{"15m", `it\'s an hour`}},
		{"backticks", "const X = durationPairs(`15m`, `1 hour`);", []string{"15m", "1 hour"}},
		{"paren token", `const X = durationPairs('15m', '1 hour (and change)');`, []string{"15m", "1 hour (and change)"}},
		{"unbalanced paren token", `const X = durationPairs('15m', 'half ) hour');`, []string{"15m", "half ) hour"}},
		{"line comment skipped, not paired", "const X = durationPairs('15m', // it's a whole hour\n'1h');", []string{"15m", "1h"}},
		{"block comment skipped, not paired", `const X = durationPairs('15m', /* it's */ '1 hour');`, []string{"15m", "1 hour"}},
	}
	for _, row := range rows {
		got := menuDurationTokens(t, row.stmt)
		if !reflect.DeepEqual(got, row.want) {
			t.Errorf("%s: extracted %v, want %v", row.name, got, row.want)
		}
	}
	for _, bad := range []struct{ name, stmt string }{
		{"unterminated literal", `const X = durationPairs('15m', 'oops);`},
		{"unterminated group", `const X = durationPairs('15m', '1 hour'`},
		{"unterminated block comment", `const X = durationPairs('15m'/* never closes, '1 hour');`},
	} {
		if _, err := menuDurationTokensErr(bad.stmt); err == nil {
			t.Errorf("%s: extraction succeeded, want a loud failure", bad.name)
		}
	}
}

// TestMenuDeclStatementCutIsLoud pins the first ";\n" cut's shape check: the
// four real statements all close their last pair or call before that
// semicolon, so a statement a comment (or anything else) truncates fails by
// name instead of silently dropping its tokens with every acceptance pin
// green.
func TestMenuDeclStatementCutIsLoud(t *testing.T) {
	// (a) a `//` comment carrying ";" at end-of-line inside the statement
	// cuts it short; the truncated text fails the ]/)-shape check loudly.
	truncated := []byte("const X = durationPairs('15m',\n  // thirty;\n  '30m');\n")
	if _, err := menuDeclStatementErr(truncated, "X"); err == nil {
		t.Error("comment-semicolon truncation extracted, want a loud failure")
	}
	// Complete statements ending in `]` or `)` still extract exactly.
	for _, tc := range []struct{ name, src, want string }{
		{"pair list", "const X = [['', 'I resume']];\nnext();\n", "const X = [['', 'I resume']]"},
		{"call tail", "const X = [['a']].concat(P.slice(1));\n", "const X = [['a']].concat(P.slice(1))"},
	} {
		got, err := menuDeclStatementErr([]byte(tc.src), "X")
		if err != nil || got != tc.want {
			t.Errorf("%s: extracted (%q, %v), want %q", tc.name, got, err, tc.want)
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
