package format

// Round 11 A5 regression: the Responses request codec owns the bound on the
// metadata map it materializes, including duplicate object members that
// encoding/json merges into the same map.

import (
	"strings"
	"testing"
)

func responsesRequestWithMetadata(objects ...string) string {
	return `{"model":"m","input":"hi","metadata":` + strings.Join(objects, `,"metadata":`) + `}`
}

func TestResponsesRequestMetadataBound(t *testing.T) {
	if _, err := DecodeResponsesRequest([]byte(responsesRequestWithMetadata(numberedStringObject("k", 10_000)))); err != nil {
		t.Fatalf("10,000 members rejected: %v", err)
	}

	_, err := DecodeResponsesRequest([]byte(responsesRequestWithMetadata(numberedStringObject("k", 10_001))))
	if err == nil || !strings.Contains(err.Error(), "metadata exceeds 10000 members") {
		t.Fatalf("10,001 members error = %v, want the metadata bound", err)
	}

	// Two disjoint 6,000-member objects merge into 12,000 distinct keys.
	_, err = DecodeResponsesRequest([]byte(responsesRequestWithMetadata(
		numberedStringObject("a", 6_000), numberedStringObject("b", 6_000))))
	if err == nil || !strings.Contains(err.Error(), "metadata exceeds 10000 members") {
		t.Fatalf("6,000+6,000 disjoint error = %v, want the merged metadata bound", err)
	}

	// The same 6,000 keys twice stay 6,000 distinct and remain accepted.
	req, err := DecodeResponsesRequest([]byte(responsesRequestWithMetadata(
		numberedStringObject("a", 6_000), numberedStringObject("a", 6_000))))
	if err != nil {
		t.Fatalf("two identical 6,000-member objects rejected: %v", err)
	}
	if len(req.Metadata) != 6_000 {
		t.Fatalf("merged metadata members = %d, want 6,000 distinct", len(req.Metadata))
	}
}
