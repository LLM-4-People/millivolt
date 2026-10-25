package sse

// F1 regression: official audio carriers are answer presence. A non-empty
// Chat audio transcript is answer content; audio data is answer-shaped output
// (never HadAnswerContent); the Responses transcript delta is content while
// the audio delta is generic output; a terminal output_audio message part
// counts its transcript as text and its data as output.

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestAnalyzerAudioPresence(t *testing.T) {
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

	t.Run("chat transcript is answer content", func(t *testing.T) {
		var a Analyzer
		a.CapturePreview = true
		a.Feed([]byte(`data: {"choices":[{"delta":{"role":"assistant"}}]}`), now)
		a.Feed([]byte(`data: {"choices":[{"delta":{"audio":{"id":"audio_1","expires_at":1,"data":"QUJD","transcript":"spoken answer"}}}]}`), now)
		a.Feed([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`), now)
		a.Feed([]byte(`data: [DONE]`), now)
		if a.firstAnswerAt.IsZero() {
			t.Fatal("a non-empty audio transcript must mark answer content")
		}
		if string(a.preview) != "spoken answer" {
			t.Fatalf("preview = %q, want the audio transcript", a.preview)
		}
		if got := a.OutcomeCode(); got != "" {
			t.Fatalf("outcome = %q, want a healthy audio completion", got)
		}
		var rec metrics.Record
		a.Fill(&rec)
		if !rec.HadAnswerContent || rec.ErrorCode != "" {
			t.Fatalf("record = %+v, want answer content and no error", rec)
		}
	})

	t.Run("chat audio data is answer-shaped output", func(t *testing.T) {
		var a Analyzer
		a.CapturePreview = true
		a.Feed([]byte(`data: {"choices":[{"delta":{"audio":{"id":"audio_1","expires_at":1,"data":"QUJD"}}}]}`), now)
		if !a.firstAnswerAt.IsZero() {
			t.Fatal("audio data must not mark answer content")
		}
		if !a.answerishSeen {
			t.Fatal("audio data must be answer-shaped output")
		}
		a.Feed([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`), now)
		a.Feed([]byte(`data: [DONE]`), now)
		if got := a.OutcomeCode(); got != "" {
			t.Fatalf("outcome = %q, want a healthy audio-data completion", got)
		}
		var rec metrics.Record
		a.Fill(&rec)
		if rec.HadAnswerContent || rec.ErrorCode != "" {
			t.Fatalf("record = %+v, want no content and no error", rec)
		}
	})

	t.Run("chat audio object without payload is not output", func(t *testing.T) {
		var a Analyzer
		a.Feed([]byte(`data: {"choices":[{"delta":{"audio":{"id":"audio_1","expires_at":1}}}]}`), now)
		if a.firstTokenAt != (time.Time{}) || a.answerishSeen {
			t.Fatal("an audio object carrying only identity/expiry is not presence")
		}
	})

	t.Run("responses transcript delta is answer content", func(t *testing.T) {
		var a Analyzer
		a.CapturePreview = true
		a.Feed([]byte(`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`), now)
		a.Feed([]byte(`data: {"type":"response.audio.transcript.delta","delta":"spoken answer","output_index":0,"content_index":0}`), now)
		a.Feed([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`), now)
		if a.firstAnswerAt.IsZero() {
			t.Fatal("a Responses transcript delta must mark answer content")
		}
		if string(a.preview) != "spoken answer" {
			t.Fatalf("preview = %q, want the transcript delta", a.preview)
		}
		if got := a.OutcomeCode(); got != "" {
			t.Fatalf("outcome = %q, want a healthy transcript completion", got)
		}
	})

	t.Run("responses audio delta is output presence", func(t *testing.T) {
		var a Analyzer
		a.Feed([]byte(`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`), now)
		a.Feed([]byte(`data: {"type":"response.audio.delta","delta":"QUJD","output_index":0,"content_index":0}`), now)
		a.Feed([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}`), now)
		if !a.firstAnswerAt.IsZero() {
			t.Fatal("audio data must not mark answer content")
		}
		if !a.responsesOutputSeen {
			t.Fatal("a non-empty audio delta must mark Responses output presence")
		}
		if got := a.OutcomeCode(); got != "" {
			t.Fatalf("outcome = %q, want a healthy audio-data completion", got)
		}
	})

	t.Run("responses terminal audio message content", func(t *testing.T) {
		var a Analyzer
		a.CapturePreview = true
		a.Feed([]byte(`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`), now)
		a.Feed([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_audio","transcript":"spoken answer","data":"QUJD"}]}]}}`), now)
		if a.firstAnswerAt.IsZero() {
			t.Fatal("a terminal output_audio transcript must mark answer content")
		}
		if string(a.preview) != "spoken answer" {
			t.Fatalf("preview = %q, want the terminal transcript", a.preview)
		}
		if !a.responsesOutputSeen {
			t.Fatal("terminal audio data must mark output presence")
		}
		if got := a.OutcomeCode(); got != "" {
			t.Fatalf("outcome = %q, want a healthy terminal audio completion", got)
		}
	})

	t.Run("responses terminal audio data without transcript", func(t *testing.T) {
		var a Analyzer
		a.Feed([]byte(`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`), now)
		a.Feed([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_audio","data":"QUJD"}]}]}}`), now)
		if !a.firstAnswerAt.IsZero() {
			t.Fatal("audio data without a transcript is not answer content")
		}
		if !a.responsesOutputSeen {
			t.Fatal("terminal audio data must mark output presence")
		}
		if got := a.OutcomeCode(); got != "" {
			t.Fatalf("outcome = %q, want a healthy terminal audio-data completion", got)
		}
	})
}

// TestAnalyzerAudioBlocksTruncationRescue pins the rescue control: audio
// transcript or data already delivered is visible output, so a truncated
// stream must stay client-retryable rather than be re-sent.
func TestAnalyzerAudioBlocksTruncationRescue(t *testing.T) {
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	for _, tc := range []struct {
		name string
		line string
	}{
		{"transcript", `data: {"choices":[{"delta":{"audio":{"transcript":"spoken"}}}]}`},
		{"data", `data: {"choices":[{"delta":{"audio":{"data":"QUJD"}}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var a Analyzer
			a.Feed([]byte(`data: {"choices":[{"delta":{"role":"assistant"}}]}`), now)
			a.Feed([]byte(tc.line), now)
			if a.RetryableTruncation() {
				t.Fatal("delivered audio must not be re-sent as a truncation")
			}
			if a.RetryableReasoningTruncation() {
				t.Fatal("delivered audio must not enter the reasoning rescue")
			}
		})
	}
}
