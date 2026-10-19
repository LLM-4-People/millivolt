package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
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

// wideModelEnvelope builds an OpenAI envelope with one small data array plus
// n distinct unrelated top-level members. The members are the cheapest shape
// for the replaced map decode: every one materialized its own map entry, key
// string header and raw-message slice even though discovery only reads data.
func wideModelEnvelope(n int, entries string) []byte {
	body := make([]byte, 0, n*8+len(entries)+32)
	body = append(body, `{"data":[`...)
	body = append(body, entries...)
	body = append(body, ']')
	for i := 0; i < n; i++ {
		body = append(body, ',', '"', 'k')
		body = strconv.AppendInt(body, int64(i), 10)
		body = append(body, '"', ':', '0')
	}
	body = append(body, '}')
	return body
}

// TestModelsDiscoveryUnrelatedEnvelopeMembersStayBounded is the envelope
// allocation regression: finding the top-level data array must not decode
// every sibling member. A ~7.7 MiB body with 650k unrelated members expands
// into a 650k-entry map (over 100 MiB allocated) when the envelope is decoded
// wholesale, while the lexical selection costs the bounded body plus the
// small served list.
func TestModelsDiscoveryUnrelatedEnvelopeMembersStayBounded(t *testing.T) {
	raw := wideModelEnvelope(650_000, `{"id":"model-one","context_length":4096},{"id":"model-two"}`)
	if len(raw) > int(config.Default().ModelsDiscoveryMaxBytes) {
		t.Fatalf("fixture body = %d bytes, want <= the default discovery cap", len(raw))
	}
	p := New(config.Default(), metrics.Noop{})
	p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return modelHTTPResponse(r, raw), nil
	})})
	w := httptest.NewRecorder()
	alloc := proxyAllocDelta(func() {
		p.ServeHTTP(w, modelsLimitRequest("openai"))
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body prefix %q)", w.Code, w.Body.String()[:min(len(w.Body.String()), 200)])
	}
	var doc struct {
		Data []struct {
			ID            string  `json:"id"`
			ContextLength float64 `json:"context_length"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Data) != 2 || doc.Data[0].ID != "model-one" || doc.Data[1].ID != "model-two" {
		t.Fatalf("served entries = %+v, want the two data entries", doc.Data)
	}
	// A wholesale map decode allocates over 100 MiB for this fixture. The
	// lexical walk allocates with the body plus the output; the bound leaves
	// room for race instrumentation in CI.
	const allocBound = 32 << 20
	if alloc > allocBound {
		t.Fatalf("unrelated envelope members allocated %d bytes for a %d-byte body, want <= %d", alloc, len(raw), allocBound)
	}
	t.Logf("wide envelope: body=%d members=650000 alloc=%d", len(raw), alloc)
}

// TestModelsDiscoveryEnvelopeShapes pins the envelope-selection contract that
// the lexical scan must preserve from the replaced map decode: supported root
// shapes, duplicate-key last-wins, nested-data exclusion, strict requested-key
// typing, malformed-document fallback, and the entry budget.
func TestModelsDiscoveryEnvelopeShapes(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		keys        []string
		wantIDs     []string
		constructed bool
	}{
		{
			name:        "openai wrapper",
			body:        `{"object":"list","data":[{"id":"a"},{"id":"b"}]}`,
			keys:        []string{"data"},
			wantIDs:     []string{"a", "b"},
			constructed: true,
		},
		{
			name:        "anthropic wrapper",
			body:        `{"data":[{"id":"a"}],"has_more":false}`,
			keys:        []string{"data"},
			wantIDs:     []string{"a"},
			constructed: true,
		},
		{
			name:        "bare array",
			body:        `[{"id":"a"}]`,
			keys:        []string{"data"},
			wantIDs:     []string{"a"},
			constructed: true,
		},
		{
			name:        "empty array",
			body:        `{"data":[]}`,
			keys:        []string{"data"},
			wantIDs:     nil,
			constructed: true,
		},
		{
			name:        "keys select in argument order",
			body:        `{"models":[{"id":"from-models"}],"data":[{"id":"from-data"}]}`,
			keys:        []string{"data", "models"},
			wantIDs:     []string{"from-data"},
			constructed: true,
		},
		{
			name:        "duplicate key last wins",
			body:        `{"data":[{"id":"first"}],"data":[{"id":"last"}]}`,
			keys:        []string{"data"},
			wantIDs:     []string{"last"},
			constructed: true,
		},
		{
			name:        "duplicate null last is absent",
			body:        `{"data":[{"id":"first"}],"data":null}`,
			keys:        []string{"data"},
			constructed: false,
		},
		{
			name:        "duplicate non-array last rejects wrapper",
			body:        `{"data":[{"id":"first"}],"data":"not-an-array"}`,
			keys:        []string{"data"},
			constructed: false,
		},
		{
			name:        "nested data is not a candidate",
			body:        `{"object":"list","meta":{"data":[{"id":"nested"}]}}`,
			keys:        []string{"data"},
			constructed: false,
		},
		{
			name:        "wrong-shape key rejects whole wrapper",
			body:        `{"data":{},"models":[{"id":"from-models"}]}`,
			keys:        []string{"data", "models"},
			constructed: false,
		},
		{
			name:        "null key falls through",
			body:        `{"data":null,"models":[{"id":"from-models"}]}`,
			keys:        []string{"data", "models"},
			wantIDs:     []string{"from-models"},
			constructed: true,
		},
		{
			name:        "escaped key",
			body:        `{"da\u0074a":[{"id":"a"}]}`,
			keys:        []string{"data"},
			wantIDs:     []string{"a"},
			constructed: true,
		},
		{
			name:        "whitespace",
			body:        " \n\t{ \"data\" : [ {\"id\":\"a\"} ] }\r\n",
			keys:        []string{"data"},
			wantIDs:     []string{"a"},
			constructed: true,
		},
		{
			name:        "malformed object is verbatim",
			body:        `{"data":[{"id":"a"}]`,
			keys:        []string{"data"},
			constructed: false,
		},
		{
			name:        "trailing garbage is verbatim",
			body:        `{"data":[{"id":"a"}]}x`,
			keys:        []string{"data"},
			constructed: false,
		},
		{
			name:        "malformed sibling is verbatim",
			body:        `{"data":[{"id":"a"}],"junk":}`,
			keys:        []string{"data"},
			constructed: false,
		},
		{
			name:        "malformed bare array is not constructed",
			body:        `[{"id":"a"},`,
			keys:        []string{"data"},
			constructed: false,
		},
		{
			name:        "non-object entry is not constructed",
			body:        `{"data":[1]}`,
			keys:        []string{"data"},
			constructed: false,
		},
		{
			name:        "null root",
			body:        `null`,
			keys:        []string{"data"},
			constructed: false,
		},
		{
			name:        "empty body",
			body:        ``,
			keys:        []string{"data"},
			constructed: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, constructed, err := unwrapEntries([]byte(tc.body), modelsDiscoveryMaxEntries, tc.keys...)
			if err != nil {
				t.Fatalf("unwrapEntries error = %v, want none", err)
			}
			if constructed != tc.constructed {
				t.Fatalf("constructed = %v, want %v", constructed, tc.constructed)
			}
			if len(entries) != len(tc.wantIDs) {
				t.Fatalf("entries = %d, want %d", len(entries), len(tc.wantIDs))
			}
			for i, want := range tc.wantIDs {
				if id, _ := entries[i]["id"].(string); id != want {
					t.Fatalf("entry %d id = %q, want %q", i, id, want)
				}
			}
		})
	}
	// The entry budget still fails closed before any list is decoded.
	if _, _, err := unwrapEntries([]byte(`{"data":[{"id":"a"}]}`), 0, "data"); !errors.Is(err, errModelsDiscoveryEntries) {
		t.Fatalf("budget error = %v, want errModelsDiscoveryEntries", err)
	}
}

// TestModelsDiscoveryEnvelopeScanNormalListsKeepsOpenAIAndAnthropic are the
// normal controls for the lexical selection: ordinary OpenAI and Anthropic
// discovery bodies still serve their complete canonical lists.
func TestModelsDiscoveryEnvelopeScanNormalListsKeepsOpenAIAndAnthropic(t *testing.T) {
	for _, tc := range []struct {
		shape string
		body  string
	}{
		{
			shape: "openai",
			body:  `{"object":"list","data":[{"id":"normal-one"},{"id":"normal-two"}]}`,
		},
		{
			shape: "anthropic",
			body:  `{"data":[{"id":"normal-one","display_name":"One"},{"id":"normal-two","display_name":"Two"}],"has_more":false}`,
		},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			p := New(config.Default(), metrics.Noop{})
			p.client.Store(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return modelHTTPResponse(r, []byte(tc.body)), nil
			})})
			w := httptest.NewRecorder()
			p.ServeHTTP(w, modelsLimitRequest(tc.shape))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
			}
			var doc struct {
				Object string `json:"object"`
				Data   []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Object != "list" || len(doc.Data) != 2 || doc.Data[0].ID != "normal-one" || doc.Data[1].ID != "normal-two" {
				t.Fatalf("served document = %+v, want the two normal entries", doc)
			}
		})
	}
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
