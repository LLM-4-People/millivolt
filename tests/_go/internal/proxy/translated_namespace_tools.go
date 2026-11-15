package proxy

// A Responses client that groups tools into namespaces (Codex does this for
// every MCP server) reaches a Chat-only upstream as flat functions, and the
// upstream's call returns to the client under its namespace.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

const namespaceProxyRequest = `{"model":"m","stream":false,"input":"hi","tools":[` +
	`{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{}}},` +
	`{"type":"namespace","name":"mcp__context7","tools":[` +
	`{"type":"function","name":"query_docs","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]},` +
	`{"type":"web_search"}]}`

const namespaceProxyChatResponse = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[` +
	`{"id":"call_1","type":"function","function":{"name":"mcp__context7__query_docs","arguments":"{\"q\":\"auth\"}"}}]},` +
	`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`

func TestTranslatedNamespaceTools(t *testing.T) {
	up := newFixtureUpstream(t, "application/json", 200, namespaceProxyChatResponse)
	cfg := config.Default()
	s := New(cfg, metrics.Noop{})
	s.client.Store(&http.Client{Transport: rewriteTransport{target: up.server.URL}})

	w := proxyRequest(t, s, "/v1/responses", "openai", namespaceProxyRequest)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}

	var sent struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(up.lastBody(), &sent); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	if len(sent.Tools) != 2 {
		t.Fatalf("upstream tool count = %d, want 2 (namespace member flattened, web_search dropped): %s",
			len(sent.Tools), up.lastBody())
	}
	if sent.Tools[1].Function.Name != "mcp__context7__query_docs" {
		t.Fatalf("upstream member name = %q", sent.Tools[1].Function.Name)
	}
	for _, tool := range sent.Tools {
		if tool.Type != "function" {
			t.Fatalf("upstream tool type = %q", tool.Type)
		}
	}
	if !strings.Contains(w.Header().Get("X-Proxy-Warnings"), "tool.web_search") {
		t.Fatalf("dropped provider tool was not reported: %q", w.Header().Get("X-Proxy-Warnings"))
	}

	var got struct {
		Output []struct {
			Type      string `json:"type"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode client body: %v", err)
	}
	if len(got.Output) != 1 {
		t.Fatalf("client output = %s", w.Body.String())
	}
	if got.Output[0].Name != "query_docs" || got.Output[0].Namespace != "mcp__context7" {
		t.Fatalf("client call lost its namespace: %+v", got.Output[0])
	}
}
