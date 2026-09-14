package storage

import (
	"reflect"
	"strings"
	"testing"
)

// insertColumnNames parses the column sequence out of the INSERT statement:
// the names live in the parenthesized list between the statement's first '('
// and its last ')'.
func insertColumnNames() []string {
	start := strings.IndexByte(insertColumns, '(')
	end := strings.LastIndexByte(insertColumns, ')')
	if start < 0 || end <= start {
		panic("insertColumns is not an INSERT ... (...) statement")
	}
	names := strings.Split(insertColumns[start+1:end], ",")
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = strings.TrimSpace(name)
	}
	return out
}

// recordColumnNames parses the select-side column sequence: the raw string is
// a comma-separated list carrying only names, whitespace and newlines.
func recordColumnNames() []string {
	var out []string
	for _, name := range strings.Split(recordColumns, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
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
