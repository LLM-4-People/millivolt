package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// DatabaseHeader is the fixed 16-byte header of a SQLite database file.
const DatabaseHeader = "SQLite format 3\x00"

// SQLiteURI composes the file: URI every SQLite connection uses for path.
// The path is percent-escaped and params are query-escaped, so metacharacters
// that would otherwise split the DSN (?, #), be decoded a second time (%), or
// delimit a URI (spaces and non-ASCII bytes) stay literal filename bytes.
// OmitHost keeps a relative path relative (file:proxy.db) instead of making
// its first segment a URI authority; absolute paths keep the conventional
// file:/... spelling SQLite accepts.
func SQLiteURI(path string, params url.Values) string {
	u := url.URL{Scheme: "file", Path: path, OmitHost: true}
	if len(params) > 0 {
		u.RawQuery = params.Encode()
	}
	return u.String()
}

// ErrInvalidSnapshot is a semantic reject: the bytes are not a usable
// millivolt SQLite snapshot. Transient I/O while verifying is a different error.
var ErrInvalidSnapshot = errors.New("invalid sqlite snapshot")

func invalidSnapshot(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSnapshot, fmt.Sprintf(format, args...))
}

// SnapshotCounts is the row inventory of a packed millivolt SQLite snapshot.
type SnapshotCounts struct {
	Requests int   `json:"requests"`
	Debug    int   `json:"debug"`
	OldestMs int64 `json:"oldest_ms,omitempty"`
	NewestMs int64 `json:"newest_ms,omitempty"`
}

// CheckDatabase admits a packed SQLite snapshot: header, integrity_check, and
// the requests table millivolt history requires. stageDir must be the
// directory that owns the live database: inspection stages a database-sized
// transient file there, so capacity follows the database volume instead of a
// generic /tmp that hardened deployments size as a small tmpfs.
func CheckDatabase(stageDir string, data []byte) error {
	_, err := InspectDatabase(stageDir, data)
	return err
}

// InspectDatabase is CheckDatabase plus request/debug row counts.
func InspectDatabase(stageDir string, data []byte) (SnapshotCounts, error) {
	return inspectSQLiteSnapshot(stageDir, data)
}

// CheckDatabaseFile admits an already-staged packed snapshot at path
// without copying it. Snapshot's VACUUM dest and a pending .incoming file
// already occupy one packed-snapshot of space on the live database volume;
// writing a second copy for integrity_check would need two.
func CheckDatabaseFile(path string) error {
	_, err := InspectDatabaseFile(path)
	return err
}

// InspectDatabaseFile is InspectDatabase for an already-staged private packed
// snapshot: header, integrity_check and row counts are read in place, without
// writing a second database-sized copy.
func InspectDatabaseFile(path string) (SnapshotCounts, error) {
	return inspectSQLiteFile(path)
}

// StageSnapshotError wraps a failure to create the staging directory for a
// database-sized transient file, naming the directory that rejected it.
// Staging must live on the live database's own volume (the backup-capacity
// contract), so the directory names the capacity owner. Shared by the
// backup inspector and storage's snapshot/overlap/merge staging sites so the
// wording cannot drift.
func StageSnapshotError(dir string, err error) error {
	return fmt.Errorf("stage snapshot in %s: %w", dir, err)
}

func inspectSQLiteSnapshot(stageDir string, data []byte) (SnapshotCounts, error) {
	if stageDir == "" {
		return SnapshotCounts{}, ErrSnapshotStaging
	}
	if len(data) < len(DatabaseHeader) || string(data[:len(DatabaseHeader)]) != DatabaseHeader {
		return SnapshotCounts{}, invalidSnapshot("not a SQLite database")
	}
	dir, err := os.MkdirTemp(stageDir, InspectDatabaseStagingPattern)
	if err != nil {
		return SnapshotCounts{}, StageSnapshotError(stageDir, err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return SnapshotCounts{}, err
	}
	return inspectSQLiteFile(path)
}

func inspectSQLiteFile(path string) (SnapshotCounts, error) {
	if path == "" {
		return SnapshotCounts{}, fmt.Errorf("snapshot path is required")
	}
	f, err := os.Open(path)
	if err != nil {
		return SnapshotCounts{}, err
	}
	hdr := make([]byte, len(DatabaseHeader))
	n, readErr := f.Read(hdr)
	_ = f.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return SnapshotCounts{}, readErr
	}
	if n < len(DatabaseHeader) || string(hdr[:n]) != DatabaseHeader {
		return SnapshotCounts{}, invalidSnapshot("not a SQLite database")
	}
	db, err := sql.Open("sqlite", SQLiteURI(path, url.Values{"mode": {"ro"}}))
	if err != nil {
		return SnapshotCounts{}, err
	}
	defer db.Close()
	var check string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil {
		return SnapshotCounts{}, invalidSnapshot("integrity_check: %v", err)
	}
	if !strings.EqualFold(check, "ok") {
		return SnapshotCounts{}, invalidSnapshot("integrity_check: %s", check)
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='requests'`).Scan(&name); err != nil {
		return SnapshotCounts{}, invalidSnapshot("missing requests table")
	}
	var info SnapshotCounts
	if err := db.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&info.Requests); err != nil {
		return SnapshotCounts{}, err
	}
	hasStart, err := sqliteHasColumn(db, "started_at")
	if err != nil {
		return SnapshotCounts{}, err
	}
	if hasStart {
		var oldest, newest sql.NullInt64
		if err := db.QueryRow(`SELECT MIN(started_at), MAX(started_at) FROM requests`).Scan(&oldest, &newest); err != nil {
			return SnapshotCounts{}, err
		}
		if oldest.Valid {
			info.OldestMs = oldest.Int64
		}
		if newest.Valid {
			info.NewestMs = newest.Int64
		}
	}
	var debugTable string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='request_debug'`).Scan(&debugTable); err == nil {
		if err := db.QueryRow(`SELECT COUNT(*) FROM request_debug`).Scan(&info.Debug); err != nil {
			return SnapshotCounts{}, err
		}
	}
	return info, nil
}

// TableColumnSet reads one table's column-name set through PRAGMA table_info.
// schema may name an ATTACHed schema ("" reads the main database); the caller
// owns the allowlist guards on both arguments before calling, because the
// query is built by concatenation. Shared by storage's migration/snapshot
// paths (through its tableColumnSet wrapper, which keeps the allowlist
// guards) and this package's snapshot inspector.
func TableColumnSet(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, schema, table string) (map[string]struct{}, error) {
	query := "PRAGMA table_info(" + table + ")"
	if schema != "" {
		query = "PRAGMA " + schema + ".table_info(" + table + ")"
	}
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := make(map[string]struct{})
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = struct{}{}
	}
	return cols, rows.Err()
}

// sqliteHasColumn reports whether the staged snapshot's requests table has the
// named column (the started_at marker of a migrated database). One membership
// check over the shared PRAGMA reader: table_info is a schema-page scan on a
// database that already passed integrity_check.
func sqliteHasColumn(db *sql.DB, name string) (bool, error) {
	cols, err := TableColumnSet(context.Background(), db, "", "requests")
	if err != nil {
		return false, err
	}
	_, ok := cols[name]
	return ok, nil
}
