package metrics

// Round 12 F-U regression: one wrong-typed error member must not erase its
// valid string siblings. The client still receives the error frame, so a
// healthy record would hide a real provider failure.

import (
	"encoding/json"
	"testing"
)

func TestParseErrorComponentsKeepsStringSiblings(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantTyp string
		wantCde string
		wantMsg string
	}{
		{
			name:    "object message keeps type",
			raw:     `{"type":"server_error","message":{"detail":"x"}}`,
			wantTyp: "server_error",
		},
		{
			name:    "numeric type keeps message",
			raw:     `{"type":42,"message":"boom"}`,
			wantMsg: "boom",
		},
		{
			name:    "numeric type keeps string message and numeric code",
			raw:     `{"type":42,"code":503,"message":"boom"}`,
			wantCde: "503",
			wantMsg: "boom",
		},
		{
			name:    "valid numeric code with string members",
			raw:     `{"type":"rate_limit_error","code":429,"message":"slow"}`,
			wantTyp: "rate_limit_error",
			wantCde: "429",
			wantMsg: "slow",
		},
		{
			name: "pure decoy object stays empty",
			raw:  `{"type":{"x":1},"message":{"y":2}}`,
		},
		{
			name:    "string error",
			raw:     `"boom"`,
			wantMsg: "boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typ, code, msg := ParseErrorComponents(json.RawMessage(tc.raw))
			if typ != tc.wantTyp || code != tc.wantCde || msg != tc.wantMsg {
				t.Fatalf("components = %q/%q/%q, want %q/%q/%q", typ, code, msg, tc.wantTyp, tc.wantCde, tc.wantMsg)
			}
			// The record path adds the provider_error fallback when a member
			// survived but the source named no type.
			recordType, recordCode, recordMsg := ParseErrorValue(json.RawMessage(tc.raw))
			wantRecordType := tc.wantTyp
			if wantRecordType == "" && (tc.wantCde != "" || tc.wantMsg != "") {
				wantRecordType = "provider_error"
			}
			if recordType != wantRecordType || recordCode != tc.wantCde || recordMsg != tc.wantMsg {
				t.Fatalf("record components = %q/%q/%q, want %q/%q/%q",
					recordType, recordCode, recordMsg, wantRecordType, tc.wantCde, tc.wantMsg)
			}
		})
	}
}
