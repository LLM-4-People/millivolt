package format

// Round 3 R3-2 and docs regressions: one Responses annotation array keeps the
// codec's per-array limit, and docs/protocol.md states the same content-part
// and annotation limits the proxy and codec enforce.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestResponsesAnnotationArrayBoundary(t *testing.T) {
	if _, err := DecodeResponsesRequest(responsesAnnotationTestBody([]int{metrics.ContentPartMax})); err != nil {
		t.Fatalf("at-limit annotation array rejected: %v", err)
	}
	want := "annotations exceeds " + strconv.Itoa(metrics.ContentPartMax) + " items"
	_, err := DecodeResponsesRequest(responsesAnnotationTestBody([]int{metrics.ContentPartMax + 1}))
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("over-limit annotation array error = %v, want %q", err, want)
	}
}

// groupedDecimal renders a limit in the comma form docs/protocol.md uses.
func groupedDecimal(n int) string {
	digits := strconv.Itoa(n)
	for i := len(digits) - 3; i > 0; i -= 3 {
		digits = digits[:i] + "," + digits[i:]
	}
	return digits
}

// TestProtocolDocumentsResponsesNestedBudgets pins the documented request
// budget to the constants the proxy and codec enforce, so the three cannot
// drift apart again.
func TestProtocolDocumentsResponsesNestedBudgets(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "protocol.md"))
	if err != nil {
		t.Fatalf("read docs/protocol.md: %v", err)
	}
	text := string(doc)
	start := strings.Index(text, "Separately, the aggregate")
	end := strings.Index(text, "Invalid JSON is passed through")
	if start < 0 || end <= start {
		t.Fatal("docs/protocol.md has no nested request budget paragraph")
	}
	paragraph := strings.Join(strings.Fields(text[start:end]), " ")
	wants := []string{
		"content-part count over every message content array, a top-level `system` array and every Responses `input` item's nested `content`/`output` array is capped at " + groupedDecimal(WireContentPartsMax),
		"Each Responses annotation array is capped at " + groupedDecimal(metrics.ContentPartMax) + " members",
		"aggregate inside those content parts is capped at " + groupedDecimal(WireNestedCollectionMax),
	}
	for _, want := range wants {
		if !strings.Contains(paragraph, want) {
			t.Fatalf("docs/protocol.md does not state %q: %s", want, paragraph)
		}
	}
}
