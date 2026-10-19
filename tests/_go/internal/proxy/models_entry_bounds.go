package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// proxyAllocDelta measures bytes allocated by fn without timing it.
func proxyAllocDelta(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// compactModelList builds the cheapest valid OpenAI list body for n entries:
// each entry is a tiny object, so a body just under the raw byte cap can hold
// hundreds of thousands of maps after decoding.
func compactModelList(n int) []byte {
	body := make([]byte, 0, n*11+16)
	body = append(body, `{"data":[`...)
	for i := 0; i < n; i++ {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, `{"id":"m"}`...)
	}
	body = append(body, `]}`...)
	return body
}

// compactModelListID builds a list whose entries all carry one long id.
func compactModelListID(n int, id string) []byte {
	body := make([]byte, 0, n*(len(id)+16)+16)
	body = append(body, `{"data":[`...)
	for i := 0; i < n; i++ {
		if i > 0 {
			body = append(body, ',')
		}
		body = append(body, `{"id":"`...)
		body = append(body, id...)
		body = append(body, `"}`...)
	}
	body = append(body, `]}`...)
	return body
}

// TestModelsDiscoveryEntryLimitFailsClosedWithBoundedAllocation is the memory
// regression: a compact ~7 MiB body holds 650k maps (hundreds of MiB when
// decoded). The entry bound must reject it before the map slice is
// materialized, with an explicit 502 and no partial list.
func TestModelsDiscoveryEntryLimitFailsClosedWithBoundedAllocation(t *testing.T) {
	raw := compactModelList(650_000)
	if len(raw) > int(config.Default().ModelsDiscoveryMaxBytes) {
		t.Fatalf("fixture body = %d bytes, want <= the default discovery cap", len(raw))
	}
	p := New(config.Default(), metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return modelHTTPResponse(r, raw), nil
	})})
	w := httptest.NewRecorder()
	var alloc uint64
	alloc = proxyAllocDelta(func() {
		p.ServeHTTP(w, modelsLimitRequest("openai"))
	})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body %s)", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"object":"list"`) {
		t.Fatal("entry-limit failure returned a partial list")
	}
	if !strings.Contains(w.Body.String(), "entry limit") {
		t.Fatalf("missing explicit entry-limit error: %s", w.Body.String())
	}
	// Decoding 650k entries into []map[string]any costs hundreds of MiB; the
	// bounded count walk costs the body plus a small per-element raw buffer.
	const allocBound = 64 << 20
	if alloc > allocBound {
		t.Fatalf("entry-limit rejection allocated %d bytes for a %d-byte body, want <= %d", alloc, len(raw), allocBound)
	}
	t.Logf("entry-limit rejection: body=%d entries=650000 alloc=%d", len(raw), alloc)
}

// TestModelsDiscoveryEntryLimitBoundary pins the exact inclusive bound: the
// largest accepted list is served, one more entry fails closed.
func TestModelsDiscoveryEntryLimitBoundary(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entries    int
		wantStatus int
	}{
		{"at limit", modelsDiscoveryMaxEntries, http.StatusOK},
		{"over limit", modelsDiscoveryMaxEntries + 1, http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := compactModelList(tc.entries)
			p := New(config.Default(), metrics.Noop{})
			p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return modelHTTPResponse(r, raw), nil
			})})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, modelsLimitRequest("openai"))
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if tc.wantStatus != http.StatusOK {
				if strings.Contains(w.Body.String(), `"object":"list"`) {
					t.Fatal("over-limit response carried a partial list")
				}
				return
			}
			var doc struct {
				Object string           `json:"object"`
				Data   []map[string]any `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Object != "list" || len(doc.Data) != tc.entries {
				t.Fatalf("served %d entries (object %q), want %d", len(doc.Data), doc.Object, tc.entries)
			}
		})
	}
}

// TestModelsDiscoveryOutputFollowsConfiguredCap pins the rendered-list bound
// to models_discovery_max_bytes instead of a fixed constant: a list larger
// than the old fixed 16 MiB cap is served when the configured cap allows it.
func TestModelsDiscoveryOutputFollowsConfiguredCap(t *testing.T) {
	const bodyTarget = 17 << 20
	cfg := config.Default()
	cfg.ModelsDiscoveryMaxBytes = 20 << 20
	raw := compactModelListID(1, strings.Repeat("x", bodyTarget-len(`{"data":[{"id":""}]}`)))
	if len(raw) > int(cfg.ModelsDiscoveryMaxBytes) {
		t.Fatalf("fixture body = %d bytes, want <= configured %d", len(raw), cfg.ModelsDiscoveryMaxBytes)
	}
	p := New(cfg, metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return modelHTTPResponse(r, raw), nil
	})})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, modelsLimitRequest("openai"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body prefix %q)", w.Code, w.Body.String()[:min(len(w.Body.String()), 200)])
	}
	if w.Body.Len() <= 16<<20 {
		t.Fatalf("served output = %d bytes, want past the old fixed 16 MiB cap", w.Body.Len())
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Data) != 1 || len(doc.Data[0].ID) != bodyTarget-len(`{"data":[{"id":""}]}`) {
		t.Fatalf("valid large list changed: %d entries", len(doc.Data))
	}
}

// TestModelsDiscoveryOutputDoesNotHTMLEscape bounds the rendered document for
// provider ids full of <, > and &: the wire encoder keeps them literal, so a
// body under the configured cap renders near its input size instead of
// materializing a six-fold escaped document and rejecting it.
func TestModelsDiscoveryOutputDoesNotHTMLEscape(t *testing.T) {
	const entries = 500
	id := strings.Repeat("<&>", 8000/3+1)[:8000]
	raw := compactModelListID(entries, id)
	if len(raw) > int(config.Default().ModelsDiscoveryMaxBytes) {
		t.Fatalf("fixture body = %d bytes, want <= the default discovery cap", len(raw))
	}
	p := New(config.Default(), metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return modelHTTPResponse(r, raw), nil
	})})
	w := httptest.NewRecorder()
	var alloc uint64
	alloc = proxyAllocDelta(func() {
		p.ServeHTTP(w, modelsLimitRequest("openai"))
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body prefix %q)", w.Code, w.Body.String()[:min(len(w.Body.String()), 200)])
	}
	if bytes.Contains(w.Body.Bytes(), []byte(`\u003c`)) || bytes.Contains(w.Body.Bytes(), []byte(`\u003e`)) || bytes.Contains(w.Body.Bytes(), []byte(`\u0026`)) {
		t.Fatal("served model list still HTML-escapes")
	}
	if got := bytes.Count(w.Body.Bytes(), []byte("<&>")); got != entries*(len(id)/3) {
		t.Fatalf("literal ids = %d triples, want %d", got, entries*len(id)/3)
	}
	// Old behavior materialized a ~24 MiB escaped document for this ~4 MiB
	// body before rejecting it and allocated ~200 MiB end to end. The
	// unescaped render allocates with its input; the bound leaves room for
	// race instrumentation in CI.
	const allocBound = 128 << 20
	if alloc > allocBound {
		t.Fatalf("unescaped %d-byte body allocated %d bytes, want <= %d", len(raw), alloc, allocBound)
	}
	t.Logf("unescaped output: body=%d output=%d alloc=%d", len(raw), w.Body.Len(), alloc)
}

// TestModelsDiscoveryOutputLimitFailsClosedBeforeCommit keeps the fail-closed
// contract for a document past the derived bound: malformed UTF-8 expands
// three-fold when re-encoded, the render is rejected before any response
// bytes, and no partial list reaches the client.
func TestModelsDiscoveryOutputLimitFailsClosedBeforeCommit(t *testing.T) {
	// 5 MiB of invalid UTF-8 bytes decodes to 15 MiB of U+FFFD, past the
	// default 8 MiB cap plus the 5 MiB normalization allowance.
	id := strings.Repeat("\xff", 5<<20)
	raw := compactModelListID(1, id)
	if len(raw) > int(config.Default().ModelsDiscoveryMaxBytes) {
		t.Fatalf("fixture body = %d bytes, want <= the default discovery cap", len(raw))
	}
	p := New(config.Default(), metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return modelHTTPResponse(r, raw), nil
	})})
	w := httptest.NewRecorder()
	p.ServeHTTP(w, modelsLimitRequest("openai"))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body prefix %q)", w.Code, w.Body.String()[:min(len(w.Body.String()), 200)])
	}
	if !strings.Contains(w.Body.String(), "output exceeds") {
		t.Fatalf("missing explicit output-limit error: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `{"data":[`) {
		t.Fatal("output-limit failure returned a partial list")
	}
}

// TestModelsDiscoveryCursorEntryLimitFailsBeforeMapConstruction pins the Cursor
// preflight: entries are counted from the decoded slice before CursorModelEntries
// materializes one map per model, so an over-budget unary response fails with
// bounded allocation.
func TestModelsDiscoveryCursorEntryLimitFailsBeforeMapConstruction(t *testing.T) {
	raw := cursorModelsBody(modelsDiscoveryMaxEntries + 1)
	p := New(config.Default(), metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return modelHTTPResponse(r, raw), nil
	})})
	w := httptest.NewRecorder()
	var alloc uint64
	alloc = proxyAllocDelta(func() {
		p.ServeHTTP(w, modelsLimitRequest("cursor"))
	})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body prefix %q)", w.Code, w.Body.String()[:min(len(w.Body.String()), 200)])
	}
	if !strings.Contains(w.Body.String(), "entry limit") {
		t.Fatalf("missing explicit entry-limit error: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `{"data":[`) {
		t.Fatal("cursor entry-limit failure returned a partial list")
	}
	// One ModelDetails per five wire bytes; no model maps are built. Building
	// the maps costs about 9.6 MiB; the preflight parse stays under 6 MiB.
	const allocBound = 8 << 20
	if alloc > allocBound {
		t.Fatalf("cursor entry-limit rejection allocated %d bytes for %d entries, want <= %d", alloc, modelsDiscoveryMaxEntries+1, allocBound)
	}
	t.Logf("cursor entry-limit rejection: entries=%d alloc=%d", modelsDiscoveryMaxEntries+1, alloc)
}

// cursorModelsBody builds a raw GetUsableModelsResponse carrying n ModelDetails
// entries, each with model_id "m" (field 1 = models, inner field 1 = model_id).
func cursorModelsBody(n int) []byte {
	entry := []byte{0x0a, 0x03, 0x0a, 0x01, 'm'}
	return bytes.Repeat(entry, n)
}

// anthropicEntryPage renders one Anthropic models page with n tiny entries.
func anthropicEntryPage(prefix string, n int, more bool, lastID string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"data":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"id":%q}`, fmt.Sprintf("%s-%d", prefix, i))
	}
	fmt.Fprintf(&b, `],"has_more":%t`, more)
	if lastID != "" {
		fmt.Fprintf(&b, `,"last_id":%q`, lastID)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// TestModelsDiscoveryAnthropicAggregateEntryLimit proves the entry budget
// spans pages: two in-budget pages whose sum exceeds the bound fail closed
// instead of returning the first page's partial list.
func TestModelsDiscoveryAnthropicAggregateEntryLimit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		perPage    int
		wantStatus int
		wantCalls  int
	}{
		{"aggregate at limit", modelsDiscoveryMaxEntries / 2, http.StatusOK, 2},
		{"aggregate over limit", modelsDiscoveryMaxEntries/2 + 1, http.StatusBadGateway, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(config.Default(), metrics.Noop{})
			calls := 0
			p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				raw := anthropicEntryPage("p1", tc.perPage, true, "p1")
				if calls == 2 {
					raw = anthropicEntryPage("p2", tc.perPage, false, "")
				}
				return modelHTTPResponse(r, raw), nil
			})})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, modelsLimitRequest("anthropic"))
			if w.Code != tc.wantStatus || calls != tc.wantCalls {
				t.Fatalf("status = %d calls = %d, want %d/%d (body prefix %q)",
					w.Code, calls, tc.wantStatus, tc.wantCalls, w.Body.String()[:min(len(w.Body.String()), 200)])
			}
			if tc.wantStatus != http.StatusOK {
				if strings.Contains(w.Body.String(), `"object":"list"`) {
					t.Fatal("aggregate entry-limit failure returned a partial list")
				}
				return
			}
			var doc struct {
				Data []map[string]any `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if len(doc.Data) != tc.perPage*2 {
				t.Fatalf("merged entries = %d, want %d", len(doc.Data), tc.perPage*2)
			}
		})
	}
}
