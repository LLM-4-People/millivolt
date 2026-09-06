package adminjson

import (
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeStrictOperatorObject(t *testing.T) {
	type input struct {
		Paused *bool             `json:"paused"`
		Labels map[string]string `json:"labels"`
	}
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"object", `{"paused":true}`, true},
		{"whitespace around object", " \n{\"paused\":false}\t", true},
		{"non-JSON leading whitespace", "\v{\"paused\":false}", false},
		{"non-JSON trailing whitespace", "{\"paused\":false}\u00a0", false},
		{"map keys preserve case", `{"labels":{"A":"one","a":"two"}}`, true},
		{"unknown field", `{"pause":true}`, false},
		{"duplicate", `{"paused":true,"paused":false}`, false},
		{"duplicate alias", `{"paused":true,"PAUSED":false}`, false},
		{"escaped duplicate", `{"paused":true,"\u0070aused":false}`, false},
		{"nested duplicate", `{"labels":{"a":"one","a":"two"}}`, false},
		{"trailing object", `{"paused":true}{"paused":false}`, false},
		{"trailing garbage", `{"paused":true}oops`, false},
		{"null", `null`, false},
		{"array", `[]`, false},
		{"whitespace only", " \n\t", false},
		{"malformed", `{"paused":`, false},
		{"wrong type", `{"paused":"yes"}`, false},
		{"oversized", `{"labels":{"a":"` + strings.Repeat("x", maxBodyBytes) + `"}}`, false},
		{"nested too deeply", `{"labels":` + strings.Repeat("[", maxDepth+1) + `0` + strings.Repeat("]", maxDepth+1) + `}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/admin/pause", strings.NewReader(tc.raw))
			// Content-Length is not trusted to distinguish empty commands or
			// bound a body: chunked requests have unknown lengths.
			r.ContentLength = -1
			var dst input
			err := Decode(httptest.NewRecorder(), r, &dst)
			if (err == nil) != tc.valid {
				t.Fatalf("Decode() error=%v, valid=%v", err, tc.valid)
			}
			if !tc.valid && errors.Is(err, io.EOF) {
				t.Fatal("nonempty body must not become an empty-body command")
			}
		})
	}
}

type failingBodyReader struct{ data string }

func (r failingBodyReader) Read(p []byte) (int, error) {
	return copy(p, r.data), fmt.Errorf("transport failure: %w", io.EOF)
}

func TestDecodeReadFailureCannotBecomeEmptyCommand(t *testing.T) {
	for _, data := range []string{"", "{", "{}"} {
		r := httptest.NewRequest("POST", "/admin/purge", nil)
		r.Body = io.NopCloser(failingBodyReader{data: data})
		var dst map[string]any
		err := Decode(httptest.NewRecorder(), r, &dst)
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("failed read with body %q became empty command: %v", data, err)
		}
	}
}

func TestOptionalID(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		valid     bool
	}{
		{"", "", true}, {`"scope-id"`, "scope-id", true}, {`" scope-id "`, "scope-id", true},
		{`null`, "", false}, {`""`, "", false}, {`" \t "`, "", false},
		{`0`, "", false}, {`[]`, "", false}, {`{}`, "", false},
	} {
		got, err := OptionalID([]byte(tc.raw))
		if (err == nil) != tc.valid || got != tc.want {
			t.Fatalf("raw=%s got=%q err=%v", tc.raw, got, err)
		}
	}
}

func TestDecodeEmptyCommand(t *testing.T) {
	for _, body := range []io.Reader{nil, strings.NewReader("")} {
		r := httptest.NewRequest("POST", "/admin/purge", body)
		r.ContentLength = -1
		var dst map[string]any
		if err := Decode(httptest.NewRecorder(), r, &dst); err != io.EOF {
			t.Fatalf("empty command error=%v", err)
		}
	}
}

// A malformed nonempty document must never become the empty-body full-wipe
// command. This property applies to every parser error path, not just seeds.
func FuzzDecodeEmptyBoundary(f *testing.F) {
	for _, raw := range []string{"", " ", `{`, `{"a":`, `{"a":[`, `null`, `{}`, `{} {}`} {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		r := httptest.NewRequest("POST", "/admin/purge", strings.NewReader(raw))
		r.ContentLength = -1
		var dst map[string]any
		err := Decode(httptest.NewRecorder(), r, &dst)
		if errors.Is(err, io.EOF) != (raw == "") {
			t.Fatalf("body %q: ambiguous empty-body error %v", raw, err)
		}
	})
}
