package storage

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A test binary pays the fresh-schema migration once per opened database:
// table and index creation, planner statistics, and the writer/read pools.
// Tests that start from an empty scratch store only need a schema, so the
// package migrates one template under sync.Once and hands each test a private
// copy. A test that observes migration, repair or admission itself must keep
// calling Open on an empty path: the template is already migrated by
// construction.

var (
	testTemplateOnce sync.Once
	testTemplatePath string
	testTemplateDir  string
	testTemplateErr  error
)

func buildTestTemplate() {
	directory, err := os.MkdirTemp("", "millivolt-storage-template-")
	if err != nil {
		testTemplateErr = err
		return
	}
	testTemplateDir = directory
	path := filepath.Join(directory, "template.db")
	store, err := Open(path, testOpts)
	if err != nil {
		testTemplateErr = err
		return
	}
	if err := store.Close(); err != nil {
		testTemplateErr = err
		return
	}
	testTemplatePath = path
}

// testDatabase returns a fresh database path holding a private copy of the
// migrated template. Close every Store opened on it before the test ends, the
// same contract as an empty directory.
func testDatabase(t *testing.T) string {
	t.Helper()
	testTemplateOnce.Do(buildTestTemplate)
	if testTemplateErr != nil {
		t.Fatalf("build SQLite test template: %v", testTemplateErr)
	}
	contents, err := os.ReadFile(testTemplatePath)
	if err != nil {
		t.Fatalf("read SQLite test template: %v", err)
	}
	path := filepath.Join(t.TempDir(), "store.db")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write SQLite test database: %v", err)
	}
	return path
}

// TestTestTemplateMatchesRealOpen proves the copy is the schema a fresh Open
// produces, not a lookalike: user version, journal mode and every schema entry
// must match byte-for-byte. The template's WAL is checkpointed and removed by
// Close, so only the main file is copied and WAL is re-established when the
// copy opens.
func TestTestTemplateMatchesRealOpen(t *testing.T) {
	schema := func(store *Store) []string {
		t.Helper()
		rows, err := store.rdb.QueryContext(t.Context(),
			"SELECT type || ':' || name || ':' || COALESCE(sql, '') FROM sqlite_master ORDER BY type, name")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var entries []string
		for rows.Next() {
			var entry string
			if err := rows.Scan(&entry); err != nil {
				t.Fatal(err)
			}
			entries = append(entries, entry)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return entries
	}
	pragmas := func(store *Store) (int, string) {
		t.Helper()
		var version int
		if err := store.rdb.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&version); err != nil {
			t.Fatal(err)
		}
		var journal string
		if err := store.db.QueryRowContext(t.Context(), "PRAGMA journal_mode").Scan(&journal); err != nil {
			t.Fatal(err)
		}
		return version, journal
	}

	realPath := filepath.Join(t.TempDir(), "real.db")
	real, err := Open(realPath, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer real.Close()
	copied, err := Open(testDatabase(t), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()

	realVersion, realJournal := pragmas(real)
	copiedVersion, copiedJournal := pragmas(copied)
	if copiedVersion != realVersion || copiedJournal != realJournal {
		t.Fatalf("template version/journal = %d/%q, want %d/%q", copiedVersion, copiedJournal, realVersion, realJournal)
	}
	want, got := schema(real), schema(copied)
	if len(want) != len(got) {
		t.Fatalf("schema entries = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("schema entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestMain owns the one template directory for the whole test binary.
func TestMain(m *testing.M) {
	code := m.Run()
	if testTemplateDir != "" {
		if err := os.RemoveAll(testTemplateDir); err != nil {
			os.Stderr.WriteString("storage tests: remove template: " + err.Error() + "\n")
		}
	}
	os.Exit(code)
}
