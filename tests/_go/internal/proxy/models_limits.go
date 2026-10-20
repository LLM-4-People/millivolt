package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

type modelsCountBody struct {
	*bytes.Reader
	n      int
	closed bool
}

func (b *modelsCountBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.n += n
	return n, err
}
func (b *modelsCountBody) Close() error { b.closed = true; return nil }

func modelsLimitRequest(shape string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r.Header.Set("X-Proxy-Base-URL", "http://provider.example")
	r.Header.Set("X-Proxy-Format", shape)
	r.Header.Set("Authorization", "Bearer fixture-key")
	return r
}

func modelsLimitBody(shape string, size int) []byte {
	var raw []byte
	switch shape {
	case "cursor":
		raw = []byte{10, 3, 10, 1, 'm'} // one ModelDetails with model_id=m
		if size > len(raw) {
			// Boundary fixtures are at least 4KiB: a two-byte length varint
			// plus one tag encodes this unknown field without materializing it.
			n := size - len(raw) - 3
			raw = append(raw, 18)
			raw = binary.AppendUvarint(raw, uint64(n))
			raw = append(raw, bytes.Repeat([]byte{'x'}, n)...)
		}
		return raw
	case "anthropic":
		raw = []byte(`{"data":[{"id":"m"}],"has_more":false}`)
	default:
		raw = []byte(`{"data":[{"id":"m"}]}`)
	}
	if size > len(raw) {
		raw = append(raw, bytes.Repeat([]byte{' '}, size-len(raw))...)
	}
	return raw
}

func modelHTTPResponse(r *http.Request, raw []byte) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw)), Request: r}
}

func TestModelsDiscoveryExactByteBoundaryAcrossFormats(t *testing.T) {
	for _, shape := range []string{"openai", "anthropic", "cursor"} {
		for _, overflow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/overflow=%v", shape, overflow), func(t *testing.T) {
				cfg := config.Default()
				cfg.ModelsDiscoveryMaxBytes = config.ModelsDiscoveryMaxBytesMin
				p := New(cfg, metrics.Noop{})
				n := int(cfg.ModelsDiscoveryMaxBytes)
				if overflow {
					n++
				}
				b := &modelsCountBody{Reader: bytes.NewReader(modelsLimitBody(shape, n))}
				p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					// Content-Length is deliberately not authoritative.
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: b, ContentLength: 1, Request: r}, nil
				})})
				w := httptest.NewRecorder()
				p.ServeHTTP(w, modelsLimitRequest(shape))
				want := 200
				if overflow {
					want = 502
				}
				if w.Code != want || b.n != n || !b.closed {
					t.Fatalf("status=%d read=%d closed=%v body=%s", w.Code, b.n, b.closed, w.Body.String())
				}
				if overflow && !strings.Contains(w.Body.String(), "byte limit") {
					t.Fatalf("missing explicit limit error: %s", w.Body.String())
				}
			})
		}
	}
}

func TestModelsDiscoveryNonSuccessBodyIsBounded(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	b := &modelsCountBody{Reader: bytes.NewReader(bytes.Repeat([]byte{'x'}, 128))}
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: b, Request: r}, nil
	})})
	req, err := http.NewRequest(http.MethodGet, "http://provider.example/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &modelsDiscovery{ctx: req.Context(), bytesLeft: 32, pagesLeft: 1}
	if _, err := p.fetchModelsResponse(d, req, &target{provider: "provider.example"}, ""); err == nil {
		t.Fatal("oversized error response accepted")
	}
	if b.n != 33 || !b.closed || d.bytesLeft != -1 || d.pagesLeft != 0 {
		t.Fatalf("budget bypass: read=%d closed=%v remaining=%+v", b.n, b.closed, d)
	}
}

func TestModelsDiscoveryGzipDecodedBytesAreBounded(t *testing.T) {
	var compressed bytes.Buffer
	z := gzip.NewWriter(&compressed)
	z.Write(modelsLimitBody("openai", config.ModelsDiscoveryMaxBytesMin+1))
	z.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(compressed.Bytes())
	}))
	defer upstream.Close()
	cfg := config.Default()
	cfg.ModelsDiscoveryMaxBytes = config.ModelsDiscoveryMaxBytesMin
	p := New(cfg, metrics.Noop{})
	r := modelsLimitRequest("openai")
	r.Header.Set("X-Proxy-Base-URL", upstream.URL)
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "byte limit") {
		t.Fatalf("compressed body bypassed decoded cap: %d %s", w.Code, w.Body.String())
	}
}

func TestModelsDiscoveryAggregatePageAndByteLimits(t *testing.T) {
	for _, limit := range []string{"exact", "bytes", "pages", "same-cursor", "cycle"} {
		t.Run(limit, func(t *testing.T) {
			cfg := config.Default()
			cfg.ModelsDiscoveryMaxBytes = config.ModelsDiscoveryMaxBytesMin
			cfg.ModelsDiscoveryMaxPages = 3
			if limit == "pages" {
				cfg.ModelsDiscoveryMaxPages = 1
			}
			p := New(cfg, metrics.Noop{})
			calls := 0
			p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls > 3 {
					return nil, fmt.Errorf("fixture guard: unexpected fourth request")
				}
				next := fmt.Sprintf("page-%d", calls)
				more := calls < 2
				if limit == "same-cursor" {
					next = "same"
					more = true
				}
				if limit == "cycle" {
					next = fmt.Sprintf("page-%d", calls%2)
					more = true
				}
				raw := []byte(fmt.Sprintf(`{"data":[{"id":"model-%d"}],"has_more":%t,"last_id":%q}`, calls, more, next))
				if limit == "exact" || limit == "bytes" {
					n := config.ModelsDiscoveryMaxBytesMin / 2
					if limit == "bytes" && calls == 2 {
						n++
					}
					raw = append(raw, bytes.Repeat([]byte{' '}, n-len(raw))...)
				}
				return modelHTTPResponse(r, raw), nil
			})})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, modelsLimitRequest("anthropic"))
			wantStatus, wantCalls := 502, 2
			if limit == "exact" {
				wantStatus = 200
			}
			if limit == "pages" {
				wantCalls = 1
			}
			if limit == "cycle" {
				wantCalls = 3
			}
			if w.Code != wantStatus || calls != wantCalls {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
			if w.Code == 502 && strings.Contains(w.Body.String(), `"object":"list"`) {
				t.Fatal("primary failure returned partial list")
			}
		})
	}
}

func TestModelsDiscoveryMissingContinuationFails(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return modelHTTPResponse(r, []byte(`{"data":[{"id":"m"}],"has_more":true}`)), nil
	})})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, modelsLimitRequest("anthropic"))
	if w.Code != 502 {
		t.Fatalf("partial success: %d %s", w.Code, w.Body.String())
	}
}

func TestModelsDiscoveryEnrichmentSharesBudgetAndDegrades(t *testing.T) {
	for _, limit := range []string{"none", "bytes", "pages"} {
		t.Run(limit, func(t *testing.T) {
			cfg := config.Default()
			cfg.ModelsDiscoveryMaxBytes = config.ModelsDiscoveryMaxBytesMin
			cfg.Providers = map[string]config.ProviderOverride{"provider.example": {ModelsPath: "/metadata", ModelsKeys: map[string]string{"input_modalities": "detail"}}}
			if limit == "pages" {
				cfg.ModelsDiscoveryMaxPages = 1
			}
			p := New(cfg, metrics.Noop{})
			calls := 0
			p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				raw := modelsLimitBody("openai", config.ModelsDiscoveryMaxBytesMin/2)
				if r.URL.Path == "/metadata" {
					raw = []byte(`{"data":[{"id":"m","detail":["text"]}]}`)
					if limit == "bytes" {
						raw = append(raw, bytes.Repeat([]byte{' '}, config.ModelsDiscoveryMaxBytesMin/2+1-len(raw))...)
					}
				}
				return modelHTTPResponse(r, raw), nil
			})})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, modelsLimitRequest("openai"))
			if w.Code != 200 {
				t.Fatalf("optional enrichment failed complete primary: %d %s", w.Code, w.Body.String())
			}
			hasMeta := strings.Contains(w.Body.String(), "input_modalities")
			if hasMeta != (limit == "none") {
				t.Fatalf("unexpected enrichment: %s", w.Body.String())
			}
			want := 2
			if limit == "pages" {
				want = 1
			}
			if calls != want {
				t.Fatalf("calls=%d want%d", calls, want)
			}
		})
	}
}

// TestModelsDiscoveryEnrichmentSharesEntryBudget proves the metadata source
// draws from the discovery-wide entry budget after the primary list debited
// its entries. At the combined limit enrichment applies; one entry past it
// degrades to the complete base list instead of opening a fresh 10,000-entry
// allowance or failing the primary discovery.
func TestModelsDiscoveryEnrichmentSharesEntryBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		primary    int
		metadata   int
		wantEnrich bool
	}{
		{"combined at limit", 6_000, 4_000, true},
		{"combined over limit", 6_000, 4_001, false},
		{"metadata alone over remaining", 9_999, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Providers = map[string]config.ProviderOverride{"provider.example": {
				ModelsPath: "/metadata",
				ModelsKeys: map[string]string{"input_modalities": "detail"},
			}}
			p := New(cfg, metrics.Noop{})
			calls := 0
			p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path == "/metadata" {
					return modelHTTPResponse(r, openAIModelListIDs(tc.metadata, "m")), nil
				}
				return modelHTTPResponse(r, openAIModelListIDs(tc.primary, "m")), nil
			})})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, modelsLimitRequest("openai"))
			if w.Code != http.StatusOK || calls != 2 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
			hasMeta := strings.Contains(w.Body.String(), "input_modalities")
			if hasMeta != tc.wantEnrich {
				t.Fatalf("enriched=%v, want %v (primary=%d metadata=%d)", hasMeta, tc.wantEnrich, tc.primary, tc.metadata)
			}
			var doc struct {
				Data []map[string]any `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if len(doc.Data) != tc.primary {
				t.Fatalf("base list entries = %d, want %d", len(doc.Data), tc.primary)
			}
		})
	}
}

// openAIModelListIDs builds a compact OpenAI list with unique ids prefix-N.
func openAIModelListIDs(n int, prefix string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"data":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%q,"detail":["text"]}`, fmt.Sprintf("%s-%d", prefix, i))
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

type modelsWaitBody struct {
	ctx    context.Context
	closed atomic.Bool
}

func (b *modelsWaitBody) Read([]byte) (int, error) { <-b.ctx.Done(); return 0, b.ctx.Err() }
func (b *modelsWaitBody) Close() error             { b.closed.Store(true); return nil }

func TestModelsDiscoveryDeadlineCoversBodiesAcrossFormats(t *testing.T) {
	for _, shape := range []string{"openai", "anthropic", "cursor"} {
		for _, shortParent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/short-parent=%v", shape, shortParent), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					cfg := config.Default()
					cfg.ModelsDiscoveryTimeout = time.Second
					p := New(cfg, metrics.Noop{})
					var b *modelsWaitBody
					p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
						if _, ok := r.Context().Deadline(); !ok {
							return nil, fmt.Errorf("missing discovery deadline")
						}
						b = &modelsWaitBody{ctx: r.Context()}
						return &http.Response{StatusCode: 200, Header: make(http.Header), Body: b, Request: r}, nil
					})})
					r := modelsLimitRequest(shape)
					want := time.Second
					if shortParent {
						want = time.Second / 4
						ctx, cancel := context.WithTimeout(r.Context(), want)
						defer cancel()
						r = r.WithContext(ctx)
					}
					started := time.Now()
					w := httptest.NewRecorder()
					p.ServeHTTP(w, r)
					if w.Code != 502 || b == nil || !b.closed.Load() || time.Since(started) != want {
						t.Fatalf("status=%d elapsed=%s body=%v", w.Code, time.Since(started), b)
					}
				})
			})
		}
	}
}

// TestModelsDiscoveryHonorsConfiguredTimeoutBeyondReadFallback proves the
// configured models_discovery_timeout, up to five minutes, is authoritative
// for the whole discovery body read below the ten-minute whole-document
// ceiling; the two-minute fallback applies only when the caller context has no
// deadline at all.
func TestModelsDiscoveryHonorsConfiguredTimeoutBeyondReadFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := config.Default()
		cfg.ModelsDiscoveryTimeout = 5 * time.Minute
		bodyDelay := cfg.WholeDocumentReadTimeout + 30*time.Second
		body := &delayedReadBody{
			delay:   bodyDelay,
			data:    []byte(`{"data":[{"id":"model-one"}]}`),
			unblock: make(chan struct{}),
		}
		p := New(cfg, metrics.Noop{})
		p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if _, ok := r.Context().Deadline(); !ok {
				return nil, fmt.Errorf("missing discovery deadline")
			}
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Type": []string{"application/json"}},
				Body:          body,
				ContentLength: -1,
				Request:       r,
			}, nil
		})})
		started := time.Now()
		w := httptest.NewRecorder()
		p.ServeHTTP(w, modelsLimitRequest("openai"))
		elapsed := time.Since(started)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
		}
		if elapsed != bodyDelay {
			t.Fatalf("elapsed = %s, want %s", elapsed, bodyDelay)
		}
		if !strings.Contains(w.Body.String(), "model-one") {
			t.Fatalf("discovery lost the model entry: %s", w.Body.String())
		}
		if got := p.respBudget.bytes.Load(); got != 0 {
			t.Fatalf("response budget after successful discovery = %d, want 0", got)
		}
	})
}

func TestModelsDiscoveryDeadlineAndLimitsDoNotResetBetweenPages(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := config.Default()
		cfg.ModelsDiscoveryTimeout = time.Second
		cfg.ModelsDiscoveryMaxPages = 2
		p := New(cfg, metrics.Noop{})
		var deadlines []time.Time
		calls := 0
		p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			deadline, _ := r.Context().Deadline()
			deadlines = append(deadlines, deadline)
			if calls == 1 {
				time.Sleep(3 * time.Second / 4)
				updated := *cfg
				updated.ModelsDiscoveryTimeout = 5 * time.Minute
				updated.ModelsDiscoveryMaxPages = 1
				p.cfgSnap.Store(&updated) // same config publication owner; retain fake transport
				return modelHTTPResponse(r, []byte(`{"data":[{"id":"m"}],"has_more":true,"last_id":"next"}`)), nil
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &modelsWaitBody{ctx: r.Context()}, Request: r}, nil
		})})
		started := time.Now()
		w := httptest.NewRecorder()
		p.ServeHTTP(w, modelsLimitRequest("anthropic"))
		if w.Code != 502 || calls != 2 || len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) || time.Since(started) != time.Second {
			t.Fatalf("budget reset: status=%d calls=%d deadlines=%v elapsed=%s", w.Code, calls, deadlines, time.Since(started))
		}
	})
}

func TestModelsConfiguredHeadersAcrossFormats(t *testing.T) {
	for _, shape := range []string{"openai", "anthropic", "cursor"} {
		t.Run(shape, func(t *testing.T) {
			cfg := config.Default()
			cfg.Providers = map[string]config.ProviderOverride{"provider.example": {Headers: map[string]string{"X-Required": "fixture", "X-Unit-Key": "configured-key", "Content-Type": "operator-type", "Anthropic-Version": "operator-version"}, ModelsPath: "/metadata", ModelsKeys: map[string]string{"input_modalities": "detail"}}}
			p := New(cfg, metrics.Noop{})
			calls := 0
			p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Header.Get("X-Required") != "fixture" {
					return nil, fmt.Errorf("required configured header absent")
				}
				wantAuth := "configured-key"
				if shape == "cursor" && r.Method == http.MethodPost {
					wantAuth = "Token fixture-key"
					if r.Header.Get("Content-Type") != "application/proto" || r.Header.Get("Connect-Protocol-Version") != "1" {
						return nil, fmt.Errorf("unary identity overwritten")
					}
				}
				if r.Header.Get("X-Unit-Key") != wantAuth {
					return nil, fmt.Errorf("auth precedence: got %q want %q", r.Header.Get("X-Unit-Key"), wantAuth)
				}
				if shape == "anthropic" && r.Header.Get("Anthropic-Version") != "operator-version" {
					return nil, fmt.Errorf("configured version precedence changed")
				}
				if r.URL.Path == "/metadata" {
					return modelHTTPResponse(r, []byte(`{"data":[{"id":"m","detail":["text"]}]}`)), nil
				}
				return modelHTTPResponse(r, modelsLimitBody(shape, 0)), nil
			})})
			r := modelsLimitRequest(shape)
			r.Header.Set("X-Proxy-Auth-Header", "X-Unit-Key")
			r.Header.Set("X-Proxy-Auth-Prefix", "Token ")
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != 200 || calls != 2 || !strings.Contains(w.Body.String(), "input_modalities") {
				t.Fatalf("wire/enrichment regression: status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
			var out map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestModelsDiscoveryResponseBufferBudgetRefusesBeforeRead pins the
// process-wide reservation on discovery: a full budget refuses the page before
// its body is read, and releasing the concurrent reservation restores the
// capacity for the retry. The reservation stays held until the discovery
// document has been written.
func TestModelsDiscoveryResponseBufferBudgetRefusesBeforeRead(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	raw := bytes.Repeat([]byte{'x'}, 50)
	p.respBudget.set(int64(len(raw)))

	response := func(body *modelsCountBody) {
		p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        make(http.Header),
				Body:          body,
				ContentLength: int64(len(raw)),
				Request:       r,
			}, nil
		})})
	}
	req, err := http.NewRequest(http.MethodGet, "http://provider.example/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	d := &modelsDiscovery{ctx: req.Context(), bytesLeft: 1000, pagesLeft: 2}
	if !p.respBudget.acquire(int64(len(raw))) {
		t.Fatal("failed to hold the concurrent reservation")
	}
	refusedBody := &modelsCountBody{Reader: bytes.NewReader(raw)}
	response(refusedBody)
	if _, err := p.fetchModelsResponse(d, req, &target{provider: "provider.example"}, ""); !errors.Is(err, errResponseBufferBudget) {
		t.Fatalf("error = %v, want errResponseBufferBudget", err)
	}
	if refusedBody.n != 0 {
		t.Fatalf("refused discovery read %d bytes before admission", refusedBody.n)
	}
	p.respBudget.release(int64(len(raw)))

	acceptedBody := &modelsCountBody{Reader: bytes.NewReader(raw)}
	response(acceptedBody)
	body, err := p.fetchModelsResponse(d, req, &target{provider: "provider.example"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != len(raw) || acceptedBody.n != len(raw) {
		t.Fatalf("accepted discovery read %d bytes, want %d", acceptedBody.n, len(raw))
	}
	if got := p.respBudget.bytes.Load(); got != int64(len(raw)) {
		t.Fatalf("held discovery reservation = %d, want %d", got, len(raw))
	}
	d.releaseResponseBuffer()
	if got := p.respBudget.bytes.Load(); got != 0 {
		t.Fatalf("response budget after discovery release = %d, want 0", got)
	}

	w := httptest.NewRecorder()
	writeModelsError(w, errResponseBufferBudget)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" {
		t.Fatalf("discovery admission status=%d Retry-After=%q, want 503 and 1", w.Code, w.Header().Get("Retry-After"))
	}
}
