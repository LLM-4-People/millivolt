package storage

import (
	"reflect"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestRenameProvidersMergesHistory pins the provider_aliases repair: rows
// recorded under an old label are rewritten to the canonical one in place,
// other rows are untouched, the operation is idempotent, and a malformed pair
// is a no-op (config Validate rejects those - this is the last-chance deny).
// Labels are neutral fixtures; the mechanism is provider-agnostic.
func TestRenameProvidersMergesHistory(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(id, provider string) *metrics.Record {
		r := &metrics.Record{ID: id, Provider: provider, Model: "m", KeyHash: "k", StatusCode: 200, Start: time.Now()}
		metrics.FinalizeRecord(r)
		return r
	}
	for _, r := range []*metrics.Record{mk("a-1", "old.example"), mk("a-2", "old.example"), mk("b-1", "new.example")} {
		s.Record(r)
	}
	s.Flush()
	s.Close()

	s2, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	n, err := s2.RenameProviders(t.Context(), map[string]string{"old.example": "new.example"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("renamed %d rows, want 2", n)
	}
	recs, err := s2.LoadRecent(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}
	byProvider := map[string]int{}
	for _, r := range recs {
		byProvider[r.Provider]++
	}
	if byProvider["new.example"] != 3 || len(byProvider) != 1 {
		t.Fatalf("post-rename providers = %v, want all 3 rows under new.example", byProvider)
	}
	// Idempotent: re-running the same merge renames nothing.
	again, err := s2.RenameProviders(t.Context(), map[string]string{"old.example": "new.example"})
	if err != nil || again != 0 {
		t.Fatalf("second rename = %d, %v; want 0, nil", again, err)
	}
	// A malformed pair is skipped, never applied.
	bad, err := s2.RenameProviders(t.Context(), map[string]string{"": "new.example", "new.example": "new.example"})
	if err != nil || bad != 0 {
		t.Fatalf("malformed aliases = %d, %v; want 0, nil", bad, err)
	}
}

// TestRenameProvidersIsAtomicAcrossAliases pins the all-or-nothing contract: a
// trigger that rejects the second alias (sorted by source label) must roll the
// first alias back, so history is never left half-merged while the caller
// reports failure. Dropping the trigger then proves the same pairs still apply
// together.
func TestRenameProvidersIsAtomicAcrossAliases(t *testing.T) {
	path := testDatabase(t)
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mk := func(id, provider string) *metrics.Record {
		r := &metrics.Record{ID: id, Provider: provider, Model: "m", KeyHash: "k", StatusCode: 200, Start: time.Now()}
		metrics.FinalizeRecord(r)
		return r
	}
	s.Record(mk("a-1", "a-old.example"))
	s.Record(mk("b-1", "b-old.example"))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_before_rename
		BEFORE UPDATE OF provider ON requests
		WHEN NEW.provider = 'blocked.example'
		BEGIN SELECT RAISE(ABORT, 'rename blocked'); END`); err != nil {
		t.Fatal(err)
	}
	aliases := map[string]string{
		"a-old.example": "first.example",
		"b-old.example": "blocked.example",
	}
	if n, err := s.RenameProviders(t.Context(), aliases); err == nil {
		t.Fatalf("rename with rejecting trigger = %d, nil; want error", n)
	}
	assertProviders := func(want map[string]string) {
		t.Helper()
		recs, err := s.LoadRecent(t.Context(), 10)
		if err != nil {
			t.Fatal(err)
		}
		got := make(map[string]string, len(recs))
		for _, r := range recs {
			got[r.ID] = r.Provider
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("providers after rename = %v, want %v", got, want)
		}
	}
	// The first alias matched rows inside the transaction, but the second
	// trigger abort rolled every rewrite back.
	assertProviders(map[string]string{"a-1": "a-old.example", "b-1": "b-old.example"})
	if _, err := s.db.Exec("DROP TRIGGER reject_before_rename"); err != nil {
		t.Fatal(err)
	}
	n, err := s.RenameProviders(t.Context(), aliases)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("successful rename = %d rows, want 2", n)
	}
	assertProviders(map[string]string{"a-1": "first.example", "b-1": "blocked.example"})
}
