package format

// N10 safety regression: buffering output_item.added arguments until the first
// delta or completion must stay bounded. A stream that opens tool blocks
// without ever closing them cannot retain unbounded initial JSON.

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestResponsesInitialArgumentsBound(t *testing.T) {
	openCall := func(index int, args string) string {
		return fmt.Sprintf(`data: {"type":"response.output_item.added","output_index":%d,"item":{"type":"function_call","id":"call_%d","name":"n","arguments":%q}}`+"\n\n", index, index, args)
	}
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "concurrent tool blocks",
			src: func() string {
				var b strings.Builder
				for i := 0; i <= toolBlocksMax; i++ {
					b.WriteString(openCall(i, "{}"))
				}
				return b.String()
			}(),
			want: "concurrent tool calls",
		},
		{
			name: "initial argument bytes",
			src: func() string {
				var b strings.Builder
				args := strings.Repeat("a", 900<<10)
				for i := 0; i < 5; i++ {
					b.WriteString(openCall(i, args))
				}
				return b.String()
			}(),
			want: "retained initial tool bytes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dec := NewResponsesStreamDecoder(strings.NewReader(tc.src))
			var err error
			for {
				_, err = dec.Next()
				if err != nil {
					break
				}
			}
			if errors.Is(err, io.EOF) || err == nil {
				t.Fatalf("stream ended with %v, want the initial-arguments bound", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name %q", err, tc.want)
			}
		})
	}
}
