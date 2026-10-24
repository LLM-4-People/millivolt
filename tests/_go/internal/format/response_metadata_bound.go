package format

// B1 regression: Chat and Responses response metadata objects are bounded
// before the typed map decode.

import (
	"fmt"
	"strings"
	"testing"
)

func numberedStringObject(prefix string, n int) string {
	var b strings.Builder
	b.WriteString("{")
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"%s%d":"v"`, prefix, i)
	}
	b.WriteString("}")
	return b.String()
}

func TestResponseMetadataBound(t *testing.T) {
	chat := func(metadata string) string {
		return `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"metadata":` + metadata + `}`
	}
	responses := func(metadata string) string {
		return `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[],"metadata":` + metadata + `}`
	}
	cases := []struct {
		name   string
		decode func([]byte) error
		wire   func(string) string
	}{
		{
			name: "chat",
			decode: func(body []byte) error {
				resp, err := DecodeChatResponse(body)
				if err == nil && len(resp.Metadata) != 10_000 {
					return fmt.Errorf("metadata members = %d, want 10000", len(resp.Metadata))
				}
				return err
			},
			wire: chat,
		},
		{
			name: "responses",
			decode: func(body []byte) error {
				resp, err := DecodeResponsesResponse(body)
				if err == nil && len(resp.Metadata) != 10_000 {
					return fmt.Errorf("metadata members = %d, want 10000", len(resp.Metadata))
				}
				return err
			},
			wire: responses,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.decode([]byte(tc.wire(numberedStringObject("m", 10_000)))); err != nil {
				t.Fatalf("10,000-member metadata was rejected: %v", err)
			}
			err := tc.decode([]byte(tc.wire(numberedStringObject("m", 10_001))))
			if err == nil {
				t.Fatal("10,001-member metadata was accepted")
			}
			if !strings.Contains(err.Error(), "metadata exceeds 10000 members") {
				t.Fatalf("error = %v, want the metadata member bound", err)
			}
		})
	}
}

// TestResponseMetadataDuplicateObjectsMergedBound is the NEW-2 regression:
// encoding/json merges duplicate metadata members into one map, so a bound
// applied to each occurrence separately misses the merged size.
func TestResponseMetadataDuplicateObjectsMergedBound(t *testing.T) {
	chat := func(objects ...string) (int, error) {
		body := `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"metadata":` +
			strings.Join(objects, `,"metadata":`) + `}`
		resp, err := DecodeChatResponse([]byte(body))
		if err != nil {
			return 0, err
		}
		return len(resp.Metadata), nil
	}
	responses := func(objects ...string) (int, error) {
		body := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[],"metadata":` +
			strings.Join(objects, `,"metadata":`) + `}`
		resp, err := DecodeResponsesResponse([]byte(body))
		if err != nil {
			return 0, err
		}
		return len(resp.Metadata), nil
	}
	cases := []struct {
		name   string
		decode func(...string) (int, error)
	}{
		{"chat", chat},
		{"responses", responses},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disjoint := []string{numberedStringObject("a", 5_001), numberedStringObject("b", 5_001)}
			if _, err := tc.decode(disjoint...); err == nil {
				t.Fatal("two disjoint 5,001-member metadata objects were accepted")
			} else if !strings.Contains(err.Error(), "metadata exceeds 10000 members") {
				t.Fatalf("error = %v, want the merged metadata member bound", err)
			}

			identical := []string{numberedStringObject("a", 5_001), numberedStringObject("a", 5_001)}
			got, err := tc.decode(identical...)
			if err != nil {
				t.Fatalf("two identical 5,001-member objects were rejected: %v", err)
			}
			if got != 5_001 {
				t.Fatalf("merged metadata members = %d, want 5001 distinct", got)
			}
		})
	}
}

// TestResponseMetadataDuplicateAllocationBounded pins the allocation guard:
// duplicate objects are rejected at the merged bound before their members
// accumulate, so a body of many 5,000-member objects cannot expand into a map
// proportional to every occurrence.
func TestResponseMetadataDuplicateAllocationBounded(t *testing.T) {
	objects := make([]string, 100)
	for i := range objects {
		objects[i] = numberedStringObject(fmt.Sprintf("k%d_", i), 5_000)
	}
	body := `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"metadata":` +
		strings.Join(objects, `,"metadata":`) + `}`
	alloc := allocDelta(func() {
		if _, err := DecodeChatResponse([]byte(body)); err == nil {
			t.Fatal("100 duplicate 5,000-member metadata objects were accepted")
		}
	})
	if alloc > 16<<20 {
		t.Fatalf("%d-byte duplicate metadata allocated %d bytes, want <= 16 MiB", len(body), alloc)
	}
	t.Logf("duplicate metadata: %d bytes, %d bytes allocated", len(body), alloc)
}
