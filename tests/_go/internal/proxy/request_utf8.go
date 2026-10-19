package proxy

// Raw invalid UTF-8 inside a JSON string is replaced with U+FFFD (three bytes
// per invalid byte) by encoding/json while decoding. A body-sized string could
// therefore triple in memory across the metadata decode and translation before
// any configured size bound saw the re-encoded result. This suite pins the
// string trust boundary: a syntactically valid request whose decoded strings
// outgrow the body by more than the small allowance is rejected with the
// shared 400 before any upstream send at bounded allocation, small invalid
// values still relay on the passthrough path, malformed JSON keeps its
// passthrough contract, and valid UTF-8 plus every JSON escape spelling still
// decodes and translates unchanged.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const (
	requestUTF8Prefix = `{"model":"m","messages":[{"role":"user","content":"`
	requestUTF8Suffix = `"}]}`
	// requestUTF8Size matches the confirmed amplification fixture: 8 MiB of
	// invalid bytes previously decoded to 25 MiB of U+FFFD and allocated about
	// 300 MiB across the metadata decode and the Anthropic translation.
	requestUTF8Size = 8 << 20
	// requestUTF8DecodeAllocMax bounds one metadata decode or translation of
	// the rejected body: it must fail before materializing the replacement
	// string, so only the error and its envelope remain.
	requestUTF8DecodeAllocMax = 1 << 20
	// requestUTF8HTTPAllocMax covers the one buffered request copy plus the
	// HTTP recorder and error envelope on the same rejected body.
	requestUTF8HTTPAllocMax = 32 << 20
)

func requestUTF8RawBody(size int) []byte {
	n := size - len(requestUTF8Prefix) - len(requestUTF8Suffix)
	if n < 0 {
		panic("body too small")
	}
	return []byte(requestUTF8Prefix + strings.Repeat("\xff", n) + requestUTF8Suffix)
}

// requestUTF8MalformedBody is the same string with the closing object members
// missing: valid JSON syntax is never reached, so the body must relay through
// the nontranslated path unchanged.
func requestUTF8MalformedBody(size int) []byte {
	n := size - len(requestUTF8Prefix) - 1
	if n < 0 {
		panic("body too small")
	}
	return []byte(requestUTF8Prefix + strings.Repeat("\xff", n) + `"`)
}

func requestUTF8HTTPRequest(body []byte, format string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer fixture-key")
	r.Header.Set("X-Proxy-Base-URL", "http://provider.example")
	if format != "" {
		r.Header.Set("X-Proxy-Format", format)
		r.Header.Set("X-Proxy-Path", "/messages")
	}
	return r
}

func TestRequestExpansionRejectedBeforeTypedDecode(t *testing.T) {
	body := requestUTF8RawBody(requestUTF8Size)
	if !json.Valid(body) {
		t.Fatal("fixture is not valid JSON under encoding/json")
	}
	var rec metrics.Record
	var err error
	alloc := proxyContentAllocDelta(func() {
		err = parseLLMRequest(body, &rec, false)
	})
	if err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("parseLLMRequest error = %v, want the expansion rejection", err)
	}
	// The rejection owns the whole document: no model or turn metadata from a
	// partially decoded body may reach the record.
	if rec.Model != "" || rec.TurnsUser != 0 || rec.CharsUser != 0 {
		t.Fatalf("rejected body retained metadata: %+v", rec)
	}
	if alloc > requestUTF8DecodeAllocMax {
		t.Fatalf("%d-byte invalid-UTF-8 body allocated %d bytes, want <= %d before the U+FFFD expansion",
			len(body), alloc, requestUTF8DecodeAllocMax)
	}
	t.Logf("invalid UTF-8 metadata decode: input=%d alloc=%d", len(body), alloc)
}

func TestRequestStringExpansionRejectedInEveryStringField(t *testing.T) {
	const size = 1 << 20
	filler := strings.Repeat("\xff", size)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"message content", requestUTF8Prefix + filler + requestUTF8Suffix},
		{"model", `{"model":"` + filler + `","messages":[]}`},
		{"message role", `{"model":"m","messages":[{"role":"` + filler + `","content":"x"}]}`},
		{"content part text", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"` + filler + `"}]}]}`},
		{"top-level key", `{"model":"m","` + filler + `":1}`},
		{"stop string", `{"model":"m","stop":"` + filler + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			if !json.Valid(body) {
				t.Fatal("fixture is not valid JSON under encoding/json")
			}
			var rec metrics.Record
			err := parseLLMRequest(body, &rec, false)
			if err == nil || !strings.Contains(err.Error(), "UTF-8") {
				t.Fatalf("parseLLMRequest error = %v, want the expansion rejection", err)
			}
		})
	}
}

// TestRequestSmallInvalidUTF8WithinAllowanceAccepted keeps the documented
// drop-not-reject case: a small raw invalid value is not itself a client
// error, and a nontranslated body still relays byte-identically. The 4 KiB
// expansion allowance bounds this path.
func TestRequestSmallInvalidUTF8WithinAllowanceAccepted(t *testing.T) {
	body := []byte(requestUTF8Prefix + "a\xffb" + requestUTF8Suffix)
	if !json.Valid(body) {
		t.Fatal("fixture is not valid JSON under encoding/json")
	}
	var rec metrics.Record
	if err := parseLLMRequest(body, &rec, false); err != nil {
		t.Fatalf("small invalid value parse = %v, want accepted", err)
	}
	if want := len("a\ufffdb"); rec.CharsUser != want {
		t.Fatalf("CharsUser = %d, want the standard decoder's %d-byte replacement length", rec.CharsUser, want)
	}
	var previewRec metrics.Record
	if err := parseLLMRequest(body, &previewRec, true); err != nil {
		t.Fatalf("small invalid value preview parse = %v, want accepted", err)
	}
	if want := metrics.TruncatePreview("a\ufffdb"); previewRec.PromptPreview != want {
		t.Fatalf("PromptPreview = %q, want %q", previewRec.PromptPreview, want)
	}

	var calls atomic.Int32
	var seen []byte
	p := New(config.Default(), metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		seen, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"fixture"},"finish_reason":"stop"}]}`)),
			Request:    r,
		}, nil
	})})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, requestUTF8HTTPRequest(body, ""))
	if w.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status = %d calls = %d, want the passthrough request accepted once", w.Code, calls.Load())
	}
	if !bytes.Equal(seen, body) {
		t.Fatalf("small invalid value changed on the passthrough wire: got %d bytes, want %d", len(seen), len(body))
	}
}

// TestRequestStringExpansionAllowanceBoundary pins the headroom: each invalid
// byte costs two extra decoded bytes (one raw byte becomes a three-byte
// U+FFFD), so 1,024 invalid bytes stay inside the 4 KiB allowance while 4,096
// cross it even after the body's structural bytes provide a little slack.
func TestRequestStringExpansionAllowanceBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		invalid int
		wantErr bool
	}{
		{"inside the allowance", 1024, false},
		{"past the allowance", 4096, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(requestUTF8Prefix + strings.Repeat("\xff", tc.invalid) + requestUTF8Suffix)
			if !json.Valid(body) {
				t.Fatal("fixture is not valid JSON under encoding/json")
			}
			var rec metrics.Record
			err := parseLLMRequest(body, &rec, false)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseLLMRequest error = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "UTF-8") {
				t.Fatalf("expansion error = %v, want it to name UTF-8", err)
			}
		})
	}
}

// TestRequestSmallInvalidUTF8TranslatedRejected proves the translator's own
// boundary: a body small enough for the metadata allowance still fails before
// the typed Anthropic decode, so no U+FFFD replacement is materialized.
func TestRequestSmallInvalidUTF8TranslatedRejected(t *testing.T) {
	var calls atomic.Int32
	p := New(config.Default(), metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, io.ErrUnexpectedEOF
	})})
	body := []byte(requestUTF8Prefix + "a\xffb" + requestUTF8Suffix)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, requestUTF8HTTPRequest(body, "anthropic"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
	if !strings.Contains(w.Body.String(), "invalid_request_error") || !strings.Contains(w.Body.String(), "UTF-8") {
		t.Fatalf("response = %q, want the shared invalid_request_error naming UTF-8", w.Body.String())
	}
}

// TestRequestInvalidUTF8MalformedJSONPassesThrough keeps the documented
// passthrough contract: the UTF-8 preflight rejects only syntactically valid
// documents, so malformed bytes still relay unchanged and json.Unmarshal
// rejects their syntax before decoding any string.
func TestRequestInvalidUTF8MalformedJSONPassesThrough(t *testing.T) {
	body := requestUTF8MalformedBody(1 << 20)
	if json.Valid(body) {
		t.Fatal("fixture is unexpectedly valid JSON")
	}
	var rec metrics.Record
	if err := parseLLMRequest(body, &rec, false); err != nil {
		t.Fatalf("malformed body parse = %v, want passthrough", err)
	}

	var calls atomic.Int32
	var seen []byte
	p := New(config.Default(), metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		seen, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"fixture"},"finish_reason":"stop"}]}`)),
			Request:    r,
		}, nil
	})})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, requestUTF8HTTPRequest(body, ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
	if !bytes.Equal(seen, body) {
		t.Fatalf("malformed passthrough body changed: got %d bytes, want %d", len(seen), len(body))
	}
}

func TestRequestInvalidUTF8Returns400BeforeUpstream(t *testing.T) {
	for _, format := range []string{"", "anthropic"} {
		name := "passthrough"
		if format != "" {
			name = "translated"
		}
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			p := New(config.Default(), metrics.Noop{})
			p.client.Store(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, io.ErrUnexpectedEOF
			})})
			body := requestUTF8RawBody(requestUTF8Size)
			w := httptest.NewRecorder()
			var alloc uint64
			alloc = proxyContentAllocDelta(func() {
				p.ServeHTTP(w, requestUTF8HTTPRequest(body, format))
			})
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", w.Code, w.Body.String())
			}
			if calls.Load() != 0 {
				t.Fatalf("upstream calls = %d, want 0", calls.Load())
			}
			if !strings.Contains(w.Body.String(), "invalid_request_error") || !strings.Contains(w.Body.String(), "UTF-8") {
				t.Fatalf("response = %q, want the shared invalid_request_error naming UTF-8", w.Body.String())
			}
			if alloc > requestUTF8HTTPAllocMax {
				t.Fatalf("%d-byte invalid-UTF-8 request allocated %d bytes, want <= %d", len(body), alloc, requestUTF8HTTPAllocMax)
			}
			t.Logf("%s invalid UTF-8: input=%d alloc=%d", name, len(body), alloc)
		})
	}
}

// FuzzDecodedJSONStringLength pins the counting helper's two safety
// invariants: it never undercounts the standard decoder's UTF-8 output (an
// undercount would let replacement expansion past the allowance), and it never
// counts more bytes than the raw spelling (an overcount would reject a valid
// request). Escapes are the interesting half: \uXXXX is bounded at three bytes
// rather than decoded exactly.
func FuzzDecodedJSONStringLength(f *testing.F) {
	for _, seed := range []string{"", "plain", "hé😀", "line\nquote\"slash\\", "\u2028\u2029", "\ufffd"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, s string) {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatal(err)
		}
		got, ok := decodedJSONStringLength(raw)
		if !ok {
			t.Fatalf("raw=%q: length helper rejected its own marshal", raw)
		}
		if got < len(decoded) {
			t.Fatalf("raw=%q: counted %d bytes, standard decoder produced %d", raw, got, len(decoded))
		}
		if got > len(raw) {
			t.Fatalf("raw=%q: counted %d bytes, more than the %d-byte spelling", raw, got, len(raw))
		}
	})
}

// FuzzRequestStringExpansion guards the preflight's no-false-positive
// invariant: a syntactically valid document whose bytes are valid UTF-8 can
// never exceed the decoded-string allowance, because escapes and multi-byte
// runes decode to no more bytes than their spelling.
func FuzzRequestStringExpansion(f *testing.F) {
	for _, seed := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[{"role":"user","content":"hé😀\u2028\ud83d\ude00"}]}`,
		"{\"model\":\"m\",\"messages\":[{\"role\":\"user\",\"content\":\"a\xffb\"}]}",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, body string) {
		if !utf8.ValidString(body) {
			return
		}
		if requestStringExpansionExceeded([]byte(body)) {
			t.Fatalf("valid UTF-8 document rejected by the expansion preflight: %q", body)
		}
	})
}

// TestRequestValidUnicodeAndEscapesAccepted is the control: valid raw UTF-8,
// every JSON escape spelling, escaped lone surrogates, and line separators all
// decode exactly as encoding/json does and still translate unchanged.
func TestRequestValidUnicodeAndEscapesAccepted(t *testing.T) {
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
		{"raw umlaut and four-byte rune", `"grüße 🌍"`, "grüße 🌍"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"m","messages":[{"role":"user","content":` + tc.raw + `}]}`)
			if !json.Valid(body) {
				t.Fatal("fixture is not valid JSON")
			}
			var rec metrics.Record
			if err := parseLLMRequest(body, &rec, true); err != nil {
				t.Fatalf("parseLLMRequest error = %v, want accepted", err)
			}
			if rec.CharsUser != len(tc.want) || rec.PromptPreview != tc.want {
				t.Fatalf("metadata = chars %d preview %q, want %d/%q", rec.CharsUser, rec.PromptPreview, len(tc.want), tc.want)
			}
			out, err := translateRequest("anthropic", body, 64, "")
			if err != nil {
				t.Fatalf("translateRequest error = %v, want accepted", err)
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
