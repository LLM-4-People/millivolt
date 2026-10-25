package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// preflightPendingSnapshot initializes the staged database in place before any
// live file is touched. It opens read-write with the standard bounded busy
// wait and switches to a rollback journal: admission renames only the main
// file over the live path, so a WAL sidecar left behind would either be lost
// or apply to the wrong database. The initializer is the same one Open runs,
// so a snapshot admitted here cannot fail Open's schema sequence later.
//
// The pending path must already exist as a regular file. sql.Open would
// otherwise create an empty database that passes preflight, and a later boot
// would admit it over live history; an absent file is a missing snapshot
// (errors.Is(err, fs.ErrNotExist)), never an empty one.
func preflightPendingSnapshot(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("pending snapshot: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("pending snapshot is not a regular file: %s", path)
	}
	// The packed file is self-contained; sidecars beside it are debris from an
	// interrupted earlier preflight. Remove them before opening: a hot journal
	// would otherwise be applied to this (possibly different) snapshot and
	// silently replace its pages with older data. A clean preflight close
	// leaves none of its own, so the post-close pass below is a fence for a
	// filesystem that retained the unlink.
	if err := removeSnapshotSidecars(path); err != nil {
		return err
	}
	params := url.Values{}
	params.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeout.Milliseconds()))
	db, err := sql.Open("sqlite", backup.SQLiteURI(path, params))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode=DELETE").Scan(&mode); err != nil {
		db.Close()
		return fmt.Errorf("journal_mode: %w", err)
	}
	if !strings.EqualFold(mode, "delete") {
		db.Close()
		return fmt.Errorf("journal_mode: got %q, want delete", mode)
	}
	if err := initializeDatabase(db, path); err != nil {
		db.Close()
		return err
	}
	// initializeDatabase reconciles the four owned analytical triggers but
	// leaves every other trigger in place. An unowned write-path trigger on
	// requests could delete or rewrite accepted rows after the writer has
	// accounted them, so replacement admission rejects the whole set instead
	// of trusting the writer probe (which reads its own sentinel back).
	if err := preflightRequestTriggers(context.Background(), db); err != nil {
		db.Close()
		return err
	}
	// The initializer proves the writer INSERT executes, but a NULL request id
	// or another stored value scanRecord cannot read into its destination
	// would still pass that probe and then make every later history scan fail.
	// SQLite permits NULL in a TEXT PRIMARY KEY, including tables created
	// before the fresh schema gained NOT NULL, and it stores any value class in
	// any non-STRICT column (stream='abc', stream=2), so check the staged rows
	// before any live file is touched.
	if err := preflightRequestScanValues(context.Background(), db, "", "pending snapshot"); err != nil {
		db.Close()
		return err
	}
	// The legacy empty attempts spelling is a valid admission input (scans
	// treat it as no attempts), but HasError/count/export/purge run json_each
	// over the raw column. Canonicalize it to the writer's own no-value
	// document in the staged file before any live file is touched; merge does
	// the equivalent during its INSERT.
	if err := canonicalizePendingAttempts(context.Background(), db); err != nil {
		db.Close()
		return err
	}
	// The derived totals are the real aggregate owner: Totals.Add chooses the
	// effective TPS source, folds only cost-bearing cost/token denominators and
	// applies the checked integer arithmetic. Prove the whole staged snapshot
	// produces representable totals before any live file is touched.
	totals, err := readTotals(context.Background(), db)
	if err != nil {
		db.Close()
		return fmt.Errorf("pending snapshot aggregate totals: %w", err)
	}
	if err := totals.Err(); err != nil {
		db.Close()
		return fmt.Errorf("pending snapshot aggregate totals: %w", err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close preflight database: %w", err)
	}
	// A clean close after switching to DELETE leaves no sidecar. Remove any
	// stale one only now: deleting a WAL while it is open would discard its
	// committed pages.
	return removeSnapshotSidecars(path)
}

// preflightRequestTriggers rejects every trigger on requests that is not one of
// the four analytical triggers this package owns. ensureProjectionTriggers has
// already restored the owned definitions, so any other name is foreign: a
// replacement snapshot is a database file, and an unowned INSERT/UPDATE/DELETE
// trigger could silently delete or rewrite writes that insertBatch still counts.
func preflightRequestTriggers(ctx context.Context, db mergeConn) error {
	owned := make(map[string]struct{}, len(projectionTriggers))
	for _, tr := range projectionTriggers {
		owned[tr.name] = struct{}{}
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_schema WHERE type = 'trigger' AND tbl_name = 'requests'`)
	if err != nil {
		return fmt.Errorf("pending snapshot triggers: %w", err)
	}
	defer rows.Close()
	var foreign []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if _, ok := owned[name]; !ok {
			foreign = append(foreign, name)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(foreign) != 0 {
		sort.Strings(foreign)
		return fmt.Errorf("pending snapshot requests has %d unowned trigger(s) (%s); only the analytical projection triggers are permitted",
			len(foreign), strings.Join(foreign, ", "))
	}
	return nil
}

// canonicalizePendingAttempts turns the legacy empty attempts spelling into the
// writer's canonical no-value document in a staged replacement. The empty
// spelling is admission-compatible for scans, but HasError's json_each over the
// raw column would fail on it. A missing legacy attempts column needs no work.
func canonicalizePendingAttempts(ctx context.Context, db *sql.DB) error {
	columns, err := tableColumnSet(ctx, db, "", "requests")
	if err != nil {
		return err
	}
	if _, ok := columns["attempts"]; !ok {
		return nil
	}
	if _, err := db.ExecContext(ctx, `UPDATE requests SET attempts = 'null' WHERE attempts = ''`); err != nil {
		return fmt.Errorf("canonicalize pending attempts: %w", err)
	}
	return nil
}

// writerProbeIDAttempts bounds the sentinel-id search for the admission
// insert probe. Sixteen collisions on a reserved prefix are not a real schema,
// only tampering or a pathological fixture.
const writerProbeIDAttempts = 16

// probeWriterInsert proves the staged database accepts the writer's exact
// INSERT: the shared column list, conflict target and value ordering. The row
// is rolled back, so preflight leaves no data behind.
func probeWriterInsert(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	insert := batchInsert{tx: tx}
	defer insert.close()
	// The probe must actually execute the INSERT: ON CONFLICT(id) DO NOTHING
	// would make an existing sentinel id a silent no-op that never proves the
	// table's required columns. Pick a free sentinel inside the transaction.
	probeID := "millivolt-preflight"
	free := false
	for suffix := 0; suffix < writerProbeIDAttempts; suffix++ {
		var exists int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM requests WHERE id = ?)`, probeID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			free = true
			break
		}
		probeID = fmt.Sprintf("millivolt-preflight-%d", suffix)
	}
	if !free {
		return fmt.Errorf("no free preflight request id")
	}
	probe := &metrics.Record{ID: probeID, StatusCode: 200, Start: time.Unix(0, 0)}
	values := appendRecordValues(nil, probe, jsonNull, jsonNull, jsonNull)
	inserted, err := insert.exec(values)
	if err != nil {
		return err
	}
	if !inserted {
		return fmt.Errorf("writer insert probe did not insert its row")
	}
	// RowsAffected alone proves only that SQLite executed the INSERT. Read the
	// sentinel back inside the same transaction so a trigger that deletes or
	// rewrites the inserted row fails admission here, where the writer would
	// otherwise account a durable row that is not in history.
	var gotID string
	var gotStatus int
	if err := tx.QueryRow(`SELECT id, status_code FROM requests WHERE id = ?`, probeID).Scan(&gotID, &gotStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("writer insert probe row is missing after insert")
		}
		return err
	}
	if gotID != probeID || gotStatus != probe.StatusCode {
		return fmt.Errorf("writer insert probe row changed after insert (id %q status %d)", gotID, gotStatus)
	}
	return tx.Rollback()
}
