package metrics

import (
	"bytes"
	"math/rand"
	"testing"
)

// skipJSONStringRef is the straightforward escape-aware byte loop: every byte
// is inspected in order and a backslash consumes the character after it. It is
// the oracle for the parity-counting scanner.
func skipJSONStringRef(data []byte, i int) int {
	if i >= len(data) || data[i] != '"' {
		return -1
	}
	i++
	for i < len(data) {
		switch data[i] {
		case '\\':
			i += 2
		case '"':
			return i + 1
		default:
			i++
		}
	}
	return -1
}

// TestSkipJSONStringEscapeParity pins that only an odd backslash run escapes a
// quote, so an even run terminates the string, at every position and at the
// unterminated boundary.
func TestSkipJSONStringEscapeParity(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"empty string", `""`, 2},
		{"plain", `"abc"`, 5},
		{"one backslash escapes", `"a\"b"`, 6},
		{"two backslashes terminate", `"a\\"`, 5},
		{"three backslashes escape", `"a\\\"b"`, 8},
		{"four backslashes terminate", `"a\\\\"`, 7},
		{"escape at the start", `"\"rest"`, 8},
		{"trailing backslash run escapes", `"a\` + `"`, -1},
		{"trailing odd run unterminated", `"a\`, -1},
		{"quote only", `"`, -1},
		{"not a string", `x`, -1},
		{"out of range", ``, -1},
		{"control char before quote", "\"a\tb\"", 5},
		{"mid offset", `xx"a\"b"`, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := 0
			if tc.name == "mid offset" {
				start = 2
			}
			if got := SkipJSONString([]byte(tc.body), start); got != tc.want {
				t.Fatalf("SkipJSONString(%q, %d) = %d, want %d", tc.body, start, got, tc.want)
			}
			if got := skipJSONStringRef([]byte(tc.body), start); got != tc.want {
				t.Fatalf("reference(%q, %d) = %d, want %d", tc.body, start, got, tc.want)
			}
		})
	}
}

// FuzzSkipJSONStringEquivalence checks the parity scanner against the byte-loop
// oracle over generated strings: backslashes, quotes, a closing quote and the
// unterminated tail all appear.
func FuzzSkipJSONStringEquivalence(f *testing.F) {
	for _, seed := range []string{
		`""`, `"a\"b"`, `"a\\"`, `"a\\\"b"`, `"a\\\\"`, `"a\`, `"\`, `"`,
		`"text\n text\\ text\" end"`, `"\\\\\\\\\"`, `"\\\"\\\"\\\"`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 1<<16 {
			t.Skip()
		}
		for start := 0; start < len(body) && start < 3; start++ {
			if got, want := SkipJSONString(body, start), skipJSONStringRef(body, start); got != want {
				t.Fatalf("SkipJSONString(%q, %d) = %d, reference = %d", body, start, got, want)
			}
		}
	})
}

// TestSkipJSONStringRandomizedEquivalence sweeps generated bodies so the fuzz
// function has a deterministic companion in the plain test run.
func TestSkipJSONStringRandomizedEquivalence(t *testing.T) {
	const alphabet = `"\\ab`
	rng := rand.New(rand.NewSource(7))
	for iteration := 0; iteration < 2000; iteration++ {
		n := rng.Intn(24)
		body := make([]byte, 0, n+1)
		body = append(body, '"')
		for range n {
			body = append(body, alphabet[rng.Intn(len(alphabet))])
		}
		if got, want := SkipJSONString(body, 0), skipJSONStringRef(body, 0); got != want {
			t.Fatalf("SkipJSONString(%q) = %d, reference = %d", body, got, want)
		}
	}
}

// TestSkipJSONStringOnePass pins the scan shape: escaped content must not
// restart from the string start, so the cost stays close to the plain scan.
func TestSkipJSONStringOnePass(t *testing.T) {
	escaped := append([]byte(`"`), bytes.Repeat([]byte(`a\n`), 200_000)...)
	escaped = append(escaped, '"')
	if got := SkipJSONString(escaped, 0); got != len(escaped) {
		t.Fatalf("SkipJSONString escaped = %d, want %d", got, len(escaped))
	}
}
