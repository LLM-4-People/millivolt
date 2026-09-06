package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// cstr/cmsg are defined in format_cursor.go in this test directory.

// TestCursorModelsEndpoint drives GET /v1/models (format=cursor) against a mock
// agent.v1 GetUsableModels h2c upstream and asserts the OpenAI list shape.
func TestCursorModelsEndpoint(t *testing.T) {
	// Build a GetUsableModelsResponse: repeated ModelDetails at field 1.
	// ModelDetails: model_id=1, thinking_details=2 (empty msg), display_name=4, max_mode=7.
	md := func(id, name string, thinking, maxMode bool) []byte {
		var m []byte
		m = append(m, cstr(1, id)...)
		if thinking {
			m = append(m, cmsg(2, nil)...)
		}
		m = append(m, cstr(4, name)...)
		if maxMode {
			m = append(m, cvint(7, 1)...)
		}
		return m
	}
	var resp []byte
	resp = append(resp, cmsg(1, md("claude-sonnet-5", "Claude Sonnet 5", true, true))...)
	resp = append(resp, cmsg(1, md("gpt-5.3-codex", "GPT-5.3 Codex", false, false))...)

	h2s := &http2.Server{}
	upstream := httptest.NewUnstartedServer(h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agent.v1.AgentService/GetUsableModels" {
			t.Errorf("path = %q, want GetUsableModels", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/proto" {
			t.Errorf("Content-Type = %q, want application/proto (unary raw proto)", ct)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer cursor-key" {
			t.Errorf("Authorization = %q", got)
		}
		io.ReadAll(r.Body) // GetUsableModelsRequest (empty)
		w.Header().Set("Content-Type", "application/proto")
		w.Write(resp)
	}), h2s))
	upstream.EnableHTTP2 = true
	upstream.Start()
	defer upstream.Close()

	srv := httptest.NewServer(New(cursorTestCfg(upstream.URL), metrics.NewBuffer(100)))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer cursor-key")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Format", "cursor")

	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(resp2.Body)

	var out struct {
		Object string `json:"object"`
		Data   []struct {
			ID          string `json:"id"`
			Object      string `json:"object"`
			DisplayName string `json:"display_name"`
			Reasoning   bool   `json:"reasoning"`
			MaxMode     bool   `json:"max_mode"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("response is not an OpenAI models list: %v (%s)", err, body)
	}
	if out.Object != "list" {
		t.Errorf("object = %q, want list", out.Object)
	}
	if len(out.Data) != 2 {
		t.Fatalf("expected 2 models, got %d: %s", len(out.Data), body)
	}
	if out.Data[0].ID != "claude-sonnet-5" || out.Data[0].DisplayName != "Claude Sonnet 5" {
		t.Errorf("model[0] wrong: %+v", out.Data[0])
	}
	if !out.Data[0].Reasoning || !out.Data[0].MaxMode {
		t.Errorf("model[0] capabilities wrong: %+v", out.Data[0])
	}
	if out.Data[1].ID != "gpt-5.3-codex" || out.Data[1].Reasoning {
		t.Errorf("model[1] wrong: %+v", out.Data[1])
	}
	if !strings.Contains(string(body), `"owned_by":"cursor"`) {
		t.Errorf("owned_by missing: %s", body)
	}
}
