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
