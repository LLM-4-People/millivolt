package metrics

// The Go IsError corpus (iserror_corpus.go) and its JS twin in the extracted
// tests/ui_harness/sections/w16_mirrors.js (the recordIsError rows) agree
// today only by comment: a one-side edit passes every suite. This file locks
// them together by deriving the JS rows from that section's source and
// asserting they match the Go table row-for-row (names and wantErr, count
// included), the same source-derived contract the web suite applies to the
// harness fixtures.

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
)

// TestIsErrorCorpusLockstepWithJSRecordIsErrorRows reads the recordIsError
// corpus out of the extracted w16_mirrors section and asserts it equals the Go
// isErrorCorpus row-for-row: same row count, same names in the same order,
// same wantErr. A row renamed, added, removed or flipped on either side
// reddens here.
func TestIsErrorCorpusLockstepWithJSRecordIsErrorRows(t *testing.T) {
	ui, err := os.ReadFile(filepath.Join(moduleRoot(t), "tests", "ui_harness", "sections", "w16_mirrors.js"))
	if err != nil {
		t.Fatalf("read tests/ui_harness/sections/w16_mirrors.js: %v", err)
	}
	rows := jsIsErrorCorpusRows(t, ui)
	if len(rows) != len(isErrorCorpus) {
		t.Fatalf("JS recordIsError corpus has %d rows, Go isErrorCorpus has %d", len(rows), len(isErrorCorpus))
	}
	for i, row := range rows {
		if row.name != isErrorCorpus[i].name || row.wantErr != isErrorCorpus[i].wantErr {
			t.Errorf("corpus row %d disagrees: JS (%q, wantErr %v) vs Go (%q, wantErr %v)",
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
// w16_mirrors.js source: everything from the "const corpus = [" marker to its
// closing "];" line. A missing marker or terminator fails the test loudly.
func jsIsErrorCorpusRows(t *testing.T, src []byte) []jsCorpusRow {
	t.Helper()
	start := bytes.Index(src, []byte("const corpus = ["))
	if start < 0 {
		t.Fatal("w16_mirrors.js recordIsError corpus marker (const corpus = [) not found")
	}
	block := src[start:]
	if loc := jsCorpusEndRe.FindIndex(block); loc != nil {
		block = block[:loc[0]]
	} else {
		t.Fatal("w16_mirrors.js recordIsError corpus terminator (];) not found")
	}
	matches := jsCorpusRowRe.FindAllSubmatch(block, -1)
	rows := make([]jsCorpusRow, 0, len(matches))
	for _, m := range matches {
		rows = append(rows, jsCorpusRow{name: string(m[1]), wantErr: string(m[2]) == "true"})
	}
	return rows
}

// moduleRoot locates the repo root from this file's compile-time source path
// (the same self-location rule the web suite's JS harness contract
// applies). A stripped source view (for example a -trimpath build) cannot
// self-locate its source, so the lockstep pin fails with that reason instead
// of skipping or guessing a path.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok || file == "" {
		t.Fatalf("cannot locate own source path (source paths stripped, e.g. -trimpath): JS harness contract cannot be verified")
	}
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s: JS harness contract cannot be verified without the repository source view", dir)
		}
		dir = parent
	}
}
