package proxy

import (
	"fmt"
	"testing"
)

func BenchmarkNameSetList4096(b *testing.B) {
	var set nameSet
	names := make([]string, nameSetMax)
	for i := range names {
		names[i] = fmt.Sprintf("name-%04d", i)
	}
	set.seed(names)
	if got := len(set.list()); got != nameSetMax {
		b.Fatalf("names = %d, want %d", got, nameSetMax)
	}
	b.ReportAllocs()
	for b.Loop() {
		if got := len(set.list()); got != nameSetMax {
			b.Fatalf("names = %d, want %d", got, nameSetMax)
		}
	}
}
