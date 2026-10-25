package proxy

// Round 34 NEW-2 regression: a valid escaped include option decodes to the
// same logprob opt-in the Responses codec maps; the lexical scan used a raw
// byte comparison and missed it.

import (
	"fmt"
	"testing"
)

func TestRequestIncludeEscapedOption(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{name: "literal", value: `["message.output_text.logprobs"]`, want: true},
		{name: "escaped dot", value: `["message.output_text\u002elogprobs"]`, want: true},
		{name: "fully escaped", value: `["\u006dessage.output_text.logprobs"]`, want: true},
		{name: "escaped other member", value: `["message.output_text\u002eannotations"]`, want: false},
		{name: "non-string member", value: `[42,"message.output_text.logprobs"]`, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":"m","input":"hi","include":%s}`, tc.value)
			rec := parseRequestRecord(t, body)
			if rec.ReqLogprobs != tc.want {
				t.Fatalf("req_logprobs = %v, want %v for include %s", rec.ReqLogprobs, tc.want, tc.value)
			}
		})
	}
}
