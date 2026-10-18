package proxy

import (
	"fmt"
	"slices"
	"sync"
	"testing"
)

func TestNameSetListSortedAndInvalidated(t *testing.T) {
	var set nameSet
	if got := set.list(); got != nil {
		t.Fatalf("empty list = %v, want nil", got)
	}
	set.seed([]string{"b", "a", "c", "b", ""})
	if got, want := set.list(), []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Fatalf("seeded list = %v, want %v", got, want)
	}
	revision := set.revision
	if revision == 0 {
		t.Fatal("seed did not advance revision")
	}
	set.add("b")
	set.add("")
	set.seed([]string{"a", "c", ""})
	if set.revision != revision {
		t.Fatalf("duplicate add/seed advanced revision %d -> %d", revision, set.revision)
	}
	if got, want := set.list(), []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Fatalf("duplicate list = %v, want %v", got, want)
	}

	set.add("d")
	if set.revision == revision {
		t.Fatal("add did not advance revision")
	}
	if got, want := set.list(), []string{"a", "b", "c", "d"}; !slices.Equal(got, want) {
		t.Fatalf("added list = %v, want %v", got, want)
	}
	revision = set.revision
	set.seed([]string{"d", "a", "0"})
	if set.revision == revision {
		t.Fatal("new seeded name did not advance revision")
	}
	if got, want := set.list(), []string{"0", "a", "b", "c", "d"}; !slices.Equal(got, want) {
		t.Fatalf("newly seeded list = %v, want %v", got, want)
	}
}

func TestNameSetListReturnsAClone(t *testing.T) {
	var set nameSet
	set.seed([]string{"a", "b", "c"})
	first := set.list()
	first[0] = "mutated"
	appended := append(set.list(), "extra")
	for i := range appended {
		appended[i] = "mutated"
	}
	if got, want := set.list(), []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Fatalf("list after caller mutation = %v, want %v", got, want)
	}
}

func TestNameSetEmptyBehavior(t *testing.T) {
	var zero nameSet
	if got := zero.list(); got != nil {
		t.Fatalf("zero list = %v, want nil", got)
	}
	zero.add("")
	if got := zero.list(); got != nil || zero.revision != 0 {
		t.Fatalf("empty add changed set: list=%v revision=%d", got, zero.revision)
	}

	var seeded nameSet
	seeded.seed(nil)
	if got := seeded.list(); got != nil || seeded.revision != 0 {
		t.Fatalf("nil seed changed set: list=%v revision=%d", got, seeded.revision)
	}
	seeded.seed([]string{""})
	if got := seeded.list(); got != nil || seeded.revision != 0 {
		t.Fatalf("empty seed changed set: list=%v revision=%d", got, seeded.revision)
	}
}

func TestNameSetCap(t *testing.T) {
	names := make([]string, nameSetMax+256)
	for i := range names {
		names[i] = fmt.Sprintf("name-%05d", i)
	}
	overflow := names[nameSetMax:]

	var added nameSet
	for _, name := range names {
		added.add(name)
	}
	got := added.list()
	if len(got) != nameSetMax {
		t.Fatalf("add cap size = %d, want %d", len(got), nameSetMax)
	}
	if got[0] != names[0] || got[len(got)-1] != names[nameSetMax-1] {
		t.Fatalf("add cap kept unexpected names %q..%q", got[0], got[len(got)-1])
	}
	for _, name := range overflow {
		if slices.Contains(got, name) {
			t.Fatalf("add retained overflow name %q", name)
		}
	}
	revision := added.revision
	added.add(names[0])
	added.add("another-overflow")
	if len(added.list()) != nameSetMax || added.revision != revision {
		t.Fatalf("cap add changed set: size=%d revision=%d", len(added.list()), added.revision)
	}

	var seeded nameSet
	seeded.seed(names)
	got = seeded.list()
	if len(got) != nameSetMax || got[0] != names[0] || got[len(got)-1] != names[nameSetMax-1] {
		t.Fatalf("seed cap list = %d names %q..%q", len(got), got[0], got[len(got)-1])
	}
	for _, name := range overflow {
		if slices.Contains(got, name) {
			t.Fatalf("seed retained overflow name %q", name)
		}
	}
	revision = seeded.revision
	seeded.seed(overflow)
	seeded.add("another-overflow")
	if len(seeded.list()) != nameSetMax || seeded.revision != revision {
		t.Fatalf("cap seed changed set: size=%d revision=%d", len(seeded.list()), seeded.revision)
	}
}

func TestNameSetListConcurrentAdds(t *testing.T) {
	var set nameSet
	set.seed([]string{"base"})
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				set.add(fmt.Sprintf("worker-%d-%03d", worker, i%16))
				if got := set.list(); !slices.IsSorted(got) {
					t.Errorf("list not sorted: %v", got)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
}
