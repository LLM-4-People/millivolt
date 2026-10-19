package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestNonStreamAnswerShapedBodiesRelayWithoutRetry pins the non-streaming
// classifier's answer-shaped forms: a refusal-only message, a deprecated
// message.function_call declaration and a legacy /v1/completions
// choices[].text body are healthy responses, not empty_completion voids. Any
// regression would consume the quality budget and answer 502 after the
// retries, so the test drives the real relay with a fast fixture and requires
// exactly one upstream call.
func TestNonStreamAnswerShapedBodiesRelayWithoutRetry(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		requestBody string
		body        string
		answer      bool
	}{
		{
			name:        "refusal only",
			path:        "/v1/chat/completions",
			requestBody: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			body:        `{"id":"chat-refusal","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":null,"refusal":"I cannot help with that"}}]}`,
		},
		{
			name:        "deprecated function_call",
			path:        "/v1/chat/completions",
			requestBody: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			body:        `{"id":"chat-function","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":null,"function_call":{"name":"lookup","arguments":"{}"}}}]}`,
		},
		{
			name:        "legacy completions text",
			path:        "/v1/completions",
			requestBody: `{"model":"m","prompt":"hi"}`,
			body:        `{"id":"cmpl-text","object":"text_completion","model":"m","choices":[{"index":0,"finish_reason":"stop","text":"hello from completions"}]}`,
			answer:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				io.WriteString(w, tc.body)
			}))
			defer upstream.Close()

			cfg := config.Default()
			cfg.QualityRetries = 2 // plenty to absorb a regression, then answer 502
			cfg.CaptureBodyPreview = true
			buf := metrics.NewBuffer(10)
			srv := httptest.NewServer(New(cfg, buf))
			defer srv.Close()

			req, err := http.NewRequest(http.MethodPost, srv.URL+tc.path, strings.NewReader(tc.requestBody))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer sk-k")
			req.Header.Set("X-Proxy-Base-URL", upstream.URL)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
			}
			if string(body) != tc.body {
				t.Fatalf("client body = %q, want the verbatim upstream body", body)
			}
			if n := calls.Load(); n != 1 {
				t.Fatalf("upstream calls = %d, want 1 (healthy body must not be retried)", n)
			}
			rec := waitForRecord(t, buf, 1)[0]
			if rec.IsError() || rec.Retries != 0 || len(rec.Attempts) != 0 {
				t.Fatalf("record = %+v, want a healthy first attempt", rec)
			}
			if rec.FinishReason != "stop" {
				t.Fatalf("finish reason = %q, want stop", rec.FinishReason)
			}
			if rec.HadAnswerContent != tc.answer {
				t.Fatalf("HadAnswerContent = %v, want %v", rec.HadAnswerContent, tc.answer)
			}
			if tc.answer && rec.ResponsePreview != "hello from completions" {
				t.Fatalf("preview = %q, want the completions text", rec.ResponsePreview)
			}
		})
	}
}
