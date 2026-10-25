package storage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/config"
)

// queryGoldenStatement exercises every SQLite value type the endpoint can
// return, plus the one shape a map rendering collapses and a column-order
// encoder would not: a repeated column name. The object keys are the distinct
// names in byte order and the duplicate keeps the LAST occurrence's value.
const queryGoldenStatement = `SELECT NULL AS null_col, 1 AS int_col,
	-9223372036854775808 AS min_int, 9223372036854775807 AS max_int,
	1.5 AS real_col, 0.000001 AS small_real, 1.0e300 AS big_real,
	-0.0 AS neg_zero, '' AS empty_text, 'plain' AS text_col,
	'<&>' AS html_text, 'café 漢字' AS unicode_text, 'a"b\c' AS quoted_text,
	x'00ff10' AS blob_col, 1 AS dup, 2 AS dup, 'last' AS dup`

// queryGolden is the endpoint's captured response for queryGoldenStatement.
// It was recorded from the pre-change implementation (base 0b28628, before the
// row-streaming rewrite) and must not move: keys sorted, HTML and Unicode
// escaping as encoding/json does it, blob base64, -0.0 rendered as -0, and one
// "dup" key carrying the last column's value.
const queryGolden = `[{"big_real":1e+300,"blob_col":"AP8Q","dup":"last","empty_text":"","html_text":"\u003c\u0026\u003e","int_col":1,"max_int":9223372036854775807,"min_int":-9223372036854775808,"neg_zero":-0,"null_col":null,"quoted_text":"a\"b\\c","real_col":1.5,"small_real":0.000001,"text_col":"plain","unicode_text":"café 漢字"}]`

// TestHandleQueryGoldenResponse pins the streamed endpoint body byte for byte
// over every SQLite type and the duplicate-name collapse, and asserts the
// retained-row rendering (Store.Query + json.Marshal) is the same bytes, so
// the two renderings cannot drift apart.
func TestHandleQueryGoldenResponse(t *testing.T) {
	s := openPurgeTestStore(t, testOpts)
	rows, err := s.Query(t.Context(), queryGoldenStatement)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	mapped, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	if string(mapped) != queryGolden {
		t.Fatalf("retained-row rendering changed:\n got: %s\nwant: %s", mapped, queryGolden)
	}
	w := serveQuery(t, s, queryGoldenStatement)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != queryGolden {
		t.Fatalf("streamed body changed:\n got: %s\nwant: %s", w.Body.String(), queryGolden)
	}
}

// TestHandleQueryDuplicateColumnByteCap pins the byte cap on the collapse a
// map rendering performs: the row's length is the distinct-key object, counted
// on the encoded stream. The statement repeats "v" with a discarded first value
// that is on its own as large as the whole cap, so a cap computed from every
// column would reject a body that fits.
func TestHandleQueryDuplicateColumnByteCap(t *testing.T) {
	opts := testOpts
	opts.QueryMaxBytes = config.StorageQueryMaxBytesMin
	s := openPurgeTestStore(t, opts)
	wantLimitBody := string(adminjson.ErrorBody(ErrQueryLimit.Error())) + "\n"

	// [{"v":""}] is ten bytes, so this padding puts the retained row exactly on
	// the byte cap; the discarded first column is itself cap-sized.
	pad := strings.Repeat("x", opts.QueryMaxBytes-10)
	want := fmt.Sprintf(`[{"v":"%s"}]`, pad)
	if len(want) != opts.QueryMaxBytes {
		t.Fatalf("fixture row is %d bytes, want %d", len(want), opts.QueryMaxBytes)
	}
	discarded := strings.Repeat("z", opts.QueryMaxBytes)
	statement := fmt.Sprintf(`SELECT '%s' AS v, '%s' AS v`, discarded, pad)
	rows, err := s.Query(t.Context(), statement)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	mapped, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	if string(mapped) != want {
		t.Fatalf("collapsed row is %d bytes, want %d", len(mapped), len(want))
	}
	w := serveQuery(t, s, statement)
	if w.Code != http.StatusOK || w.Body.String() != want {
		t.Fatalf("collapsed duplicate at the cap: status=%d body=%s", w.Code, w.Body.String())
	}

	// One byte over on the retained value is the clean 413, never a prefix.
	over := fmt.Sprintf(`SELECT 'short' AS v, '%s' AS v`, pad+"y")
	w = serveQuery(t, s, over)
	if w.Code != http.StatusRequestEntityTooLarge || w.Body.String() != wantLimitBody {
		t.Fatalf("one byte over: status=%d body=%q, want 413 %q", w.Code, w.Body.String(), wantLimitBody)
	}
}
