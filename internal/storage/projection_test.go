package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func projectionStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func projectionWrite(t *testing.T, s *Store, id, provider string) {
	t.Helper()
	s.Record(&metrics.Record{ID: id, Provider: provider, Start: time.Now(), StatusCode: 200})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
}

func TestProjectionSnapshotAndAppendCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projection.db")
	s := projectionStore(t, path)
	projectionWrite(t, s, "first", "old.example")
	var cursor ProjectionCursor
	read := func(wantFull bool, wantIDs ...string) {
		t.Helper()
		var ids []string
		next, full, err := s.StreamProjection(t.Context(), "id", cursor, func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
			return nil
		})
		slices.Sort(ids)
		slices.Sort(wantIDs)
		if err != nil || !next.Ready || full != wantFull || !reflect.DeepEqual(ids, wantIDs) {
			t.Fatalf("projection = %v, full=%v, cursor=%+v, err=%v; want %v, full=%v", ids, full, next, err, wantIDs, wantFull)
		}
		cursor = next
	}
	read(true, "first")
	read(false)
	projectionWrite(t, s, "second", "old.example")
	read(false, "second")
	// An independent Store is the restart handoff's old/new process pair.
	other := projectionStore(t, path)
	projectionWrite(t, other, "external", "old.example")
	read(false, "external")
	tx, err := other.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("DELETE FROM requests"); err != nil {
		t.Fatal(err)
	}
	read(false) // uncommitted trigger changes are invisible too
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	read(false) // rolled-back mutations never invalidate/advance the cursor
	if _, err := other.db.Exec("INSERT OR REPLACE INTO meta(key,value) VALUES('fixture','value')"); err != nil {
		t.Fatal(err)
	}
	read(false)                                        // unrelated commits do not re-materialize analytical rows
	projectionWrite(t, other, "second", "new.example") // INSERT OR REPLACE
	read(true, "first", "external", "second")
	if _, err := other.db.Exec("UPDATE requests SET provider = ? WHERE id = ?", "changed.example", "first"); err != nil {
		t.Fatal(err)
	}
	read(true, "first", "external", "second")
	if _, err := other.db.Exec("DELETE FROM requests WHERE id = ?", "external"); err != nil {
		t.Fatal(err)
	}
	read(true, "first", "second")
	if _, err := other.db.Exec("DELETE FROM requests"); err != nil {
		t.Fatal(err)
	}
	read(true)
	if cursor.RowID != 0 {
		t.Fatalf("empty history retained obsolete rowid high-water: %+v", cursor)
	}
	projectionWrite(t, other, "after-clear", "new.example") // SQLite can reuse rowid 1
	read(false, "after-clear")                              // a completed rebuild must restore append-only efficiency
}

func TestProjectionLowRowIDAndSchemaInvalidation(t *testing.T) {
	s := projectionStore(t, filepath.Join(t.TempDir(), "projection.db"))
	projectionWrite(t, s, "high", "old.example")
	if _, err := s.db.Exec("UPDATE requests SET rowid = 100 WHERE id = 'high'"); err != nil {
		t.Fatal(err)
	}
	consume := func(*sql.Rows) error { return nil }
	cursor, _, err := s.StreamProjection(t.Context(), "id", ProjectionCursor{}, consume)
	if err != nil || cursor.RowID != 100 {
		t.Fatalf("initial high-water cursor = %+v, %v", cursor, err)
	}
	// Clone the existing valid fixture's columns, changing only id and rowid.
	// This keeps the regression independent of the request schema's width.
	cols, err := s.db.Query("SELECT name FROM pragma_table_info('requests')")
	if err != nil {
		t.Fatal(err)
	}
	names, values := []string{"rowid"}, []string{"50"}
	for cols.Next() {
		var name string
		if err := cols.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
		if name == "id" {
			values = append(values, "'low'")
		} else {
			values = append(values, name)
		}
	}
	if err := cols.Err(); err != nil {
		t.Fatal(err)
	}
	cols.Close()
	if _, err := s.db.Exec("INSERT INTO requests (" + strings.Join(names, ",") + ") SELECT " + strings.Join(values, ",") + " FROM requests WHERE id = 'high'"); err != nil {
		t.Fatal(err)
	}
	next, full, err := s.StreamProjection(t.Context(), "id", cursor, consume)
	if err != nil || !full || next.RowID != 100 || next.Epoch == cursor.Epoch {
		t.Fatalf("explicit lower rowid did not invalidate: %+v, full=%v, %v", next, full, err)
	}
	cursor = next
	if _, err := s.db.Exec("UPDATE requests SET rowid = 75 WHERE id = 'high'"); err != nil {
		t.Fatal(err)
	}
	next, full, err = s.StreamProjection(t.Context(), "id", cursor, consume)
	if err != nil || !full || next.RowID != 75 {
		t.Fatalf("lowered highest rowid did not rebase: %+v, full=%v, %v", next, full, err)
	}
	cursor = next
	projectionWrite(t, s, "after-rebase", "new.example")
	next, full, err = s.StreamProjection(t.Context(), "id", cursor, consume)
	if err != nil || full || next.RowID != 76 {
		t.Fatalf("rebased history lost append efficiency: %+v, full=%v, %v", next, full, err)
	}
	cursor = next
	if _, err := s.db.Exec("CREATE INDEX projection_fixture_index ON requests(client)"); err != nil {
		t.Fatal(err)
	}
	next, full, err = s.StreamProjection(t.Context(), "id", cursor, consume)
	if err != nil || !full || next.Schema == cursor.Schema {
		t.Fatalf("schema mutation did not invalidate: %+v, full=%v, %v", next, full, err)
	}
	cursor = next
	tr := projectionTriggers[0]
	if _, err := s.db.Exec("DROP TRIGGER " + tr.name); err != nil {
		t.Fatal(err)
	}
	if next, _, err := s.StreamProjection(t.Context(), "id", cursor, consume); err == nil || next.Ready {
		t.Fatalf("missing mutation trigger silently allowed projection reuse: %+v, %v", next, err)
	}
}

func TestProjectionConcurrentCommitKeepsCursorSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projection.db")
	s, other := projectionStore(t, path), projectionStore(t, path)
	projectionWrite(t, s, "before", "old.example")
	var ids []string
	cursor, full, err := s.StreamProjection(t.Context(), "id", ProjectionCursor{}, func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
		projectionWrite(t, other, "during", "new.example")
		return nil
	})
	if err != nil || !full || !reflect.DeepEqual(ids, []string{"before"}) {
		t.Fatalf("read transaction included a racing commit: %v, %v", ids, err)
	}
	ids = nil
	next, full, err := s.StreamProjection(t.Context(), "id", cursor, func(rows *sql.Rows) error {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
		return nil
	})
	if err != nil || full || !reflect.DeepEqual(ids, []string{"during"}) || next.RowID <= cursor.RowID {
		t.Fatalf("cursor skipped racing commit: %v, full=%v, %+v -> %+v, %v", ids, full, cursor, next, err)
	}
}

func TestProjectionErrorsNeverAdvanceCursor(t *testing.T) {
	s := projectionStore(t, filepath.Join(t.TempDir(), "projection.db"))
	projectionWrite(t, s, "a", "old.example")
	stop := errors.New("decode failed")
	if next, _, err := s.StreamProjection(t.Context(), "id", ProjectionCursor{}, func(*sql.Rows) error { return stop }); !errors.Is(err, stop) || next.Ready {
		t.Fatalf("failed decode advanced projection: %+v, %v", next, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if next, _, err := s.StreamProjection(ctx, "id", ProjectionCursor{}, func(*sql.Rows) error { return nil }); !errors.Is(err, context.Canceled) || next.Ready {
		t.Fatalf("canceled snapshot advanced projection: %+v, %v", next, err)
	}
	if next, _, err := s.StreamProjection(t.Context(), "missing_column", ProjectionCursor{}, func(*sql.Rows) error { return nil }); err == nil || next.Ready {
		t.Fatalf("invalid query advanced projection: %+v, %v", next, err)
	}
}

func TestProjectionTriggerDefinitionsUpgradeAtStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projection.db")
	s := projectionStore(t, path)
	projectionWrite(t, s, "a", "old.example")
	consume := func(*sql.Rows) error { return nil }
	before, _, err := s.StreamProjection(t.Context(), "id", ProjectionCursor{}, consume)
	if err != nil {
		t.Fatal(err)
	}
	tr := projectionTriggers[2]
	if _, err := s.db.Exec("DROP TRIGGER " + tr.name); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("CREATE TRIGGER " + tr.name + " AFTER UPDATE ON requests BEGIN SELECT 1; END"); err != nil {
		t.Fatal(err)
	}
	_ = projectionStore(t, path) // the next binary reconciles its owned definitions
	after, full, err := s.StreamProjection(t.Context(), "id", before, consume)
	if err != nil || !full || after.Epoch <= before.Epoch {
		t.Fatalf("trigger upgrade did not invalidate old projection: %+v -> %+v, full=%v, %v", before, after, full, err)
	}
	_ = projectionStore(t, path)
	unchanged, full, err := s.StreamProjection(t.Context(), "id", after, consume)
	if err != nil || full || unchanged != after {
		t.Fatalf("identical definitions should not invalidate again: %+v -> %+v, full=%v, %v", after, unchanged, full, err)
	}
}
