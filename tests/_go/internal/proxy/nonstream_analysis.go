package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// legacyClassifyNonStreamBody is the frozen pre-analysis classifier. The
// equivalence test compares the new single-decode outcome against it, so a
// classification drift is caught independently of the production wrapper.
func legacyClassifyNonStreamBody(body []byte) string {
	var raw struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content       json.RawMessage `json:"content"`
				Reasoning     json.RawMessage `json:"reasoning_content"`
				ReasoningBare json.RawMessage `json:"reasoning"`
				ReasoningDet  json.RawMessage `json:"reasoning_details"`
				ReasoningText json.RawMessage `json:"reasoning_text"`
				ToolCalls     []struct{}      `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	if len(raw.Choices) == 0 {
		return ""
	}
	ch := raw.Choices[0]
	hadAnswer := legacyContentPresent(ch.Message.Content)
	hadReasoning := legacyContentPresent(ch.Message.Reasoning) ||
		legacyContentPresent(ch.Message.ReasoningBare) ||
		legacyContentPresent(ch.Message.ReasoningDet) ||
		legacyContentPresent(ch.Message.ReasoningText)
	return metrics.ClassifyOutcome(ch.FinishReason, true, hadAnswer, hadReasoning, false, len(ch.Message.ToolCalls), true)
}

// legacyContentPresent is the frozen presence predicate the classifier used.
func legacyContentPresent(content json.RawMessage) bool {
	t := bytes.TrimLeft(content, " \t\r\n")
	if len(t) == 0 {
		return false
	}
	switch t[0] {
	case '"':
		var s string
		if json.Unmarshal(t, &s) == nil {
			return s != ""
		}
	case '[':
		var parts []json.RawMessage
		if json.Unmarshal(t, &parts) == nil {
			return len(parts) > 0
		}
	case 'n':
		return false
	default:
		return true
	}
	return false
}

// legacyExtractUsageBytes is the frozen pre-analysis record extractor.
func legacyExtractUsageBytes(body []byte, rec *metrics.Record, usageKeys map[string]string, preview bool) {
	var raw struct {
		Usage   map[string]any `json:"usage"`
		Model   string         `json:"model"`
		ID      string         `json:"id"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role string `json:"role"`
				// Content can be a string or an array of parts.
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return
	}
	if raw.Usage != nil {
		rec.Usage = metrics.ParseUsage(raw.Usage, usageKeys)
	}
	if raw.Model != "" && rec.ProviderModel == "" {
		rec.ProviderModel = raw.Model
	}
	if raw.ID != "" && rec.ProviderRequestID == "" {
		rec.ProviderRequestID = raw.ID
	}
	if len(raw.Choices) > 0 {
		if raw.Choices[0].FinishReason != "" {
			rec.FinishReason = raw.Choices[0].FinishReason
		}
		if s, ok := flattenContent(raw.Choices[0].Message.Content); ok && s != "" {
			rec.HadAnswerContent = true
			if preview && rec.ResponsePreview == "" {
				rec.ResponsePreview = metrics.TruncatePreview(s)
			}
		}
		if n := len(raw.Choices[0].Message.ToolCalls); n > 0 {
			rec.ToolCalls = n
			names := make([]string, 0, n)
			for _, tc := range raw.Choices[0].Message.ToolCalls {
				if tc.Function.Name != "" {
					names = append(names, tc.Function.Name)
				}
			}
			rec.ToolNames = names
		}
	}
}

// legacyAnalyzeNonStreamBytes is the frozen pre-analysis commit/extraction
// body: the shared record assignment sequence the new apply owner must match.
func legacyAnalyzeNonStreamBytes(buf []byte, rec *metrics.Record, usageKeys map[string]string, costKeys []string, preview bool) {
	if len(buf) == 0 {
		return
	}
	if rec.Usage.OutputTokens == 0 {
		legacyExtractUsageBytes(buf, rec, usageKeys, preview)
	}
	if cost, ok := metrics.ExtractCost(buf, costKeys); ok {
		rec.Cost = cost
	}
	if rec.ErrorType == "" && metrics.HasErrorKey(buf) {
		if typ, code, msg := metrics.ParseErrorEnvelope(buf); typ != "" {
			rec.ErrorType, rec.ErrorCode, rec.ErrorMsg = typ, code, msg
		}
	}
}

// TestApplyNonStreamAnalysisEquivalence verifies that the new analysis applied
// to a record is field-for-field identical to the frozen legacy classifier
// plus record extraction, including its only-when-empty and OutputTokens gates.
func TestApplyNonStreamAnalysisEquivalence(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		usageKeys map[string]string
		costKeys  []string
		preview   bool
		initial   metrics.Record
	}{
		{
			name:    "content usage model id",
			body:    `{"id":"chat-1","model":"gpt-x","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
			preview: true,
		},
		{
			name:    "multimodal parts",
			body:    `{"choices":[{"finish_reason":"stop","message":{"content":[{"type":"text","text":"hel"},{"type":"image_url","image_url":{"url":"x"}},{"type":"text","text":"lo"}]}}]}`,
			preview: true,
		},
		{
			name:    "content null tool calls",
			body:    `{"id":"chat-2","choices":[{"finish_reason":"tool_calls","message":{"content":null,"tool_calls":[{"id":"c1","function":{"name":"lookup"}},{"id":"c2","function":{"name":"read"}}]}}]}`,
			preview: true,
		},
		{
			name:    "reasoning only",
			body:    `{"choices":[{"finish_reason":"stop","message":{"content":null,"reasoning_content":"thought"}}]}`,
			preview: true,
		},
		{
			name:    "error envelope",
			body:    `{"error":{"type":"server_error","code":503,"message":"boom"}}`,
			preview: true,
		},
		{
			name:    "error decoy leaves record clean",
			body:    `{"id":"chat-3","choices":[{"finish_reason":"stop","message":{"content":""}}],"error":null}`,
			preview: true,
		},
		{
			name:      "provider usage and cost overrides",
			body:      `{"id":"chat-4","model":"m","choices":[{"finish_reason":"stop","message":{"content":"hi"}}],"usage":{"x":{"in":7},"completion_tokens":3},"billing":{"amount":0.03}}`,
			usageKeys: map[string]string{"input_tokens": "x.in"},
			costKeys:  []string{"billing.amount"},
			preview:   true,
		},
		{
			name:    "malformed json applies nothing",
			body:    `{"choices":[{"message":{"content":"hi"}`,
			preview: true,
		},
		{
			name:    "usage wrong type suppresses record fields",
			body:    `{"id":"chat-5","model":"m","choices":[{"finish_reason":"stop","message":{"content":"hi"}}],"usage":"bad"}`,
			preview: true,
		},
		{
			name:    "existing model id preview error survive only-when-empty",
			body:    `{"id":"new-id","model":"new-model","choices":[{"finish_reason":"stop","message":{"content":"new preview"}}],"cost":0.2,"error":null}`,
			preview: true,
			initial: metrics.Record{
				ProviderModel:     "old-model",
				ProviderRequestID: "old-id",
				ResponsePreview:   "old preview",
				ErrorType:         "existing_error",
				Cost:              0.1,
			},
		},
		{
			name:    "output tokens nonzero keeps extraction gate closed",
			body:    `{"id":"new-id","model":"new-model","choices":[{"finish_reason":"stop","message":{"content":"new"}}],"usage":{"prompt_tokens":99,"completion_tokens":88},"cost":0.2}`,
			preview: true,
			initial: metrics.Record{Usage: metrics.Usage{InputTokens: 7, OutputTokens: 9}},
		},
		{
			name:    "usage absent preserves prior usage",
			body:    `{"choices":[{"finish_reason":"stop","message":{"content":"hi"}}]}`,
			preview: true,
			initial: metrics.Record{Usage: metrics.Usage{InputTokens: 7}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := c.initial
			legacyAnalyzeNonStreamBytes([]byte(c.body), &want, c.usageKeys, c.costKeys, c.preview)

			got := c.initial
			analysis := metrics.AnalyzeNonStreamBody([]byte(c.body), c.usageKeys, c.costKeys, c.preview)
			applyNonStreamAnalysis(&got, analysis)

			if !reflect.DeepEqual(got, want) {
				t.Fatalf("record mismatch\n got: %+v\nwant: %+v", got, want)
			}
			if wantOutcome := legacyClassifyNonStreamBody([]byte(c.body)); analysis.Outcome != wantOutcome {
				t.Fatalf("outcome = %q, want legacy %q", analysis.Outcome, wantOutcome)
			}
		})
	}
}

// TestNonStreamSpooledUsesOneAnalysis runs one healthy spooled non-stream
// request through the relay and compares its record fields with the frozen
// legacy extractor. The analysis seam must be entered exactly once: the
// quality gate and the commit share one decoded view, never a second pass.
func TestNonStreamSpooledUsesOneAnalysis(t *testing.T) {
	const upstreamBody = `{"id":"chatcmpl-regression","object":"chat.completion","model":"gpt-regression","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"I will look","reasoning_content":"thinking","tool_calls":[{"id":"call_a","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	var analyses atomic.Int64
	original := nonStreamAnalysis
	nonStreamAnalysis = func(body []byte, usageKeys map[string]string, costKeys []string, preview bool) metrics.NonStreamAnalysis {
		analyses.Add(1)
		return metrics.AnalyzeNonStreamBody(body, usageKeys, costKeys, preview)
	}
	defer func() { nonStreamAnalysis = original }()

	cfg := config.Default()
	cfg.CaptureBodyPreview = true
	buf := metrics.NewBuffer(10)
	srv := httptest.NewServer(New(cfg, buf))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-k")
	req.Header.Set("X-Proxy-Base-URL", upstream.URL)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != upstreamBody {
		t.Fatalf("status/body = %d/%q, want 200 and the verbatim upstream body", resp.StatusCode, body)
	}
	if got := analyses.Load(); got != 1 {
		t.Fatalf("analysis calls = %d, want 1 (quality gate and commit share one decode)", got)
	}

	rec := waitForRecord(t, buf, 1)[0]
	if rec.StatusCode != http.StatusOK {
		t.Fatalf("record status = %d, want 200", rec.StatusCode)
	}
	if rec.ResponseBytes != int64(len(upstreamBody)) {
		t.Fatalf("response bytes = %d, want %d", rec.ResponseBytes, len(upstreamBody))
	}

	var want metrics.Record
	legacyAnalyzeNonStreamBytes([]byte(upstreamBody), &want, nil, nil, true)
	type derived struct {
		Usage             metrics.Usage
		ProviderModel     string
		ProviderRequestID string
		FinishReason      string
		HadAnswerContent  bool
		ToolCalls         int
		ToolNames         []string
		ResponsePreview   string
		Cost              float64
		ErrorType         string
		ErrorCode         string
		ErrorMsg          string
	}
	got := derived{
		Usage:             rec.Usage,
		ProviderModel:     rec.ProviderModel,
		ProviderRequestID: rec.ProviderRequestID,
		FinishReason:      rec.FinishReason,
		HadAnswerContent:  rec.HadAnswerContent,
		ToolCalls:         rec.ToolCalls,
		ToolNames:         rec.ToolNames,
		ResponsePreview:   rec.ResponsePreview,
		Cost:              rec.Cost,
		ErrorType:         rec.ErrorType,
		ErrorCode:         rec.ErrorCode,
		ErrorMsg:          rec.ErrorMsg,
	}
	expected := derived{
		Usage:             want.Usage,
		ProviderModel:     want.ProviderModel,
		ProviderRequestID: want.ProviderRequestID,
		FinishReason:      want.FinishReason,
		HadAnswerContent:  want.HadAnswerContent,
		ToolCalls:         want.ToolCalls,
		ToolNames:         want.ToolNames,
		ResponsePreview:   want.ResponsePreview,
		Cost:              want.Cost,
		ErrorType:         want.ErrorType,
		ErrorCode:         want.ErrorCode,
		ErrorMsg:          want.ErrorMsg,
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("request record differs from legacy extraction\n got: %+v\nwant: %+v", got, expected)
	}
}
