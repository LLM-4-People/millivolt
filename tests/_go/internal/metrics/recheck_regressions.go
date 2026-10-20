package metrics

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func choicesBody(key string, n int) []byte {
	var b strings.Builder
	b.WriteString(`{"id":"x","model":"m","`)
	b.WriteString(key)
	b.WriteString(`":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"finish_reason":""}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// TestNonStreamChoicesBoundMatchesTypedDecode pins PRO-1: every top-level key
// spelling encoding/json honors (ASCII case variants and Unicode escapes) trips
// the lexical bound before the typed decode materializes the array.
func TestNonStreamChoicesBoundMatchesTypedDecode(t *testing.T) {
	// encoding/json's name fold accepts raw U+017F and any \u escape. U+0130
	// does not match "choices" on this toolchain, so it has no positive case.
	for _, key := range []string{"choices", "Choices", "CHOICES", `\u0063hoices`, `\u0043hoices`, "choiceſ", `choice\u017F`} {
		t.Run(key, func(t *testing.T) {
			over := AnalyzeNonStreamBody(choicesBody(key, nonStreamChoicesMax+1), nil, nil, false)
			if over.ErrorType != nonStreamChoicesErrorType || over.RecordFieldsOK {
				t.Fatalf("over-bound %q = %+v, want %s with no record fields", key, over, nonStreamChoicesErrorType)
			}
			at := AnalyzeNonStreamBody(choicesBody(key, nonStreamChoicesMax), nil, nil, false)
			if at.ErrorType == nonStreamChoicesErrorType {
				t.Fatalf("at-bound %q was rejected as too large", key)
			}
			if !at.RecordFieldsOK {
				t.Fatalf("at-bound %q did not materialize its typed fields", key)
			}
		})
	}
}

// TestNonStreamChoicesBoundIsCaseInsensitiveScanOnly is the fast-path control:
// a body without any choices spelling stays on the negative path, while a
// differently-cased key still reaches the traversal.
func TestNonStreamChoicesBoundFastPath(t *testing.T) {
	if containsChoicesKeyToken([]byte(`{"other":[1,2,3]}`)) {
		t.Fatal("unrelated key triggered the choices traversal")
	}
	for _, key := range []string{"choices", "Choices", "CHOICES", `\u0063hoices`, "choiceſ", `choice\u017F`} {
		body := []byte(fmt.Sprintf(`{"%s":[]}`, key))
		if !containsChoicesKeyToken(body) {
			t.Fatalf("%q did not trigger the choices traversal", key)
		}
	}
	// U+0130 does not match "choices" under the current encoding/json fold, so
	// the gate does not need to force a traversal for a raw choİce spelling.
	if containsChoicesKeyToken([]byte(`{"choİce":[]}`)) {
		t.Fatal("raw U+0130 incorrectly forced the choices traversal")
	}
}

// parentChoicesKeyGate is the gate as shipped at 0cb4a0d, the immediate parent
// before the raw-Unicode fallback. The benchmark keeps that exact baseline
// beside production so the fixed-width scan cannot silently regress against it.
func parentChoicesKeyGate(body []byte) bool {
	const token = `choices`
	if bytes.Contains(body, []byte(`"choices"`)) || bytes.Contains(body, []byte(`\u`)) {
		return true
	}
	for offset := 0; offset < len(body); {
		quote := bytes.IndexByte(body[offset:], '"')
		if quote < 0 {
			return false
		}
		quote += offset
		closing := quote + len(token) + 1
		if closing < len(body) && parentChoicesKeyFold(body[quote+1:closing]) && body[closing] == '"' {
			return true
		}
		offset = quote + 1
	}
	return false
}

func parentChoicesKeyFold(candidate []byte) bool {
	const token = "choices"
	if len(candidate) != len(token) {
		return false
	}
	for i := range candidate {
		c := candidate[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != token[i] {
			return false
		}
	}
	return true
}

var choicesGateSink bool

func benchmarkChoicesGate(b *testing.B, name string, body []byte, gate func([]byte) bool) {
	b.Run(name, func(b *testing.B) {
		b.SetBytes(int64(len(body)))
		b.ReportAllocs()
		for b.Loop() {
			choicesGateSink = gate(body)
		}
	})
}

func BenchmarkChoicesKeyGate(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20} {
		prefix := strings.Repeat("x", size)
		bodies := []struct {
			name string
			body []byte
		}{
			{"exact", []byte(`{"prefix":"` + prefix + `","choices":[]}`)},
			{"mixed", []byte(`{"prefix":"` + prefix + `","CHOICES":[]}`)},
			{"absent", []byte(`{"prefix":"` + prefix + `","other":[]}`)},
		}
		for _, tc := range bodies {
			label := fmt.Sprintf("%dKiB/%s", size>>10, tc.name)
			benchmarkChoicesGate(b, label+"/parent", tc.body, parentChoicesKeyGate)
			benchmarkChoicesGate(b, label+"/new", tc.body, containsChoicesKeyToken)
		}
	}
	_ = choicesGateSink
}
