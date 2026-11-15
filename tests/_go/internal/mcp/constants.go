package mcp

import (
	"testing"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// TestPublishedMCPBoundsMatchTheConstants pins the numbers SECURITY.md and
// docs/mcp.md publish: the 64 MiB response read, the 4 KiB error excerpt and
// its 12-byte truncation marker. These are prose copies of code bounds, so a
// changed constant must redden this test rather than leave the docs lying.
func TestPublishedMCPBoundsMatchTheConstants(t *testing.T) {
	if MaxResponseBytes != config.MCPResponseBytesMax {
		t.Fatalf("MaxResponseBytes = %d, want the config owner %d", MaxResponseBytes, config.MCPResponseBytesMax)
	}
	if MaxResponseBytes != 64<<20 {
		t.Fatalf("MaxResponseBytes = %d, want the published 64 MiB", MaxResponseBytes)
	}
	if maxErrorBodyBytes != 4<<10 {
		t.Fatalf("maxErrorBodyBytes = %d, want the published 4 KiB", maxErrorBodyBytes)
	}
	if len(truncationMarker) != 12 {
		t.Fatalf("truncationMarker %q is %d bytes, want the published 12", truncationMarker, len(truncationMarker))
	}
}
