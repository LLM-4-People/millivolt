package format

// Round 6 C4 regression: the streaming Responses decoder preflights the output
// array in response.created and in the terminal document before the typed
// decode. A 1 MiB line of empty items must fail at the shared 4096-item bound
// instead of materializing hundreds of thousands of typed wire items.

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func responsesOutputArray(n int) string {
	if n == 0 {
		return "[]"
	}
	return "[" + strings.Repeat("{},", n-1) + "{}]"
}

func responsesCreatedOutputStream(n int) string {
	return `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":` + responsesOutputArray(n) + `}}

`
}

func responsesCompletedOutputStream(n int) string {
	return `event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":` + responsesOutputArray(n) + `}}

`
}

func TestResponsesStreamResponsePreflightsOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
	}{
		{name: "created over bound", source: responsesCreatedOutputStream(4097)},
		{name: "completed over bound", source: responsesCompletedOutputStream(4097)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec := NewResponsesStreamDecoder(strings.NewReader(tc.source))
			for {
				ev, err := dec.Next()
				if err == io.EOF {
					t.Fatal("over-bound stream reached EOF without an error")
				}
				if err != nil {
					if !strings.Contains(err.Error(), "responses output exceeds") &&
						!strings.Contains(err.Error(), "exceeds") {
						t.Fatalf("error = %v, want the output bound", err)
					}
					return
				}
				if ev.Type == ir.EventStart {
					t.Fatalf("over-bound created emitted EventStart: %+v", ev)
				}
			}
		})
	}

	for _, n := range []int{4096} {
		t.Run(fmt.Sprintf("created accepts %d", n), func(t *testing.T) {
			ev, err := NewResponsesStreamDecoder(strings.NewReader(responsesCreatedOutputStream(n))).Next()
			if err != nil {
				t.Fatal(err)
			}
			if ev.Type != ir.EventStart {
				t.Fatalf("first event = %v, want start", ev.Type)
			}
		})
		t.Run(fmt.Sprintf("completed accepts %d", n), func(t *testing.T) {
			dec := NewResponsesStreamDecoder(strings.NewReader(responsesCompletedOutputStream(n)))
			for {
				ev, err := dec.Next()
				if err == io.EOF {
					return
				}
				if err != nil {
					t.Fatalf("accepted terminal failed: %v", err)
				}
				_ = ev
			}
		})
	}
}
