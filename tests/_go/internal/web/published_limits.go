package web

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/mcp"
	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// TestExplorerNodeCapMatchesThePublishedReference pins the explorer gallery's
// per-dimension node cap, whose owner is xpNodeCap in internal/web/aggregate.go,
// to the cap the MCP reference publishes and enforces as its explorer group
// limit (internal/mcp describe.go explorerMaxGroups, itself pinned by
// tests/_go/internal/mcp/drift.go). Changing the owner without the published
// reference would advertise a cap the server does not apply, and the mutation
// round proved nothing failed when xpNodeCap moved.
func TestExplorerNodeCapMatchesThePublishedReference(t *testing.T) {
	if xpNodeCap != 24 {
		t.Fatalf("xpNodeCap = %d, want 24 (published by internal/mcp describe explorerMaxGroups)", xpNodeCap)
	}
}

// TestMCPErrorKeyExamplesAreProducedByTheOwner pins the error filter key
// examples the MCP reference publishes to keys this package's errorEntries and
// errorKey actually produce. The old reference showed "other||", which no
// record shape reaches (a blank code requires a zero status, and an envelope
// type always arrives with a response status), and the surviving example was
// never checked against the owner at all.
func TestMCPErrorKeyExamplesAreProducedByTheOwner(t *testing.T) {
	start := time.UnixMilli(1_700_000_000_000)
	for _, tc := range []struct {
		name   string
		record metrics.Record
		want   string
	}{
		{
			// An upstream 500 whose error envelope carries a type but no code
			// and no message: errorEntries fills the code from the status, so
			// the message part stays empty.
			name:   "final server error with no code or message",
			record: metrics.Record{StatusCode: http.StatusInternalServerError, ErrorType: "server_error", Start: start},
			want:   "server_error|500|",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries := errorEntries(&tc.record)
			if len(entries) != 1 {
				t.Fatalf("errorEntries produced %d entries for %+v, want 1", len(entries), tc.record)
			}
			got := errorKey(entries[0])
			if got != tc.want {
				t.Fatalf("errorKey = %q, want %q", got, tc.want)
			}
			if !slices.Contains(mcp.ErrorKeyExamples, got) {
				t.Fatalf("mcp.ErrorKeyExamples = %v must contain the producible key %q", mcp.ErrorKeyExamples, got)
			}
		})
	}
	if len(mcp.ErrorKeyExamples) != 1 || mcp.ErrorKeyExamples[0] != "server_error|500|" {
		t.Fatalf("mcp.ErrorKeyExamples = %v, want exactly the one producible example", mcp.ErrorKeyExamples)
	}
}
