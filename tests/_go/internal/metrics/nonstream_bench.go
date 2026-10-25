package metrics

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// legacyNonStreamClassify and legacyNonStreamExtract are the two full-body
// typed decodes the pre-analysis relay performed. The benchmark keeps them
// beside the new owner so the duplicate work is measured, not assumed.
type legacyNonStreamClassify struct {
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

type legacyNonStreamExtract struct {
	Usage   map[string]any `json:"usage"`
	Model   string         `json:"model"`
	ID      string         `json:"id"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
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

var (
	benchmarkOutcome string
	benchmarkUsage   Usage
	benchmarkCost    float64
	benchmarkPreview string
)

func benchmarkChatBody(contentBytes int) []byte {
	content := strings.Repeat("a", contentBytes)
	return []byte(fmt.Sprintf(`{"id":"chatcmpl-bench","object":"chat.completion","model":"bench-model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"%s"}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`, content))
}

// benchmarkCostChatBody keeps the same shape as benchmarkChatBody but carries
// a reported USD cost, so ExtractCost's key gate opens and both decoders pay
// the cost-bearing full-body path instead of returning immediately.
func benchmarkCostChatBody(contentBytes int) []byte {
	content := strings.Repeat("a", contentBytes)
	return []byte(fmt.Sprintf(`{"id":"chatcmpl-bench","object":"chat.completion","model":"bench-model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"%s"}}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150,"cost":0.42}}`, content))
}

func benchmarkLegacyNonStream(body []byte, preview bool) (string, Usage, float64, string) {
	var classify legacyNonStreamClassify
	_ = json.Unmarshal(body, &classify)
	var extract legacyNonStreamExtract
	_ = json.Unmarshal(body, &extract)
	var usage Usage
	if extract.Usage != nil {
		usage = ParseUsage(extract.Usage, nil)
	}
	previewText := ""
	if len(extract.Choices) > 0 {
		// The legacy extractor flattened content on every body, independent
		// of the preview capture gate; omitting it would make the old path
		// look cheaper than it was.
		if s, ok := FlattenContent(extract.Choices[0].Message.Content); ok && s != "" && preview {
			previewText = TruncatePreview(s)
		}
	}
	cost, _ := ExtractCost(body, nil)
	outcome := ""
	if len(classify.Choices) > 0 {
		ch := classify.Choices[0]
		outcome = ClassifyOutcome(ch.FinishReason, true, contentPresent(ch.Message.Content), false, false, len(ch.Message.ToolCalls), true)
	}
	return outcome, usage, cost, previewText
}

// benchmarkNonStreamBodyPair measures the legacy two-decode relay path beside
// the single-decode analysis owner for one body shape.
func benchmarkNonStreamBodyPair(b *testing.B, name string, body []byte) {
	b.Run(name+"/legacy-two-decode", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		for b.Loop() {
			outcome, usage, cost, preview := benchmarkLegacyNonStream(body, false)
			benchmarkOutcome, benchmarkUsage, benchmarkCost, benchmarkPreview = outcome, usage, cost, preview
		}
	})
	b.Run(name+"/analysis-one-decode", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(body)))
		for b.Loop() {
			analysis := AnalyzeNonStreamBody(body, nil, nil, false)
			benchmarkOutcome, benchmarkUsage, benchmarkCost, benchmarkPreview = analysis.Outcome, analysis.Usage, analysis.Cost, analysis.Preview
		}
	})
}

func BenchmarkNonStreamAnalysis(b *testing.B) {
	for _, size := range []struct {
		name  string
		bytes int
	}{
		{"64KiB", 64 << 10},
		{"1MiB", 1 << 20},
	} {
		benchmarkNonStreamBodyPair(b, size.name, benchmarkChatBody(size.bytes))
		costBody := benchmarkCostChatBody(size.bytes)
		if cost, ok := ExtractCost(costBody, nil); !ok || cost != 0.42 {
			b.Fatalf("cost fixture does not carry a reported cost: cost=%v ok=%v", cost, ok)
		}
		benchmarkNonStreamBodyPair(b, size.name+"-cost", costBody)
	}
}
