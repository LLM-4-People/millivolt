package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func loadFileOK(t *testing.T, raw string) *Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile(%q) = %v, want a skipped overlay", raw, err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("LoadFile(%q) produced invalid config: %v", raw, err)
	}
	return c
}

func assertOverlaySkipped(t *testing.T, raw, key string) {
	t.Helper()
	c := loadFileOK(t, raw)
	if key == "" {
		return
	}
	if !reflect.DeepEqual(c.Map()[key], Default().Map()[key]) {
		t.Errorf("LoadFile(%q) applied %s = %v, want default %v", raw, key, c.Map()[key], Default().Map()[key])
	}
}

// assertOverlayRejected pins the fail-closed boundary: an invalid recognized
// value is an error, never a silently restored default. The key names the
// offending field when the error has one.
func assertOverlayRejected(t *testing.T, raw, key string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(path)
	if err == nil {
		t.Fatalf("LoadFile(%q) = %+v, want a fail-closed error", raw, c)
	}
	if key != "" && !strings.Contains(err.Error(), key) {
		t.Errorf("LoadFile(%q) error = %q, want it to name %q", raw, err, key)
	}
	if c != nil {
		t.Errorf("LoadFile(%q) returned a config with the error: %+v", raw, c)
	}
}

func TestCursorDefaultContextWindowNegativeRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, []byte("cursor_default_context_window: -1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertOverlaySkipped(t, "cursor_default_context_window: -1\n", "cursor_default_context_window")
	c := Default()
	c.CursorDefaultContextWindow = -1
	if err := c.Validate(); err == nil {
		t.Fatal("Validate must reject negative cursor_default_context_window")
	}
}

// TestInvalidTranslationLossRejected pins the enum boundary at both the
// reload admission path and Validate: a misspelled policy must fail closed
// instead of silently falling back to warn, because the two policies have
// different upstream-send behavior.
func TestInvalidTranslationLossRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("translation_loss: strict\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg, _, err := LoadFileStrict(path); err == nil || cfg != nil || !strings.Contains(err.Error(), "translation_loss") {
		t.Fatalf("LoadFileStrict() = %+v, %v; want a translation_loss rejection", cfg, err)
	}
	// The boot repair/overlay path drops an invalid recognized value instead of
	// applying it; the running policy keeps the default.
	assertOverlaySkipped(t, "translation_loss: strict\n", "translation_loss")
	c := Default()
	c.TranslationLoss = "strict"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "translation_loss") {
		t.Fatalf("Validate() = %v, want a translation_loss rejection", err)
	}
}

// A partial YAML file overrides only the keys it sets; everything else keeps
// the default.
func TestLoadFilePartialOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	yaml := "listen: \":9090\"\nmax_retries: 9\nbase_backoff: 250ms\n"
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":9090" {
		t.Errorf("listen = %q, want :9090", c.Listen)
	}
	if c.MaxRetries != 9 {
		t.Errorf("max_retries = %d, want 9", c.MaxRetries)
	}
	if c.BaseBackoff != 250*time.Millisecond {
		t.Errorf("base_backoff = %v, want 250ms", c.BaseBackoff)
	}
	// Untouched keys keep defaults.
	if c.MaxBackoff != 2*time.Minute {
		t.Errorf("max_backoff = %v, want default 2m", c.MaxBackoff)
	}
	if c.HistorySize != 10000 {
		t.Errorf("history_size = %d, want default 10000", c.HistorySize)
	}
}

// A missing file yields the defaults (not an error).
func TestLoadFileMissing(t *testing.T) {
	c, err := LoadFile(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" {
		t.Errorf("listen = %q, want default :8080", c.Listen)
	}
}

// LoadFile drops unknown keys and keeps defaults so a typo cannot block boot.
// Settings POST still rejects unknown keys. Repair rewrites the file.
func TestLoadFileSkipsUnknownKey(t *testing.T) {
	assertOverlaySkipped(t, "max_retriez: 3\n", "max_retries")
	c := loadFileOK(t, "providers:\n  gw.example:\n    cost_keyz: [\"a.b\"]\n")
	if len(c.Providers["gw.example"].CostKeys) != 0 {
		t.Fatalf("unknown provider field applied: %+v", c.Providers)
	}

	p := filepath.Join(t.TempDir(), "c.yaml")
	raw := []byte("max_retriez: 3\n")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(p); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("LoadFile must not rewrite the file")
	}
}

func TestLoadFileKnownKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ok.yaml")
	if err := os.WriteFile(p, []byte("upstream_timeout: 5m\nmax_retries: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile(known keys) = %v, want nil", err)
	}
	if c.UpstreamTimeout != 5*time.Minute || c.MaxRetries != 3 {
		t.Errorf("strict decode lost values: upstream_timeout=%v max_retries=%d", c.UpstreamTimeout, c.MaxRetries)
	}
}

// Per-provider overrides load and merge by key.
func TestProviderOverrides(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	yaml := `providers:
  hyper:
    cost_keys: ["usage.cost"]
  gw:
    cost_keys: ["billing.charge_usd"]
    usage_keys:
      input_tokens: "usage.prompt_tokens"
`
	if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Providers["hyper"].CostKeys; len(got) != 1 || got[0] != "usage.cost" {
		t.Errorf("hyper cost_keys = %v", got)
	}
	if got := c.Providers["gw"].CostKeys; len(got) != 1 || got[0] != "billing.charge_usd" {
		t.Errorf("gw cost_keys = %v", got)
	}
	if got := c.Providers["gw"].UsageKeys["input_tokens"]; got != "usage.prompt_tokens" {
		t.Errorf("gw usage_keys.input_tokens = %q", got)
	}
}

// LoadFile is the lenient library read: an invalid recognized value is
// skipped. Boot uses LoadFileRepair and operational consumers use
// LoadFileStrict, both of which fail closed. Validate still rejects a Config
// that actually holds those values.
func TestLoadFileSkipsInvalidValues(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{"history_size zero", "history_size: 0"},
		{"history_size negative", "history_size: -10"},
		{"history_size too big", "history_size: 2000000"},
		{"max_request_bytes too small", "max_request_bytes: 10"},
		{"max_request_bytes negative", "max_request_bytes: -1"},
		{"go_memory_limit too small", "go_memory_limit: 16MiB"},
		{"go_memory_limit negative", "go_memory_limit: -1"},
		{"go_memory_limit too big", "go_memory_limit: 2TiB"},
		{"negative pool size", "max_conns_per_host: -1"},
		{"idle-per-host zero", "max_idle_conns_per_host: 0"},
		{"idle-per-host exceeds conns", "max_conns_per_host: 10\nmax_idle_conns_per_host: 20"},
		{"storage flush zero", "storage_flush_interval: 0s"},
		{"storage query timeout zero", "storage_query_timeout: 0s"},
		{"conversation idle gap zero", "conversation_idle_gap: 0s"},
		{"base_backoff zero", "base_backoff: 0s"},
		{"max_backoff zero", "max_backoff: 0s"},
		{"shutdown_timeout zero", "shutdown_timeout: 0s"},
		{"restart_drain_timeout negative", "restart_drain_timeout: -5s"},
		{"negative max_retries", "max_retries: -1"},
		{"queue_retry_after negative", "queue_retry_after: -5s"},
		{"queue_retry_after huge", "queue_retry_after: 25h"},
		{"queue_retry_after fractional second", "queue_retry_after: 2500ms"},
		{"conversation_max_open zero", "conversation_max_open: 0"},
		{"anthropic_default_max_tokens zero", "anthropic_default_max_tokens: 0"},
		{"cursor_heartbeat_interval zero", "cursor_heartbeat_interval: 0s"},
		{"cursor_heartbeat_interval negative", "cursor_heartbeat_interval: -2s"},
		{"cursor_heartbeat_interval huge", "cursor_heartbeat_interval: 1h"},
		{"shutdown_timeout integer", "shutdown_timeout: 5"},
		{"queue_retry_after integer", "queue_retry_after: 2"},
		{"quality_retries negative", "quality_retries: -1"},
		{"quality_retries too high", "quality_retries: 7"},
		{"thinking_retries negative", "thinking_retries: -1"},
		{"thinking_retries too high", "thinking_retries: 4"},
		{"sse keepalive zero", "sse_keepalive_interval: 0s"},
		{"dash_log_rows too small", "dash_log_rows: 1"},
		{"dash_poll zero", "dash_poll_interval: 0s"},
		{"batch cap exceeds chan cap", "storage_write_chan_cap: 100\nstorage_batch_cap: 200"},
		{"write channel cap too high", "storage_write_chan_cap: 1000001"},
		{"negative duration", "idle_conn_timeout: -5s"},
		{"negative upstream_timeout", "upstream_timeout: -5s"},
		{"negative backoff", "base_backoff: -1s"},
		{"base_backoff exceeds max_backoff", "base_backoff: 10m\nmax_backoff: 1m"},
		{"bad duration syntax", "upstream_timeout: notaduration"},
		{"bad type for int", "max_retries: five"},
		{"bad type for bool", "capture_body_preview: maybe"},
		{"int as float", "anthropic_default_max_tokens: 4096.0"},
		{"auto token refresh null", "auto_token_refresh: null"},
		{"auto token refresh yes", "auto_token_refresh: yes"},
		{"auto token refresh no", "auto_token_refresh: no"},
		{"backup too small", "backup_max_bytes: 10"},
		{"debug store too small", "debug_store_max_bytes: 1MiB"},
		{"debug store too big", "debug_store_max_bytes: 9GiB"},
		{"inflight body too small", "max_inflight_body_bytes: 512KiB"},
		{"inflight body too big", "max_inflight_body_bytes: 65GiB"},
		{"inflight response too small", "max_inflight_response_bytes: 512KiB"},
		{"inflight response too big", "max_inflight_response_bytes: 65GiB"},
		{"providers null", "providers: null"},
		{"model_rules null", "model_rules: null"},
		{"provider alias self-map", "provider_aliases:\n  old.example: old.example"},
		{"provider alias chain", "provider_aliases:\n  a.example: b.example\n  b.example: c.example"},
		{"provider alias empty target", "provider_aliases:\n  old.example: \"\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.TrimSpace(tc.yaml) + "\n"
			if strings.Contains(strings.TrimSuffix(body, "\n"), "\n") {
				loadFileOK(t, body)
				return
			}
			key := strings.TrimSpace(strings.SplitN(body, ":", 2)[0])
			assertOverlaySkipped(t, body, key)
		})
	}
}

func TestLoadFileKeepsRelatedKeys(t *testing.T) {
	c := loadFileOK(t, "max_conns_per_host: 10\nmax_idle_conns_per_host: 8\n")
	if c.MaxConnsPerHost != 10 || c.MaxIdleConnsPerHost != 8 {
		t.Fatalf("pool overlay = %d/%d, want 10/8", c.MaxConnsPerHost, c.MaxIdleConnsPerHost)
	}
	c = loadFileOK(t, "base_backoff: 5m\nmax_backoff: 10m\n")
	if c.BaseBackoff != 5*time.Minute || c.MaxBackoff != 10*time.Minute {
		t.Fatalf("backoff overlay = %v/%v, want 5m/10m", c.BaseBackoff, c.MaxBackoff)
	}
	c = loadFileOK(t, "storm_initial_backoff: 1h\nstorm_max_backoff: 1h\n")
	if c.StormInitialBackoff != time.Hour || c.StormMaxBackoff != time.Hour {
		t.Fatalf("storm backoff overlay = %v/%v, want 1h/1h", c.StormInitialBackoff, c.StormMaxBackoff)
	}
	c = loadFileOK(t, "storage_write_chan_cap: 20000\nstorage_batch_cap: 10000\n")
	if c.StorageWriteChanCap != 20000 || c.StorageBatchCap != 10000 {
		t.Fatalf("storage overlay = %d/%d, want 20000/10000", c.StorageWriteChanCap, c.StorageBatchCap)
	}
	c = loadFileOK(t, "max_conns_per_host: 10\nmax_idle_conns_per_host: 8\nbase_backoff: 10m\nmax_backoff: 1m\n")
	if c.MaxConnsPerHost != 10 || c.MaxIdleConnsPerHost != 8 {
		t.Fatalf("conflicting backoff split the pool: %d/%d", c.MaxConnsPerHost, c.MaxIdleConnsPerHost)
	}
}
