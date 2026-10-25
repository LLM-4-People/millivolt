package web

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestGzipMiddleware: accepted encodings compress with Vary + ETag/304 intact;
// missing headers, HEAD, and 304 responses stay uncompressed.
func TestGzipMiddleware(t *testing.T) {
	body := strings.Repeat("millivolt dashboard payload ", 500) // ~5.5 KB compressible
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(body))
	})
	h := Gzip(next)

	get := func(accept string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		if accept != "" {
			req.Header.Set("Accept-Encoding", accept)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := get("gzip")
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", enc)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("body not gzip: %v", err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != body {
		t.Fatalf("decompressed body mismatch (%d vs %d bytes)", len(got), len(body))
	}

	// A fractional non-zero qvalue is acceptance (RFC 9110 §12.5.3) - the
	// old parser refused every q=0.x, so pin the fix.
	rec = get("br;q=1.0, gzip;q=0.5")
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("gzip;q=0.5: Content-Encoding = %q, want gzip", enc)
	}

	for _, deny := range []string{"", "identity", "br", "gzip;q=0", "br;q=1.0, gzip;q=0.000", "gzip;q=high", "gzip;q=", "gzip;q=NaN", "gzip;q=+Inf", "gzip;q=-Inf", "gzip;q=2", "gzip;q=-0.5"} {
		rec := get(deny)
		if enc := rec.Header().Get("Content-Encoding"); enc != "" {
			t.Errorf("Accept-Encoding %q: Content-Encoding = %q, want none", deny, enc)
		}
		if rec.Body.String() != body {
			t.Errorf("Accept-Encoding %q: body wrong (%d bytes)", deny, rec.Body.Len())
		}
	}

	// HEAD never claims gzip (no body to describe).
	req := httptest.NewRequest(http.MethodHead, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	recH := httptest.NewRecorder()
	h.ServeHTTP(recH, req)
	if recH.Header().Get("Content-Encoding") != "" {
		t.Fatal("HEAD answered with Content-Encoding: gzip")
	}

	// 304 flows through without an encoding claim.
	etagged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `W/"x"`)
		if r.Header.Get("If-None-Match") == `W/"x"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Write([]byte(body))
	})
	req304 := httptest.NewRequest(http.MethodGet, "/", nil)
	req304.Header.Set("Accept-Encoding", "gzip")
	req304.Header.Set("If-None-Match", `W/"x"`)
	rec304 := httptest.NewRecorder()
	Gzip(etagged).ServeHTTP(rec304, req304)
	if rec304.Code != http.StatusNotModified || rec304.Header().Get("Content-Encoding") != "" {
		t.Fatalf("304 under gzip: code=%d enc=%q", rec304.Code, rec304.Header().Get("Content-Encoding"))
	}
}

// Build the payload through the production HTML/bootstrap owners. Varied IDs,
// timestamps, metrics, metadata, and retries avoid an unrealistically repeated
// string fixture; no live/user history is read by this benchmark.
func benchmarkDashboardGzipBody(tb testing.TB) []byte {
	tb.Helper()
	d := config.Default()
	n := BootRingCapMul * d.DashLogRows
	buf := metrics.NewBuffer(n)
	rng := rand.New(rand.NewPCG(0x232745, 0x298671))
	start := time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	for i := range n {
		at := start.Add(time.Duration(i)*time.Second + time.Duration(rng.IntN(1000))*time.Millisecond)
		rec := mkRec(fmt.Sprintf("%016x-%016x", rng.Uint64(), rng.Uint64()), at, 200, "", nil,
			int64(1+rng.IntN(4000)), int64(5000+rng.IntN(60000)), int64(rng.IntN(200000)), int64(rng.IntN(4000)), int64(rng.IntN(30000)), rng.Float64())
		rec.Provider, rec.Model, rec.Client = fmt.Sprintf("provider-%d.example", i%4), fmt.Sprintf("model-%d", i%12), fmt.Sprintf("client-%d", i%3)
		rec.KeyHash, rec.ConversationID = fmt.Sprintf("key-%064x", i%16), fmt.Sprintf("conversation-%032x", i/3)
		rec.UserAgent, rec.ClientIP = "NeutralSDK/1.0 (linux; amd64)", "127.0.0.1"
		rec.Path, rec.Method, rec.ClientLang = "/v1/chat/completions", "POST", "go"
		rec.ClientMeta = metrics.ClientMeta{OS: "linux", Arch: "amd64", Runtime: "go", RuntimeVer: "1.26", PkgVer: "1.0", Format: "chat"}
		rec.ProviderRequestID, rec.ProviderServer = fmt.Sprintf("%016x", rng.Uint64()), "neutral-gateway"
		rec.ProcessingMs, rec.Stream = rng.IntN(1000), true
		rec.FirstTokenAt, rec.LastTokenAt = at.Add(time.Duration(rec.TTFTMs)*time.Millisecond), rec.End
		rec.FinalAttemptAt, rec.FirstAnswerAt = at, rec.FirstTokenAt.Add(time.Millisecond)
		rec.FinishReason = "stop"
		rec.DecodeTPS = rng.Float64() * 200
		rec.Usage.ReasoningTokens = int64(rng.IntN(1000))
		rec.Usage.TotalTokens = rec.Usage.InputTokens + rec.Usage.OutputTokens
		rec.AnswerTokens, rec.GenTokens = rec.Usage.OutputTokens-rec.Usage.ReasoningTokens, rec.Usage.OutputTokens
		rec.ReqToolsCount, rec.ReqToolChoice, rec.ReqStreamOpts = 5, "auto", true
		rec.TurnsUser, rec.TurnsAssistant, rec.TurnsTool = i%32+1, i%31, i%12
		rec.CharsSystem, rec.CharsUser, rec.CharsAssistant, rec.CharsTool = 4096, rng.IntN(100000), rng.IntN(100000), rng.IntN(10000)
		rec.LastTurnRole = "user"
		if i%7 == 0 {
			rec.Retries = 1
			rec.Attempts = []metrics.RetryAttempt{{StatusCode: 503, ErrorType: "server_error", ErrorMsg: "Upstream capacity is temporarily unavailable; retry this request.", At: at.Add(-time.Second)}}
		}
		buf.Record(rec)
	}
	api := NewAggAPI(buf, nil, d.StorageQueryTimeout)
	response := httptest.NewRecorder()
	Handler(api).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		tb.Fatalf("dashboard fixture status=%d", response.Code)
	}
	return response.Body.Bytes()
}

// Reset reuses the same writer workspace as the production gzip pool. The
// body and output buffer are prepared before timing: this isolates compression
// CPU and wire size from snapshot building, JSON marshaling, and networking.
func BenchmarkDynamicDashboardGzip(b *testing.B) {
	body := benchmarkDashboardGzipBody(b)
	if fixture := os.Getenv("MILLIVOLT_GZIP_BENCH_FIXTURE"); fixture != "" {
		var err error
		body, err = os.ReadFile(fixture)
		if err != nil {
			b.Fatal(err)
		}
	}
	for _, level := range []struct {
		name  string
		value int
	}{{"default", gzip.DefaultCompression}, {"best_speed", gzip.BestSpeed}, {"level_2", 2}, {"level_3", 3}} {
		b.Run(level.name, func(b *testing.B) {
			var encoded bytes.Buffer
			writer, err := gzip.NewWriterLevel(&encoded, level.value)
			if err != nil {
				b.Fatal(err)
			}
			compress := func() {
				encoded.Reset()
				writer.Reset(&encoded)
				if _, err := writer.Write(body); err != nil {
					b.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					b.Fatal(err)
				}
			}
			compress()
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for b.Loop() {
				compress()
			}
			b.ReportMetric(float64(encoded.Len()), "compressed-bytes/op")
			b.ReportMetric(float64(len(body)), "raw-bytes/op")
		})
	}
}
