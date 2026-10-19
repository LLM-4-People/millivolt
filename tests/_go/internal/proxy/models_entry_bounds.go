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

// TestModelsDiscoveryOutputLimitFailsClosed bounds the normalized document:
// a list under the entry cap can still expand past the output cap when JSON
// string escaping inflates provider strings. A normal large list passes.
func TestModelsDiscoveryOutputLimitFailsClosed(t *testing.T) {
	const entries = 500
	for _, tc := range []struct {
		name       string
		id         string
		wantStatus int
	}{
		{"normal large list", strings.Repeat("x", 8000), http.StatusOK},
		{"escaped output past cap", strings.Repeat("<", 8000), http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := compactModelListID(entries, tc.id)
			if len(raw) > int(config.Default().ModelsDiscoveryMaxBytes) {
				t.Fatalf("fixture body = %d bytes, want <= the default discovery cap", len(raw))
			}
			p := New(config.Default(), metrics.Noop{})
			p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return modelHTTPResponse(r, raw), nil
			})})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, modelsLimitRequest("openai"))
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body prefix %q)", w.Code, tc.wantStatus, w.Body.String()[:min(len(w.Body.String()), 200)])
			}
			if tc.wantStatus != http.StatusOK {
				if strings.Contains(w.Body.String(), `"object":"list"`) {
					t.Fatal("output-limit failure returned a partial list")
				}
				if !strings.Contains(w.Body.String(), "output exceeds") {
					t.Fatalf("missing explicit output-limit error: %s", w.Body.String())
				}
				return
			}
			var doc struct {
				Data []map[string]any `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if len(doc.Data) != entries || doc.Data[0]["id"] != tc.id {
				t.Fatalf("normal output changed: %d entries", len(doc.Data))
			}
		})
	}
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
