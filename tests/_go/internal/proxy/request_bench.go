package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func benchmarkRequestBody(n int) string {
	return `{"model":"fixture-model","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", n) + `"}]}`
}

func BenchmarkRequestMetadata(b *testing.B) {
	for _, size := range []int{1024, 100 * 1024, 1024 * 1024} {
		b.Run(fmt.Sprintf("bytes%d", size), func(b *testing.B) {
			body := []byte(benchmarkRequestBody(size))
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				var rec metrics.Record
				parseLLMRequest(body, &rec, false)
				if rec.CharsUser != size {
					b.Fatal("incorrect content size")
				}
			}
		})
	}
}

func BenchmarkRequestMetadataShapes(b *testing.B) {
	const size = 100 * 1024
	for _, tc := range []struct{ name, body string }{
		{"escaped", `{"messages":[{"role":"user","content":"` + strings.Repeat(`text\n`, size/6) + `"}]}`},
		{"multimodal", `{"messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,` + strings.Repeat("A", size) + `"}}]}]}`},
		{"tool_schema", `{"tools":[{"type":"function","function":{"name":"fixture","description":"` + strings.Repeat("description ", size/12) + `","parameters":{"type":"object","properties":{"value":{"type":"string"}}}}}],"metadata":{"nested":{"value":"` + strings.Repeat("x", size/10) + `"}}}`},
		{"digit_description", `{"tools":[{"description":"` + strings.Repeat("1234567890", size/10) + `"}]}`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			body := []byte(tc.body)
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				var rec metrics.Record
				parseLLMRequest(body, &rec, false)
			}
		})
	}
}

type benchmarkResponseSink struct {
	header http.Header
	status int
	bytes  int
}

func (w *benchmarkResponseSink) Header() http.Header         { return w.header }
func (w *benchmarkResponseSink) WriteHeader(status int)      { w.status = status }
func (w *benchmarkResponseSink) Write(p []byte) (int, error) { w.bytes += len(p); return len(p), nil }
func (w *benchmarkResponseSink) Flush()                      {}

// BenchmarkProxyRequest includes parsing, SSE analysis/relay, and bounded ring
// accounting, but no upstream network or provider inference time.
func BenchmarkProxyRequest(b *testing.B) {
	for _, tc := range []struct {
		name          string
		input, frames int
	}{
		{"prompt1KiB_stream100", 1024, 100},
		{"prompt100KiB_stream100", 100 * 1024, 100},
		{"prompt1MiB_stream100", 1024 * 1024, 100},
		{"prompt1KiB_stream1000", 1024, 1000},
	} {
		b.Run(tc.name, func(b *testing.B) {
			cfg := config.Default()
			s := New(cfg, metrics.NewBuffer(cfg.HistorySize))
			payload := benchmarkRequestBody(tc.input)
			response := strings.Repeat("data: {\"choices\":[{\"delta\":{\"content\":\"hello world \"}}]}\n\n", tc.frames) +
				fmt.Sprintf("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\ndata: [DONE]\n\n", tc.frames, tc.frames+100)
			s.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
			})})
			b.ReportAllocs()
			for b.Loop() {
				r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer fixture-secret")
				r.Header.Set("X-Proxy-Base-URL", "http://fixture.example")
				w := &benchmarkResponseSink{header: make(http.Header)}
				s.ServeHTTP(w, r)
				if w.status != http.StatusOK || w.bytes != len(response) {
					b.Fatalf("status=%d bytes=%d want=%d", w.status, w.bytes, len(response))
				}
			}
		})
	}
}
