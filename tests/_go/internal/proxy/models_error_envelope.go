package proxy

// Round 11 D1 regression: /v1/models is the OpenAI discovery route, so its
// early error sinks must stay OpenAI-shaped even when X-Proxy-In-Format names
// an inference client wire.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func modelsErrorBody(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("status %d body is not JSON: %s", resp.StatusCode, raw)
	}
	return doc
}

func assertOpenAIModelsError(t *testing.T, doc map[string]any) {
	t.Helper()
	if doc["type"] == "error" {
		t.Fatalf("discovery error used the Anthropic envelope: %+v", doc)
	}
	if _, ok := doc["error"].(map[string]any); !ok {
		t.Fatalf("discovery error has no OpenAI error object: %+v", doc)
	}
}

func TestModelsDiscoveryErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(New(config.Default(), metrics.Noop{}))
	defer srv.Close()

	t.Run("unroutable discovery request", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
		req.Header.Set("X-Proxy-In-Format", "anthropic")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", resp.StatusCode)
		}
		assertOpenAIModelsError(t, modelsErrorBody(t, resp))
	})

	t.Run("discovery upstream failure", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":{"message":"nope","type":"server_error"}}`))
		}))
		defer upstream.Close()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
		req.Header.Set("X-Proxy-In-Format", "anthropic")
		req.Header.Set("X-Proxy-Base-URL", upstream.URL)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502", resp.StatusCode)
		}
		assertOpenAIModelsError(t, modelsErrorBody(t, resp))
	})
}
