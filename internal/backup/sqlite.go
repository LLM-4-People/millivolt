package backup

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const sqliteHeader = "SQLite format 3\x00"

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
// the requests table millivolt history requires.
func CheckDatabase(data []byte) error {
	_, err := InspectDatabase(data)
	return err
}

// InspectDatabase is CheckDatabase plus request/debug row counts.
func InspectDatabase(data []byte) (SnapshotCounts, error) {
	return inspectSQLiteSnapshot(data)
}

func inspectSQLiteSnapshot(data []byte) (SnapshotCounts, error) {
	if len(data) < len(sqliteHeader) || string(data[:len(sqliteHeader)]) != sqliteHeader {
		return SnapshotCounts{}, invalidSnapshot("not a SQLite database")
	}
	dir, err := os.MkdirTemp("", "millivolt-backup-db-*")
	if err != nil {
		return SnapshotCounts{}, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return SnapshotCounts{}, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
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

func sqliteHasColumn(db *sql.DB, name string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(requests)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var col, ctype string
		var notnull int
		var dflt any
		var pk int
		if err := rows.Scan(&cid, &col, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if col == name {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}
