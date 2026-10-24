package proxy

// Round 12 F2 regression: a native Anthropic or Responses body the codec
// cannot represent still applies its parsed folded usage. The later
// Chat-shaped fallback would otherwise read raw counters as Chat input/output.

import (
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func anthropicManyPartsBody(parts int, usage string) []byte {
	var b strings.Builder
	b.WriteString(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[`)
	for i := 0; i < parts; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"type":"text","text":"p%d"}`, i)
	}
	b.WriteString(`],"stop_reason":"end_turn","usage":`)
	b.WriteString(usage)
	b.WriteByte('}')
	return []byte(b.String())
}

func TestNativeDecodeFailureKeepsFoldedUsage(t *testing.T) {
	s := New(config.Default(), metrics.Noop{})
	target := &target{inbound: ir.Anthropic, outbound: ir.Anthropic}
	usage := `{"input_tokens":100,"cache_read_input_tokens":50,"cache_creation_input_tokens":20,"output_tokens":10}`

	rec := &metrics.Record{}
	s.applyNativeNonChatStructure(rec, target, anthropicManyPartsBody(4097, usage))
	if rec.UsageInvalid {
		t.Fatalf("folded usage was invalidated: %+v", rec)
	}
	if rec.Usage.InputTokens != 170 || rec.Usage.OutputTokens != 10 || rec.Usage.TotalTokens != 180 ||
		rec.Usage.CacheReadTokens != 50 || rec.Usage.CacheWrite != 20 {
		t.Fatalf("usage = %+v, want folded 170/10/180 cache 50/20", rec.Usage)
	}
	if got := settleTokens(rec); got != 180 {
		t.Fatalf("settleTokens = %d, want 180", got)
	}

	// A 4096-part body still decodes structurally and keeps the same fold.
	rec = &metrics.Record{}
	s.applyNativeNonChatStructure(rec, target, anthropicManyPartsBody(4096, usage))
	if rec.Usage.InputTokens != 170 || rec.Usage.OutputTokens != 10 || rec.Usage.TotalTokens != 180 {
		t.Fatalf("decodable control usage = %+v, want 170/10/180", rec.Usage)
	}

	// An unrepresentable sum is invalid, not silently replaced by the raw view.
	rec = &metrics.Record{}
	s.applyNativeNonChatStructure(rec, target, anthropicManyPartsBody(4097,
		`{"input_tokens":5000000000000000000,"cache_read_input_tokens":5000000000000000000,"output_tokens":10}`))
	if !rec.UsageInvalid || rec.Usage != (metrics.Usage{}) {
		t.Fatalf("overflowing usage = %+v invalid=%v, want an invalidated zero usage", rec.Usage, rec.UsageInvalid)
	}
	if got := settleTokens(rec); got != -1 {
		t.Fatalf("settleTokens = %d, want unavailable", got)
	}
}
