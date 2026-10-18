package storage

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDebugExpireQueryPlanAvoidsSort pins the physical-reclamation query shape:
// batch order is not semantic, and ORDER BY adds a temp B-tree to every batch.
func TestDebugExpireQueryPlanAvoidsSort(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "debug-plan.db"), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, withBytes := range []bool{false, true} {
		rows, err := s.rdb.Query("EXPLAIN QUERY PLAN "+debugExpireQuery(withBytes), 0, debugExpireBatch)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if strings.Contains(strings.ToUpper(detail), "TEMP B-TREE") {
				rows.Close()
				t.Fatalf("withBytes=%v: expired-capture query plan sorts: %s", withBytes, detail)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
}

// TestDebugExpireSweepIsBoundedAndContinues seeds more expired rows than one
// sweep's bound, proves one call deletes only that bound, and proves the
// writer's near-term continuation drains the rest without another explicit
// call. The cap-active byte counter and read visibility stay exact throughout.
func TestDebugExpireSweepIsBoundedAndContinues(t *testing.T) {
	opts := testOpts
	opts.DebugMaxBytes = 1 << 20
	opts.FlushInterval = time.Hour // keep the periodic writer sweep out of the way
	s, err := Open(filepath.Join(t.TempDir(), "debug-sweep.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	expired := now.Add(-time.Minute).UnixMilli()
	payload := []byte("0123456789ab") // 12 bytes
	const expiredRows = debugExpireBatch*debugExpireBatchesPerSweep + 5
	for i := range expiredRows {
		if err := s.SaveDebugCapture(t.Context(), fmt.Sprintf("expired-%04d", i), "sess", now.UnixMilli(), expired, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveDebugCapture(t.Context(), "live", "sess", now.UnixMilli(), now.Add(time.Hour).UnixMilli(), payload); err != nil {
		t.Fatal(err)
	}
	wantBytes := int64((expiredRows + 1) * len(payload))
	if got := debugAccounting(t, s); got != wantBytes {
		t.Fatalf("seeded debug bytes = %d, want %d", got, wantBytes)
	}
	// TTL visibility is independent of physical reclamation.
	if raw, err := s.LoadDebugCapture(t.Context(), "expired-0000"); err != nil || raw != nil {
		t.Fatalf("expired capture visible before reclamation: %q %v", raw, err)
	}

	deleted, err := s.ExpireDebugCaptures(t.Context(), now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	wantBound := int64(debugExpireBatch * debugExpireBatchesPerSweep)
	if deleted != wantBound {
		t.Fatalf("one sweep deleted %d rows, want the %d-row bound", deleted, wantBound)
	}

	// The writer's near-term tick owns continuation: no second explicit call.
	deadline := time.Now().Add(5 * time.Second)
	for {
		var rows int64
		if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM request_debug`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows == 1 && !s.debugExpirePending.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("writer continuation did not drain the expired set: rows=%d pending=%v", rows, s.debugExpirePending.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := debugAccounting(t, s); got != int64(len(payload)) {
		t.Fatalf("debug bytes after continuation = %d, want the live row's %d", got, len(payload))
	}
	if raw, err := s.LoadDebugCapture(t.Context(), "live"); err != nil || string(raw) != string(payload) {
		t.Fatalf("live capture after sweep: %q %v", raw, err)
	}
	if raw, err := s.LoadDebugCapture(t.Context(), "expired-0005"); err != nil || raw != nil {
		t.Fatalf("expired capture visible after reclamation: %q %v", raw, err)
	}
}

// TestDebugExpireCapDisabledSkipsPayloadLengths pins the disabled-cap path: no
// payload length is selected, and the byte counter stays unseeded and unused.
func TestDebugExpireCapDisabledSkipsPayloadLengths(t *testing.T) {
	opts := testOpts
	opts.DebugMaxBytes = 0
	opts.FlushInterval = time.Hour
	s, err := Open(filepath.Join(t.TempDir(), "debug-no-cap.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	disabled := strings.ToUpper(debugExpireQuery(false))
	if strings.Contains(disabled, "LENGTH") || strings.Contains(disabled, "PAYLOAD") {
		t.Fatalf("cap-disabled expiry reads payload lengths: %s", disabled)
	}
	if enabled := strings.ToUpper(debugExpireQuery(true)); !strings.Contains(enabled, "LENGTH") {
		t.Fatalf("cap-active expiry must carry payload lengths: %s", enabled)
	}

	now := time.Now()
	expired := now.Add(-time.Minute).UnixMilli()
	for i := range 3 {
		if err := s.SaveDebugCapture(t.Context(), fmt.Sprintf("expired-%d", i), "sess", now.UnixMilli(), expired, []byte("capture")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveDebugCapture(t.Context(), "live", "sess", now.UnixMilli(), now.Add(time.Hour).UnixMilli(), []byte("live")); err != nil {
		t.Fatal(err)
	}
	if seeded, bytes := debugAccountingState(t, s); seeded || bytes != 0 {
		t.Fatalf("cap-disabled accounting = seeded:%v bytes:%d, want unused", seeded, bytes)
	}
	deleted, err := s.ExpireDebugCaptures(t.Context(), now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 3 {
		t.Fatalf("cap-disabled sweep deleted %d rows, want 3", deleted)
	}
	var rows int64
	if err := s.rdb.QueryRow(`SELECT COUNT(*) FROM request_debug`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("rows after cap-disabled sweep = %d, want the live row", rows)
	}
	if seeded, bytes := debugAccountingState(t, s); seeded || bytes != 0 {
		t.Fatalf("cap-disabled accounting after sweep = seeded:%v bytes:%d, want unused", seeded, bytes)
	}
	if raw, err := s.LoadDebugCapture(t.Context(), "live"); err != nil || string(raw) != "live" {
		t.Fatalf("live capture after cap-disabled sweep: %q %v", raw, err)
	}
}

func debugAccounting(t *testing.T, s *Store) int64 {
	t.Helper()
	_, bytes := debugAccountingState(t, s)
	return bytes
}

func debugAccountingState(t *testing.T, s *Store) (bool, int64) {
	t.Helper()
	s.debugMu.Lock()
	defer s.debugMu.Unlock()
	return s.debugSeeded, s.debugBytes
}

// TestSaveDebugCaptureReclaimsExpiredBytesAtCap pins S12: expired rows stay
// hidden from reads but must not block a new capture until the periodic sweep.
// The cap path runs one bounded sweep on its held connection, and the counter
// ends at the unexpired durable sum.
func TestSaveDebugCaptureReclaimsExpiredBytesAtCap(t *testing.T) {
	opts := testOpts
	opts.DebugMaxBytes = 100
	opts.FlushInterval = time.Hour // keep the periodic sweep out of this test
	s, err := Open(filepath.Join(t.TempDir(), "debug-cap-expired.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	expired := now.Add(-time.Minute).UnixMilli()
	payload := []byte("0123456789")
	for i := range 9 {
		if err := s.SaveDebugCapture(t.Context(), fmt.Sprintf("expired-%d", i), "sess", now.UnixMilli(), expired, payload); err != nil {
			t.Fatalf("expired seed %d: %v", i, err)
		}
	}
	if got := debugAccounting(t, s); got != 90 {
		t.Fatalf("seeded expired bytes = %d, want 90", got)
	}

	fresh := []byte("fresh-capture-0123456789")
	if err := s.SaveDebugCapture(t.Context(), "live", "sess", now.UnixMilli(), now.Add(time.Hour).UnixMilli(), fresh); err != nil {
		t.Fatalf("capture rejected while only expired rows filled the cap: %v", err)
	}
	if got, want := debugAccounting(t, s), int64(len(fresh)); got != want {
		t.Fatalf("debug counter after cap reclamation = %d, want %d", got, want)
	}
	var rows, bytes int64
	if err := s.rdb.QueryRow(`SELECT COUNT(*), COALESCE(SUM(LENGTH(CAST(payload AS BLOB))), 0) FROM request_debug`).Scan(&rows, &bytes); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || bytes != int64(len(fresh)) {
		t.Fatalf("durable sidecars after reclamation = %d rows / %d bytes, want 1 / %d", rows, bytes, len(fresh))
	}
	if got := debugAccounting(t, s); got != unresolvedDebugBytes(t, s) {
		t.Fatalf("counter = %d, unexpired sum = %d", got, unresolvedDebugBytes(t, s))
	}
}
