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
	// Seeding follows the same recency policy as add: the oldest seeded names
	// are displaced, so boot-time vocabulary is not the alphabetically-first
	// slice of a SQL ORDER BY.
	if slices.Contains(got, names[0]) || !slices.Contains(got, names[len(names)-1]) {
		t.Fatalf("seed cap kept %q..%q, want the newest %d names",
			got[0], got[len(got)-1], nameSetMax)
	}
}

// TestNameSetOperatorNamesSurviveAFlood pins the defect the vocabulary cap used
// to have. Client labels and model ids are caller-chosen, so an unauthenticated
// peer could submit nameSetMax distinct ones and fill the whole set; after that
// the operator's own names were never stored, a "new clients" hold classified
// real clients as new and parked them, and the pickers could not name them.
// Operator-document scopes are pinned and must outlive any later traffic.
func TestNameSetOperatorNamesSurviveAFlood(t *testing.T) {
	// Flood first, operator scope afterwards: this is the ordering a first-wins
	// cap could not survive.
	var late nameSet
	for i := range nameSetMax * 2 {
		late.add(fmt.Sprintf("flood-%05d", i))
	}
	if dropped := late.setPins("pause", []string{"opencode", "claude-code"}); len(dropped) != 0 {
		t.Fatalf("pinning a scope into a flooded vocabulary dropped %v", dropped)
	}
	got := late.list()
	for _, name := range []string{"opencode", "claude-code"} {
		if !slices.Contains(got, name) {
			t.Fatalf("operator scope %q was never stored: the vocabulary is still first-wins", name)
		}
	}
	if len(got) != nameSetMax {
		t.Fatalf("size after a late pin = %d, want the cap %d", len(got), nameSetMax)
	}

	// A later flood must not take the pinned scope back.
	for i := range nameSetMax * 2 {
		late.add(fmt.Sprintf("later-flood-%05d", i))
	}
	if !slices.Contains(late.list(), "opencode") {
		t.Fatal("an operator-pinned name was evicted by later traffic")
	}

	// When every stored name is pinned, an observed name is dropped rather than
	// displacing operator intent, and the drop is reported, not silent.
	var full nameSet
	pins := make([]string, nameSetMax)
	for i := range pins {
		pins[i] = fmt.Sprintf("pinned-%05d", i)
	}
	if dropped := full.setPins("pause", pins); len(dropped) != 0 {
		t.Fatalf("pinning an empty vocabulary dropped %v", dropped)
	}
	full.add("cannot-fit")
	if got := full.list(); len(got) != nameSetMax || slices.Contains(got, "cannot-fit") {
		t.Fatalf("an observed name displaced a fully pinned vocabulary: size=%d", len(got))
	}
	if !slices.Contains(full.list(), pins[0]) {
		t.Fatal("a pinned name was evicted")
	}
	if dropped := full.setPins("debug", []string{"newer-scope"}); len(dropped) != 1 || dropped[0] != "newer-scope" {
		t.Fatalf("an unplaceable scope was not reported: %v", dropped)
	}
}

// TestNameSetRecencyEviction pins the eviction policy. FIFO on first sight let a
// long-lived client lose to a flood of one-shot labels that arrived after it,
// which is the original defect with a shorter window: the client is evicted, a
// "new clients" hold then classifies it as new, and the picker loses it.
func TestNameSetRecencyEviction(t *testing.T) {
	var set nameSet
	set.add("laptop")
	for i := range nameSetMax * 2 {
		set.add("laptop") // continuously active
		set.add(fmt.Sprintf("flood-%05d", i))
	}
	if !slices.Contains(set.list(), "laptop") {
		t.Fatal("an actively used name was evicted by one-shot flood labels: eviction is not recency")
	}
	// The newest one-shot label survives too; the evicted ones are the oldest.
	if !slices.Contains(set.list(), fmt.Sprintf("flood-%05d", nameSetMax*2-1)) {
		t.Fatal("the newest observed name was not stored")
	}
	if len(set.list()) != nameSetMax {
		t.Fatalf("size = %d, want %d", len(set.list()), nameSetMax)
	}
}

// TestNameSetPinsAreReleasedWithTheirDocument pins the derivation contract. An
// append-only pin set let a retired hold or debug session pin its scope for the
// process lifetime; a full pin set then froze the vocabulary, so a New-clients
// hold snapped an empty snapshot and parked every real client - the original
// defect reached through pins.
func TestNameSetPinsAreReleasedWithTheirDocument(t *testing.T) {
	var set nameSet
	set.setPins("pause", []string{"one-hold"})
	if !slices.Contains(set.list(), "one-hold") {
		t.Fatal("the hold scope was not pinned")
	}
	// The hold is resumed: its family contributes no names again.
	set.setPins("pause", nil)
	for i := range nameSetMax {
		set.add(fmt.Sprintf("flood-%05d", i))
	}
	if slices.Contains(set.list(), "one-hold") {
		t.Fatal("a retired hold's scope is still pinned: pins never released")
	}

	// A debug session pins independently, and each family's set replaces only
	// itself: syncing one family must not release another's names.
	set.setPins("debug", []string{"debug-scope"})
	set.setPins("pause", []string{"hold-scope"})
	got := set.list()
	for _, name := range []string{"debug-scope", "hold-scope"} {
		if !slices.Contains(got, name) {
			t.Fatalf("%q was released by the other family's sync", name)
		}
	}
	// Release the debug family, then flood: the released name is now evictable,
	// the still-pinned one is not. Release does not delete the name, it only
	// gives up the protection.
	set.setPins("debug", nil)
	for i := range nameSetMax {
		set.add(fmt.Sprintf("flood-%05d", i))
	}
	got = set.list()
	if slices.Contains(got, "debug-scope") {
		t.Fatal("a released debug scope still survived a flood: its pin was not released")
	}
	if !slices.Contains(got, "hold-scope") {
		t.Fatal("releasing the debug family also released the hold family's pin")
	}

	// Retiring every document must leave the vocabulary usable by traffic
	// again: a saturated set is what froze it before.
	set.setPins("pause", nil)
	for i := range nameSetMax * 2 {
		set.add(fmt.Sprintf("after-%05d", i))
	}
	if got := len(set.list()); got != nameSetMax {
		t.Fatalf("vocabulary after full release = %d names, want %d", got, nameSetMax)
	}
}

// TestNameSetSeedEvictsLikeAdd pins the asymmetry the verifier found: seed
// returned at the cap while add evicted, so boot-time vocabulary was the
// alphabetically-first 4096 history values (storage returns ORDER BY name) and
// a real client at the end of the alphabet was unlistable until its first
// request. Both now share the recency policy.
func TestNameSetSeedEvictsLikeAdd(t *testing.T) {
	var seeded nameSet
	names := make([]string, 0, nameSetMax+1)
	for i := range nameSetMax {
		names = append(names, fmt.Sprintf("a-%05d", i))
	}
	names = append(names, "zzz-real-client")
	seeded.seed(names)
	if !slices.Contains(seeded.list(), "zzz-real-client") {
		t.Fatal("a seeded name past the cap was dropped: seed is still first-wins")
	}
	if len(seeded.list()) != nameSetMax {
		t.Fatalf("seeded size = %d, want %d", len(seeded.list()), nameSetMax)
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
