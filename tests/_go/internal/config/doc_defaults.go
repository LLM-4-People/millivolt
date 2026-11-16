package config_test

// Prose pages restate a few setting defaults for readability. Each of those is
// a second copy of a fact whose owner is Default(); this guard reads the claim
// next to the backticked setting key and compares it with the generated
// settings reference, which is itself byte-checked against the code. A default
// that changes now reddens the prose instead of leaving a stale number behind.
//
// The scan is deliberately narrow: it only considers a "default" claim that
// sits within one sentence-sized window after a setting key it can name. A
// number with no setting key is a different kind of fact and is not judged
// here.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/settingsdoc"
)

var (
	codeSpan     = regexp.MustCompile("`([^`\n]+)`")
	defaultWord  = regexp.MustCompile(`\bdefaults?\b`)
	claimAfter   = regexp.MustCompile(`^\s+(?:to\s+)?([0-9][0-9,._]*)\s*([A-Za-zµ%]*)`)
	claimBefore  = regexp.MustCompile(`([0-9][0-9,._]*)\s*([A-Za-zµ]*)\s+by\s*$`)
	claimWindow  = 300
	proseSources = []string{"README.md", "DESIGN.md", "SECURITY.md", "CONTRIBUTING.md", "OPERATIONS.md"}
)

type keySpan struct {
	start, end int
	key        string
}

// flatText collapses newlines to spaces and records the source line of every
// byte, so a claim wrapped across lines is still read as one sentence.
func flatText(text string) (string, []int) {
	flat := make([]byte, 0, len(text))
	lineAt := make([]int, 0, len(text))
	line := 1
	for index := 0; index < len(text); index++ {
		b := text[index]
		if b == '\n' {
			line++
			b = ' '
		}
		flat = append(flat, b)
		lineAt = append(lineAt, line)
	}
	return string(flat), lineAt
}

// stripFences blanks fenced code blocks without changing byte offsets. A
// fenced block can contain any number of backticks, and an unfiltered scan
// pairs them across the document, which moves every later code span.
func stripFences(text string) string {
	var out strings.Builder
	out.Grow(len(text))
	inFence := false
	for _, line := range strings.SplitAfter(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			line = blank(line)
		} else if inFence {
			line = blank(line)
		}
		out.WriteString(line)
	}
	return out.String()
}

func blank(line string) string {
	bytes := []byte(line)
	for index := range bytes {
		if bytes[index] != '\n' {
			bytes[index] = ' '
		}
	}
	return string(bytes)
}

func proseFiles(t *testing.T, root string) []string {
	t.Helper()
	files := make([]string, 0, len(proseSources)+8)
	for _, name := range proseSources {
		path := filepath.Join(root, name)
		if _, err := os.Stat(path); err == nil {
			files = append(files, path)
		}
	}
	docs, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatalf("list docs: %v", err)
	}
	for _, path := range docs {
		switch filepath.Base(path) {
		case "settings.md", "performance.md":
			continue // generated, and checked against their own owners
		}
		files = append(files, path)
	}
	return files
}

var (
	byteUnits = map[string]float64{
		"": 1, "B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9,
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30,
	}
	durationUnits = map[string]time.Duration{
		"": time.Second, "s": time.Second, "second": time.Second, "seconds": time.Second,
		"ms": time.Millisecond, "us": time.Microsecond, "µs": time.Microsecond,
		"ns": time.Nanosecond,
		"m":  time.Minute, "min": time.Minute, "minute": time.Minute, "minutes": time.Minute,
		"h": time.Hour, "hour": time.Hour, "hours": time.Hour,
	}
)

// unitValid reports whether a word after the number is a unit of this kind. A
// word that is not ("closes", "per") is prose, not a unit, and the number is
// read alone.
func unitValid(unit string, kind config.Kind) bool {
	switch kind {
	case config.KindBytes:
		_, ok := byteUnits[unit]
		return ok
	case config.KindDuration:
		_, ok := durationUnits[unit]
		return ok
	case config.KindInt:
		return unit == "" || unit == "%"
	}
	return false
}

func parseProseValue(raw, unit string, kind config.Kind) (int64, bool) {
	clean := strings.NewReplacer(",", "", "_", "").Replace(raw)
	number, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		return 0, false
	}
	if !unitValid(unit, kind) {
		unit = ""
	}
	switch kind {
	case config.KindBytes:
		scale, ok := byteUnits[unit]
		if !ok {
			return 0, false
		}
		return int64(number * scale), true
	case config.KindDuration:
		scale, ok := durationUnits[unit]
		if !ok {
			return 0, false
		}
		return int64(number * float64(scale)), true
	case config.KindInt:
		if !unitValid(unit, kind) {
			return 0, false
		}
		return int64(number), true
	}
	return 0, false
}

func TestProseDefaultsMatchTheSettingsReference(t *testing.T) {
	root := filepath.Join("..", "..")
	page, err := os.ReadFile(filepath.Join(root, settingsdoc.PagePath))
	if err != nil {
		t.Fatalf("read %s: %v; run go run ./cmd/docgen -write", settingsdoc.PagePath, err)
	}
	defaults := generatedDefaults(t, string(page))
	checked := 0

	for _, file := range proseFiles(t, root) {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		text, lineAt := flatText(stripFences(string(source)))
		spans := codeSpans(text, defaults)
		masked := maskSpans(text, spans)

		for _, word := range defaultWord.FindAllStringIndex(masked, -1) {
			number, unit, at, ok := claimAround(masked, word[0], word[1])
			if !ok {
				continue
			}
			span, ok := nearestKey(spans, at)
			if !ok {
				continue
			}
			field := config.FieldByKey(span.key)
			if field == nil {
				continue
			}
			wantText, ok := defaults[span.key]
			if !ok {
				t.Errorf("%s:%d: no generated default for %s", file, lineAt[at], span.key)
				continue
			}
			wantNumber, wantUnit := wantText, ""
			if field.Kind == config.KindBytes || field.Kind == config.KindDuration {
				wantNumber, wantUnit = splitUnit(wantText)
			}
			want, ok := parseProseValue(wantNumber, wantUnit, field.Kind)
			if !ok {
				t.Errorf("%s:%d: %s: the generated default %q is not readable", file, lineAt[at], span.key, wantText)
				continue
			}
			got, ok := parseProseValue(number, unit, field.Kind)
			if !ok {
				t.Errorf("%s:%d: %s: prose says %q %q, which is not a %s value",
					file, lineAt[at], span.key, number, unit, field.Kind)
				continue
			}
			if got != want {
				t.Errorf("%s:%d: %s: prose says %q %q (%d), generated default is %q (%d)",
					file, lineAt[at], span.key, number, unit, got, wantText, want)
			}
			checked++
			t.Logf("%s:%d: %s = %s %s", filepath.Base(file), lineAt[at], span.key, number, unit)
		}
	}
	if checked < 8 {
		t.Errorf("only %d prose defaults were checked; the scan no longer sees the documented claims", checked)
	}
}

// splitUnit separates a generated default like "256MiB" into its magnitude and
// unit, the same shape a prose claim carries.
func splitUnit(value string) (string, string) {
	index := 0
	for index < len(value) && (value[index] == '.' || value[index] == '-' || (value[index] >= '0' && value[index] <= '9')) {
		index++
	}
	return value[:index], value[index:]
}

func codeSpans(text string, defaults map[string]string) []keySpan {
	var spans []keySpan
	for _, match := range codeSpan.FindAllStringSubmatchIndex(text, -1) {
		key := text[match[2]:match[3]]
		if _, ok := defaults[key]; !ok {
			continue
		}
		spans = append(spans, keySpan{start: match[0], end: match[1], key: key})
	}
	return spans
}

func maskSpans(text string, spans []keySpan) string {
	masked := []byte(text)
	for _, span := range spans {
		for index := span.start; index < span.end; index++ {
			masked[index] = ' '
		}
	}
	return string(masked)
}

// claimAround reads the number a "default" word introduces, or the number that
// precedes "by default".
func claimAround(text string, start, end int) (string, string, int, bool) {
	after := text[end:]
	if len(after) > 60 {
		after = after[:60]
	}
	if match := claimAfter.FindStringSubmatch(after); match != nil {
		return match[1], match[2], start, true
	}
	from := start - 60
	if from < 0 {
		from = 0
	}
	if match := claimBefore.FindStringSubmatch(text[from:start]); match != nil {
		return match[1], match[2], start, true
	}
	return "", "", 0, false
}

// nearestKey is the last setting key named within one sentence of the claim.
// A key further back is a different sentence, and a key after the claim cannot
// own it.
func nearestKey(spans []keySpan, at int) (keySpan, bool) {
	var best keySpan
	found := false
	for _, span := range spans {
		if span.end > at || at-span.end > claimWindow {
			continue
		}
		if !found || span.end > best.end {
			best = span
			found = true
		}
	}
	return best, found
}
