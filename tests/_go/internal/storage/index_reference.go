package storage

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/mcp"
)

// createIndexDDL matches one schema-created index and captures its name and
// column list, so the reference can be compared against the DDL that owns it.
var createIndexDDL = regexp.MustCompile(`(?s)CREATE INDEX IF NOT EXISTS\s+(\w+)\s+ON\s+\w+\s*\(([^)]*)\)`)

// TestRequestIndexReferenceMatchesTheDurableSchema pins the MCP planner
// reference to the DDL that owns it, in both directions: every index the
// durable schema creates must be published with exactly those columns, every
// published index must exist, and the published no-index claim must name
// exactly the columns no index covers. An index added, removed, renamed or
// re-columned on either side fails here.
func TestRequestIndexReferenceMatchesTheDurableSchema(t *testing.T) {
	declared := map[string]string{}
	for _, match := range createIndexDDL.FindAllStringSubmatch(schema, -1) {
		declared[match[1]] = normalizeIndexColumns(match[2])
	}
	if len(declared) == 0 {
		t.Fatal("the durable schema declares no indexes; this contract test is no longer exercising the owner")
	}
	published := map[string]string{}
	var noIndexClaims []string
	for _, entry := range mcp.RequestIndexes {
		signature := entry
		if cut := strings.IndexByte(entry, ':'); cut >= 0 {
			signature = entry[:cut]
		}
		open := strings.IndexByte(signature, '(')
		if open < 0 {
			noIndexClaims = append(noIndexClaims, entry)
			continue
		}
		published[signature[:open]] = normalizeIndexColumns(strings.TrimSuffix(signature[open+1:], ")"))
	}
	if len(declared) != len(published) {
		t.Fatalf("the DDL creates %d indexes but describe publishes %d: declared=%v published=%v",
			len(declared), len(published), sortedIndexNames(declared), sortedIndexNames(published))
	}
	for name, columns := range declared {
		got, ok := published[name]
		if !ok {
			t.Fatalf("the DDL creates index %s, which describe does not publish", name)
		}
		if got != columns {
			t.Fatalf("index %s: describe publishes columns %q, the DDL creates %q", name, got, columns)
		}
	}
	// The published claim that nothing indexes these columns must stay true.
	// The list is the named exemplar subset the reference calls out, not the
	// full unindexed set: every column the declared indexes do not cover is
	// unindexed too, so the reference states the class, not just these three.
	unindexed := []string{"status_code", "error_type", "cost"}
	for name, columns := range declared {
		for _, column := range unindexed {
			if strings.Contains(columns, column) {
				t.Fatalf("index %s now covers %s, so the published no-index claim is stale", name, column)
			}
		}
	}
	if len(noIndexClaims) != 1 {
		t.Fatalf("describe must publish exactly one no-index claim, got %v", noIndexClaims)
	}
	for _, column := range unindexed {
		if !strings.Contains(noIndexClaims[0], column) {
			t.Fatalf("the no-index claim must name %s: %q", column, noIndexClaims[0])
		}
	}
}

// normalizeIndexColumns renders a DDL column list in one canonical spelling so
// whitespace and line breaks cannot make equal lists compare unequal.
func normalizeIndexColumns(raw string) string {
	return strings.Join(strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
	}), ",")
}

func sortedIndexNames(indexes map[string]string) []string {
	out := make([]string, 0, len(indexes))
	for name := range indexes {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
