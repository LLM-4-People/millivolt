package storage

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// The accounting revision is the KPI memo's durable witness. Every owner that
// publishes totals or changes the written-id set must advance it, under aggMu,
// and a no-op call must not publish a revision that invalidates unrelated
// memos.
func TestAccountingRevisionTracksEveryDurableMutation(t *testing.T) {
	s := openTestStore(t)
	base := s.AccountingRevision()

	s.account([]*metrics.Record{{ID: "accounted", Start: time.Now(), StatusCode: 200}}, nil)
	afterAccount := s.AccountingRevision()
	if afterAccount <= base {
		t.Fatalf("account did not advance accounting revision: %d -> %d", base, afterAccount)
	}
	if s.Totals().Requests != 1 {
		t.Fatalf("account totals = %+v, want one row", s.Totals())
	}

	next := Totals{Requests: 7}
	s.account([]*metrics.Record{{ID: "accounted-next", Start: time.Now(), StatusCode: 200}}, &next)
	afterNext := s.AccountingRevision()
	if afterNext <= afterAccount {
		t.Fatalf("account(next) did not advance accounting revision: %d -> %d", afterAccount, afterNext)
	}
	if s.Totals().Requests != 7 {
		t.Fatalf("installed batch totals = %+v, want 7", s.Totals())
	}

	// IDs already in the written set (including duplicates within one call)
	// change nothing, so they must not publish a misleading new revision.
	s.MarkWritten([]string{"accounted", "accounted-next"})
	if got := s.AccountingRevision(); got != afterNext {
		t.Fatalf("duplicate MarkWritten advanced revision: %d -> %d", afterNext, got)
	}
	s.MarkWritten(nil)
	if got := s.AccountingRevision(); got != afterNext {
		t.Fatalf("empty MarkWritten advanced revision: %d -> %d", afterNext, got)
	}

	s.MarkWritten([]string{"primed"})
	afterMark := s.AccountingRevision()
	if afterMark <= afterNext {
		t.Fatalf("MarkWritten did not advance accounting revision: %d -> %d", afterNext, afterMark)
	}

	s.installTotals(Totals{Requests: 9, Errors: 1})
	afterInstall := s.AccountingRevision()
	if afterInstall <= afterMark {
		t.Fatalf("installTotals did not advance accounting revision: %d -> %d", afterMark, afterInstall)
	}
	if s.Totals().Requests != 9 || s.Totals().Errors != 1 {
		t.Fatalf("installed totals = %+v, want 9/1", s.Totals())
	}
}
