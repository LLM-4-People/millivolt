package format

// Round 13 A1 regression: every target receives a root-object tool schema.
// Anthropic requires input_schema.type:"object"; an omitted or type-less
// source schema must not reach the target unchanged.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func chatToolRequest(parameters string) string {
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"`
	if parameters != "" {
		body += `,"parameters":` + parameters
	}
	return body + `}}]}`
}

func translatedToolSchema(t *testing.T, target ir.Protocol, parameters string) map[string]any {
	t.Helper()
	out, _, err := TranslateRequestBetween(ir.Chat, target, []byte(chatToolRequest(parameters)), 16, "")
	if err != nil {
		t.Fatalf("%s translate: %v", target, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	tools, _ := doc["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("%s tools = %#v", target, doc["tools"])
	}
	tool := tools[0].(map[string]any)
	switch target {
	case ir.Anthropic:
		return tool["input_schema"].(map[string]any)
	case ir.Responses:
		return tool["parameters"].(map[string]any)
	default:
		return tool["function"].(map[string]any)["parameters"].(map[string]any)
	}
}

func TestToolSchemaRootCanonical(t *testing.T) {
	empty := `{"type":"object","properties":{}}`
	for _, tc := range []struct {
		name       string
		parameters string
		want       string
		keep       []string
	}{
		{name: "omitted", want: empty},
		{name: "explicit empty", parameters: `{}`, want: empty},
		{
			name:       "no root type",
			parameters: `{"properties":{"a":{"type":"string"}},"required":["a"]}`,
			want:       `{"type":"object","properties":{"a":{"type":"string"}},"required":["a"]}`,
		},
		{
			name:       "root anyOf",
			parameters: `{"anyOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"number"}}}]}`,
			want:       `{"type":"object","anyOf":[{"type":"object","properties":{"a":{"type":"string"}}},{"type":"object","properties":{"b":{"type":"number"}}}]}`,
		},
		{
			name:       "root ref",
			parameters: `{"$ref":"#/$defs/Args","$defs":{"Args":{"type":"object"}}}`,
			want:       `{"type":"object","$ref":"#/$defs/Args","$defs":{"Args":{"type":"object"}}}`,
		},
	} {
		for _, target := range []ir.Protocol{ir.Anthropic, ir.Chat, ir.Responses} {
			t.Run(tc.name+"/"+string(target), func(t *testing.T) {
				got := translatedToolSchema(t, target, tc.parameters)
				if !jsonEqual(t, got, tc.want) {
					t.Fatalf("schema = %#v, want %s", got, tc.want)
				}
			})
		}
	}

	for _, tc := range []struct {
		name       string
		parameters string
	}{
		{name: "non-object root", parameters: `{"type":"string"}`},
		{name: "nullable root", parameters: `{"type":["object","null"]}`},
		{name: "numeric root", parameters: `{"type":7}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, target := range []ir.Protocol{ir.Anthropic, ir.Chat, ir.Responses} {
				_, _, err := TranslateRequestBetween(ir.Chat, target, []byte(chatToolRequest(tc.parameters)), 16, "")
				if err == nil || !strings.Contains(err.Error(), "root type must be") {
					t.Fatalf("%s error = %v, want the root-type rejection", target, err)
				}
			}
		})
	}
}

func jsonEqual(t *testing.T, got map[string]any, want string) bool {
	t.Helper()
	var wantDoc map[string]any
	if err := json.Unmarshal([]byte(want), &wantDoc); err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(wantDoc)
	if err != nil {
		t.Fatal(err)
	}
	return string(gotJSON) == string(wantJSON)
}
