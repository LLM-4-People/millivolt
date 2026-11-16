package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

// TestIsClosedClass pins the closed-handle class against real handles, not a
// fake error: database/sql answers a closed *sql.DB with its unexported
// errDBClosed message ("sql: database is closed") and a closed *sql.Conn with
// sql.ErrConnDone, and only the second is classified by errors.Is alone. A
// retry loop keyed on ErrConnDone therefore never stops on the DB case.
func TestIsClosedClass(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}

	// Live handle: an ordinary statement is not a closed error.
	if _, err := db.ExecContext(ctx, "select 1"); err != nil || IsClosed(err) {
		t.Fatalf("live exec = %v, classified closed=%v", err, IsClosed(err))
	}
	// A transient neighbour stays open-classified as false.
	if IsClosed(errors.New("database is locked")) || IsClosed(nil) {
		t.Fatal("busy or nil error classified as closed")
	}

	// Closed *sql.Conn: sql.ErrConnDone.
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "select 1"); err == nil || !IsClosed(err) {
		t.Fatalf("exec on a closed conn = %v, classified closed=%v", err, IsClosed(err))
	}

	// Closed *sql.DB: database/sql's own "database is closed" answer, raw and
	// wrapped the way a caller reports it.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, "select 1")
	if err == nil || !IsClosed(err) {
		t.Fatalf("exec on a closed db = %v, classified closed=%v", err, IsClosed(err))
	}
	if wrapped := fmt.Errorf("meta persist: %w", err); !IsClosed(wrapped) {
		t.Fatalf("wrapped closed error = %v, classified closed=false", wrapped)
	}
}
