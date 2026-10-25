package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
