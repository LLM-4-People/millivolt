package metrics

import (
	"strings"
	"testing"
)

// BenchmarkBufferRecordRetentionNormal measures the unchanged common path:
// every field already fits, so the retention pass must add no allocation and
// no whole-record copy. The ring stores the same event-shaped record the
// dashboard benchmarks use.
func BenchmarkBufferRecordRetentionNormal(b *testing.B) {
	rec := benchmarkEventRecord()
	rec.ToolNames = []string{"read", "write"}
	buf := NewBuffer(1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Record(rec)
	}
}

// BenchmarkBufferRecordRetentionHostile measures the bounded path: one 1 MiB
// multi-byte value on every record text field, a 65-name tool list, and an
// attempt carrying the same hostile metadata. The input strings are built once
// outside the timer and restored each iteration, so the result is the
// retention work rather than fixture allocation.
func BenchmarkBufferRecordRetentionHostile(b *testing.B) {
	hostile := strings.Repeat("é", 1<<19)
	attempt := RetryAttempt{
		StatusCode: 429, ErrorType: hostile, ErrorCode: hostile, ErrorMsg: "rate limited",
		ProviderModel: hostile, ProviderRequestID: hostile, ProviderServer: hostile,
	}
	attempts := make([]RetryAttempt, 1)
	toolNames := make([]string, RetainedToolNamesMax+1)
	for i := range toolNames {
		toolNames[i] = "tool"
	}
	toolNames[0] = hostile
	rec := benchmarkEventRecord()

	buf := NewBuffer(1024)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.Path = hostile
		rec.Method = hostile
		rec.Model = hostile
		rec.ProviderModel = hostile
		rec.ProviderRequestID = hostile
		rec.ProviderServer = hostile
		rec.FinishReason = hostile
		rec.ErrorType = hostile
		rec.ErrorCode = hostile
		rec.DebugSessionID = hostile
		rec.ToolNames = toolNames
		attempts[0] = attempt
		rec.Attempts = attempts
		buf.Record(rec)
	}
}
