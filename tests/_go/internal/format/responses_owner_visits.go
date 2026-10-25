package format

// Round 2 E2 regression: the owner index must keep terminal reconciliation
// proportional to one item. Wall time was removed as a pass/fail signal; the
// decoder's test-only hook now reports how many owners a lookup visited, so a
// full emittedText scan fails deterministically at 1k and 10k owners.

import "testing"

func TestResponsesOwnerLookupVisitsConstant(t *testing.T) {
	const lookups = 1_000
	for _, owners := range []int{1_000, 10_000} {
		d, names := responsesOwnerFixture(owners)
		visits, calls := 0, 0
		d.testOwnerVisits = func(visited int) {
			visits += visited
			calls++
		}
		for i := 0; i < lookups; i++ {
			if parts := d.emittedContentParts(names[i%owners], partChannelText); len(parts) != 1 {
				t.Fatalf("%d owners: lookup %d parts = %d, want 1", owners, i, len(parts))
			}
		}
		if calls != lookups {
			t.Fatalf("%d owners: %d owner lookups, want %d", owners, calls, lookups)
		}
		if visits != lookups {
			t.Fatalf("%d owners: lookup visited %d owners, want %d (constant one per lookup)",
				owners, visits, lookups)
		}
	}
}
