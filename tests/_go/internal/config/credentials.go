package config

import "testing"

func TestRedactProviderHeaders(t *testing.T) {
	values := map[string]any{
		"name": "live",
		"providers": map[string]any{
			"p": map[string]any{
				"headers": map[string]any{
					"Authorization":   "Bearer secret",
					"Cookie":          "session=secret",
					"X-Trace":         "visible",
					"x-prompt-tokens": "5",
				},
			},
			"opaque": "kept",
		},
	}
	redacted := RedactProviderHeaders(values, "[removed]")
	providers := redacted["providers"].(map[string]any)
	headers := providers["p"].(map[string]any)["headers"].(map[string]any)
	for _, key := range []string{"Authorization", "Cookie"} {
		if headers[key] != "[removed]" {
			t.Errorf("%s = %v, want the marker", key, headers[key])
		}
	}
	for _, key := range []string{"X-Trace", "x-prompt-tokens"} {
		if headers[key] == "[removed]" {
			t.Errorf("%s was redacted, want a non-credential value preserved", key)
		}
	}
	if providers["opaque"] != "kept" || redacted["name"] != "live" {
		t.Fatal("non-provider values were not preserved")
	}
	original := values["providers"].(map[string]any)["p"].(map[string]any)["headers"].(map[string]any)
	if original["Authorization"] != "Bearer secret" {
		t.Fatal("RedactProviderHeaders mutated its input")
	}
}

func TestRedactProviderHeadersWithoutProviders(t *testing.T) {
	values := map[string]any{"name": "live"}
	redacted := RedactProviderHeaders(values, "[removed]")
	if redacted["name"] != "live" {
		t.Fatal("a document without providers lost a value")
	}
	if _, ok := redacted["providers"]; ok {
		t.Fatal("a document without providers gained one")
	}
}

func TestRedactProviderHeadersNativeMap(t *testing.T) {
	cfg := Default()
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderOverride{}
	}
	cfg.Providers["p"] = ProviderOverride{Headers: map[string]string{
		"Authorization": "Bearer secret",
		"X-Trace":       "visible",
	}}
	values := cfg.Map()
	redacted := RedactProviderHeaders(values, "[removed]")
	providers, ok := redacted["providers"].(map[string]ProviderOverride)
	if !ok {
		t.Fatalf("providers = %T, want the native map[string]ProviderOverride shape", redacted["providers"])
	}
	headers := providers["p"].Headers
	if headers["Authorization"] != "[removed]" {
		t.Errorf("Authorization = %q, want the marker", headers["Authorization"])
	}
	if headers["X-Trace"] != "visible" {
		t.Errorf("X-Trace = %q, want the non-credential value preserved", headers["X-Trace"])
	}
	original, ok := values["providers"].(map[string]ProviderOverride)
	if !ok {
		t.Fatalf("Map providers = %T, want map[string]ProviderOverride", values["providers"])
	}
	if original["p"].Headers["Authorization"] != "Bearer secret" {
		t.Fatal("RedactProviderHeaders mutated the native Config.Map value")
	}
}
