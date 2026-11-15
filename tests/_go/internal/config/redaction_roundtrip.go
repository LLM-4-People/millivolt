package config

import (
	"encoding/json"
	"testing"
)

// postedDocument renders a config values document the way a model receives
// it: through JSON. It is the exact shape a set_config call posts back.
func postedDocument(t *testing.T, values map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	var posted map[string]any
	if err := json.Unmarshal(raw, &posted); err != nil {
		t.Fatal(err)
	}
	return posted
}

// TestApplyRestoresRedactedProviderHeaders pins S14: a redacted document that
// config_get returned applies back to the same config without overwriting the
// secret with the marker. A real change still wins, and a marker with no prior
// value stays visible.
func TestApplyRestoresRedactedProviderHeaders(t *testing.T) {
	c := Default()
	c.Providers = map[string]ProviderOverride{
		"neutral": {Headers: map[string]string{"Authorization": "Bearer secret", "X-Plain": "visible"}},
	}
	posted := postedDocument(t, RedactProviderHeaders(c.Map(), RedactionMarker))

	next := c.Clone()
	if err := next.Apply(posted); err != nil {
		t.Fatal(err)
	}
	headers := next.Providers["neutral"].Headers
	if headers["Authorization"] != "Bearer secret" {
		t.Fatalf("authorization = %q, want the loaded secret", headers["Authorization"])
	}
	if headers["X-Plain"] != "visible" {
		t.Fatalf("plain header = %q, want the unchanged value", headers["X-Plain"])
	}

	// A posted value that is not the marker wins over the loaded one.
	provider := posted["providers"].(map[string]any)["neutral"].(map[string]any)
	provider["headers"].(map[string]any)["X-Plain"] = "changed"
	next = c.Clone()
	if err := next.Apply(posted); err != nil {
		t.Fatal(err)
	}
	if got := next.Providers["neutral"].Headers["X-Plain"]; got != "changed" {
		t.Fatalf("changed plain header = %q, want the posted value", got)
	}
	if got := next.Providers["neutral"].Headers["Authorization"]; got != "Bearer secret" {
		t.Fatalf("authorization after an unrelated change = %q, want the secret", got)
	}

	// A marker with no prior value is left visible instead of becoming empty.
	posted = postedDocument(t, RedactProviderHeaders(c.Map(), RedactionMarker))
	provider = posted["providers"].(map[string]any)["neutral"].(map[string]any)
	provider["headers"].(map[string]any)["X-New"] = RedactionMarker
	fresh := Default()
	if err := fresh.Apply(posted); err != nil {
		t.Fatal(err)
	}
	if got := fresh.Providers["neutral"].Headers["X-New"]; got != RedactionMarker {
		t.Fatalf("new marker header = %q, want the marker", got)
	}
}
