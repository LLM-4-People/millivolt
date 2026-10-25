package format

// Round 9 C3 regression: terminal reconciliation must scan only the parts
// owned by the terminal item. The owner index keeps that scan proportional to
// the item's own parts instead of the whole stream.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesTerminalReconciliationKeepsOwnedParts(t *testing.T) {
	source := terminalStreamCreated + `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[]}}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"alpha"}]}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"reasoning","id":"rs_2","summary":[]}}

event: response.reasoning_summary_part.added
data: {"type":"response.reasoning_summary_part.added","output_index":1,"summary_index":0,"part":{"type":"summary_text","text":""}}

event: response.reasoning_summary_text.delta
data: {"type":"response.reasoning_summary_text.delta","output_index":1,"summary_index":0,"delta":"beta"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":1,"item":{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"beta"}]}}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"alpha"}]},{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"beta"}]},{"type":"reasoning","id":"rs_3","summary":[{"type":"summary_text","text":"gamma"}]}]}}

`
	for _, target := range terminalTargets {
		t.Run(target.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := TranslateStreamBetween(target.inbound, ir.Responses, &buf, strings.NewReader(source), nil, nil, StreamOptions{}); err != nil {
				t.Fatalf("TranslateStreamBetween: %v", err)
			}
			if got := strings.Join(reasoningEmissionOrder(t, target.inbound, buf.String()), ""); got != "alphabetagamma" {
				t.Fatalf("reasoning = %q, want alphabetagamma once: %s", got, buf.String())
			}
		})
	}
}

func TestResponsesOwnerIndexIsolatesParts(t *testing.T) {
	d := NewResponsesStreamDecoder(strings.NewReader(""))
	add := func(partKey, owner, value string, channel partChannel) {
		var b strings.Builder
		b.WriteString(value)
		d.emittedText[partKey] = &b
		d.partKind[partKey] = channel
		if owner != "" {
			d.setPartOwner(partKey, owner)
		}
	}
	add("part:rs_1:0", "item:rs_1", "owned", partChannelText)
	add("part:rs_2:1", "", "foreign", partChannelText)
	add("part:rs_1:1", "item:rs_1", "", partChannelText)

	got := d.emittedContentParts("item:rs_1", partChannelText)
	if len(got) != 1 || got[0].value != "owned" {
		t.Fatalf("owned parts = %+v, want only the non-empty owned part", got)
	}
	if d.hasEmittedParts("item:rs_2") {
		t.Fatal("unowned emitted part leaked into the index")
	}
	d.setPartOwner("part:rs_1:0", "item:rs_3")
	if got := d.emittedContentParts("item:rs_1", partChannelText); len(got) != 0 {
		t.Fatalf("reassigned part stayed with its old owner: %+v", got)
	}
	if got := d.emittedContentParts("item:rs_3", partChannelText); len(got) != 1 || got[0].value != "owned" {
		t.Fatalf("reassigned part missing from its new owner: %+v", got)
	}
}

// BenchmarkResponsesTerminalReconciliation models 10,000 owned parts across
// 10,000 items with 4,096 terminal lookups. The owner index keeps each lookup
// at one part; the former full-map scan visited every emitted part per item.
func BenchmarkResponsesTerminalReconciliation(b *testing.B) {
	const announced = WireCollectionMax
	const terminal = 4096
	d := NewResponsesStreamDecoder(strings.NewReader(""))
	for i := 0; i < announced; i++ {
		partKey := fmt.Sprintf("part:rs_%d:%d", i, i)
		owner := fmt.Sprintf("item:rs_%d", i)
		var value strings.Builder
		value.WriteString("x")
		d.emittedText[partKey] = &value
		d.partKind[partKey] = partChannelText
		d.setPartOwner(partKey, owner)
	}
	owners := make([]string, terminal)
	for i := range owners {
		owners[i] = fmt.Sprintf("item:rs_%d", i)
	}
	b.ResetTimer()
	for b.Loop() {
		for _, owner := range owners {
			if parts := d.emittedContentParts(owner, partChannelText); len(parts) != 1 {
				b.Fatalf("owner %s parts = %d, want 1", owner, len(parts))
			}
			if !d.hasEmittedParts(owner) {
				b.Fatalf("owner %s reported no parts", owner)
			}
		}
	}
}

// TestResponsesOwnerIndexScalesSublinearly guards the owner index against a
// regression to a full-map scan. The lookup count is fixed while the owner
// population grows tenfold: the index stays proportional to one item's parts,
// while a full emittedText scan would visit every owner per lookup. The bound
// is deliberately wide (5x plus 100µs) to avoid a wall-clock threshold.
func TestResponsesOwnerIndexScalesSublinearly(t *testing.T) {
	small := benchmarkResponsesOwnerLookup(1_000)
	large := benchmarkResponsesOwnerLookup(10_000)
	limit := small.NsPerOp()*5 + 100_000
	if large.NsPerOp() > limit {
		t.Fatalf("owner lookup grew from %d ns/op at 1,000 owners to %d ns/op at 10,000 (limit %d)",
			small.NsPerOp(), large.NsPerOp(), limit)
	}
	t.Logf("owner lookup: 1,000 owners %d ns/op, 10,000 owners %d ns/op",
		small.NsPerOp(), large.NsPerOp())
}

func benchmarkResponsesOwnerLookup(owners int) testing.BenchmarkResult {
	const lookups = 1_000
	return testing.Benchmark(func(b *testing.B) {
		d := NewResponsesStreamDecoder(strings.NewReader(""))
		names := make([]string, owners)
		for i := 0; i < owners; i++ {
			partKey := fmt.Sprintf("part:rs_%d:%d", i, i)
			owner := fmt.Sprintf("item:rs_%d", i)
			names[i] = owner
			var value strings.Builder
			value.WriteString("x")
			d.emittedText[partKey] = &value
			d.partKind[partKey] = partChannelText
			d.setPartOwner(partKey, owner)
		}
		b.ResetTimer()
		for b.Loop() {
			for i := 0; i < lookups; i++ {
				owner := names[i%owners]
				if parts := d.emittedContentParts(owner, partChannelText); len(parts) != 1 {
					b.Fatalf("owner %s parts = %d, want 1", owner, len(parts))
				}
			}
		}
	})
}
