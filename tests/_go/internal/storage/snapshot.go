package storage

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestSnapshotIncludesFlushedRows(t *testing.T) {
	s := openTestStore(t)
	s.Record(&metrics.Record{ID: "snap-1", Provider: "neutral.example", Model: "neutral", Start: time.Now(), StatusCode: 200})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := backup.CheckDatabase(s.stageDir(), data); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.Encode(s.stageDir(), backup.Archive{Database: data}); err != nil {
		t.Fatal(err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir()+"/snap.db", testOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func packedSnapshot(t *testing.T, id string) []byte {
	t.Helper()
	s := openTestStore(t)
	s.Record(&metrics.Record{ID: id, Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := s.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOpenQuarantinesInvalidPendingSnapshot(t *testing.T) {
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "dst.db")
	dst, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	dst.Record(&metrics.Record{ID: "keep-old", Provider: "neutral.example", Model: "n", StatusCode: 200})
	if err := dst.Flush(); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	incoming := PendingSnapshotPath(dstPath)
	if err := os.WriteFile(incoming, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dstPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := os.Stat(incoming); !os.IsNotExist(err) {
		t.Fatal("invalid pending was not consumed")
	}
	if _, err := os.Stat(incoming + ".invalid"); err != nil {
		t.Fatalf("invalid pending was not quarantined: %v", err)
	}
	got, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "keep-old" {
		t.Fatalf("live rows after quarantine = %#v", got)
	}
}

func TestOpenLeavesPendingOnUnreadableIncoming(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "dst.db")
	if err := StageSnapshot(dstPath, packedSnapshot(t, "staged")); err != nil {
		t.Fatal(err)
	}
	incoming := PendingSnapshotPath(dstPath)
	if err := os.Chmod(incoming, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(incoming, 0o600) })
	if _, err := Open(dstPath, testOpts); err == nil {
		t.Fatal("Open succeeded with an unreadable pending snapshot")
	}
	if _, err := os.Stat(incoming); err != nil {
		t.Fatalf("unreadable pending was discarded: %v", err)
	}
	if _, err := os.Stat(incoming + ".invalid"); !os.IsNotExist(err) {
		t.Fatal("unreadable pending was quarantined as invalid")
	}
}

func TestOpenAbortsAdmitWhenSidecarCannotBeRemoved(t *testing.T) {
	for _, sidecar := range []string{"-wal", "-shm"} {
		t.Run(sidecar, func(t *testing.T) {
			dir := t.TempDir()
			dstPath := filepath.Join(dir, "dst.db")
			dst, err := Open(dstPath, testOpts)
			if err != nil {
				t.Fatal(err)
			}
			dst.Record(&metrics.Record{ID: "keep-old", Provider: "neutral.example", Model: "n", StatusCode: 200})
			if err := dst.Flush(); err != nil {
				t.Fatal(err)
			}
			dst.Close()
			before, err := os.ReadFile(dstPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := StageSnapshot(dstPath, packedSnapshot(t, "incoming-row")); err != nil {
				t.Fatal(err)
			}
			blockRemove(t, dstPath+sidecar)
			if _, err := Open(dstPath, testOpts); err == nil {
				t.Fatal("Open succeeded when a WAL/SHM sidecar could not be removed")
			} else {
				msg := err.Error()
				if !strings.Contains(msg, "pending snapshot") {
					t.Fatalf("error %q should name the pending snapshot", msg)
				}
				// Exactly one level owns the label: the Open wrap. A doubled
				// "pending snapshot: pending snapshot:" means admitPendingSnapshot
				// started wrapping its own cause again.
				if n := strings.Count(msg, "pending snapshot:"); n != 1 {
					t.Fatalf("error %q carries %d \"pending snapshot:\" prefixes, want exactly 1", msg, n)
				}
			}
			if _, err := os.Stat(PendingSnapshotPath(dstPath)); err != nil {
				t.Fatalf("pending snapshot was discarded: %v", err)
			}
			after, err := os.ReadFile(dstPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("live database was replaced after a sidecar remove failure")
			}
		})
	}
}

func blockRemove(t *testing.T, path string) {
	t.Helper()
	_ = os.Remove(path)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "blocker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMergeSnapshotKeepsLiveRows(t *testing.T) {
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "shared", Provider: "live.example", Model: "n", StatusCode: 200})
	live.Record(&metrics.Record{ID: "live-only", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	src := openTestStore(t)
	src.Record(&metrics.Record{ID: "shared", Provider: "backup.example", Model: "n", StatusCode: 200})
	src.Record(&metrics.Record{ID: "backup-only", Provider: "backup.example", Model: "n", StatusCode: 200})
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	inserted, skipped, err := live.MergeSnapshot(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 1 || skipped != 1 {
		t.Fatalf("inserted=%d skipped=%d", inserted, skipped)
	}
	if _, err := live.db.Exec("DETACH DATABASE backup_merge"); err == nil {
		t.Fatal("backup_merge still attached after MergeSnapshot")
	}
	got, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*metrics.Record{}
	for _, r := range got {
		byID[r.ID] = r
	}
	if byID["shared"] == nil || byID["shared"].Provider != "live.example" {
		t.Fatalf("live row lost on conflict: %#v", byID["shared"])
	}
	if byID["live-only"] == nil || byID["backup-only"] == nil {
		t.Fatalf("merged rows = %#v", got)
	}
	if live.Totals().Requests != 3 {
		t.Fatalf("totals after merge = %d, want 3", live.Totals().Requests)
	}
}

func TestMergeSnapshotTotalsAfterDebugError(t *testing.T) {
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	data := packedSnapshot(t, "from-backup")
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE request_debug; CREATE TABLE request_debug (foo TEXT); INSERT INTO request_debug(foo) VALUES ('x')`); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(dir, "broken.db")
	if _, err := db.Exec("VACUUM INTO ?", broken); err != nil {
		t.Fatal(err)
	}
	db.Close()
	brokenData, err := os.ReadFile(broken)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = live.MergeSnapshot(t.Context(), brokenData)
	if err == nil {
		t.Fatal("merge succeeded with incompatible request_debug")
	}
	if live.Totals().Requests != 2 {
		t.Fatalf("totals after failed debug merge = %d, want 2 (requests already inserted)", live.Totals().Requests)
	}
	if n := countRows(t, live); n != 2 {
		t.Fatalf("rows after failed debug merge = %d, want the merged request committed", n)
	}
}

// TestMergeSnapshotSkipsDebugOverCap pins the sidecar cap across a restore:
// when the incoming fetchable captures do not fit the remaining budget, the
// whole debug set is skipped while request rows still merge.
func TestMergeSnapshotSkipsDebugOverCap(t *testing.T) {
	opts := testOpts
	opts.DebugMaxBytes = 64 << 10
	live, err := Open(filepath.Join(t.TempDir(), "live-debug-cap.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	now := time.Now()
	if err := live.SaveDebugCapture(t.Context(), "live-cap", "sess", now.UnixMilli(), 0, make([]byte, 32<<10)); err != nil {
		t.Fatal(err)
	}
	src := openTestStore(t)
	if err := src.SaveDebugCapture(t.Context(), "backup-cap", "sess", now.UnixMilli(), 0, make([]byte, 100<<10)); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := live.MergeSnapshot(t.Context(), data); err != nil {
		t.Fatal(err)
	}
	got, err := live.CountDebugSession(t.Context(), "sess")
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("debug captures after capped merge = %d, want only the live row", got)
	}
	live.debugMu.Lock()
	bytes := live.debugBytes
	live.debugMu.Unlock()
	if bytes != 32<<10 {
		t.Fatalf("debug byte counter = %d, want %d", bytes, 32<<10)
	}
}

func debugByteCounter(s *Store) int64 {
	s.debugMu.Lock()
	defer s.debugMu.Unlock()
	return s.debugBytes
}

func metaValue(t *testing.T, s *Store, key string) string {
	t.Helper()
	var value string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func projectionStateEpoch(t *testing.T, s *Store) int64 {
	t.Helper()
	var epoch int64
	if err := s.db.QueryRow(`SELECT epoch FROM request_projection_state WHERE singleton = 1`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	return epoch
}

func attachedDatabaseCount(t *testing.T, s *Store, name string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_database_list WHERE name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestMergeSnapshotTotalsFailureRollsBack pins merge atomicity: when the
// totals document cannot be persisted, the request rows and debug sidecars
// merged earlier in the same transaction disappear with it, in-memory totals
// and the sidecar byte counter stay untouched, ATTACH is cleaned up, and a
// healthy retry merges the archive exactly once.
func TestMergeSnapshotTotalsFailureRollsBack(t *testing.T) {
	opts := testOpts
	opts.DebugMaxBytes = 1 << 20
	live, err := Open(filepath.Join(t.TempDir(), "merge-rollback.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	const livePayload = "live-debug"
	if err := live.SaveDebugCapture(t.Context(), "live-debug", "sess", time.Now().UnixMilli(), 0, []byte(livePayload)); err != nil {
		t.Fatal(err)
	}

	src := openTestStore(t)
	src.Record(&metrics.Record{ID: "backup-row", Provider: "backup.example", Model: "n", StatusCode: 200})
	const backupPayload = "backup-debug"
	if err := src.SaveDebugCapture(t.Context(), "backup-debug", "sess", time.Now().UnixMilli(), 0, []byte(backupPayload)); err != nil {
		t.Fatal(err)
	}
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	beforeTotals := live.Totals()
	beforeDoc := metaValue(t, live, totalsMetaKey)
	beforeEpoch := projectionStateEpoch(t, live)
	beforeBytes := debugByteCounter(live)
	if beforeTotals.Requests != 1 || beforeBytes != int64(len(livePayload)) {
		t.Fatalf("test setup: totals=%+v debug bytes=%d", beforeTotals, beforeBytes)
	}
	if _, err := live.db.Exec(`CREATE TRIGGER test_merge_totals_abort BEFORE INSERT ON meta
		WHEN NEW.key = 'totals' BEGIN SELECT RAISE(ABORT, 'injected totals persist failure'); END`); err != nil {
		t.Fatal(err)
	}

	inserted, skipped, err := live.MergeSnapshot(t.Context(), data)
	if err == nil {
		t.Fatal("merge succeeded while the totals document was rejected")
	}
	if !strings.Contains(err.Error(), "totals") {
		t.Fatalf("merge error %q does not name the totals failure", err)
	}
	if inserted != 1 || skipped != 0 {
		t.Fatalf("failed merge counts inserted=%d skipped=%d, want 1/0", inserted, skipped)
	}
	if n := countRows(t, live); n != 1 {
		t.Fatalf("request rows after failed merge = %d, want only the live row", n)
	}
	recent, err := live.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recent {
		if r.ID == "backup-row" {
			t.Fatal("backup request row survived the rolled-back merge")
		}
	}
	if raw, err := live.LoadDebugCapture(t.Context(), "backup-debug"); err != nil || raw != nil {
		t.Fatalf("backup debug sidecar after failed merge = %q, %v", raw, err)
	}
	if raw, err := live.LoadDebugCapture(t.Context(), "live-debug"); err != nil || string(raw) != livePayload {
		t.Fatalf("live debug sidecar after failed merge = %q, %v", raw, err)
	}
	if got := live.Totals(); got != beforeTotals {
		t.Fatalf("in-memory totals after failed merge = %+v, want %+v", got, beforeTotals)
	}
	if got := metaValue(t, live, totalsMetaKey); got != beforeDoc {
		t.Fatalf("persisted totals after failed merge changed: %s", got)
	}
	if got := projectionStateEpoch(t, live); got != beforeEpoch {
		t.Fatalf("projection epoch after failed merge = %d, want %d", got, beforeEpoch)
	}
	if got := debugByteCounter(live); got != beforeBytes {
		t.Fatalf("debug byte counter after failed merge = %d, want %d", got, beforeBytes)
	}
	if n := attachedDatabaseCount(t, live, "backup_merge"); n != 0 {
		t.Fatal("backup_merge still attached after failed merge")
	}

	if _, err := live.db.Exec(`DROP TRIGGER test_merge_totals_abort`); err != nil {
		t.Fatal(err)
	}
	inserted, skipped, err = live.MergeSnapshot(t.Context(), data)
	if err != nil {
		t.Fatalf("healthy retry: %v", err)
	}
	if inserted != 1 || skipped != 0 {
		t.Fatalf("retry counts inserted=%d skipped=%d, want 1/0", inserted, skipped)
	}
	if n := countRows(t, live); n != 2 {
		t.Fatalf("request rows after retry = %d, want 2", n)
	}
	if got := live.Totals().Requests; got != 2 {
		t.Fatalf("totals after retry = %d, want 2", got)
	}
	if raw, err := live.LoadDebugCapture(t.Context(), "backup-debug"); err != nil || string(raw) != backupPayload {
		t.Fatalf("backup debug sidecar after retry = %q, %v", raw, err)
	}
	if got, want := debugByteCounter(live), int64(len(livePayload)+len(backupPayload)); got != want {
		t.Fatalf("debug byte counter after retry = %d, want %d", got, want)
	}
	if n := attachedDatabaseCount(t, live, "backup_merge"); n != 0 {
		t.Fatal("backup_merge still attached after retry")
	}
}

// TestMergeSnapshotDebugCapDisabledMergesSidecars covers the inserted==0
// branch with the sidecar cap disabled: debug-only backup rows commit without
// recomputing request totals.
func TestMergeSnapshotDebugCapDisabledMergesSidecars(t *testing.T) {
	live := openTestStore(t)
	src := openTestStore(t)
	payload := []byte(`{"body":"neutral"}`)
	if err := src.SaveDebugCapture(t.Context(), "backup-debug", "sess", time.Now().UnixMilli(), 0, payload); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	before := live.Totals()
	inserted, skipped, err := live.MergeSnapshot(t.Context(), data)
	if err != nil {
		t.Fatal(err)
	}
	if inserted != 0 || skipped != 0 {
		t.Fatalf("debug-only merge counts inserted=%d skipped=%d, want 0/0", inserted, skipped)
	}
	if got := live.Totals(); got != before {
		t.Fatalf("debug-only merge recomputed totals: %+v -> %+v", before, got)
	}
	raw, err := live.LoadDebugCapture(t.Context(), "backup-debug")
	if err != nil || string(raw) != string(payload) {
		t.Fatalf("backup debug sidecar = %q, %v", raw, err)
	}
	if n := attachedDatabaseCount(t, live, "backup_merge"); n != 0 {
		t.Fatal("backup_merge still attached after debug-only merge")
	}
}

// TestMergeSnapshotExpiredMaintenanceDeadlineRollsBack pins fail-closed totals
// scanning: an already expired maintenance budget fails the totals phase and
// rolls the earlier request and debug merges back instead of publishing rows
// with stale totals.
func TestMergeSnapshotExpiredMaintenanceDeadlineRollsBack(t *testing.T) {
	opts := testOpts
	opts.DebugMaxBytes = 1 << 20
	opts.MaintenanceTimeout = time.Nanosecond
	live, err := Open(filepath.Join(t.TempDir(), "merge-deadline.db"), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.Close() })
	live.Record(&metrics.Record{ID: "live-row", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	const livePayload = "live-debug"
	if err := live.SaveDebugCapture(t.Context(), "live-debug", "sess", time.Now().UnixMilli(), 0, []byte(livePayload)); err != nil {
		t.Fatal(err)
	}

	src := openTestStore(t)
	src.Record(&metrics.Record{ID: "backup-row", Provider: "backup.example", Model: "n", StatusCode: 200})
	if err := src.SaveDebugCapture(t.Context(), "backup-debug", "sess", time.Now().UnixMilli(), 0, []byte("backup-debug")); err != nil {
		t.Fatal(err)
	}
	if err := src.Flush(); err != nil {
		t.Fatal(err)
	}
	data, err := src.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	beforeTotals := live.Totals()
	beforeBytes := debugByteCounter(live)
	if _, _, err := live.MergeSnapshot(t.Context(), data); err == nil {
		t.Fatal("merge succeeded with an expired maintenance budget")
	}
	if n := countRows(t, live); n != 1 {
		t.Fatalf("request rows after expired-deadline merge = %d, want only the live row", n)
	}
	if raw, err := live.LoadDebugCapture(t.Context(), "backup-debug"); err != nil || raw != nil {
		t.Fatalf("backup debug sidecar after expired-deadline merge = %q, %v", raw, err)
	}
	if got := live.Totals(); got != beforeTotals {
		t.Fatalf("totals after expired-deadline merge = %+v, want %+v", got, beforeTotals)
	}
	if got := debugByteCounter(live); got != beforeBytes {
		t.Fatalf("debug byte counter after expired-deadline merge = %d, want %d", got, beforeBytes)
	}
	if n := attachedDatabaseCount(t, live, "backup_merge"); n != 0 {
		t.Fatal("backup_merge still attached after expired-deadline merge")
	}
}

func TestSnapshotOverlapDetaches(t *testing.T) {
	live := openTestStore(t)
	live.Record(&metrics.Record{ID: "shared", Provider: "live.example", Model: "n", StatusCode: 200})
	if err := live.Flush(); err != nil {
		t.Fatal(err)
	}
	n, err := live.SnapshotOverlap(t.Context(), packedSnapshot(t, "shared"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("overlap = %d, want 1", n)
	}
	n, err = live.SnapshotOverlap(t.Context(), packedSnapshot(t, "other"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("second overlap = %d, want 0 (leftover attach would fail this call)", n)
	}
}
