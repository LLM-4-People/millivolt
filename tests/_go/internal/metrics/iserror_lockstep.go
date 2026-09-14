package metrics

// The Go IsError corpus (iserror_corpus.go) and its JS twin in
// tests/ui_check.js (the recordIsError rows) agree today only by comment:
// a one-side edit passes every suite. This file locks them together by
// deriving the JS rows from the ui_check.js source and asserting they match
// the Go table row-for-row (names and wantErr, count included), the same
// source-derived contract the web suite applies to ui_check.js.

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// TestIsErrorCorpusLockstepWithJSRecordIsErrorRows reads the recordIsError
// corpus out of tests/ui_check.js and asserts it equals the Go isErrorCorpus
// row-for-row: same row count, same names in the same order, same wantErr.
// A row renamed, added, removed or flipped on either side reddens here.
func TestIsErrorCorpusLockstepWithJSRecordIsErrorRows(t *testing.T) {
	ui, err := os.ReadFile(filepath.Join(moduleRoot(t), "tests", "ui_check.js"))
	if err != nil {
		t.Fatalf("read tests/ui_check.js: %v", err)
	}
	rows := jsIsErrorCorpusRows(t, ui)
	if len(rows) != len(isErrorCorpus) {
		t.Fatalf("ui_check.js recordIsError corpus has %d rows, Go isErrorCorpus has %d", len(rows), len(isErrorCorpus))
	}
	for i, row := range rows {
		if row.name != isErrorCorpus[i].name || row.wantErr != isErrorCorpus[i].wantErr {
			t.Errorf("corpus row %d disagrees: ui_check.js (%q, wantErr %v) vs Go (%q, wantErr %v)",
				i, row.name, row.wantErr, isErrorCorpus[i].name, isErrorCorpus[i].wantErr)
		}
	}
}

type jsCorpusRow struct {
	name    string
	wantErr bool
}

var (
	// One row per line: ['name', {record}, wantErr]. Names carry no quotes,
	// so a reshaped or quoted row fails to match and the row-count
	// assertion reddens instead of the pin guessing.
	jsCorpusRowRe = regexp.MustCompile(`(?m)^\s*\['([^']*)',\s*\{.*\},\s*(true|false)\],?$`)
	// The corpus block ends at its own closing "];" line; row lines end
	// with "]," so the first line-anchored "];" after the marker closes it.
	jsCorpusEndRe = regexp.MustCompile(`(?m)^[ \t]*\];`)
)

// jsIsErrorCorpusRows extracts the recordIsError corpus rows from the
// ui_check.js source: everything from the "const corpus = [" marker to its
// closing "];" line. A missing marker or terminator fails the test loudly.
func jsIsErrorCorpusRows(t *testing.T, src []byte) []jsCorpusRow {
	t.Helper()
	start := bytes.Index(src, []byte("const corpus = ["))
	if start < 0 {
		t.Fatal("ui_check.js recordIsError corpus marker (const corpus = [) not found")
	}
	block := src[start:]
	if loc := jsCorpusEndRe.FindIndex(block); loc != nil {
		block = block[:loc[0]]
	} else {
		t.Fatal("ui_check.js recordIsError corpus terminator (];) not found")
	}
	matches := jsCorpusRowRe.FindAllSubmatch(block, -1)
	rows := make([]jsCorpusRow, 0, len(matches))
	for _, m := range matches {
		rows = append(rows, jsCorpusRow{name: string(m[1]), wantErr: string(m[2]) == "true"})
	}
	return rows
}

// moduleRoot locates the repo root from this file's compile-time source path
// (the same self-location rule the web suite's ui_check.js contract
// applies). A -trimpath build cannot self-locate its source - the lockstep
// pin then skips with the reason instead of guessing a path.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok || file == "" {
		t.Skip("cannot locate own source path (trimpath build) - ui_check.js contract unchecked")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("no go.mod above " + dir + " - ui_check.js contract unchecked")
		}
		dir = parent
	}
}
