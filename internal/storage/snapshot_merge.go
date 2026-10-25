package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
)

// MergeSnapshot inserts snapshot rows whose ids are not already in the live
// store (INSERT OR IGNORE). Existing live rows win. The live file is not
// replaced; no restart is required. It is the byte-buffered wrapper over
// MergeSnapshotFile for callers that hold the members in heap.
func (s *Store) MergeSnapshot(ctx context.Context, data []byte) (inserted, skipped int64, err error) {
	if s == nil {
		return 0, 0, ErrStorageDisabled
	}
	// Admin mutations serialize in MergeSnapshotFile, the one merge owner:
	// purge holds the same mutex, so a merge can never queue behind (or race) a
	// delete and starve the writer twice. Staging the caller's bytes does not
	// need the lock.
	dir, err := os.MkdirTemp(s.stageDir(), mergeStagingPattern)
	if err != nil {
		return 0, 0, backup.StageSnapshotError(s.stageDir(), err)
	}
	defer os.RemoveAll(dir)
	src := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(src, data, 0o600); err != nil {
		return 0, 0, err
	}
	return s.MergeSnapshotFile(ctx, src)
}

// MergeSnapshotFile inserts the rows of an already-staged private packed
// snapshot whose ids are not already in the live store. The caller's file is
// attached in place and is not copied, removed, or otherwise owned by the
// store. The transaction, live-row-wins rule, debug cap, totals and rollback
// semantics are the merge owner's; the byte wrapper only stages a file.
func (s *Store) MergeSnapshotFile(ctx context.Context, path string) (inserted, skipped int64, err error) {
	if s == nil {
		return 0, 0, ErrStorageDisabled
	}
	var debugDelta int64
	// Admin mutations serialize: purge holds the same mutex, so a merge can
	// never queue behind (or race) a delete and starve the writer twice.
	s.purgeMu.Lock()
	defer s.purgeMu.Unlock()
	if _, err := backup.InspectDatabaseFile(path); err != nil {
		return 0, 0, err
	}
	if err := s.Flush(); err != nil {
		return 0, 0, fmt.Errorf("flush before merge: %w", err)
	}
	// Hold the single writer connection for ATTACH through the merge
	// transaction and installTotals so insertBatch cannot account() a
	// concurrent commit against pre-merge aggregates. ATTACH stays outside
	// the transaction; the merge writes only the main database, and the
	// transaction's own view includes its uncommitted rows for the totals scan.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()
	if err := attachSnapshotDatabase(ctx, conn, "backup_merge", path); err != nil {
		return 0, 0, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "DETACH DATABASE backup_merge") }()
	// Prove the backup's writable shape before touching any row. INNER-style
	// column intersection alone would silently drop required columns or an
	// incoming NOT NULL column that has no live home; both are data loss.
	if err := preflightMergeSchema(ctx, conn); err != nil {
		return 0, 0, fmt.Errorf("preflight backup merge: %w", err)
	}
	// Request rows, debug sidecars and the totals document commit as one
	// transaction: a failure in any later phase rolls the earlier merges back
	// instead of publishing rows whose totals were never persisted.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	skipped, err = countMergeDuplicates(ctx, tx, "requests")
	if err != nil {
		return 0, 0, fmt.Errorf("count duplicate request rows from backup: %w", err)
	}
	n, err := mergeAttachedTable(ctx, tx, "requests")
	if err != nil {
		return 0, 0, fmt.Errorf("merge request rows from backup: %w", err)
	}
	inserted = n
	if s.debugMaxBytes > 0 {
		// The net sidecar delta is applied under debugMu after commit. Never
		// publish an absolute total computed in this transaction: a capture
		// save cannot interleave while the writer connection is held, but the
		// delta contract is shared with purge so both owners stay exact.
		debugBefore, err := debugByteTotal(ctx, tx)
		if err != nil {
			return 0, 0, fmt.Errorf("debug byte counter before backup merge: %w", err)
		}
		if _, err := s.mergeDebugTable(ctx, tx); err != nil {
			return 0, 0, fmt.Errorf("merge debug captures from backup: %w", err)
		}
		debugAfter, err := debugByteTotal(ctx, tx)
		if err != nil {
			return 0, 0, fmt.Errorf("debug byte counter after backup merge: %w", err)
		}
		debugDelta = debugAfter - debugBefore
	} else if _, err := mergeAttachedTable(ctx, tx, "request_debug"); err != nil {
		return 0, 0, fmt.Errorf("merge debug captures from backup: %w", err)
	}
	var totals Totals
	var totalsEpoch, totalsHighRowID int64
	if inserted > 0 {
		// The post-merge totals scan is maintenance work on the held writer
		// connection: it legitimately reads all history and must not inherit the
		// interactive dashboard query timeout.
		tctx, cancel := s.withMaintenanceTimeout(context.Background())
		defer cancel()
		// The transaction sees its own merged rows, so this scan is exact.
		totals, err = readTotals(tctx, tx)
		if err != nil {
			return inserted, skipped, fmt.Errorf("totals after backup merge: %w", err)
		}
		if err := totals.Err(); err != nil {
			// The transaction sees live plus incoming rows, so this is the exact
			// combined total. Reject before saveTotals: the deferred rollback
			// leaves the live file and its prior totals untouched.
			return inserted, skipped, fmt.Errorf("totals after backup merge: %w", err)
		}
		totalsEpoch, err = projectionEpoch(tctx, tx)
		if err != nil {
			return inserted, skipped, fmt.Errorf("projection epoch after backup merge: %w", err)
		}
		totalsHighRowID, err = projectionHighRowID(tctx, tx)
		if err != nil {
			return inserted, skipped, fmt.Errorf("projection high rowid after backup merge: %w", err)
		}
		if err := saveTotals(tctx, tx, totals, false, totalsEpoch, totalsHighRowID); err != nil {
			return inserted, skipped, fmt.Errorf("persist totals after backup merge: %w", err)
		}
	}
	if err := s.commitDebugDelta(tx, debugDelta); err != nil {
		return inserted, skipped, fmt.Errorf("commit backup merge: %w", err)
	}
	// Committed: these in-memory publications are mandatory and never consult
	// the caller's context.
	if inserted > 0 {
		s.installTotals(totals, totalsEpoch, totalsHighRowID)
	}
	return inserted, skipped, nil
}

type mergeConn interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// mergeColumnDetails is the PRAGMA table_info view merge preflight needs:
// whether a column is required and whether SQLite can supply it when omitted.
type mergeColumnDetails struct {
	name       string
	notNull    bool
	hasDefault bool
}

// tableColumnDetails reads one table's columns with their required/default
// metadata. The schema/table arguments are allowlisted because the query is
// built by concatenation.
func tableColumnDetails(ctx context.Context, db mergeConn, schema, table string) ([]mergeColumnDetails, error) {
	if schema != "" && schema != "backup_merge" && schema != "backup_inspect" {
		return nil, fmt.Errorf("unknown backup schema")
	}
	if table != "requests" && table != "request_debug" {
		return nil, fmt.Errorf("unknown merge table")
	}
	query := "PRAGMA table_info(" + table + ")"
	if schema != "" {
		query = "PRAGMA " + schema + ".table_info(" + table + ")"
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []mergeColumnDetails
	for rows.Next() {
		var cid, notNull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols = append(cols, mergeColumnDetails{
			name:       name,
			notNull:    notNull != 0,
			hasDefault: dflt.Valid,
		})
	}
	return cols, rows.Err()
}

// preflightMergeSchema rejects a backup whose request or debug table cannot
// accept the live writer's required values before the merge transaction
// touches any row. Two directions matter: a live required column the backup
// omits would insert NULL (or disappear), and an incoming required column the
// live schema does not have would be silently dropped.
func preflightMergeSchema(ctx context.Context, db mergeConn) error {
	var name string
	if err := db.QueryRowContext(ctx, `SELECT name FROM backup_merge.sqlite_master WHERE type='table' AND name='requests'`).Scan(&name); err != nil {
		return fmt.Errorf("backup requests table: %w", err)
	}
	if err := preflightMergeTable(ctx, db, "requests"); err != nil {
		return err
	}
	if err := preflightRequestScanValues(ctx, db, "backup_merge", "backup"); err != nil {
		return err
	}
	if err := db.QueryRowContext(ctx, `SELECT name FROM backup_merge.sqlite_master WHERE type='table' AND name='request_debug'`).Scan(&name); err == nil {
		if err := preflightMergeTable(ctx, db, "request_debug"); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func preflightMergeTable(ctx context.Context, db mergeConn, table string) error {
	live, err := tableColumnDetails(ctx, db, "", table)
	if err != nil {
		return err
	}
	incoming, err := tableColumnDetails(ctx, db, "backup_merge", table)
	if err != nil {
		return err
	}
	liveSet := make(map[string]mergeColumnDetails, len(live))
	for _, col := range live {
		liveSet[col.name] = col
	}
	incomingSet := make(map[string]mergeColumnDetails, len(incoming))
	for _, col := range incoming {
		incomingSet[col.name] = col
	}
	if _, ok := incomingSet["id"]; !ok {
		return fmt.Errorf("backup %s is missing the id column", table)
	}
	for _, col := range live {
		if !col.notNull || col.hasDefault {
			continue
		}
		if _, ok := incomingSet[col.name]; !ok {
			return fmt.Errorf("backup %s is missing required column %q", table, col.name)
		}
	}
	for _, col := range incoming {
		if _, ok := liveSet[col.name]; ok {
			continue
		}
		if col.notNull && !col.hasDefault {
			return fmt.Errorf("backup %s has unsupported required column %q", table, col.name)
		}
	}
	if table == "requests" {
		// A nullable live column cannot guarantee a non-NULL value when the
		// incoming table omits it: its default may itself be NULL or an
		// expression that yields one, and the intersection INSERT would then
		// write NULL into every merged row and poison scanRecord. Reject that
		// shape before the transaction. A required live column with a default
		// still admits a pre-column backup: SQLite's NOT NULL constraint
		// guarantees the omitted value cannot be NULL.
		for _, name := range recordScanColumns() {
			liveCol, ok := liveSet[name]
			if !ok {
				return fmt.Errorf("live requests is missing scan-required column %q", name)
			}
			if _, ok := incomingSet[name]; ok {
				continue
			}
			if !liveCol.notNull {
				return fmt.Errorf("backup requests is missing scan-required column %q; the live column is nullable and omission would insert NULL", name)
			}
		}
	}
	return nil
}

// countMergeDuplicates counts incoming ids that already exist live. The count
// is the explicit skip count; constraint failures are no longer folded into
// it, because INSERT OR IGNORE is gone.
func countMergeDuplicates(ctx context.Context, db mergeConn, table string) (int64, error) {
	if table != "requests" && table != "request_debug" {
		return 0, fmt.Errorf("unknown merge table")
	}
	var n int64
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_merge.`+table+` AS incoming
		WHERE EXISTS (SELECT 1 FROM main.`+table+` AS live WHERE live.id = incoming.id)`).Scan(&n)
	return n, err
}

// mergeDebugTable enforces the sidecar byte cap during a restore merge. With
// an active cap it counts only fetchable incoming rows and skips the whole
// debug set (logging the amount) when it cannot fit; request rows still merge.
func (s *Store) mergeDebugTable(ctx context.Context, db mergeConn) (int64, error) {
	if s.debugMaxBytes <= 0 {
		return mergeAttachedTable(ctx, db, "request_debug")
	}
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM backup_merge.sqlite_master WHERE type='table' AND name='request_debug'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	incoming, err := pragmaTableInfo(ctx, db, "backup_merge", "request_debug")
	if err != nil {
		return 0, err
	}
	where := ""
	var args []any
	if _, ok := incoming["expires_at"]; ok {
		where = `(incoming."expires_at" = 0 OR incoming."expires_at" > ?)`
		args = append(args, time.Now().UnixMilli())
	}
	var current int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(LENGTH(CAST(payload AS BLOB))), 0) FROM request_debug`).Scan(&current); err != nil {
		return 0, err
	}
	statsQuery := `SELECT COUNT(*), COALESCE(SUM(LENGTH(CAST(payload AS BLOB))), 0) FROM backup_merge.request_debug AS incoming`
	if where != "" {
		statsQuery += " WHERE " + where
	}
	var incomingRows, incomingBytes int64
	if err := db.QueryRowContext(ctx, statsQuery, args...).Scan(&incomingRows, &incomingBytes); err != nil {
		return 0, err
	}
	if remaining := s.debugMaxBytes - current; incomingBytes > remaining {
		log.Printf("storage: backup merge skipped %d debug captures (%d bytes) over the %d-byte store cap",
			incomingRows, incomingBytes, s.debugMaxBytes)
		return 0, nil
	}
	return mergeAttachedTableWhere(ctx, db, "request_debug", where, args...)
}

func mergeAttachedTable(ctx context.Context, db mergeConn, table string) (int64, error) {
	return mergeAttachedTableWhere(ctx, db, table, "", nil)
}

func mergeAttachedTableWhere(ctx context.Context, db mergeConn, table, where string, whereArgs ...any) (int64, error) {
	if table != "requests" && table != "request_debug" {
		return 0, fmt.Errorf("unknown merge table")
	}
	var name string
	err := db.QueryRowContext(ctx, `SELECT name FROM backup_merge.sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	live, err := pragmaTableInfo(ctx, db, "", table)
	if err != nil {
		return 0, err
	}
	incoming, err := pragmaTableInfo(ctx, db, "backup_merge", table)
	if err != nil {
		return 0, err
	}
	cols := intersectColumns(live, incoming)
	if len(cols) == 0 {
		return 0, fmt.Errorf("backup %s has no columns in common", table)
	}
	selects := make([]string, len(cols))
	for i, col := range cols {
		quoted := `incoming."` + col + `"`
		// Merge owns canonicalization of JSON-bearing columns: the legacy
		// empty spelling becomes the canonical no-value document, while a
		// non-empty value already passed preflight's json_valid check and is
		// copied byte-for-byte.
		if table == "requests" && col == "attempts" {
			quoted = `CASE WHEN incoming."attempts" IS NULL OR incoming."attempts" = '' THEN 'null' ELSE incoming."attempts" END`
		}
		selects[i] = quoted
	}
	list := quoteIdents(cols)
	// No INSERT OR IGNORE: the explicit NOT EXISTS owns duplicate suppression,
	// and every other constraint failure must abort the merge transaction
	// instead of silently discarding an incompatible row.
	q := "INSERT INTO " + table + " (" + list + ") SELECT " + strings.Join(selects, ", ") +
		" FROM backup_merge." + table + " AS incoming" +
		" WHERE NOT EXISTS (SELECT 1 FROM main." + table + " AS live WHERE live.id = incoming.id)"
	if where != "" {
		q += " AND (" + where + ")"
	}
	res, err := db.ExecContext(ctx, q, whereArgs...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// pragmaTableInfo guards the schema/table allowlists (the query is built by
// concatenation, so the guards are load-bearing) and delegates the shared
// PRAGMA read to tableColumnSet.
func pragmaTableInfo(ctx context.Context, db mergeConn, schema, table string) (map[string]struct{}, error) {
	if schema != "" && schema != "backup_merge" && schema != "backup_inspect" {
		return nil, fmt.Errorf("unknown backup schema")
	}
	if table != "requests" && table != "request_debug" {
		return nil, fmt.Errorf("unknown merge table")
	}
	return tableColumnSet(ctx, db, schema, table)
}

func intersectColumns(a, b map[string]struct{}) []string {
	var out []string
	for name := range a {
		if _, ok := b[name]; ok && sqliteIdent(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func quoteIdents(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = `"` + n + `"`
	}
	return strings.Join(quoted, ", ")
}

func sqliteIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		if r != '_' && (r < 'a' || r > 'z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}
