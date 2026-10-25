package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A valid config (and valid overrides) must load cleanly and pass validation.
func TestValidateAcceptsValidValues(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"empty", ""},
		{"upstream_timeout disabled", "upstream_timeout: 0s"},
		{"unlimited queue", "max_concurrent: 0\nmax_queue_size: 0\nmax_queue_wait: 0s"},
		{"valid overrides", "max_retries: 10\nmax_backoff: 5m\nbase_backoff: 2s\ncapture_body_preview: true"},
		{"cursor heartbeat tune", "cursor_heartbeat_interval: 2s"},
		{"quality retries disabled", "quality_retries: 0"},
		{"quality retries max", "quality_retries: 3"},
		{"thinking retries disabled", "thinking_retries: 0"},
		{"thinking retries max", "thinking_retries: 3"},
		{"boundary values", "history_size: 1\nconversation_max_open: 1\nqueue_retry_after: 0s"},
		{"go memory limit floor", "go_memory_limit: 32MiB"},
		{"go memory limit disabled", "go_memory_limit: 0"},
		{"debug store floor", "debug_store_max_bytes: 16MiB"},
		{"debug store max", "debug_store_max_bytes: 8GiB"},
		{"debug store unlimited", "debug_store_max_bytes: 0"},
		{"inflight body floor", "max_inflight_body_bytes: 1MiB"},
		{"inflight body max", "max_inflight_body_bytes: 64GiB"},
		{"inflight body unlimited", "max_inflight_body_bytes: 0"},
		{"inflight response floor", "max_inflight_response_bytes: 1MiB"},
		{"inflight response max", "max_inflight_response_bytes: 64GiB"},
		{"inflight response unlimited", "max_inflight_response_bytes: 0"},
		{"provider alias merge", "provider_aliases:\n  old.example: new.example\n  legacy.example: new.example"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			c, err := LoadFile(p)
			if err != nil {
				t.Fatalf("LoadFile(%q) = %v, want valid", tc.yaml, err)
			}
			if err := c.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

// Default() itself must always validate - the built-in defaults are the
// single source of truth and must be sane.
func TestDefaultValidates(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Errorf("Default().Validate() = %v, want nil", err)
	}
}

// The dash_log_rows rejection wording is a pinned contract (the errRange
// owner, shared with the schema bounds): byte-exact, with the integer band
// rendered without formatting drift.
func TestDashLogRowsValidateWording(t *testing.T) {
	c := Default()
	c.DashLogRows = DashLogRowsMax + 1
	err := c.Validate()
	if err == nil || err.Error() != "dash_log_rows: must be 10..500, got 501" {
		t.Fatalf("Validate error = %v, want the exact errRange wording", err)
	}
}

// The heartbeat pair's rejection wording is the same pinned errRange
// contract, byte-exact in canonical duration spellings (heartbeatRange
// stays the schema Help-string owner; the error bytes come from errRange).
func TestHeartbeatValidateWording(t *testing.T) {
	c := Default()
	c.CursorHeartbeatInterval = HeartbeatIntervalMax + time.Second
	err := c.Validate()
	if err == nil || err.Error() != "cursor_heartbeat_interval: must be 1ns..10m, got 601s" {
		t.Fatalf("Validate error = %v, want the exact errRange wording", err)
	}
	c = Default()
	c.SSEKeepaliveInterval = 0
	err = c.Validate()
	if err == nil || err.Error() != "sse_keepalive_interval: must be 1ns..10m, got 0s" {
		t.Fatalf("Validate error = %v, want the exact errRange wording", err)
	}
}

// TestConfigDocumentSizeBound pins the 16 MiB config-document contract used by
// the backup config member. A valid config must still validate and encode; a
// provider header value that pushes the canonical YAML over the bound must be
// rejected at Validate, not only when a backup is encoded or restored.
func TestConfigDocumentSizeBound(t *testing.T) {
	tooLarge := Default()
	tooLarge.Providers = map[string]ProviderOverride{
		"p": {Headers: map[string]string{"x-large": strings.Repeat("x", 17<<20)}},
	}
	err := tooLarge.Validate()
	if err == nil || !strings.Contains(err.Error(), "config document") ||
		!strings.Contains(err.Error(), FormatByteSize(ConfigDocumentMaxBytes)) {
		t.Fatalf("Validate() = %v, want the config-document size rejection", err)
	}
	tooLarge.Providers = nil

	// Build an at-cap document from the canonical writer, so the boundary is
	// the same one the backup encoder receives.
	atCap := Default()
	atCap.Providers = map[string]ProviderOverride{
		"p": {Headers: map[string]string{"x-large": ""}},
	}
	var base bytes.Buffer
	if err := WriteYAML(&base, atCap); err != nil {
		t.Fatal(err)
	}
	headerBytes := ConfigDocumentMaxBytes - base.Len()
	if headerBytes <= 0 {
		t.Fatalf("default document is already %d bytes, above the %d-byte bound", base.Len(), ConfigDocumentMaxBytes)
	}
	atCap.Providers["p"] = ProviderOverride{
		Headers: map[string]string{"x-large": strings.Repeat("x", headerBytes)},
	}
	var rendered bytes.Buffer
	if err := WriteYAML(&rendered, atCap); err != nil {
		t.Fatal(err)
	}
	if rendered.Len() != ConfigDocumentMaxBytes {
		t.Fatalf("at-cap document = %d bytes, want exactly %d", rendered.Len(), ConfigDocumentMaxBytes)
	}
	if err := atCap.Validate(); err != nil {
		t.Fatalf("at-cap config Validate() = %v, want accepted", err)
	}
}

// Validate's providers loop must report the FIRST failing provider
// deterministically. Regression: the loop iterated c.Providers in Go map
// order (randomized), so a file with two invalid providers failed with a
// DIFFERENT first error on each load. The loop now visits labels in sorted
// order, so the error always names the alphabetically-first failing label -
// asserted across many loads, because map order varies per iteration and a
// single run could get lucky.
func TestValidateProvidersErrorDeterministic(t *testing.T) {
	yaml := `providers:
  zeta.example:
    cost_keys: ["not a path"]
  alpha.example:
    usage_keys:
      input_tokenz: "a.b"
`
	c := loadFileOK(t, yaml)
	if len(c.Providers) != 0 {
		t.Fatal("invalid providers overlay was applied")
	}
	broken := Default()
	broken.Providers = map[string]ProviderOverride{
		"zeta.example":  {CostKeys: []string{"not a path"}},
		"alpha.example": {UsageKeys: map[string]string{"input_tokenz": "a.b"}},
	}
	for i := 0; i < 200; i++ {
		err := broken.Validate()
		if err == nil {
			t.Fatal("Validate accepted two invalid providers")
		}
		if !strings.Contains(err.Error(), "providers.alpha.example") {
			t.Fatalf("validate %d: error = %q, want the first error to always name the alphabetically-first label providers.alpha.example", i, err)
		}
	}
}

func TestAllowedBaseURLsValidated(t *testing.T) {
	bad := []struct{ name, entry, wantErr string }{
		{"missing scheme", "api.openai.com",
			`allowed_base_urls: "api.openai.com" must be an absolute http:// or https:// URL`},
		{"unsupported scheme", "ftp://api.openai.com",
			`allowed_base_urls: "ftp://api.openai.com" must be an absolute http:// or https:// URL`},
		{"empty host", "https://",
			`allowed_base_urls: "https://" must be an absolute http:// or https:// URL`},
		{"hostless port", "https://:8080",
			`allowed_base_urls: "https://:8080" must name a host`},
		{"hostless port with path", "http://:8080/v1",
			`allowed_base_urls: "http://:8080/v1" must name a host`},
		{"unparseable", "://",
			`allowed_base_urls: "://" must be an absolute http:// or https:// URL`},
		{"trailing slash", "https://api.openai.com/",
			`allowed_base_urls: "https://api.openai.com/" must not end in "/"`},
		{"trailing slash with path", "https://api.openai.com/v1/",
			`allowed_base_urls: "https://api.openai.com/v1/" must not end in "/"`},
		{"userinfo", "https://user:pass@api.openai.com",
			`allowed_base_urls: "https://user:pass@api.openai.com" must not carry userinfo`},
		{"query string", "https://api.openai.com/v1?x=1",
			`allowed_base_urls: "https://api.openai.com/v1?x=1" must not carry a query`},
		{"bare query marker", "https://api.openai.com/v1?",
			`allowed_base_urls: "https://api.openai.com/v1?" must not carry a query`},
		{"bare query marker on host", "https://api.openai.com?",
			`allowed_base_urls: "https://api.openai.com?" must not carry a query`},
		{"fragment", "https://api.openai.com/v1#frag",
			`allowed_base_urls: "https://api.openai.com/v1#frag" must not carry a fragment`},
		{"bare fragment marker", "https://api.openai.com/v1#",
			`allowed_base_urls: "https://api.openai.com/v1#" must not carry a fragment`},
		{"uppercase host", "https://API.OPENAI.COM/v1",
			`allowed_base_urls: "https://API.OPENAI.COM/v1": host must be lowercase (the runtime prefix match is byte-sensitive); use "https://api.openai.com/v1"`},
		{"uppercase scheme", "HTTP://api.openai.com",
			`allowed_base_urls: "HTTP://api.openai.com" must start with lowercase http:// or https://`},
		{"uppercase scheme and host", "HTTP://API.OPENAI.COM",
			`allowed_base_urls: "HTTP://API.OPENAI.COM" must start with lowercase http:// or https://`},
		{"port above range", "http://api.openai.com:70000",
			`allowed_base_urls: "http://api.openai.com:70000" must use a port between 1 and 65535`},
		{"port zero", "http://api.openai.com:0",
			`allowed_base_urls: "http://api.openai.com:0" must use a port between 1 and 65535`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "allowed_base_urls:\n  - " + strconv.Quote(tc.entry) + "\n"
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadFile(p)
			if err == nil {
				t.Fatalf("LoadFile accepted bad allowed_base_urls entry %q", tc.entry)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("LoadFile error = %q, want %q", err, tc.wantErr)
			}
			if _, _, err := LoadFileRepair(p); err == nil {
				t.Fatalf("LoadFileRepair accepted bad allowed_base_urls entry %q", tc.entry)
			}
			got, readErr := os.ReadFile(p)
			if readErr != nil || !bytes.Equal(got, []byte(yaml)) {
				t.Fatalf("invalid allowlist source was rewritten/removed: %v %q", readErr, got)
			}
		})
	}

	// Well-formed entries load cleanly: both spellings from the proxy.yaml
	// example (an https host and an http host:port), a path suffix, and an
	// IPv6 host:port (its bracketed host is already canonical lowercase).
	good := "allowed_base_urls:\n  - \"https://api.example.com\"\n  - \"http://10.0.0.5:8000\"\n  - \"https://gw.example.com/v1\"\n  - \"http://[::1]:8080\"\n  - \"http://api.example.com:1\"\n  - \"http://api.example.com:65535\"\n"
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile(good allowed_base_urls) = %v, want nil", err)
	}
	if len(c.AllowedBaseURLs) != 6 {
		t.Errorf("allowed_base_urls = %v, want 6 entries", c.AllowedBaseURLs)
	}
}

// Provider field-map mechanisms accept valid custom paths without depending on
// operator configuration or treating a provider-specific mapping as a default.
func TestValidateAcceptsProviderFieldMappings(t *testing.T) {
	yaml := `providers:
  billing.example:
    cost_keys: ["billing.cost"]
    usage_keys:
      input_tokens: "billing.inputTokens"
      output_tokens: "billing.outputTokens"
      cache_read_tokens: "billing.cacheReadInputTokens"
  models.example:
    models_path: "/language-models"
    models_keys:
      input_modalities: "input_modalities"
      output_modalities: "output_modalities"
      system_fingerprint: "fingerprint"
      version: "version"
`
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatalf("provider field mappings rejected: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}
