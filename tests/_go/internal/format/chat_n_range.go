package format

// A7 regression: Chat n is bounded to the schema's 1..128 range.

import (
	"strings"
	"testing"
)

func TestChatNRange(t *testing.T) {
	body := func(n string) string {
		return `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":` + n + `}`
	}
	for _, n := range []string{"0", "-5", "129"} {
		if _, err := DecodeChatRequest([]byte(body(n))); err == nil {
			t.Fatalf("n=%s was accepted", n)
		} else if !strings.Contains(err.Error(), "n must be between 1 and 128") {
			t.Fatalf("n=%s error = %v, want the range rejection", n, err)
		}
	}
	for _, tc := range []struct {
		n     string
		warns int
	}{
		{"1", 0},
		{"2", 1},
		{"128", 1},
	} {
		req, err := DecodeChatRequest([]byte(body(tc.n)))
		if err != nil {
			t.Fatalf("n=%s rejected: %v", tc.n, err)
		}
		warns := 0
		for _, warning := range req.Warnings {
			if warning.Field == "n" {
				warns++
			}
		}
		if warns != tc.warns {
			t.Fatalf("n=%s loss warnings = %d, want %d (%+v)", tc.n, warns, tc.warns, req.Warnings)
		}
	}
}
