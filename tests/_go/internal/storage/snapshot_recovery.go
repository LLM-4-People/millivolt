package storage

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFileBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// walDatabaseBytes builds a canonical database whose only request row exists
// solely in un-checkpointed WAL frames. main and wal must be restored together;
// dropping the wal loses a committed row.
func walDatabaseBytes(t *testing.T) (mainBytes, walBytes []byte) {
	t.Helper()
	src := filepath.Join(t.TempDir(), "wal-src.db")
	db, err := sql.Open("sqlite", "file:"+src+"?_pragma=journal_mode(WAL)&_pragma=wal_autocheckpoint(0)&_pragma=synchronous(NORMAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO requests (
		id, provider, model, key_hash, user_agent, stream, status_code,
		started_at, duration_ms, ttft_ms, first_token_at, last_token_at,
		finish_reason, error_type, error_msg, error_code, tool_calls,
		input_tokens, output_tokens, total_tokens, cache_read_tokens,
		cache_write_tokens, reasoning_tokens, client_disconnected,
		rate_limit_remaining, rate_limit_limit, cost,
		turns_user, turns_assistant, turns_tool, response_headers
	) VALUES ('wal-row', 'neutral.example', 'm', 'k', 'ua', 0, 200,
		?, 0, 0, 0, 0, '', '', '', '', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, '')`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	var logFrames, checkpointed int
	if err := db.QueryRow(`PRAGMA wal_checkpoint(NOOP)`).Scan(new(int), &logFrames, &checkpointed); err != nil {
		t.Fatal(err)
	}
	if logFrames == 0 || checkpointed >= logFrames {
		t.Fatalf("fixture must leave committed frames only in the WAL: frames=%d checkpointed=%d", logFrames, checkpointed)
	}
	mainBytes, err = os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	walBytes, err = os.ReadFile(src + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	return mainBytes, walBytes
}

func controlWALRowReadable(t *testing.T, mainBytes, walBytes []byte) {
	t.Helper()
	control := filepath.Join(t.TempDir(), "control.db")
	writeFileBytes(t, control, mainBytes)
	writeFileBytes(t, control+"-wal", walBytes)
	db, err := sql.Open("sqlite", "file:"+control+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM requests WHERE id = 'wal-row'`).Scan(&n); err != nil {
		t.Fatalf("control read: %v", err)
	}
	if n != 1 {
		t.Fatalf("control read = %d, want the committed WAL row", n)
	}
}

// TestRecoveryPreservesCommittedWALAcrossRenameSteps models every rename
// position of an interrupted admission, including a crash during recovery
// after the previous main was restored but before its WAL moved. The durable
// pending marker, not the presence of `.previous`, decides that sidecars are
// part of the restore set, so the committed WAL row survives every ordering.
//
// Admission moves the live sidecars before the live main, so a surviving
// `.previous` main can never be paired with a path sidecar that belongs to the
// previous database. The recovery step that covers both "install moved the
// sidecars but not the main" and "recovery restored the main before its WAL"
// is the same on-disk shape: a live main plus `.previous-wal`.
func TestRecoveryPreservesCommittedWALAcrossRenameSteps(t *testing.T) {
	mainBytes, walBytes := walDatabaseBytes(t)
	controlWALRowReadable(t, mainBytes, walBytes)
	newBytes := packedSnapshot(t, "incoming-row")

	type state struct {
		name          string
		path          []byte
		pathWAL       []byte
		previous      []byte
		previousWAL   []byte
		incoming      []byte
		state         string
		wantCommitted bool
	}
	states := []state{
		{
			name: "before-any-rename", path: mainBytes, pathWAL: walBytes,
			incoming: newBytes, state: snapshotStatePending,
		},
		{
			// Admission moved the WAL first and crashed before moving the
			// main. Recovery moves the WAL back to the live name.
			name: "live-wal-moved", path: mainBytes, previousWAL: walBytes,
			incoming: newBytes, state: snapshotStatePending,
		},
		{
			// A pending marker from the earlier main-before-sidecars
			// ordering: the previous main moved first and the WAL is still at
			// the live name. The staged incoming file proves the replacement
			// was never installed, so the WAL must move back, not be deleted.
			name: "legacy-live-main-moved", previous: mainBytes, pathWAL: walBytes,
			incoming: newBytes, state: snapshotStatePending,
		},
		{
			// The whole previous set is aside and the incoming file is still
			// staged. Recovery restores the main before its sidecars.
			name: "live-set-moved", previous: mainBytes, previousWAL: walBytes,
			incoming: newBytes, state: snapshotStatePending,
		},
		{
			// The incoming main replaced the live name but never committed.
			name: "incoming-installed", previous: mainBytes, previousWAL: walBytes,
			path: newBytes, state: snapshotStatePending,
		},
		{
			// The reported defect: recovery restored the previous main, then
			// crashed before moving its WAL. The next boot must move the WAL
			// back, not delete it as commit debris.
			name: "recovery-main-restored", path: mainBytes, previousWAL: walBytes,
			state: snapshotStatePending,
		},
		{
			name: "committed-with-stale-previous", path: newBytes,
			previous: mainBytes, previousWAL: walBytes, state: snapshotStateCommitted,
			wantCommitted: true,
		},
	}
	for _, tc := range states {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "live.db")
			previous := path + pendingSnapshotPreviousSuffix
			incoming := path + pendingSnapshotSuffix
			if tc.path != nil {
				writeFileBytes(t, path, tc.path)
			}
			if tc.pathWAL != nil {
				writeFileBytes(t, path+"-wal", tc.pathWAL)
			}
			if tc.previous != nil {
				writeFileBytes(t, previous, tc.previous)
			}
			if tc.previousWAL != nil {
				writeFileBytes(t, previous+"-wal", tc.previousWAL)
			}
			if tc.incoming != nil {
				writeFileBytes(t, incoming, tc.incoming)
			}
			if err := writeSnapshotState(path, tc.state); err != nil {
				t.Fatal(err)
			}

			s, err := Open(path, testOpts)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			rows, err := s.LoadRecent(t.Context(), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("rows after recovery = %d, want 1", len(rows))
			}
			wantID := "wal-row"
			if tc.wantCommitted {
				wantID = "incoming-row"
			}
			if rows[0].ID != wantID {
				t.Fatalf("row after recovery = %q, want %q", rows[0].ID, wantID)
			}
			if _, err := os.Stat(previous); !os.IsNotExist(err) {
				t.Fatalf("recovery left a rollback sibling: %v", err)
			}
			if _, err := os.Stat(incoming); !os.IsNotExist(err) {
				t.Fatalf("recovery left the incoming file: %v", err)
			}
			if !tc.wantCommitted {
				if _, err := os.Stat(incoming + ".invalid-interrupted"); err != nil && tc.incoming != nil {
					t.Fatalf("interrupted incoming was not quarantined: %v", err)
				}
				if state, err := readSnapshotState(path); err != nil || state != "" {
					t.Fatalf("pending marker after recovery = %q, %v; want removed", state, err)
				}
			} else if state, err := readSnapshotState(path); err != nil || state != snapshotStateCommitted {
				t.Fatalf("committed marker after recovery = %q, %v; want committed", state, err)
			}
		})
	}
}

// TestRestorePreviousFailureKeepsPendingFence pins the rollback fence: a
// removal, rename, or directory sync failure must leave the pending marker and
// the surviving previous files for the next boot. Clearing the marker while a
// previous WAL is still aside would let the next boot classify that committed
// WAL as commit debris and delete it.
func TestRestorePreviousFailureKeepsPendingFence(t *testing.T) {
	type failure struct {
		name             string
		apply            func(t *testing.T, path string)
		clear            func(t *testing.T, path string)
		wantPreviousMain bool
		wantPreviousWAL  bool
		wantIncomingMain bool
	}
	cases := []failure{
		{
			// Removing the live incoming WAL fails (a non-empty directory
			// stands in for an I/O error), so the installed-set phase must
			// abort before the previous main can overwrite the live incoming
			// main. The previous set stays aside.
			name: "incoming-wal-unlink",
			apply: func(t *testing.T, path string) {
				blockPath(t, path+"-wal")
			},
			clear: func(t *testing.T, path string) {
				if err := os.RemoveAll(path + "-wal"); err != nil {
					t.Fatal(err)
				}
			},
			wantPreviousMain: true,
			wantPreviousWAL:  true,
			wantIncomingMain: true,
		},
		{
			// Removing the live incoming main fails, so rollback stops with
			// the entire previous set still aside.
			name: "incoming-main-unlink",
			apply: func(t *testing.T, path string) {
				blockPath(t, path)
			},
			clear: func(t *testing.T, path string) {
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
			},
			wantPreviousMain: true,
			wantPreviousWAL:  true,
		},
		{
			// The restore itself completed, but its directory sync failed, so
			// the marker must survive until a later boot can make it durable.
			name: "directory-sync",
			apply: func(t *testing.T, path string) {
				previousHook := snapshotDirSyncHook
				snapshotDirSyncHook = func(string) error {
					return errors.New("injected directory sync failure")
				}
				t.Cleanup(func() { snapshotDirSyncHook = previousHook })
			},
			clear: func(t *testing.T, path string) { snapshotDirSyncHook = nil },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "live.db")
			oldMain, oldWAL := walDatabaseBytes(t)
			controlWALRowReadable(t, oldMain, oldWAL)
			writeFileBytes(t, path, oldMain)
			writeFileBytes(t, path+"-wal", oldWAL)
			if err := StageSnapshot(path, packedSnapshot(t, "incoming-row")); err != nil {
				t.Fatal(err)
			}
			admission, err := admitPendingSnapshot(path)
			if err != nil {
				t.Fatal(err)
			}
			if admission == nil {
				t.Fatal("compatible pending snapshot was not admitted")
			}
			var incomingMain []byte
			if tc.wantIncomingMain {
				incomingMain, err = os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			tc.apply(t, path)
			if err := admission.rollback(); err == nil {
				t.Fatal("rollback succeeded despite the injected failure")
			}
			if state, err := readSnapshotState(path); err != nil || state != snapshotStatePending {
				t.Fatalf("state after failed rollback = %q, %v; want pending", state, err)
			}
			if tc.wantPreviousMain {
				if _, err := os.Stat(path + pendingSnapshotPreviousSuffix); err != nil {
					t.Fatalf("previous main after failed rollback: %v", err)
				}
			}
			if tc.wantPreviousWAL {
				if _, err := os.Stat(path + pendingSnapshotPreviousSuffix + "-wal"); err != nil {
					t.Fatalf("previous WAL after failed rollback: %v", err)
				}
			}
			if tc.wantIncomingMain {
				got, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("live incoming main after failed rollback: %v", err)
				}
				if !bytes.Equal(got, incomingMain) {
					t.Fatal("failed rollback replaced the live incoming main while its WAL removal was blocked")
				}
			}
			tc.clear(t, path)
			reopened, err := Open(path, testOpts)
			if err != nil {
				t.Fatalf("Open after clearing the obstruction: %v", err)
			}
			defer reopened.Close()
			rows, err := reopened.LoadRecent(t.Context(), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].ID != "wal-row" {
				t.Fatalf("rows after retried restore = %#v, want the committed WAL row", rows)
			}
			for _, row := range rows {
				if row.ID == "incoming-row" {
					t.Fatal("uncommitted incoming row survived the retried restore")
				}
			}
			if state, err := readSnapshotState(path); err != nil || state != "" {
				t.Fatalf("state after retried restore = %q, %v; want removed", state, err)
			}
			if _, err := os.Stat(path + pendingSnapshotPreviousSuffix); !os.IsNotExist(err) {
				t.Fatalf("retried restore left a rollback sibling: %v", err)
			}
		})
	}
}

// TestRecoveryRemovesIncomingWALBeforeRestoringPrevious pins the crash window
// between removing an uncommitted incoming main and removing its WAL. With no
// path main left, the next boot used to skip the incoming-set removal block,
// restore the previous main, and let SQLite apply the incoming WAL to it. The
// pending marker identifies every path sidecar as incoming debris when a
// previous main still exists, so the previous row is restored and the stale
// WAL is neutralized.
func TestRecoveryRemovesIncomingWALBeforeRestoringPrevious(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.db")
	previousBytes := packedSnapshot(t, "previous-row")
	_, incomingWAL := walDatabaseBytes(t)

	// Prove the hazard: applied to the previous main, the uncommitted WAL
	// surfaces its own row.
	control := filepath.Join(t.TempDir(), "control.db")
	writeFileBytes(t, control, previousBytes)
	writeFileBytes(t, control+"-wal", incomingWAL)
	controlDB, err := sql.Open("sqlite", "file:"+control+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var exposed int
	if err := controlDB.QueryRow(`SELECT COUNT(*) FROM requests WHERE id = 'wal-row'`).Scan(&exposed); err != nil {
		controlDB.Close()
		t.Fatal(err)
	}
	if err := controlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if exposed != 1 {
		t.Fatalf("control read exposed %d incoming WAL rows, want the stale WAL to be dangerous", exposed)
	}

	// Crash state: a failed earlier recovery removed the incoming main at
	// path, but not its WAL, and the pending marker is still durable.
	writeFileBytes(t, path+pendingSnapshotPreviousSuffix, previousBytes)
	writeFileBytes(t, path+"-wal", incomingWAL)
	if err := writeSnapshotState(path, snapshotStatePending); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rows, err := reopened.LoadRecent(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "previous-row" {
		t.Fatalf("rows after recovery = %#v, want only previous-row", rows)
	}
	if _, err := os.Stat(path + pendingSnapshotPreviousSuffix); !os.IsNotExist(err) {
		t.Fatalf("recovery left the previous main: %v", err)
	}
	// Open switches the restored database to WAL and may create a fresh
	// sidecar; the stale incoming frames must not survive it.
	if stale, err := os.ReadFile(path + "-wal"); err == nil && bytes.Equal(stale, incomingWAL) {
		t.Fatal("stale incoming WAL still attached to the restored previous main")
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if state, err := readSnapshotState(path); err != nil || state != "" {
		t.Fatalf("state after recovery = %q, %v; want removed", state, err)
	}
}
