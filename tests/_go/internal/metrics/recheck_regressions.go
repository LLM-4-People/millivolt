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
	for _, key := range []string{"choices", "Choices", "CHOICES", `\u0063hoices`, `\u0043hoices`} {
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
	for _, key := range []string{"choices", "Choices", "CHOICES", `\u0063hoices`} {
		body := []byte(fmt.Sprintf(`{"%s":[]}`, key))
		if !containsChoicesKeyToken(body) {
			t.Fatalf("%q did not trigger the choices traversal", key)
		}
	}
}

// legacyChoicesKeyGate is the pre-fast-path gate: a byte-by-byte quote scan
// with bytes.EqualFold. The benchmark keeps it beside the production gate so
// the exact-token path can be compared fairly on the same fixtures.
func legacyChoicesKeyGate(body []byte) bool {
	const token = `choices`
	if bytes.Contains(body, []byte(`\u`)) {
		return true
	}
	for i := 0; i+len(token)+2 <= len(body); i++ {
		if body[i] != '"' || body[i+len(token)+1] != '"' {
			continue
		}
		if bytes.EqualFold(body[i+1:i+len(token)+1], []byte(token)) {
			return true
		}
	}
	return false
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
	prefix := strings.Repeat("x", 64<<10)
	bodies := []struct {
		name string
		body []byte
	}{
		{"exact", []byte(`{"prefix":"` + prefix + `","choices":[]}`)},
		{"mixed", []byte(`{"prefix":"` + prefix + `","CHOICES":[]}`)},
		{"absent", []byte(`{"prefix":"` + prefix + `","other":[]}`)},
	}
	for _, tc := range bodies {
		benchmarkChoicesGate(b, tc.name+"/old", tc.body, legacyChoicesKeyGate)
		benchmarkChoicesGate(b, tc.name+"/new", tc.body, containsChoicesKeyToken)
	}
	_ = choicesGateSink
}
