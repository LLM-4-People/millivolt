package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestMergeDebugCounterReadFailureRollsBack pins S15: merge must never commit
// a request set plus sidecar rows while publishing an unknown or stale byte
// counter. The sum read is commit-blocking, so the transaction rolls back and
// a reopen seeds the exact durable total.
func TestMergeDebugCounterReadFailureRollsBack(t *testing.T) {
	opts := testOpts
	opts.DebugMaxBytes = 1 << 20
	live, err := Open(filepath.Join(t.TempDir(), "merge-debug-read.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "m", StatusCode: 200, Start: now})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	const livePayload = "live-debug"
	if err := live.SaveDebugCapture(t.Context(), "live-debug", "sess", now.UnixMilli(), 0, []byte(livePayload)); err != nil {
		t.Fatal(err)
	}
	beforeTotals := live.Totals()
	beforeBytes := debugAccounting(t, live)

	src := openTestStore(t)
	src.Record(&metrics.Record{ID: "backup-row", Provider: "backup.example", Model: "m", StatusCode: 200, Start: now.Add(time.Millisecond)})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := src.SaveDebugCapture(t.Context(), "backup-debug", "sess", now.UnixMilli(), 0, []byte("backup-debug")); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}

	previousHook := debugByteTotalHook
	reads := 0
	debugByteTotalHook = func() error {
		reads++
		if reads >= 2 {
			return errors.New("injected sidecar sum failure")
		}
		return nil
	}
	defer func() { debugByteTotalHook = previousHook }()
	inserted, skipped, err := live.MergeSnapshot(t.Context(), data)
	if err == nil {
		t.Fatalf("merge committed with an unreadable sidecar total (inserted=%d skipped=%d)", inserted, skipped)
	}
	if reads < 2 {
		t.Fatalf("merge returned before the post-merge sidecar sum: reads=%d", reads)
	}
	if !strings.Contains(err.Error(), "debug byte counter") {
		t.Fatalf("merge error %q does not name the sidecar counter phase", err)
	}
	if n := countRows(t, live); n != 1 {
		t.Fatalf("rows after failed merge = %d, want only the live row", n)
	}
	if got := live.Totals(); got != beforeTotals {
		t.Fatalf("totals after failed merge = %+v, want %+v", got, beforeTotals)
	}
	if got := debugAccounting(t, live); got != beforeBytes {
		t.Fatalf("debug counter after failed merge = %d, want %d", got, beforeBytes)
	}
	if raw, err := live.LoadDebugCapture(t.Context(), "backup-debug"); err != nil || raw != nil {
		t.Fatalf("backup capture after failed merge = %q, %v", raw, err)
	}
	if raw, err := live.LoadDebugCapture(t.Context(), "live-debug"); err != nil || string(raw) != livePayload {
		t.Fatalf("live capture after failed merge = %q, %v", raw, err)
	}

	path := live.path
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	debugByteTotalHook = previousHook
	reopened, err := Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, want := debugAccounting(t, reopened), int64(len(livePayload)); got != want {
		t.Fatalf("reopened debug counter = %d, want %d", got, want)
	}
	if raw, err := reopened.LoadDebugCapture(t.Context(), "backup-debug"); err != nil || raw != nil {
		t.Fatalf("backup capture survived reopen: %q, %v", raw, err)
	}
}
