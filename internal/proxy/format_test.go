package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnthropicFormatTranslation(t *testing.T) {
	// Mock upstream speaks native Anthropic Messages.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "sk-ant" {
			t.Errorf("x-api-key = %q, want sk-ant", r.Header.Get("x-api-key"))
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"max_tokens"`) {
			t.Errorf("expected translated body with max_tokens, got %s", body)
		}
		if strings.Contains(string(body), `"max_completion_tokens"`) {
			t.Errorf("openai field leaked: %s", body)
		}
		// Non-streaming response in Anthropic format.
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","model":"claude","role":"assistant","content":[{"type":"text","text":"bonjour"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	buf := metrics.NewBuffer(100)
	srv := httptest.NewServer(New(config.Default(), buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"claude","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":50}`))
	req.Header.Set("Authorization", "Bearer sk-ant")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)
	req.Header.Set("X-Proxy-Auth-Header", "x-api-key")
	req.Header.Set("X-Proxy-Auth-Prefix", "")
	req.Header.Set("X-Proxy-Path", "/messages")
	req.Header.Set("X-Proxy-Format", "anthropic")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	got := string(body)
	if !strings.Contains(got, `"object":"chat.completion"`) {
		t.Errorf("response not translated to OpenAI format: %s", got)
	}
	if !strings.Contains(got, `"content":"bonjour"`) {
		t.Errorf("content missing: %s", got)
	}
	if !strings.Contains(got, `"finish_reason":"stop"`) {
		t.Errorf("finish_reason missing: %s", got)
	}
}
