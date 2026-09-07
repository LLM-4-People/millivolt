package backup

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

const sqliteHeader = "SQLite format 3\x00"

// CheckDatabase admits a packed SQLite snapshot: header, integrity_check, and
// the requests table millivolt history requires.
func CheckDatabase(data []byte) error {
	return verifySQLiteSnapshot(data)
}

func verifySQLiteSnapshot(data []byte) error {
	if len(data) < len(sqliteHeader) || string(data[:len(sqliteHeader)]) != sqliteHeader {
		return fmt.Errorf("not a SQLite database")
	}
	dir, err := os.MkdirTemp("", "millivolt-backup-db-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var check string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil {
		return err
	}
	if !strings.EqualFold(check, "ok") {
		return fmt.Errorf("integrity_check: %s", check)
	}
	var name string
	if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='requests'`).Scan(&name); err != nil {
		return fmt.Errorf("missing requests table")
	}
	return nil
}
