package web

import "testing"

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
