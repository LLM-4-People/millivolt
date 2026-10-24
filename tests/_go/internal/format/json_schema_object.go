package format

// A4 regression: json_schema.schema must be a valid JSON object on every
// protocol; null, an array and a boolean fail before any target encode.

import (
	"strings"
	"testing"
)

func TestJSONSchemaMustBeObject(t *testing.T) {
	cases := []struct {
		name   string
		decode func([]byte) error
		wire   func(string) string
	}{
		{
			name: "chat",
			decode: func(body []byte) error {
				_, err := DecodeChatRequest(body)
				return err
			},
			wire: func(schema string) string {
				return `{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"a","schema":` + schema + `}}}`
			},
		},
		{
			name: "responses",
			decode: func(body []byte) error {
				_, err := DecodeResponsesRequest(body)
				return err
			},
			wire: func(schema string) string {
				return `{"model":"m","input":"hi","text":{"format":{"type":"json_schema","name":"a","schema":` + schema + `}}}`
			},
		},
		{
			name: "anthropic",
			decode: func(body []byte) error {
				_, err := DecodeAnthropicRequest(body)
				return err
			},
			wire: func(schema string) string {
				return `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"output_config":{"format":{"type":"json_schema","schema":` + schema + `}}}`
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, schema := range []string{"null", "[]", "true", `"text"`} {
				if err := tc.decode([]byte(tc.wire(schema))); err == nil {
					t.Fatalf("schema %s was accepted", schema)
				} else if !strings.Contains(err.Error(), "JSON object schema") {
					t.Fatalf("schema %s error = %v, want the object-schema rejection", schema, err)
				}
			}
			if err := tc.decode([]byte(tc.wire(`{}`))); err != nil {
				t.Fatalf("empty object schema rejected: %v", err)
			}
			if err := tc.decode([]byte(tc.wire(`{"type":"object"}`))); err != nil {
				t.Fatalf("object schema rejected: %v", err)
			}
		})
	}
}
