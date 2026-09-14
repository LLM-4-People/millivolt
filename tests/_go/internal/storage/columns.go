package storage

import (
	"reflect"
	"strings"
	"testing"
)

// stripSQLLineComments removes `--`-to-EOL text from a hand-maintained SQL
// fragment before parsing: SQLite ignores such comments, but one inside
// either const would glue into an adjacent parsed name and false-red the pin.
// Column names are plain unquoted identifiers and never contain `--`, so the
// strip is unambiguous.
func stripSQLLineComments(s string) string {
	if !strings.Contains(s, "--") {
		return s
	}
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// insertColumnNamesIn parses the column sequence out of one INSERT
// statement: the names live in the parenthesized list between the
// statement's first '(' and its last ')'.
func insertColumnNamesIn(raw string) []string {
	body := stripSQLLineComments(raw)
	start := strings.IndexByte(body, '(')
	end := strings.LastIndexByte(body, ')')
	if start < 0 || end <= start {
		panic("insertColumns is not an INSERT ... (...) statement")
	}
	names := strings.Split(body[start+1:end], ",")
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = strings.TrimSpace(name)
	}
	return out
}

func insertColumnNames() []string {
	return insertColumnNamesIn(insertColumns)
}

// recordColumnNamesIn parses one select-side column sequence: the raw
// string is a comma-separated list carrying only names, whitespace,
// newlines and `--` line comments.
func recordColumnNamesIn(raw string) []string {
	var out []string
	for _, name := range strings.Split(stripSQLLineComments(raw), ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func recordColumnNames() []string {
	return recordColumnNamesIn(recordColumns)
}

// TestInsertColumnsMatchRecordColumns pins the two hand-maintained column
// lists (writer.go insertColumns, store.go recordColumns) to content AND
// order equality. They are written in lockstep by hand (the write side's
// values list and the read side's scanRecord follow the same order), and the
// dangerous drift direction - a column added to the INSERT but not the SELECT
// - fails silently as zero values. This pin makes every direction loud before
// any SQL runs.
func TestInsertColumnsMatchRecordColumns(t *testing.T) {
	insert, record := insertColumnNames(), recordColumnNames()
	if len(insert) == 0 || len(record) == 0 {
		t.Fatalf("parsed empty column sequence: insert=%v record=%v", insert, record)
	}
	if !reflect.DeepEqual(insert, record) {
		t.Fatalf("insert/record column sequences differ (content or order):\ninsert: %v\nrecord: %v", insert, record)
	}
}

// A `--` line comment inside either const is documentation SQLite ignores;
// the parse must strip it too, or the comment glues into an adjacent name
// and false-reds the equality pin above. This row proves a comment-carrying
// pair still parses equal on both sides.
func TestColumnParsersStripSQLLineComments(t *testing.T) {
	const insert = "INSERT OR REPLACE INTO t\n-- the writer's sequence\n(a, -- the id\n b)"
	const record = "\n-- the reader's sequence\n a, -- the id\n b\n-- trailing note\n"
	ins, rec := insertColumnNamesIn(insert), recordColumnNamesIn(record)
	want := []string{"a", "b"}
	if !reflect.DeepEqual(ins, want) || !reflect.DeepEqual(rec, want) {
		t.Fatalf("comment-carrying fragments parsed insert=%v record=%v, want %v on both sides", ins, rec, want)
	}
}
