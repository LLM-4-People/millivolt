package format

// Round 6 A1 regression: a Chat or Anthropic request translated to a Responses
// target must set store:false when the source never chose storage. OpenAI's
// Responses default is store:true with at least 30 days of retention, while the
// source protocol has no such opt-in and the proxy cannot retrieve the stored
// response. An explicit source value is preserved.

import (
	"encoding/json"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func translatedRequestDoc(t *testing.T, inbound, outbound ir.Protocol, body string) map[string]any {
	t.Helper()
	_, out, _, err := TranslateRequestBetweenIR(inbound, outbound, []byte(body), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode translated request: %v (%s)", err, out)
	}
	return doc
}

func TestTranslatedResponsesDefaultsToStoreFalse(t *testing.T) {
	const chat = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	const anthropic = `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

	for _, tc := range []struct {
		name    string
		inbound ir.Protocol
		body    string
	}{
		{name: "chat", inbound: ir.Chat, body: chat},
		{name: "anthropic", inbound: ir.Anthropic, body: anthropic},
	} {
		t.Run(tc.name+" default", func(t *testing.T) {
			doc := translatedRequestDoc(t, tc.inbound, ir.Responses, tc.body)
			if store, ok := doc["store"]; !ok || store != false {
				t.Fatalf("store = %v (present %v), want an explicit false: %v", store, ok, doc)
			}
		})
	}

	for _, explicit := range []bool{true, false} {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"store":` + map[bool]string{true: "true", false: "false"}[explicit] + `}`
		doc := translatedRequestDoc(t, ir.Chat, ir.Responses, body)
		if store, ok := doc["store"]; !ok || store != explicit {
			t.Fatalf("explicit store %v was not preserved: %v", explicit, doc)
		}
	}

	// The reverse direction cannot honor implicit Responses storage. It must
	// not add a store field to Chat and must not warn (a warning would fire on
	// nearly every default request and reject mode would 400 a representable
	// request).
	doc := translatedRequestDoc(t, ir.Responses, ir.Chat, `{"model":"m","input":"hi"}`)
	if _, ok := doc["store"]; ok {
		t.Fatalf("Responses->Chat body gained store: %v", doc)
	}
	req, _, warnings, err := TranslateRequestBetweenIR(ir.Responses, ir.Chat, []byte(`{"model":"m","input":"hi"}`), 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if req.Store != nil {
		t.Fatalf("reverse translation installed store %v", *req.Store)
	}
	if len(warnings) != 0 {
		t.Fatalf("reverse translation warned about an unrepresentable optional capability: %+v", warnings)
	}
}
