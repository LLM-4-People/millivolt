package proxy

import (
	"reflect"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestNonStreamNonStringRoleSuppressesRecordFields pins parity with the legacy
// typed extraction: a non-string message.role anywhere in choices cleared every
// record field while the raw classifier still produced an outcome. The shared
// apply owner must therefore leave the record untouched but keep the outcome.
func TestNonStreamNonStringRoleSuppressesRecordFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			name: "first choice",
			body: `{"id":"chat-role","model":"m","choices":[{"finish_reason":"stop","message":{"role":123,"content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		},
		{
			name: "later choice",
			body: `{"id":"chat-role","model":"m","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}},{"finish_reason":"stop","message":{"role":123,"content":"later"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			analysis := metrics.AnalyzeNonStreamBody([]byte(tc.body), nil, nil, false)
			if analysis.RecordFieldsOK {
				t.Fatal("RecordFieldsOK = true, want false for a non-string role")
			}
			if got, want := analysis.Outcome, legacyClassifyNonStreamBody([]byte(tc.body)); got != want {
				t.Fatalf("outcome = %q, want legacy %q", got, want)
			}
			var rec metrics.Record
			applyNonStreamAnalysis(&rec, analysis)
			if !reflect.DeepEqual(rec, metrics.Record{}) {
				t.Fatalf("record fields applied despite a non-string role: %+v", rec)
			}
		})
	}

	const normalRole = `{"id":"chat-role","model":"m","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`
	analysis := metrics.AnalyzeNonStreamBody([]byte(normalRole), nil, nil, false)
	if !analysis.RecordFieldsOK {
		t.Fatal("RecordFieldsOK = false for a normal string role")
	}
	if got, want := analysis.Outcome, legacyClassifyNonStreamBody([]byte(normalRole)); got != want {
		t.Fatalf("outcome = %q, want legacy %q", got, want)
	}
	var rec metrics.Record
	applyNonStreamAnalysis(&rec, analysis)
	if rec.ProviderModel != "m" || rec.ProviderRequestID != "chat-role" ||
		rec.FinishReason != "stop" || !rec.HadAnswerContent ||
		rec.Usage != (metrics.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15}) {
		t.Fatalf("normal role record fields = %+v", rec)
	}
}
