package storage

import (
	"path/filepath"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestSQLiteURIAgreementAcrossWriterReadPreflightAndInspect pins the shared
// DSN owner across every connection site. A filename containing a query
// marker, fragment marker, percent sign, space, or non-ASCII character must
// remain one literal path for the writer pool, the read pool, the pending
// snapshot preflight, and the packed-snapshot inspector. Before the shared
// URI helper, the hand-built DSNs split on '?' and percent-decoded the path,
// so these names opened a different file or failed to open at all.
func TestSQLiteURIAgreementAcrossWriterReadPreflightAndInspect(t *testing.T) {
	for _, tc := range []struct {
		name string
		file string
	}{
		{name: "hash fragment", file: "hash#frag.db"},
		{name: "query marker", file: "query?opt.db"},
		{name: "percent escape", file: "pct%25.db"},
		{name: "space", file: "space name.db"},
		{name: "unicode", file: "unicode-é.db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.file)
			live, err := Open(path, testOpts)
			if err != nil {
				t.Fatalf("Open(%q): %v", path, err)
			}
			id := "literal-" + tc.name
			live.Record(&metrics.Record{ID: id, Provider: "neutral.example", Model: "n", StatusCode: 200})
			if err := live.Flush(); err != nil {
				t.Fatalf("writer pool at %q: %v", path, err)
			}
			rows, err := live.LoadRecent(t.Context(), 10)
			if err != nil {
				t.Fatalf("read pool at %q: %v", path, err)
			}
			if len(rows) != 1 || rows[0].ID != id {
				t.Fatalf("read pool rows at %q = %#v, want id %q", path, rows, id)
			}
			data, err := live.Snapshot(t.Context())
			if err != nil {
				t.Fatalf("snapshot at %q: %v", path, err)
			}
			if err := live.Close(); err != nil {
				t.Fatalf("Close(%q): %v", path, err)
			}

			info, err := backup.InspectDatabaseFile(path)
			if err != nil {
				t.Fatalf("InspectDatabaseFile(%q): %v", path, err)
			}
			if info.Requests != 1 {
				t.Fatalf("inspect at %q counted %d requests, want 1", path, info.Requests)
			}

			// Stage at the same metacharacter path: the .incoming sibling
			// carries the same name, so preflight and boot admission must
			// resolve it literally too.
			if err := StageSnapshot(path, data); err != nil {
				t.Fatalf("StageSnapshot(%q): %v", path, err)
			}
			if err := ValidatePendingSnapshot(path); err != nil {
				t.Fatalf("ValidatePendingSnapshot(%q): %v", path, err)
			}
			reopened, err := Open(path, testOpts)
			if err != nil {
				t.Fatalf("Open(%q) after staging: %v", path, err)
			}
			rows, err = reopened.LoadRecent(t.Context(), 10)
			if err != nil {
				reopened.Close()
				t.Fatalf("read pool after admission at %q: %v", path, err)
			}
			if len(rows) != 1 || rows[0].ID != id {
				reopened.Close()
				t.Fatalf("admitted rows at %q = %#v, want id %q", path, rows, id)
			}
			if err := reopened.Close(); err != nil {
				t.Fatalf("Close(%q) after admission: %v", path, err)
			}
			if info, err := backup.InspectDatabaseFile(path); err != nil || info.Requests != 1 {
				t.Fatalf("inspect after admission at %q = %+v, %v", path, info, err)
			}
		})
	}
}
