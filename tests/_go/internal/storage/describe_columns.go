package storage

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

// TestDescribeDocumentsEveryRequestsColumn pins the hand-maintained MCP
// describe reference to the durable schema in both directions: every column
// the fresh schema holds (CREATE TABLE plus the migrations Open applies,
// req_param_presence's ALTER included) must be documented by describe, and
// every documented name must exist as a real column. The reference is
// curated rather than generated, so without this gate a column added to the
// DDL or an ALTER can ship undocumented, and a documented name can silently
// rot into a name no query can select.
func TestDescribeDocumentsEveryRequestsColumn(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "describe-columns.db"), testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// PRAGMA table_info is the schema's own authoritative column list; the
	// table-valued form keeps the read on the store's bounded SELECT reader.
	rows, err := s.Query(t.Context(), "SELECT name FROM pragma_table_info('requests')")
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the fresh requests schema exposes no columns; this contract test is no longer exercising the owner")
	}
	schemaColumns := make(map[string]bool, len(rows))
	for _, row := range rows {
		name, _ := row["name"].(string)
		if name == "" {
			t.Fatalf("table_info returned a column with no name: %v", row)
		}
		schemaColumns[name] = true
	}
	documented := mcp.RequestsColumnNames()
	if len(documented) == 0 {
		t.Fatal("describe documents no requests columns; RequestsColumnNames is no longer reading the reference")
	}
	documentedSet := make(map[string]bool, len(documented))
	for _, name := range documented {
		if documentedSet[name] {
			t.Fatalf("describe documents the requests column %q twice", name)
		}
		documentedSet[name] = true
		if !schemaColumns[name] {
			t.Errorf("describe documents %q, but the durable requests schema has no such column", name)
		}
	}
	for name := range schemaColumns {
		if !documentedSet[name] {
			t.Errorf("the durable requests schema has %q, which describe does not document", name)
		}
	}
	if t.Failed() {
		var missing, extra []string
		for name := range schemaColumns {
			if !documentedSet[name] {
				missing = append(missing, name)
			}
		}
		for name := range documentedSet {
			if !schemaColumns[name] {
				extra = append(extra, name)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		t.Logf("undocumented schema columns: %v; documented but absent: %v", missing, extra)
	}
}
