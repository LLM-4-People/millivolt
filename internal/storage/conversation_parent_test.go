package storage

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestParentConversationPersistsAndExports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lineage.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	s.Record(&metrics.Record{ID: "child", Start: time.Now(), Client: "fixture-client", KeyHash: "key", ConversationID: "s:child", ParentConversationID: "s:parent"})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.LoadRecent(t.Context(), 8)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if rows[0].ParentConversationID != "s:parent" || rows[0].ConversationID != "s:child" {
		t.Fatalf("lost declaration: %+v", rows[0])
	}
	var out bytes.Buffer
	if _, err := s.ExportWhere(t.Context(), PurgeFilter{}, &out); err != nil {
		t.Fatal(err)
	}
	var exported []metrics.Record
	if err := json.Unmarshal(out.Bytes(), &exported); err != nil {
		t.Fatal(err)
	}
	if len(exported) != 1 || exported[0].ParentConversationID != "s:parent" {
		t.Fatalf("export lost declaration: %s", out.String())
	}
}

func TestParentConversationMigrationLeavesLegacyUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-lineage.db")
	s, err := Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	s.Record(&metrics.Record{ID: "legacy", Start: time.Now(), ConversationID: "s:legacy"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("ALTER TABLE requests DROP COLUMN parent_conversation_id"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, testOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.LoadRecent(t.Context(), 8)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if rows[0].ConversationID != "s:legacy" || rows[0].ParentConversationID != "" {
		t.Fatalf("legacy parentage invented: %+v", rows[0])
	}
	s.Record(&metrics.Record{ID: "child", Start: time.Now(), ConversationID: "s:child", ParentConversationID: "s:legacy"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	rows, err = s.LoadRecent(t.Context(), 8)
	if err != nil || len(rows) != 2 {
		t.Fatalf("migrated write rows=%d err=%v", len(rows), err)
	}
}
