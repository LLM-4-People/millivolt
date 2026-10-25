package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// Default() must populate every tunable (no zero-valued config that code then
// has to second-guess). This is the single-source-of-truth guarantee.
func TestDefaultHasNoZeroTunables(t *testing.T) {
	c := Default()
	checks := map[string]bool{
		"listen":                      c.Listen != "",
		"db_path":                     c.DBPath != "",
		"history_size":                c.HistorySize > 0,
		"shutdown_timeout":            c.ShutdownTimeout > 0,
		"restart_drain_timeout":       c.RestartDrainTimeout > 0,
		"max_request_bytes":           c.MaxRequestBytes > 0,
		"upstream_timeout":            c.UpstreamTimeout > 0,
		"max_conns_per_host":          c.MaxConnsPerHost > 0,
		"max_idle_conns":              c.MaxIdleConns > 0,
		"max_idle_conns_per_host":     c.MaxIdleConnsPerHost > 0,
		"idle_conn_timeout":           c.IdleConnTimeout > 0,
		"max_retries":                 c.MaxRetries > 0,
		"quality_retries":             c.QualityRetries > 0,
		"thinking_retries":            c.ThinkingRetries > 0,
		"queue_retry_after":           c.QueueRetryAfter > 0,
		"base_backoff":                c.BaseBackoff > 0,
		"max_backoff":                 c.MaxBackoff > 0,
		"convo_idle_gap":              c.ConversationIdleGap > 0,
		"convo_max_open":              c.ConversationMaxOpen > 0,
		"anthropic_max_tok":           c.AnthropicDefaultMaxTokens > 0,
		"cursor_park_ttl":             c.CursorParkTTL > 0,
		"cursor_heartbeat":            c.CursorHeartbeatInterval > 0,
		"sse_keepalive":               c.SSEKeepaliveInterval > 0,
		"dash_log_rows":               c.DashLogRows > 0,
		"dash_poll":                   c.DashPollInterval > 0,
		"dash_chart":                  c.DashChartRefresh > 0,
		"dash_explorer":               c.DashExplorerStale > 0,
		"storage_wchan":               c.StorageWriteChanCap > 0,
		"storage_batch":               c.StorageBatchCap > 0,
		"storage_flush":               c.StorageFlushInterval > 0,
		"storage_qto":                 c.StorageQueryTimeout > 0,
		"backup_max_bytes":            c.BackupMaxBytes > 0,
		"debug_store_max_bytes":       c.DebugStoreMaxBytes > 0,
		"max_inflight_body_bytes":     c.MaxInflightBodyBytes > 0,
		"max_inflight_response_bytes": c.MaxInflightResponseBytes > 0,
		"read_header":                 c.ReadHeaderTimeout > 0,
		"idle_timeout":                c.IdleTimeout > 0,
		"mcp_enabled":                 c.MCPEnabled,
		"mcp_page_size":               c.MCPPageSize > 0,
		"mcp_query_max_rows":          c.MCPQueryMaxRows > 0,
		"mcp_query_max_bytes":         c.MCPQueryMaxBytes > 0,
		"mcp_capture_max_bytes":       c.MCPCaptureMaxBytes > 0,
		"mcp_query_timeout":           c.MCPQueryTimeout > 0,
		"mcp_timeout":                 c.MCPTimeout > 0,
	}
	for k, ok := range checks {
		if !ok {
			t.Errorf("Default().%s is zero/empty - every tunable must have a default", k)
		}
	}
	// Concurrency stays unlimited by default (throughput), while the queue is
	// bounded so a held provider cannot accumulate unbounded buffered bodies.
	if c.MaxConcurrent != 0 {
		t.Errorf("Default().max_concurrent = %d, want 0 (unlimited)", c.MaxConcurrent)
	}
	if c.MaxQueueSize <= 0 || c.MaxQueueWait <= 0 || c.MaxInflightRequests <= 0 {
		t.Errorf("queue bounds = (%d, %v, %d), want finite defaults", c.MaxQueueSize, c.MaxQueueWait, c.MaxInflightRequests)
	}
}

// The public example is generated, including every field and its documentation.
// Never inspect the operator's ignored local proxy.yaml in a repository test.
func TestProxyExampleMatchesExample(t *testing.T) {
	got, err := os.ReadFile(filepath.Join("..", "..", "proxy.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var want bytes.Buffer
	if err := WriteYAML(&want, Example()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatal("proxy.example.yaml differs from generated example; regenerate with go run ./cmd/proxy -print-example-config")
	}
}

// TestBackupHelpDescribesStreamingStaging pins the backup_max_bytes wording to
// the archive implementation: downloads and restores stream through private
// staging files. The old help claimed the archive was buffered in the process,
// and byte equality alone cannot catch a stale but generated statement.
func TestBackupHelpDescribesStreamingStaging(t *testing.T) {
	field := FieldByKey("backup_max_bytes")
	if field == nil {
		t.Fatal("backup_max_bytes is missing from Schema()")
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "proxy.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	example := string(raw)
	start := strings.Index(example, "# ---- Backup ----")
	end := strings.Index(example, "# ---- MCP ----")
	if start < 0 || end <= start {
		t.Fatal("proxy.example.yaml must keep the Backup section before MCP")
	}
	for _, surface := range []struct{ name, text string }{
		{"schema help", field.Help},
		{"proxy.example.yaml Backup section", example[start:end]},
	} {
		normalized := strings.Join(strings.Fields(surface.text), " ")
		for _, needle := range []string{"stream", "staging"} {
			if !strings.Contains(normalized, needle) {
				t.Errorf("%s must state %q: %q", surface.name, needle, normalized)
			}
		}
		for _, stale := range []string{"buffered in the process", "archive is buffered"} {
			if strings.Contains(normalized, stale) {
				t.Errorf("%s still claims %q: %q", surface.name, stale, normalized)
			}
		}
	}
}

func TestExampleProfilesAreIsolated(t *testing.T) {
	example, pristine, defaults := Example(), Example(), Default()
	if err := example.Validate(); err != nil {
		t.Fatalf("shipped example is invalid: %v", err)
	}
	if len(defaults.Providers) != 0 || len(example.Providers) != 3 {
		t.Fatal("only the shipped example should enable the three provider profiles")
	}
	withoutProfiles := example.Clone()
	withoutProfiles.Providers = nil
	withoutProfiles.SubConversations = nil
	if !reflect.DeepEqual(withoutProfiles.Map(), defaults.Map()) {
		t.Fatal("example duplicates or changes server defaults")
	}
	for _, label := range []string{"cursor.sh", "opencode.ai", "x.ai"} {
		profile, ok := example.Providers[label]
		if !ok || len(profile.Headers) == 0 || len(profile.CostKeys) != 0 || len(profile.UsageKeys) != 0 {
			t.Errorf("profile %q must retain headers and automatic cost/usage detection", label)
		}
		for header := range profile.Headers {
			if strings.EqualFold(header, "Authorization") || strings.EqualFold(header, "Cookie") || strings.Contains(strings.ToLower(header), "api-key") {
				t.Errorf("example contains credential header %q", header)
			}
		}
	}
	zen := example.Providers["opencode.ai"]
	if zen.ModelsPath != "" {
		t.Fatal("OpenCode Zen example must use the standard models endpoint, not an enrichment path")
	}
	if !reflect.DeepEqual(zen.EnsureTools, []string{"bash", "read"}) {
		t.Fatal("OpenCode Zen example must ensure the CLI's bash and read tool signature")
	}
	if zen.Headers["x-opencode-request"] != "{{opencode-msg-id}}" || zen.Headers["x-opencode-session"] != "{{opencode-ses-id}}" {
		t.Fatal("OpenCode Zen example must mint the CLI's per-request id headers")
	}
	ua := zen.Headers["User-Agent"]
	if !strings.HasPrefix(ua, "opencode/") || !strings.Contains(ua, " ai-sdk/provider-utils/") || !strings.Contains(ua, " runtime/bun/") {
		t.Fatal("OpenCode Zen example must carry the CLI's three-part client identity")
	}
	xai := example.Providers["x.ai"]
	if xai.ModelsPath != "/language-models" || !reflect.DeepEqual(xai.ModelsKeys, map[string]string{
		"input_modalities": "input_modalities", "output_modalities": "output_modalities",
	}) {
		t.Fatal("xAI example must use documented modality fields, never version as context length")
	}
	if xai.Headers["User-Agent"] != xai.Headers["x-grok-client-identifier"]+"/"+xai.Headers["x-grok-client-version"]+" ({{platform}})" {
		t.Fatal("Grok compatibility identity must share its version")
	}
	for _, profile := range example.Providers {
		for name := range profile.Headers {
			profile.Headers[name] = "changed"
		}
		for name := range profile.ModelsKeys {
			profile.ModelsKeys[name] = "changed"
		}
	}
	example.ModelRules[0].Mode = "changed"
	if !reflect.DeepEqual(pristine.Map(), Example().Map()) || !reflect.DeepEqual(defaults.Map(), Default().Map()) {
		t.Fatal("Example returned shared mutable configuration")
	}
}

func TestLoadFileRepairDropsBrokenKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	raw := "max_retries: 3\nmax_retriez: 9\n"
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	c, skipped, err := LoadFileRepair(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxRetries != 3 {
		t.Fatalf("repaired config max_retries=%d", c.MaxRetries)
	}
	joined := strings.Join(skipped, ",")
	if !strings.Contains(joined, "max_retriez") {
		t.Fatalf("skipped = %v", skipped)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("max_retriez")) {
		t.Fatalf("repaired file still contains dropped keys:\n%s", body)
	}
	if !bytes.Contains(body, []byte("max_retries: 3\n")) {
		t.Fatalf("repaired file lost the valid override:\n%s", body)
	}
}

// Boot repair must not manufacture a default for an invalid recognized value:
// that is policy, not debris. The source stays byte-identical for the operator
// to fix, including a mixed valid/invalid SSRF allowlist (the whole field is
// one value; salvaging the valid prefix is not a valid policy).
func TestLoadFileRepairRejectsInvalidValueWithoutRewrite(t *testing.T) {
	for _, tc := range []struct{ name, raw string }{
		{"invalid history size", "max_retries: 3\nmax_retriez: 9\nhistory_size: 0\n"},
		{"mixed allowlist", "allowed_base_urls:\n  - https://api.example.com\n  - api.openai.com\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(tc.raw), 0o644); err != nil {
				t.Fatal(err)
			}
			if cfg, skipped, err := LoadFileRepair(p); err == nil || cfg != nil || skipped != nil {
				t.Fatalf("LoadFileRepair() = %+v, %v, %v; want the invalid value rejected", cfg, skipped, err)
			}
			got, err := os.ReadFile(p)
			if err != nil || !bytes.Equal(got, []byte(tc.raw)) {
				t.Fatalf("repair rewrote the invalid source: %v %q", err, got)
			}
			if _, err := os.Stat(p + invalidConfigSuffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("repair moved the invalid source aside: %v", err)
			}
		})
	}
}

func TestLoadFileRepairUnparseable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	raw := []byte("unknown-setting: [\n")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	c, skipped, err := LoadFileRepair(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != skippedUnparseable {
		t.Fatalf("skipped = %v", skipped)
	}
	if !reflect.DeepEqual(c.Map(), Default().Map()) {
		t.Fatal("unparseable repair must yield defaults")
	}
	preserved, err := os.ReadFile(p + invalidConfigSuffix)
	if err != nil || !bytes.Equal(preserved, raw) {
		t.Fatalf("invalid original not preserved: %v %q", err, preserved)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("replacement file missing: %v", err)
	}
	got, err := LoadFile(p)
	if err != nil {
		t.Fatalf("repaired file still unreadable: %v", err)
	}
	if !reflect.DeepEqual(got.Map(), Default().Map()) {
		t.Fatal("replacement file is not Default()")
	}
}

func TestLoadFileSkipsExtraDocuments(t *testing.T) {
	for _, contents := range []string{
		"max_retries: 1\n---\nmax_retries: 2\n",
		"max_retries: 1\n---\n",
		"max_retries: 1\n...\ninvalid [",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		raw := []byte(contents)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := LoadFile(path)
		if err != nil {
			t.Fatalf("extra document %q: %v", contents, err)
		}
		if c.MaxRetries != 1 {
			t.Fatalf("extra document %q applied max_retries=%d, want first-document 1", contents, c.MaxRetries)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("LoadFile rewrote extra documents: %v %q", err, got)
		}
		repaired, skipped, err := LoadFileRepair(path)
		if err != nil {
			t.Fatalf("repair extra document %q: %v", contents, err)
		}
		if repaired.MaxRetries != 1 {
			t.Fatalf("repair extra document %q applied max_retries=%d", contents, repaired.MaxRetries)
		}
		if !strings.Contains(strings.Join(skipped, ","), skippedExtraDocument) {
			t.Fatalf("repair extra document %q skipped=%v", contents, skipped)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var want bytes.Buffer
		if err := WriteYAML(&want, repaired); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(body, want.Bytes()) {
			t.Fatalf("repair extra document %q did not write the cleaned snapshot", contents)
		}
		if bytes.Contains(body, []byte("max_retries: 2\n")) {
			t.Fatalf("repaired file kept the extra document value:\n%s", body)
		}
	}
}

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

// The mcp category's whole band is enforced at the config boundary with the
// key named, so a Settings write or a YAML load can never arm the endpoint
// with an unusable policy. internal/mcp's Limits.Validate is the
// construction-time twin of the same band; this pins the config side.
func TestMCPSettingsValidateRejections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		key    string
	}{
		{"page size zero", func(c *Config) { c.MCPPageSize = 0 }, "mcp_page_size"},
		{"page size below floor", func(c *Config) { c.MCPPageSize = 9 }, "mcp_page_size"},
		{"page size above ceiling", func(c *Config) { c.MCPPageSize = 501 }, "mcp_page_size"},
		{"query rows zero", func(c *Config) { c.MCPQueryMaxRows = 0 }, "mcp_query_max_rows"},
		{"query rows above ceiling", func(c *Config) { c.MCPQueryMaxRows = 100001 }, "mcp_query_max_rows"},
		{"query bytes below floor", func(c *Config) { c.MCPQueryMaxBytes = 0 }, "mcp_query_max_bytes"},
		{"capture bytes below floor", func(c *Config) { c.MCPCaptureMaxBytes = 0 }, "mcp_capture_max_bytes"},
		{"query timeout zero", func(c *Config) { c.MCPQueryTimeout = 0 }, "mcp_query_timeout"},
		{"query timeout negative", func(c *Config) { c.MCPQueryTimeout = -time.Second }, "mcp_query_timeout"},
		{"request timeout zero", func(c *Config) { c.MCPTimeout = 0 }, "mcp_timeout"},
		{"request timeout negative", func(c *Config) { c.MCPTimeout = -time.Second }, "mcp_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want a rejection for %s", tc.name)
			}
			if name, _, ok := strings.Cut(err.Error(), ":"); !ok || name != tc.key {
				t.Fatalf("Validate() = %q, want the rejection to name %q", err, tc.key)
			}
		})
	}
}

// TestMCPBandBoundaries pins that both ends of the MCP bands are accepted at
// the config boundary, not only the floors the endpoint used to enforce.
func TestMCPBandBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"page size minimum", func(c *Config) { c.MCPPageSize = 10 }},
		{"page size maximum", func(c *Config) { c.MCPPageSize = 500 }},
		{"query rows minimum", func(c *Config) { c.MCPQueryMaxRows = 1 }},
		{"query rows maximum", func(c *Config) { c.MCPQueryMaxRows = 100000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Default()
			tc.mutate(c)
			if err := c.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want the boundary accepted", err)
			}
		})
	}
}

// TestMCPBandsInSchema pins the dashboard ranges to the same constants and the
// query field's help to its default-plus-explicit-ceiling contract.
func TestMCPBandsInSchema(t *testing.T) {
	for _, tc := range []struct {
		key      string
		min, max int
	}{
		{"mcp_page_size", 10, 500},
		{"mcp_query_max_rows", 1, 100000},
	} {
		field := FieldByKey(tc.key)
		if field == nil {
			t.Fatalf("%s is missing from Schema()", tc.key)
		}
		if field.Min == nil || int(*field.Min) != tc.min || field.Max == nil || int(*field.Max) != tc.max {
			t.Fatalf("%s range = %v..%v, want %d..%d", tc.key, field.Min, field.Max, tc.min, tc.max)
		}
	}
	help := FieldByKey("mcp_query_max_rows").Help
	for _, needle := range []string{"Default client-side row limit", "omits max_rows", "explicit max_rows"} {
		if !strings.Contains(help, needle) {
			t.Fatalf("mcp_query_max_rows help %q must state %q", help, needle)
		}
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

// TestApplyProviderAliases pins the Settings-sheet path for provider_aliases:
// the JSON map coerces into the Go field, a malformed pair fails the Apply,
// and Map() round-trips the map so the editor cannot silently drop it.
func TestApplyProviderAliases(t *testing.T) {
	c := Default()
	if err := c.Apply(map[string]any{"provider_aliases": map[string]any{"old.example": "new.example"}}); err != nil {
		t.Fatalf("Apply(valid aliases) = %v, want nil", err)
	}
	if c.ProviderAliases["old.example"] != "new.example" {
		t.Errorf("ProviderAliases = %v, want old.example → new.example", c.ProviderAliases)
	}
	if err := c.Apply(map[string]any{"provider_aliases": map[string]any{"old.example": ""}}); err == nil {
		t.Error("Apply(empty target) = nil error, want rejection")
	}
	if err := c.Apply(map[string]any{"provider_aliases": map[string]any{"old.example": 5}}); err == nil {
		t.Error("Apply(non-string target) = nil error, want rejection")
	}
	if err := c.Apply(map[string]any{"provider_aliases": map[string]any{}}); err != nil {
		t.Errorf("Apply(empty map) = %v, want nil", err)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
	m := c.Map()
	aa, ok := m["provider_aliases"].(map[string]string)
	if !ok {
		t.Fatalf("Map()[provider_aliases] = %T, want map[string]string", m["provider_aliases"])
	}
	if _, present := aa["old.example"]; present {
		t.Error("Map() resurrected a removed alias; settings saves would never be able to delete one")
	}
}

// Clone must deep-copy EVERY nested provider map. Regression: a broken
// indentation made the ProviderAliases clone run inside `if c.Providers !=
// nil`, so a config with providers unset (Default()'s shape) but aliases set
// handed the NEW snapshot the OLD snapshot's alias map - two live snapshots
// sharing one mutable map.
func TestCloneProvidersAndAliasesIndependent(t *testing.T) {
	c := Default()
	c.Providers = nil // Default() ships Providers nil - the exact broken case
	c.ProviderAliases = map[string]string{"old.example": "new.example"}

	clone := c.Clone()
	clone.ProviderAliases["old.example"] = "mutated"
	if c.ProviderAliases["old.example"] != "new.example" {
		t.Fatalf("clone shares the ProviderAliases map with the original")
	}

	c2 := Default()
	c2.Providers = map[string]ProviderOverride{
		"prov.example": {
			CostKeys:   []string{"usage.cost"},
			UsageKeys:  map[string]string{"input_tokens": "p"},
			ModelsPath: "/meta",
			ModelsKeys: map[string]string{"input_modalities": "modalities"},
		},
	}
	clone2 := c2.Clone()
	clone2.Providers["prov.example"].UsageKeys["input_tokens"] = "mutated"
	clone2.Providers["prov.example"].ModelsKeys["input_modalities"] = "mutated"
	clone2.Providers["prov.example"].CostKeys[0] = "mutated"
	if c2.Providers["prov.example"].UsageKeys["input_tokens"] != "p" ||
		c2.Providers["prov.example"].ModelsKeys["input_modalities"] != "modalities" ||
		c2.Providers["prov.example"].CostKeys[0] != "usage.cost" {
		t.Fatalf("clone shares provider override maps with the original: %+v", c2.Providers["prov.example"])
	}
}

// The models enrichment override loads, validates, and round-trips: models_path
// must be a clean path, models_keys values must be dotted JSON paths - a bad
// value fails the load (deny by default), never clamps.
func TestProviderModelsOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	yaml := `providers:
  prov.example:
    models_path: "/language-models"
    models_keys:
      input_modalities: "input_modalities"
      max_output_tokens: "limits.max_output"
`
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	ov := c.Providers["prov.example"]
	if ov.ModelsPath != "/language-models" {
		t.Errorf("models_path = %q", ov.ModelsPath)
	}
	if ov.ModelsKeys["max_output_tokens"] != "limits.max_output" {
		t.Errorf("models_keys = %v", ov.ModelsKeys)
	}

	bad := []struct {
		name string
		yaml string
	}{
		{"relative path", "providers:\n  p:\n    models_path: language-models\n"},
		{"query in path", "providers:\n  p:\n    models_path: \"/m?x=1\"\n"},
		{"space in path", "providers:\n  p:\n    models_path: \"/mo dels\"\n"},
		{"non-dotted key path", "providers:\n  p:\n    models_keys:\n      input_modalities: \"a b\"\n"},
		{"empty field name", "providers:\n  p:\n    models_keys:\n      \" \": \"x\"\n"},
	}
	for _, tc := range bad {
		bp := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(bp, []byte(tc.yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		c := loadFileOK(t, tc.yaml)
		if len(c.Providers) != 0 {
			t.Errorf("%s: invalid models override applied: %+v", tc.name, c.Providers)
		}
	}
}

// Provider headers load, validate, and survive a WriteFile round trip: names
// must be RFC 7230 tokens, values single-line printable header values - a bad
// value fails the load (deny by default), never clamps.
func TestProviderHeaders(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	yaml := `providers:
  prov.example:
    headers:
      User-Agent: "my-shell/1.2.3 ({{platform}})"
      x-req-id: "{{uuid4}}"
      x-static: "v1"
`
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h := c.Providers["prov.example"].Headers
	if h["User-Agent"] != "my-shell/1.2.3 ({{platform}})" || h["x-req-id"] != "{{uuid4}}" || h["x-static"] != "v1" {
		t.Fatalf("headers = %v", h)
	}

	// Round trip: the written file parses back to the same map.
	out := filepath.Join(dir, "out.yaml")
	if err := WriteFile(out, c); err != nil {
		t.Fatal(err)
	}
	c2, err := LoadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := c2.Providers["prov.example"].Headers
	if len(got) != 3 || got["x-req-id"] != "{{uuid4}}" || got["User-Agent"] != "my-shell/1.2.3 ({{platform}})" {
		t.Fatalf("round-tripped headers = %v", got)
	}

	bad := []struct{ name, yaml string }{
		{"bad name (space)", "providers:\n  p:\n    headers:\n      \"bad name\": \"v\"\n"},
		{"empty name", "providers:\n  p:\n    headers:\n      \"\": \"v\"\n"},
		{"empty value", "providers:\n  p:\n    headers:\n      x-a: \"\"\n"},
		{"newline in value", "providers:\n  p:\n    headers:\n      x-a: \"one\\ntwo\"\n"},
		{"control char in value", "providers:\n  p:\n    headers:\n      x-a: \"one\\x01two\"\n"},
	}
	for _, tc := range bad {
		bp := filepath.Join(dir, "badh.yaml")
		if err := os.WriteFile(bp, []byte(tc.yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		c := loadFileOK(t, tc.yaml)
		if len(c.Providers) != 0 {
			t.Errorf("%s: invalid provider header applied: %+v", tc.name, c.Providers)
		}
	}
}

// ValidHeaderValue owns the RFC 7230 field-value grammar alone: the empty
// string passes the grammar (an X-Proxy-Auth-Prefix may legitimately be "";
// the provider-header mapping rejects empty as its own policy, pinned by
// the "empty value" row above), and control bytes never pass.
func TestValidHeaderValueGrammar(t *testing.T) {
	if !ValidHeaderValue("") {
		t.Error("empty string must pass the grammar: emptiness is caller policy")
	}
	for _, v := range []string{"v", "Bearer ", "tab\tvalue", "caf\xc3\xa9 unicode", "obs-text \xff"} {
		if !ValidHeaderValue(v) {
			t.Errorf("ValidHeaderValue(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"one\ntwo", "one\rtwo", "nul\x00byte", "ctl\x01byte", "del\x7fbyte"} {
		if ValidHeaderValue(v) {
			t.Errorf("ValidHeaderValue(%q) = true, want false", v)
		}
	}
}

// cost_keys entries are dotted JSON paths resolved by metrics.DigJSON; a
// malformed path is a cost key that can never resolve, so it is rejected at
// the load boundary with a clear error (deny by default), never silently
// applied. Before this validation existed, garbage paths loaded cleanly.
func TestProviderCostKeysPathValidated(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"space in path", "providers:\n  gw:\n    cost_keys: [\"usage cost\"]\n",
			`providers.gw.cost_keys: "usage cost" is not a dotted JSON path`},
		{"double dot", "providers:\n  gw:\n    cost_keys: [\"usage..cost\"]\n",
			`providers.gw.cost_keys: "usage..cost" is not a dotted JSON path`},
		{"empty path", "providers:\n  gw:\n    cost_keys: [\"\"]\n",
			`providers.gw.cost_keys: "" is not a dotted JSON path`},
		{"bare dot", "providers:\n  gw:\n    cost_keys: [\".\"]\n",
			`providers.gw.cost_keys: "." is not a dotted JSON path`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			c := loadFileOK(t, tc.yaml)
			if len(c.Providers) != 0 {
				t.Fatalf("LoadFile applied a bad cost_keys path:\n%s", tc.yaml)
			}
		})
	}
}

// ensure_tools entries relay into the upstream tools array verbatim; an
// empty name would inject a nameless stub and a duplicate would inject the
// same stub twice, so both are rejected at the load boundary with the entry
// dropped (deny by default), never silently applied.
func TestProviderEnsureToolsValidated(t *testing.T) {
	cases := []struct{ name, yaml string }{
		{"empty name", "providers:\n  gw:\n    ensure_tools: [\"\"]\n"},
		{"whitespace name", "providers:\n  gw:\n    ensure_tools: [\"  \"]\n"},
		{"duplicate name", "providers:\n  gw:\n    ensure_tools: [\"bash\", \"bash\"]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := loadFileOK(t, tc.yaml)
			if len(c.Providers) != 0 {
				t.Fatalf("LoadFile applied a bad ensure_tools entry:\n%s", tc.yaml)
			}
		})
	}
	valid := "providers:\n  gw:\n    ensure_tools: [\"bash\", \"read\"]\n"
	c := loadFileOK(t, valid)
	if len(c.Providers) != 1 || len(c.Providers["gw"].EnsureTools) != 2 {
		t.Fatalf("LoadFile dropped a valid ensure_tools entry:\n%s", valid)
	}
}

// usage_keys: the VALUE must be a dotted JSON path (metrics.DigJSON resolves
// it), and the KEY must be a canonical usage field - one of
// metrics.CanonicalUsageFields, the single owner the Settings dropdown offers
// and ParseUsage consumes. A typo on either side is a silent runtime no-op,
// so both are rejected at load with a clear error. Before this validation
// existed, both garbage sides loaded cleanly.
func TestProviderUsageKeysValidated(t *testing.T) {
	cases := []struct{ name, yaml, wantErr string }{
		{"bad value path", "providers:\n  gw:\n    usage_keys:\n      input_tokens: \"usage cost\"\n",
			`providers.gw.usage_keys: input_tokens: "usage cost" is not a dotted JSON path`},
		{"double-dot value path", "providers:\n  gw:\n    usage_keys:\n      output_tokens: \"toks..out\"\n",
			`providers.gw.usage_keys: output_tokens: "toks..out" is not a dotted JSON path`},
		{"typo'd canonical key", "providers:\n  gw:\n    usage_keys:\n      input_token: \"prompt_toks\"\n",
			`providers.gw.usage_keys: "input_token" is not a canonical usage field`},
		{"empty canonical key", "providers:\n  gw:\n    usage_keys:\n      \"\": \"prompt_toks\"\n",
			`providers.gw.usage_keys: "" is not a canonical usage field`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(tc.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			c := loadFileOK(t, tc.yaml)
			if len(c.Providers) != 0 {
				t.Fatalf("LoadFile applied a bad usage_keys entry:\n%s", tc.yaml)
			}
		})
	}

	// Every canonical field, mapped to a valid path, loads cleanly - the
	// validation must accept exactly the set metrics.ParseUsage consumes.
	allCanonical := "providers:\n  gw:\n    usage_keys:\n"
	for _, f := range metrics.CanonicalUsageFields {
		allCanonical += "      " + f + ": \"custom." + f + "\"\n"
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(allCanonical), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(p); err != nil {
		t.Errorf("LoadFile(all canonical usage_keys) = %v, want nil. YAML:\n%s", err, allCanonical)
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

// allowed_base_urls entries feed a plain byte-PREFIX match at runtime
// (internal/proxy's `allowed`) against client base URLs that are
// trailing-slash-trimmed and can never carry userinfo, query, or fragment
// (the proxy rejects those at the request boundary). So besides the
// scheme-less typo, four more dead forms would load cleanly and then
// silently deny every request - a trailing slash (no trimmed base can be
// longer than the entry), userinfo (also persists credentials in the
// config), a query/fragment suffix, and a non-lowercase host (the match is
// byte-sensitive, so it only ever matches byte-identical client casing).
// A BARE trailing "?" or "#" is exactly as dead (the request boundary
// rejects any base URL carrying either byte) but parses to an EMPTY
// RawQuery/Fragment, so it slipped past the parsed-field checks. All
// rejected at the load boundary with a clear error; well-formed entries
// (including path suffixes and IPv6 hosts) load.
func TestLoadFileStrictFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"invalid allowlist", "allowed_base_urls:\n  - api.openai.com\n"},
		{"mixed allowlist", "allowed_base_urls:\n  - https://api.example.com\n  - api.openai.com\n"},
		{"invalid listen", "listen: 8080\n"},
		{"invalid history size", "history_size: 0\n"},
		{"syntax error", "listen: [\n"},
		{"nested duplicate", "providers:\n  alpha:\n    models_path: a\n    models_path: b\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "strict.yaml")
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := LoadFileStrict(p); err == nil {
				t.Fatalf("LoadFileStrict accepted %s", tc.name)
			}
		})
	}
	p := filepath.Join(t.TempDir(), "strict-good.yaml")
	if err := os.WriteFile(p, []byte("max_retries: 3\nmax_retriez: 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, dropped, err := LoadFileStrict(p)
	if err != nil || cfg.MaxRetries != 3 || len(dropped) != 1 || dropped[0] != "max_retriez" {
		t.Fatalf("strict good load = %+v dropped=%v err=%v", cfg.MaxRetries, dropped, err)
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

// A provider override keyed by an empty or whitespace-only label can never
// apply - the runtime label is derived from the base URL host and is never
// empty - so the whole section would be a silent no-op. Rejected at load
// (the YAML path), mirroring the Settings POST path's rule in values.go
// (asProviders); the quoted empty key is legal YAML, so it must not slip
// through LoadFile.
func TestProviderLabelValidated(t *testing.T) {
	for _, yaml := range []string{
		"providers:\n  \"\":\n    cost_keys: [\"a.b\"]\n",
		"providers:\n  \"  \":\n    headers:\n      x-a: \"v\"\n",
	} {
		p := filepath.Join(t.TempDir(), "c.yaml")
		if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
		c := loadFileOK(t, yaml)
		if len(c.Providers) != 0 {
			t.Errorf("LoadFile applied an empty/whitespace provider label:\n%s", yaml)
		}
	}

	// A properly labeled override still loads.
	p := filepath.Join(t.TempDir(), "ok.yaml")
	if err := os.WriteFile(p, []byte("providers:\n  gw.example:\n    cost_keys: [\"a.b\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(p); err != nil {
		t.Errorf("LoadFile(labeled provider) = %v, want nil", err)
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
