package proxy

// GET /v1/models is a CONSTRUCTED service, not a passthrough: the proxy
// guarantees the canonical OpenAI wrapper/fields, mirrors the two
// context-window spellings, and merges per-provider enrichment metadata
// (config providers.<label>.models_path/models_keys) so a client needs one
// call for the complete picture. Every mechanism here is tested with neutral
// provider labels - the mock upstream's host label via providerFromURL.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// modelsUpstreamFixture builds a mock upstream + a proxy Server whose
// provider label for that upstream (its host:port, via providerFromURL)
// carries the given per-provider override - the neutral-label way to wire
// enrichment without naming any real provider.
func modelsUpstreamFixture(t *testing.T, modelsBody, metaPath, metaBody string, modelsKeys map[string]string) (*httptest.Server, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if metaPath != "" && r.URL.Path == metaPath {
			w.Write([]byte(metaBody))
			return
		}
		w.Write([]byte(modelsBody))
	}))
	t.Cleanup(upstream.Close)
	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	if modelsKeys != nil {
		cfg.Providers = map[string]config.ProviderOverride{
			providerFromURL(u): {ModelsPath: metaPath, ModelsKeys: modelsKeys},
		}
	}
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	t.Cleanup(srv.Close)
	return srv, upstream
}

func fetchModelsBody(t *testing.T, srv *httptest.Server, upstreamURL string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("X-Proxy-Base-URL", upstreamURL)
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, out
}

func modelEntries(t *testing.T, doc map[string]any) []map[string]any {
	t.Helper()
	if doc["object"] != "list" {
		t.Fatalf("wrapper object = %v, want list", doc["object"])
	}
	data, ok := doc["data"].([]any)
	if !ok {
		t.Fatalf("data = %T, want array", doc["data"])
	}
	out := make([]map[string]any, 0, len(data))
	for _, d := range data {
		m, ok := d.(map[string]any)
		if !ok {
			t.Fatalf("entry = %T, want object", d)
		}
		out = append(out, m)
	}
	return out
}

// Enrichment: the configured metadata endpoint's per-model fields merge into
// the main list by id, the context spellings are mirrored, and every upstream
// field rides through verbatim.
func TestModelsEnrichmentMerged(t *testing.T) {
	models := `{"object":"list","data":[
		{"id":"m-1","object":"model","created":100,"owned_by":"prov","context_length":1000000,"price_field":7}
	]}`
	meta := `{"models":[
		{"id":"m-1","input_modalities":["text","image"],"output_modalities":["text"],"context_length":999}
	]}`
	srv, upstream := modelsUpstreamFixture(t, models, "/metadata", meta, map[string]string{
		"input_modalities":  "input_modalities",
		"output_modalities": "output_modalities",
	})
	code, doc := fetchModelsBody(t, srv, upstream.URL)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	entries := modelEntries(t, doc)
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if v, ok := e["input_modalities"].([]any); !ok || len(v) != 2 || v[0] != "text" {
		t.Errorf("input_modalities = %v, want merged [text image]", e["input_modalities"])
	}
	if v, ok := e["output_modalities"].([]any); !ok || len(v) != 1 || v[0] != "text" {
		t.Errorf("output_modalities = %v, want merged [text]", e["output_modalities"])
	}
	// context_length comes from the MAIN list; context_window is the mirror.
	if e["context_length"] != float64(1000000) {
		t.Errorf("context_length = %v, want 1000000 (upstream value wins)", e["context_length"])
	}
	if e["context_window"] != float64(1000000) {
		t.Errorf("context_window = %v, want mirrored 1000000", e["context_window"])
	}
	// Verbatim upstream extras survive untouched.
	if e["price_field"] != float64(7) {
		t.Errorf("price_field = %v, want verbatim 7", e["price_field"])
	}
}

// A dotted path reaches nested enrichment values, and an entry that already
// carries a mapped field is never overwritten by it.
func TestModelsEnrichmentNestedPathDoesNotClobber(t *testing.T) {
	models := `{"object":"list","data":[
		{"id":"m-1","context_length":1234},
		{"id":"m-2","context_length":5678,"max_output_tokens":99}
	]}`
	meta := `{"data":[
		{"id":"m-1","limits":{"max_output":8192}},
		{"id":"m-2","limits":{"max_output":16384}}
	]}`
	srv, upstream := modelsUpstreamFixture(t, models, "/meta", meta, map[string]string{
		"max_output_tokens": "limits.max_output",
	})
	_, doc := fetchModelsBody(t, srv, upstream.URL)
	entries := modelEntries(t, doc)
	if entries[0]["max_output_tokens"] != float64(8192) {
		t.Errorf("m-1 max_output_tokens = %v, want 8192 via dotted path", entries[0]["max_output_tokens"])
	}
	if entries[1]["max_output_tokens"] != float64(99) {
		t.Errorf("m-2 max_output_tokens = %v, want upstream's own 99 (never clobbered)", entries[1]["max_output_tokens"])
	}
}

// Construction normalization: a bare-array upstream (Together-style) is
// wrapped in the canonical envelope, missing core fields are backfilled,
// a Groq-style context_window is mirrored back to context_length, and an
// id-less entry is dropped rather than served to clients that require id.
func TestModelsConstructedNormalizes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[
			{"id":"m-1","context_window":4096},
			{"id":"m-2","object":"model","created":5,"owned_by":"them"},
			{"object":"model"}
		]`))
	}))
	defer upstream.Close()
	srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
	defer srv.Close()
	_, doc := fetchModelsBody(t, srv, upstream.URL)
	entries := modelEntries(t, doc)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2 (id-less dropped)", len(entries))
	}
	e1 := entries[0]
	if e1["object"] != "model" || e1["created"] != float64(0) || e1["owned_by"] == "" {
		t.Errorf("core backfill missing: %v", e1)
	}
	if e1["context_length"] != float64(4096) || e1["context_window"] != float64(4096) {
		t.Errorf("context mirror = %v/%v, want both 4096", e1["context_length"], e1["context_window"])
	}
	if e1["owned_by"] != "127.0.0.1" && !strings.HasPrefix(e1["owned_by"].(string), "127.0.0.1") {
		t.Errorf("owned_by backfill = %v, want the derived provider label", e1["owned_by"])
	}
	// A complete entry is untouched by normalization.
	if entries[1]["created"] != float64(5) || entries[1]["owned_by"] != "them" {
		t.Errorf("normalized an already-complete entry: %v", entries[1])
	}
}

// An enrichment fetch failure is fail-open: the base list is still served
// (normalized), never a 502 - enrichment is a completeness win, not a
// dependency.
func TestModelsEnrichmentFailOpen(t *testing.T) {
	models := `{"object":"list","data":[{"id":"m-1","context_length":100}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/meta" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.Write([]byte(models))
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderOverride{
		providerFromURL(u): {
			ModelsPath: "/meta",
			ModelsKeys: map[string]string{"input_modalities": "input_modalities"},
		},
	}
	srv := httptest.NewServer(New(cfg, metrics.Noop{}))
	defer srv.Close()
	code, doc := fetchModelsBody(t, srv, upstream.URL)
	if code != http.StatusOK {
		t.Fatalf("status = %d body %v", code, doc)
	}
	entries := modelEntries(t, doc)
	if _, has := entries[0]["input_modalities"]; has {
		t.Error("enrichment field present despite the enrichment endpoint failing")
	}
	if entries[0]["context_window"] != float64(100) {
		t.Error("context mirror lost on enrichment failure")
	}
}

// A body the proxy cannot recognize as a model list is relayed VERBATIM -
// never rewritten into a fabricated empty list, never a 502.
func TestModelsUnparseableVerbatim(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"models":{},"note":"unexpected shape"}`))
	}))
	defer upstream.Close()
	srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(raw)) != `{"models":{},"note":"unexpected shape"}` {
		t.Errorf("status %d body %s, want verbatim relay", resp.StatusCode, raw)
	}
}
