package main

import (
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestDevBackupIncludesUncheckpointedWAL(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("optional dev backup requires Python3")
	}
	script, err := filepath.Abs("../../scripts/backup_db.py")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	source, destination := filepath.Join(dir, "source.db"), filepath.Join(dir, "copy.db")
	db, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range []string{"PRAGMA journal_mode=WAL", "CREATE TABLE sample(n INTEGER)", "PRAGMA wal_checkpoint(TRUNCATE)", "INSERT INTO sample VALUES(1)"} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(source + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("fixture must contain uncheckpointed WAL: %v", err)
	}
	if out, err := exec.Command(python, script, source, destination).CombinedOutput(); err != nil {
		t.Fatalf("backup: %v: %s", err, out)
	}
	copyDB, err := sql.Open("sqlite", destination)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var count int
	if err := copyDB.QueryRow("SELECT count(*) FROM sample").Scan(&count); err != nil || count != 1 {
		t.Fatalf("backup count=%d err=%v", count, err)
	}
	if out, err := exec.Command(python, script, source, destination).CombinedOutput(); err == nil {
		t.Fatalf("existing backup was overwritten: %s", out)
	}
	if err := db.QueryRow("SELECT count(*) FROM sample").Scan(&count); err != nil || count != 1 {
		t.Fatalf("source changed: count=%d err=%v", count, err)
	}
}

// Status is intentionally non-mutating, even against the pre-fix script.
// Unsafe overrides must be rejected before any build, signal, or file removal.
func TestDevScriptRejectsUnsafeOverrides(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{"DEV_PORT=8080", "DEV_PORT=0", "DEV_PORT=65536", "DEV_PORT=../8080", "DEV_HOST=0.0.0.0", "DEV_DB=" + filepath.Join(root, "proxy.db"), "DEV_DB=/tmp/millivolt/../important.db"} {
		t.Run(setting, func(t *testing.T) {
			cmd := exec.Command("bash", filepath.Join(root, "scripts/dev.sh"), "status")
			cmd.Env = append(os.Environ(), "DEV_PORT=8081", "DEV_HOST=127.0.0.1", "DEV_DB=/tmp/millivolt/millivolt-dev.db", setting)
			if out, err := cmd.CombinedOutput(); err == nil {
				t.Fatalf("unsafe override accepted: %s", out)
			}
		})
	}
}
