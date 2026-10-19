//go:build !race

package web

import (
	"fmt"
	"testing"
)

// TestLineageDirtyExplorerAllocationIsPooled pins WEB-3: a dirty overlay no
// longer allocates full-projection arrays per explorer request. The bound is a
// generous ceiling over the clean/dirty delta at this node count; the previous
// implementation allocated parent/conflict/issues/state arrays per request.
func TestLineageDirtyExplorerAllocationIsPooled(t *testing.T) {
	const n = 100_000
	rows := make([]contrib, n)
	for i := range rows {
		rows[i] = lineageRow(fmt.Sprintf("s:%d", i), "")
		rowLabels(&rows[i]).prov = "fixture.example"
	}
	p := explorerProjection(rows)
	clean := lineageRow("s:clean", "")
	rowLabels(&clean).prov = "fixture.example"
	dirty := lineageRow("s:dirty", "s:0")
	rowLabels(&dirty).prov = "fixture.example"

	measure := func(extra *contrib) testing.BenchmarkResult {
		return testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				f := newExplorerFold("provider", "", nil)
				f.prepare(p, nil)
				for i := range p.rows {
					if err := f.fold(&p.rows[i]); err != nil {
						b.Fatal(err)
					}
				}
				if extra != nil {
					if err := f.fold(extra); err != nil {
						b.Fatal(err)
					}
				}
				_ = f.payload()
			}
		})
	}
	withClean := measure(&clean)
	withDirty := measure(&dirty)
	delta := int64(withDirty.AllocedBytesPerOp()) - int64(withClean.AllocedBytesPerOp())
	t.Logf("dirty lineage adds %d B/op at %d nodes (clean=%d dirty=%d)",
		delta, n, withClean.AllocedBytesPerOp(), withDirty.AllocedBytesPerOp())
	if delta > 128<<10 {
		t.Fatalf("dirty lineage adds %d B/op at %d nodes (clean=%d dirty=%d), want a pooled bound",
			delta, n, withClean.AllocedBytesPerOp(), withDirty.AllocedBytesPerOp())
	}
}
