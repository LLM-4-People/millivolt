package format

// A1 regression: Responses stream_options is bounded before the member map and
// its per-member warnings are materialized.

import (
	"fmt"
	"strings"
	"testing"
)

func numberedJSONObject(prefix string, n int) string {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"%s%d":1`, prefix, i)
	}
	b.WriteString("}")
	return b.String()
}

func TestResponsesStreamOptionsBound(t *testing.T) {
	atLimit := `{"model":"m","stream":true,"input":"hi","stream_options":` + numberedJSONObject("k", 10_000) + `}`
	req, err := DecodeResponsesRequest([]byte(atLimit))
	if err != nil {
		t.Fatalf("10,000-member stream_options was rejected: %v", err)
	}
	if got := len(req.Warnings); got != 10_000 {
		t.Fatalf("warnings = %d, want one per present member", got)
	}

	over := `{"model":"m","stream":true,"input":"hi","stream_options":` + numberedJSONObject("k", 10_001) + `}`
	if _, err := DecodeResponsesRequest([]byte(over)); err == nil {
		t.Fatal("10,001-member stream_options was accepted")
	} else if !strings.Contains(err.Error(), "stream_options exceeds 10000 members") {
		t.Fatalf("error = %v, want the stream_options member bound", err)
	}
}
