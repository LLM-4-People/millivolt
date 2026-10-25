package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const requestAllocPrefix = `{"model":"m","messages":[{"role":"user","content":"`
const requestAllocSuffix = `"}]}`

func requestAllocBody(size int) []byte {
	n := size - len(requestAllocPrefix) - len(requestAllocSuffix)
	if n < 0 {
		panic("body too small")
	}
	return []byte(requestAllocPrefix + strings.Repeat("x", n) + requestAllocSuffix)
}

// requestAllocPattern is a deterministic non-uniform byte pattern: a grower
// that truncates, duplicates, or misorders a block is caught by a plain byte
// comparison, unlike a repeated filler byte.
func requestAllocPattern(size int) []byte {
	pattern := make([]byte, size)
	for i := range pattern {
		pattern[i] = byte(i*131 + i/7)
	}
	return pattern
}

func requestAllocMessageBody(size int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"m","messages":[`)
	// About 2 KiB per message; the exact total is approximately size.
	content := strings.Repeat("x", 1900)
	for i := 0; b.Len() < size-(len(content)+64); i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"role":"user","content":"`)
		b.WriteString(content)
		b.WriteString(`"}`)
	}
	b.WriteString(`]}`)
	return []byte(b.String())
}

// TestRequestBodyGrowerExactBytes covers every buffering mode around the
// growth boundaries: chunked, a declared body read in place, a declared body
// past the speculative hint, and a declared length understated by a body that
// keeps going. Every result must be byte-identical to the input; the grower
// paths additionally return an exact-capacity slice (no retained doubling
// spare capacity).
func TestRequestBodyGrowerExactBytes(t *testing.T) {
	const maxBytes = 4 << 20
	pattern := requestAllocPattern(maxBytes + 1)
	for _, size := range []int{0, 1, 1023, 64<<10 - 1, 64 << 10, 64<<10 + 1, 1<<20 - 1, 1 << 20, 1<<20 + 1, maxBytes} {
		want := pattern[:size]
		for _, tc := range []struct {
			name          string
			contentLength int64
			hintMax       int64
			exactCap      bool
		}{
			{"chunked", -1, 32 << 20, true},
			{"declared admitted hint", int64(size), int64(size), false},
			{"declared under-hinted", int64(size), 1024, true},
			// The declared prefix is read in place at its admitted size;
			// only an actual body longer than that takes the exact grower.
			{"declared understated length", 1024, 1024, size >= 1024},
		} {
			t.Run(fmt.Sprintf("size_%d/%s", size, tc.name), func(t *testing.T) {
				got, err := readRequestBody(bytes.NewReader(want), tc.contentLength, maxBytes, tc.hintMax)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("size=%d: len=%d err=%v", size, len(got), err)
				}
				if tc.exactCap && cap(got) != len(got) {
					t.Fatalf("size=%d: cap=%d, want an exact %d-byte buffer", size, cap(got), len(got))
				}
			})
		}
	}
}

// TestRequestBodySmallChunkedDoesNotPreallocateMax drains the shared scratch
// and reads small chunked bodies cold. The retained scratch must scale with
// the bytes that actually arrived (at most the initial read granularity or
// one doubling), never with max_request_bytes.
func TestRequestBodySmallChunkedDoesNotPreallocateMax(t *testing.T) {
	const maxBytes = 32 << 20
	for _, size := range []int{4 << 10, 64 << 10, 1 << 20} {
		t.Run(fmt.Sprintf("%dKiB", size>>10), func(t *testing.T) {
			drainRequestBodyScratch()
			want := requestAllocBody(size)
			got, err := readRequestBody(bytes.NewReader(want), -1, maxBytes, maxBytes)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("size=%d: len=%d err=%v", size, len(got), err)
			}
			if cap(got) != len(got) {
				t.Fatalf("size=%d: returned cap=%d, want exact", size, cap(got))
			}
			retained := requestBodyScratchCap()
			bound := requestBodyReadInit
			if size > bound {
				bound = 2 * size
			}
			if retained == 0 || retained > bound {
				t.Fatalf("size=%d: retained %d-byte scratch, want <= %d (no max_request_bytes preallocation)", size, retained, bound)
			}
			if retained >= maxBytes {
				t.Fatalf("size=%d: retained the %d-byte request cap", size, retained)
			}
		})
	}
}

// TestRequestBodyAllocationBounds is the efficiency regression: request-body
// buffering itself (not the metadata decode) must cost about one exact body,
// at most 1.5x, for declared and chunked inputs. The bounded scratch is warmed
// once before the measured window; a cold process pays one scratch growth, and
// every later request reuses it.
func TestRequestBodyAllocationBounds(t *testing.T) {
	body16 := requestAllocBody(16 << 20)
	body1 := requestAllocBody(1 << 20)
	for _, tc := range []struct {
		name          string
		body          []byte
		contentLength int64
		hintMax       int64
	}{
		{"declared 16 MiB", body16, int64(len(body16)), int64(len(body16))},
		{"chunked 16 MiB", body16, -1, 32 << 20},
		{"chunked 1 MiB", body1, -1, 32 << 20},
		{"declared under-hinted 16 MiB", body16, int64(len(body16)), 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alloc, allocs := allocStats(func() {
				got, err := readRequestBody(bytes.NewReader(tc.body), tc.contentLength, 32<<20, tc.hintMax)
				if err != nil || !bytes.Equal(got, tc.body) {
					t.Fatalf("len=%d err=%v", len(got), err)
				}
			})
			bound := int64(len(tc.body)) * 3 / 2
			if alloc < int64(len(tc.body)) || alloc > bound {
				t.Fatalf("%d-byte body buffered %d bytes/op, want about one body and at most %d", len(tc.body), alloc, bound)
			}
			t.Logf("%s: %d-byte body, %d bytes/op, %d allocs/op", tc.name, len(tc.body), alloc, int(allocs))
		})
	}
}

// TestRequestBodyChunkedOwnsPostUploadStart pins the timing owner for the
// growable path: Start is stamped after the last upload read, before the
// metadata decode, and the captured body size describes the exact bytes.
func TestRequestBodyChunkedOwnsPostUploadStart(t *testing.T) {
	body := requestAllocBody(1 << 20)
	reader := &requestTimedReader{Reader: bytes.NewReader(body)}
	got, rec, err := readRequest(&http.Request{Body: io.NopCloser(reader), ContentLength: -1}, 2<<20, false)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("len=%d err=%v", len(got), err)
	}
	if rec.Start.IsZero() || rec.Start.Before(reader.lastRead) || rec.Start.After(time.Now()) {
		t.Fatalf("Start must follow upload: start=%v lastRead=%v", rec.Start, reader.lastRead)
	}
	if rec.RequestBytes != int64(len(body)) || rec.Model != "m" {
		t.Fatalf("record = bytes:%d model:%q", rec.RequestBytes, rec.Model)
	}
}

// TestRequestBodyChunkedMaxBytesTypedLimit pins the fail-closed boundary on
// the growable path: one byte past maxBytes returns the typed error and the
// upload reader is closed by the read owner.
func TestRequestBodyChunkedMaxBytesTypedLimit(t *testing.T) {
	body := requestAllocBody(4096)
	reader := &requestTestBody{Reader: bytes.NewReader(body)}
	_, _, err := readRequest(&http.Request{Body: reader, ContentLength: -1}, 1024, false)
	var overflow *http.MaxBytesError
	if !errors.As(err, &overflow) || overflow.Limit != 1024 {
		t.Fatalf("err = %T %v, want MaxBytesError(1024)", err, err)
	}
	if !reader.closed {
		t.Fatal("over-limit upload reader was not closed")
	}
}

// TestRequestBodyScratchConcurrentIsolation proves the bounded scratch never
// aliases two requests: every worker reads a distinct pattern concurrently and
// must get its own bytes back unchanged.
func TestRequestBodyScratchConcurrentIsolation(t *testing.T) {
	const workers = 8
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			body := bytes.Repeat([]byte{byte(w + 1)}, 256<<10)
			for i := 0; i < 20; i++ {
				got, err := readRequestBody(bytes.NewReader(body), -1, 1<<20, 1<<20)
				if err != nil || !bytes.Equal(got, body) {
					t.Errorf("worker %d iteration %d: len=%d err=%v", w, i, len(got), err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
}

func drainRequestBodyScratch() {
	requestBodyScratchPool.mu.Lock()
	requestBodyScratchPool.buf = nil
	requestBodyScratchPool.mu.Unlock()
}

func requestBodyScratchCap() int {
	requestBodyScratchPool.mu.Lock()
	defer requestBodyScratchPool.mu.Unlock()
	return cap(requestBodyScratchPool.buf)
}

func benchmarkRequestBodyAlloc(b *testing.B, body []byte, contentLength, maxBytes, hintMax int64) {
	// Warm the bounded scratch outside the measured window. The first cold
	// read in a process still grows a scratch, but steady-state request
	// buffering must cost only the exact body returned to the caller.
	if got, err := readRequestBody(bytes.NewReader(body), contentLength, maxBytes, hintMax); err != nil || !bytes.Equal(got, body) {
		b.Fatalf("warm read: len=%d err=%v", len(got), err)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		got, err := readRequestBody(bytes.NewReader(body), contentLength, maxBytes, hintMax)
		if err != nil || !bytes.Equal(got, body) {
			b.Fatalf("len=%d err=%v", len(got), err)
		}
	}
}

func BenchmarkRequestBodyAllocDeclared16(b *testing.B) {
	body := requestAllocBody(16 << 20)
	benchmarkRequestBodyAlloc(b, body, int64(len(body)), 32<<20, int64(len(body)))
}

func BenchmarkRequestBodyAllocChunked16(b *testing.B) {
	body := requestAllocBody(16 << 20)
	benchmarkRequestBodyAlloc(b, body, -1, 32<<20, 32<<20)
}

func BenchmarkRequestBodyAllocChunked1(b *testing.B) {
	body := requestAllocBody(1 << 20)
	benchmarkRequestBodyAlloc(b, body, -1, 32<<20, 32<<20)
}

func BenchmarkRequestBodyAllocChunked4K(b *testing.B) {
	body := requestAllocBody(4 << 10)
	benchmarkRequestBodyAlloc(b, body, -1, 32<<20, 32<<20)
}

func BenchmarkRequestBodyAllocDeclaredUnderhint16(b *testing.B) {
	body := requestAllocBody(16 << 20)
	benchmarkRequestBodyAlloc(b, body, int64(len(body)), 32<<20, 1<<20)
}

func BenchmarkRequestBodyAllocReadRequestDeclared16(b *testing.B) {
	body := requestAllocBody(16 << 20)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rc := &requestTestBody{Reader: bytes.NewReader(body)}
		got, _, err := readRequestHint(&http.Request{Body: rc, ContentLength: int64(len(body))}, 32<<20, false, int64(len(body)))
		if err != nil || len(got) != len(body) {
			b.Fatalf("len=%d err=%v", len(got), err)
		}
	}
}

func BenchmarkRequestBodyAllocReadRequestChunked16(b *testing.B) {
	body := requestAllocBody(16 << 20)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		rc := &requestTestBody{Reader: bytes.NewReader(body)}
		got, _, err := readRequestHint(&http.Request{Body: rc, ContentLength: -1}, 32<<20, false, 32<<20)
		if err != nil || len(got) != len(body) {
			b.Fatalf("len=%d err=%v", len(got), err)
		}
	}
}

func BenchmarkRequestBodyAllocParseSingle16(b *testing.B) {
	body := requestAllocBody(16 << 20)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var rec metrics.Record
		if err := parseLLMRequest(body, &rec, false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRequestBodyAllocParseMessages16(b *testing.B) {
	body := requestAllocMessageBody(16 << 20)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var rec metrics.Record
		if err := parseLLMRequest(body, &rec, false); err != nil {
			b.Fatal(err)
		}
		if rec.TurnsUser == 0 {
			b.Fatal("no messages decoded")
		}
	}
}

func TestRequestBodyAllocMessageBodyShape(t *testing.T) {
	body := requestAllocMessageBody(16 << 20)
	var rec metrics.Record
	if err := parseLLMRequest(body, &rec, false); err != nil {
		t.Fatal(err)
	}
	if rec.TurnsUser == 0 || rec.Model != "m" {
		t.Fatalf("fixture shape changed: %+v", rec)
	}
	if !json.Valid(body) {
		t.Fatal("fixture is not valid JSON")
	}
}
