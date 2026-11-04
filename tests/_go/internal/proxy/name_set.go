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
	// Observed names are evicted oldest first, so the vocabulary tracks what is
	// currently in use instead of freezing on whatever arrived first. A
	// first-wins cap let any caller that could choose a label fill the set and
	// permanently hide the operator's own names.
	var added nameSet
	for _, name := range names {
		added.add(name)
	}
	got := added.list()
	if len(got) != nameSetMax {
		t.Fatalf("add cap size = %d, want %d", len(got), nameSetMax)
	}
	evicted := names[:len(names)-nameSetMax]
	kept := names[len(names)-nameSetMax:]
	for _, name := range evicted {
		if slices.Contains(got, name) {
			t.Fatalf("add retained evicted name %q", name)
		}
	}
	for _, name := range kept {
		if !slices.Contains(got, name) {
			t.Fatalf("add dropped the newest name %q", name)
		}
	}

	// Re-adding an observed name is a no-op: it does not advance the revision
	// and does not refresh its position, so the set is stable under repeats.
	revision := added.revision
	added.add(kept[0])
	if len(added.list()) != nameSetMax || added.revision != revision {
		t.Fatalf("duplicate add changed set: size=%d revision=%d", len(added.list()), added.revision)
	}

	// Seeding is bounded too, and a seeded set evicts like an observed one.
	var seeded nameSet
	seeded.seed(names)
	got = seeded.list()
	if len(got) != nameSetMax {
		t.Fatalf("seed cap list = %d, want %d", len(got), nameSetMax)
	}
	if !slices.Contains(got, names[0]) || slices.Contains(got, names[len(names)-1]) {
		t.Fatalf("seed cap kept %q..%q, want the first %d names in insertion order",
			got[0], got[len(got)-1], nameSetMax)
	}
}

// TestNameSetOperatorNamesSurviveAFlood pins the defect the vocabulary cap used
// to have. Client labels and model ids are caller-chosen, so an unauthenticated
// peer could submit nameSetMax distinct ones and fill the whole set; after that
// the operator's own names were never stored, a "new clients" hold classified
// real clients as new and parked them, and the pickers could not name them.
// Pinned names are operator intent and must outlive any later traffic.
func TestNameSetOperatorNamesSurviveAFlood(t *testing.T) {
	var set nameSet
	set.pin([]string{"opencode", "claude-code"})

	for i := range nameSetMax * 2 {
		set.add(fmt.Sprintf("flood-%05d", i))
	}
	got := set.list()
	for _, name := range []string{"opencode", "claude-code"} {
		if !slices.Contains(got, name) {
			t.Fatalf("operator-pinned name %q was evicted by a flood: %d names stored", name, len(got))
		}
	}
	if len(got) != nameSetMax {
		t.Fatalf("size after flood = %d, want the cap %d", len(got), nameSetMax)
	}
	// Pinning a name already observed promotes it: the same later flood must not
	// take it back.
	var promoted nameSet
	promoted.add("later-operator-choice")
	for i := range nameSetMax * 2 {
		promoted.add(fmt.Sprintf("flood-%05d", i))
		if i == nameSetMax/2 {
			promoted.pin([]string{"later-operator-choice"})
		}
	}
	if !slices.Contains(promoted.list(), "later-operator-choice") {
		t.Fatal("an observed name promoted to pinned was evicted by later traffic")
	}

	// The attack ordering is flood first, operator name afterwards: a first-wins
	// cap filled by traffic left no room for the name an operator scoped a hold
	// to, so the hold could never match that client again. Recency eviction must
	// make room for it.
	var late nameSet
	for i := range nameSetMax * 2 {
		late.add(fmt.Sprintf("flood-%05d", i))
	}
	late.pin([]string{"opencode"})
	if !slices.Contains(late.list(), "opencode") {
		t.Fatal("a name pinned after a flood was never stored: the vocabulary is still first-wins")
	}
	if got := len(late.list()); got != nameSetMax {
		t.Fatalf("size after a late pin = %d, want the cap %d", got, nameSetMax)
	}

	// When every stored name is pinned, an observed name is dropped rather than
	// displacing operator intent: a full operator vocabulary is the operator's
	// decision, not a limit to work around.
	var full nameSet
	pins := make([]string, nameSetMax)
	for i := range pins {
		pins[i] = fmt.Sprintf("pinned-%05d", i)
	}
	full.pin(pins)
	full.add("cannot-fit")
	if got := full.list(); len(got) != nameSetMax || slices.Contains(got, "cannot-fit") {
		t.Fatalf("an observed name displaced a fully pinned vocabulary: size=%d", len(got))
	}
	if !slices.Contains(full.list(), pins[0]) {
		t.Fatal("the oldest pinned name was evicted")
	}
}

// TestNameSetEvictionStaysProportional pins the bookkeeping invariant that makes
// recency eviction cheap: the eviction order is trimmed as it is consumed, so
// repeated evictions cannot grow it without bound. Without that the order slice
// would retain one entry per observed name ever seen, which is the same
// process-lifetime growth the cap exists to prevent.
func TestNameSetEvictionStaysProportional(t *testing.T) {
	var set nameSet
	const rounds = nameSetMax * 4
	for i := range rounds {
		set.add(fmt.Sprintf("name-%06d", i))
	}
	if len(set.order) > nameSetMax+1 {
		t.Fatalf("eviction order holds %d entries for %d stored names (cap %d): stale entries are not trimmed",
			len(set.order), len(set.names), nameSetMax)
	}
	if len(set.names) != nameSetMax || len(set.seq) != nameSetMax {
		t.Fatalf("stored names=%d seq=%d, want %d each", len(set.names), len(set.seq), nameSetMax)
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
