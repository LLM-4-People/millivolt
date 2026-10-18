package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
)

// queryStreamTransport is the fake proxy for the streaming query tests: it has
// no network hop and answers every request from one prepared body, so an
// allocation assertion observes the client's decode rather than an HTTP
// server's own buffering. open supplies a fresh reader per request when a test
// needs a stream rather than a fixed body.
type queryStreamTransport struct {
	body  []byte
	open  func() io.Reader
	calls int
	last  seenRequest
}

func (t *queryStreamTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls++
	t.last = seenRequest{
		Method: request.Method,
		Path:   request.URL.Path,
		Query:  request.URL.Query(),
		Auth:   request.Header.Get("Authorization"),
	}
	var body io.Reader
	if t.open != nil {
		body = t.open()
	} else {
		body = bytes.NewReader(t.body)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(body),
	}, nil
}

// queryStreamLimits pins the caps these tests exercise: the row cap is the
// binding limit and the byte budget is wide enough for all 200 kept rows, so a
// change to the shipped defaults cannot turn the row-cap test into a byte-cap
// test.
func queryStreamLimits() Limits {
	limits := defaultTestLimits()
	limits.QueryMaxRows = 200
	limits.QueryMaxBytes = 1 << 20
	return limits
}

// queryStreamBody builds the bare JSON array /metrics/query serves. blob is the
// byte length of one filler field, so a test can make the body large enough
// that materializing every row map is unmistakable.
func queryStreamBody(rows, blob int) []byte {
	var body strings.Builder
	filler := strings.Repeat("x", blob)
	body.WriteByte('[')
	for i := range rows {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `{"id":"r%05d","seq":%d,"blob":"%s"}`, i, i, filler)
	}
	body.WriteByte(']')
	return []byte(body.String())
}

// TestQueryStreamsBeyondTheCapWithoutRetainingWithheldRows pins the bounded
// streaming read: a 5,000-row proxy answer is counted in full, but only the
// 200 rows inside the configured row cap become maps. The allocation bound is
// the observation, not a timer: the previous full-array decode copied the
// whole body through readBody and then materialized all 5,000 row maps, so it
// could not stay under the body's own size. The stream decodes withheld rows
// into a throwaway struct and never retains them.
func TestQueryStreamsBeyondTheCapWithoutRetainingWithheldRows(t *testing.T) {
	const (
		rows = 5000
		blob = 340
	)
	body := queryStreamBody(rows, blob)
	limits := queryStreamLimits()
	transport := &queryStreamTransport{body: body}
	service, err := NewServiceWithTransport(InProcessOrigin, testToken, limits, transport)
	if err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out, err := service.query(context.Background(), QueryInput{SQL: "SELECT id, seq, blob FROM requests"})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}

	if transport.calls != 1 {
		t.Fatalf("query made %d requests, want exactly one", transport.calls)
	}
	assertAuth(t, transport.last)
	assertQuery(t, transport.last, "q", "SELECT id, seq, blob FROM requests")
	if transport.last.Method != http.MethodGet || transport.last.Path != schemaPath {
		t.Fatalf("query sent %s %s, want GET %s", transport.last.Method, transport.last.Path, schemaPath)
	}

	// Every element is counted for the total, even though the kept prefix ends
	// at the row cap, so a partial answer can never read as complete.
	if !out.Truncation.Truncated || out.Truncation.Shown != 200 || out.Truncation.Total != rows {
		t.Fatalf("truncation = %+v, want the first 200 of the proxy's %d rows", out.Truncation, rows)
	}
	if out.RowCount != 200 || len(out.Rows) != 200 {
		t.Fatalf("row_count = %d, rows = %d, want the exact row cap", out.RowCount, len(out.Rows))
	}
	if !strings.Contains(out.Truncation.Marker, "200 of 5000") {
		t.Fatalf("marker = %q, must name the kept and produced rows", out.Truncation.Marker)
	}

	// The kept rows are the source-order prefix with their full values, not a
	// sample and not a reordered set.
	wantBlob := strings.Repeat("x", blob)
	for i, row := range out.Rows {
		if wantID := fmt.Sprintf("r%05d", i); row["id"] != wantID {
			t.Fatalf("rows[%d].id = %v, want %q in source order", i, row["id"], wantID)
		}
		if row["seq"] != float64(i) {
			t.Fatalf("rows[%d].seq = %v, want %d", i, row["seq"], i)
		}
		if row["blob"] != wantBlob {
			t.Fatalf("rows[%d].blob = %v, want the full %d-byte source value", i, row["blob"], blob)
		}
	}

	// Bytes stays the exact sum of the kept rows' encoded lengths, the same
	// number the materializing byte clamp reported.
	wantBytes := 0
	for _, row := range out.Rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		wantBytes += len(encoded)
	}
	if out.Bytes != wantBytes {
		t.Fatalf("bytes = %d, want %d, the encoded size of the 200 kept rows", out.Bytes, wantBytes)
	}
	if wantBytes > limits.QueryMaxBytes {
		t.Fatalf("the row cap must bind before the byte cap: %d kept bytes", wantBytes)
	}

	// A full decode allocates the body copy plus a map per element. The
	// streaming reader never holds the body and drops each withheld row, so
	// its whole-query allocation stays below the body size even with the kept
	// rows retained. This is what "withheld rows are not retained" means here.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated >= uint64(len(body)) {
		t.Fatalf("query allocated %d bytes for a %d byte, %d row answer; the full decode is back",
			allocated, len(body), rows)
	}
}

// TestQueryStreamStopsAtTheFirstRowAboveTheByteBudget pins the whole-row
// prefix rule the old byte clamp had: the first row that would exceed the
// budget ends the result, so a smaller row later in the array is withheld
// rather than resurrected around the overflow.
func TestQueryStreamStopsAtTheFirstRowAboveTheByteBudget(t *testing.T) {
	body := []byte(`[{"id":"ra","blob":"` + strings.Repeat("x", 10) + `"},` +
		`{"id":"rb","blob":"` + strings.Repeat("y", 500) + `"},` +
		`{"id":"rc"}]`)
	limits := queryStreamLimits()
	limits.QueryMaxBytes = 150
	service, err := NewServiceWithTransport(InProcessOrigin, testToken, limits, &queryStreamTransport{body: body})
	if err != nil {
		t.Fatal(err)
	}

	out, err := service.query(context.Background(), QueryInput{SQL: "SELECT id, blob FROM requests"})
	if err != nil {
		t.Fatal(err)
	}
	if out.RowCount != 1 || out.Truncation.Total != 3 || !out.Truncation.Truncated {
		t.Fatalf("row_count = %d truncation = %+v, want the first row of three", out.RowCount, out.Truncation)
	}
	if out.Rows[0]["id"] != "ra" {
		t.Fatalf("kept row = %v, want the source-order prefix row ra", out.Rows[0])
	}
	encoded, err := json.Marshal(out.Rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if out.Bytes != len(encoded) {
		t.Fatalf("bytes = %d, want the kept row's %d encoded bytes", out.Bytes, len(encoded))
	}
	// The marker names the proxy's total and the byte budget, and the tiny
	// later row did not survive its larger predecessor's overflow.
	for _, needle := range []string{"1 of 3 rows", "150 byte result budget"} {
		if !strings.Contains(out.Truncation.Marker, needle) {
			t.Fatalf("marker = %q, must name %q", out.Truncation.Marker, needle)
		}
	}
}

// TestQueryStreamEnforcesTheResponseReadBudget pins that the streaming reader
// keeps readBody's budget rule: the limit is on the whole body, not on the
// bytes the decoder happened to consume before the JSON ended. The body here
// is pure whitespace and exceeds the budget, so it must fail with the read
// budget message rather than a decode error.
func TestQueryStreamEnforcesTheResponseReadBudget(t *testing.T) {
	transport := &queryStreamTransport{open: func() io.Reader {
		return io.LimitReader(whitespaceReader{}, MaxResponseBytes+1)
	}}
	service, err := NewServiceWithTransport(InProcessOrigin, testToken, defaultTestLimits(), transport)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.query(context.Background(), QueryInput{SQL: "SELECT 1"})
	if err == nil {
		t.Fatal("an oversized query response must be an error")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d byte read budget", MaxResponseBytes)) {
		t.Fatalf("error = %v, want the read budget message", err)
	}
}

// whitespaceReader supplies JSON whitespace forever; the transport bounds it.
type whitespaceReader struct{}

func (whitespaceReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	return len(p), nil
}

// TestQueryStreamReportsABodyReadFailure pins that a failure mid-body stays
// readBody's read error rather than becoming a JSON decode error. The decoder
// sees the underlying error, so it must be preserved through the bounded
// reader's own record of it.
func TestQueryStreamReportsABodyReadFailure(t *testing.T) {
	transport := &queryStreamTransport{open: func() io.Reader { return &failingReader{} }}
	service, err := NewServiceWithTransport(InProcessOrigin, testToken, defaultTestLimits(), transport)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.query(context.Background(), QueryInput{SQL: "SELECT 1"})
	if err == nil {
		t.Fatal("a read failure must be an error")
	}
	if !strings.Contains(err.Error(), "read millivolt response") {
		t.Fatalf("error = %v, want the read failure", err)
	}
}

// failingReader yields the opening bracket and then fails, so the decoder is
// inside the array when the transport breaks.
type failingReader struct{ opened bool }

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.opened {
		r.opened = true
		return copy(p, "["), nil
	}
	return 0, errors.New("fixture read failure")
}

// BenchmarkQueryStreamFiveThousandRows measures one bounded query over the
// same 5,000-row answer: every row is counted, 200 are kept, and 4,800 are
// skipped without row maps. b.ReportAllocs makes the per-result memory cost
// visible against the old full-array decode.
func BenchmarkQueryStreamFiveThousandRows(b *testing.B) {
	body := queryStreamBody(5000, 340)
	transport := &queryStreamTransport{body: body}
	service, err := NewServiceWithTransport(InProcessOrigin, testToken, queryStreamLimits(), transport)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		out, err := service.query(ctx, QueryInput{SQL: "SELECT id, seq, blob FROM requests"})
		if err != nil {
			b.Fatal(err)
		}
		if out.RowCount != 200 || out.Truncation.Total != 5000 {
			b.Fatalf("row_count = %d total = %d, want 200 of 5000", out.RowCount, out.Truncation.Total)
		}
	}
}
