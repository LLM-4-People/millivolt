package format

// The translator is the second string trust boundary. Even when the proxy's
// metadata preflight has already rejected raw invalid UTF-8, TranslateRequest
// itself must fail before encoding/json replaces each invalid byte with the
// three-byte U+FFFD and materializes a body-sized replacement string. Valid
// UTF-8, JSON escapes, and escaped lone surrogates remain accepted and keep
// their decoded meaning.

import (
	"encoding/json"
	"strings"
	"testing"
)

const formatUTF8DecodeAllocMax = 1 << 20

func TestTranslateRequestRejectsInvalidUTF8BeforeDecode(t *testing.T) {
	const prefix = `{"model":"m","messages":[{"role":"user","content":"`
	const suffix = `"}]}`
	const size = 8 << 20
	body := []byte(prefix + strings.Repeat("\xff", size-len(prefix)-len(suffix)) + suffix)
	if !json.Valid(body) {
		t.Fatal("fixture is not valid JSON under encoding/json")
	}
	var out []byte
	var err error
	alloc := allocDelta(func() {
		out, err = TranslateRequest(body, 64, "")
	})
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("TranslateRequest error = %v, want the invalid-UTF-8 rejection", err)
	}
	if out != nil {
		t.Fatalf("TranslateRequest output = %d bytes, want fail-closed nil", len(out))
	}
	if alloc > formatUTF8DecodeAllocMax {
		t.Fatalf("%d-byte invalid-UTF-8 body allocated %d bytes, want <= %d before the U+FFFD expansion",
			len(body), alloc, formatUTF8DecodeAllocMax)
	}
	t.Logf("invalid UTF-8 translation: input=%d alloc=%d", len(body), alloc)
}

func TestTranslateRequestValidUnicodeControl(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{"raw unicode", `"héllo 😀"`, "héllo 😀"},
		{"json escapes", `"line\nquote\"slash\\tab\t"`, "line\nquote\"slash\\tab\t"},
		{"escaped code points", `"\u0061\u00e9\ud83d\ude00"`, "aé😀"},
		{"escaped lone surrogate", `"a\ud800b"`, "a\ufffdb"},
		{"line separators", `"\u2028\u2029"`, "\u2028\u2029"},
		{"raw line separators", "\"\u2028\u2029\"", "\u2028\u2029"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","messages":[{"role":"user","content":` + tc.raw + `}]}`)
			out, err := TranslateRequest(body, 64, "")
			if err != nil {
				t.Fatalf("TranslateRequest error = %v, want accepted", err)
			}
			var got struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("translated body is not valid JSON: %v", err)
			}
			if len(got.Messages) != 1 || got.Messages[0].Content != tc.want {
				t.Fatalf("translated content = %#v, want %q", got.Messages, tc.want)
			}
		})
	}
}
