package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestFormatParseOpenAIBodyIsAWire400 pins the shared errParseOpenAIBody wrap
// on its two live adapter paths: a client body that is not JSON must surface
// as 400 invalid_request_error with the "parse openai body: " prefix (the
// single wording owner in format/models.go), never an opaque 502 - through
// the anthropic request translator (translateRequest) and the cursor run
// translator (serveCursorBidi) alike. The upstream is never contacted: both
// adapters decode before any upstream send.
func TestFormatParseOpenAIBodyIsAWire400(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format string
	}{
		{"anthropic", "anthropic"},
		{"cursor", "cursor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(config.Default(), metrics.Noop{})
			r := httptest.NewRequest(http.MethodPost, "http://proxy/v1/chat/completions",
				strings.NewReader(`{"messages":[`))
			r.Header.Set("Authorization", "Bearer sk-k")
			r.Header.Set("X-Proxy-Base-URL", "http://127.0.0.1:9")
			r.Header.Set("X-Proxy-Format", tc.format)
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, r)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
			}
			var doc struct {
				Error struct {
					Message string `json:"message"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("400 body is not the OpenAI error envelope: %v (%s)", err, rec.Body.String())
			}
			if doc.Error.Type != typeInvalidRequestError {
				t.Fatalf("error type = %q, want %q", doc.Error.Type, typeInvalidRequestError)
			}
			const wrap = "parse openai body: "
			if !strings.HasPrefix(doc.Error.Message, wrap) || len(doc.Error.Message) == len(wrap) {
				t.Fatalf("message = %q, want the %q wrap carrying the decode cause", doc.Error.Message, wrap)
			}
		})
	}
}

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
