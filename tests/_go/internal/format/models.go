package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestTranslateAnthropicModels(t *testing.T) {
	body := `{"data":[
		{"id":"model-a-4-6","type":"model","display_name":"Model A 4.6","created_at":"2026-02-04T00:00:00Z","max_input_tokens":200000},
		{"id":"model-b-4-6","type":"model","display_name":"Model B 4.6","created_at":"2026-02-04T00:00:00Z","max_input_tokens":300000}
	],"first_id":"model-a-4-6","last_id":"model-b-4-6","has_more":true}`

	page, err := TranslateAnthropicModels([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(page.Entries))
	}
	if page.NextAfter != "model-b-4-6" {
		t.Errorf("NextAfter = %q, want last_id for pagination", page.NextAfter)
	}
	e := page.Entries[0]
	// owned_by is a wire constant of the Anthropic models API, not fixture data.
	if e["id"] != "model-a-4-6" || e["object"] != "model" || e["owned_by"] != "anthropic" {
		t.Errorf("entry shape wrong: %+v", e)
	}
	if e["created"].(int64) == 0 {
		t.Errorf("created_at not converted to unix: %+v", e["created"])
	}
	if e["context_window"] != 200000 {
		t.Errorf("context_window wrong: %+v", e["context_window"])
	}
	if e["display_name"] != "Model A 4.6" {
		t.Errorf("display_name missing: %+v", e["display_name"])
	}
}

func TestTranslateAnthropicModelsNoMorePages(t *testing.T) {
	body := `{"data":[{"id":"m","type":"model","display_name":"M","created_at":"2026-01-01T00:00:00Z"}],"has_more":false,"last_id":"m"}`
	page, err := TranslateAnthropicModels([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if page.NextAfter != "" {
		t.Errorf("has_more=false must yield empty NextAfter, got %q", page.NextAfter)
	}
}

func TestTranslateAnthropicModelsMalformed(t *testing.T) {
	if _, err := TranslateAnthropicModels([]byte(`{not json`)); err == nil {
		t.Fatal("expected a parse error")
	}
}

func TestModelsListJSONShape(t *testing.T) {
	entries := []map[string]any{{"id": "x", "object": "model", "owned_by": "o", "created": 1}}
	out, err := ModelsListJSON(entries, 0)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Object != "list" || len(parsed.Data) != 1 || parsed.Data[0].ID != "x" {
		t.Errorf("bad OpenAI list shape: %s", out)
	}
	if !strings.Contains(string(out), `"object":"list"`) {
		t.Errorf("missing object:list envelope: %s", out)
	}
	// Normal output stays byte-identical to the previous map-marshal form,
	// including encoding/json's sorted envelope keys.
	want, err := json.Marshal(map[string]any{"object": "list", "data": entries})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(want) {
		t.Errorf("normal output changed:\n got %s\nwant %s", out, want)
	}
}

// TestModelsListJSONUsesWireEncodingAndBound pins the two output properties:
// <, > and & stay literal instead of expanding six-fold, and a document past
// the caller's bound fails closed without returning partial bytes.
func TestModelsListJSONUsesWireEncodingAndBound(t *testing.T) {
	entries := []map[string]any{{"id": strings.Repeat("<&>", 1000)}}
	out, err := ModelsListJSON(entries, 0)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(`\u003c`)) || bytes.Contains(out, []byte(`\u003e`)) || bytes.Contains(out, []byte(`\u0026`)) {
		t.Fatalf("model list still HTML-escapes: first bytes %q", out[:min(len(out), 256)])
	}
	if len(out) > len(entries[0]["id"].(string))+128 {
		t.Fatalf("HTML escaping inflated %d-byte id to a %d-byte document", len(entries[0]["id"].(string)), len(out))
	}

	limited, err := ModelsListJSON(entries, len(out)-1)
	if !errors.Is(err, ErrModelsListTooLarge) {
		t.Fatalf("limit error = %v, want ErrModelsListTooLarge", err)
	}
	if limited != nil {
		t.Fatalf("failed render returned %d partial bytes", len(limited))
	}
	exact, err := ModelsListJSON(entries, len(out))
	if err != nil || len(exact) != len(out) {
		t.Fatalf("exact bound rejected: len=%d err=%v", len(exact), err)
	}
}
