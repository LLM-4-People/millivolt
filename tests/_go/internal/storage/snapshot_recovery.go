package storage

import (
	"database/sql"
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
			name: "live-main-moved", previous: mainBytes, pathWAL: walBytes,
			incoming: newBytes, state: snapshotStatePending,
		},
		{
			name: "live-sidecars-moved", previous: mainBytes, previousWAL: walBytes,
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
