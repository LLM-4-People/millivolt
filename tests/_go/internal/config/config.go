package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

func TestOverlayNonDefaultSkipsBuiltInValues(t *testing.T) {
	src := Default()
	src.MaxRetries = 9
	src.CaptureBodyPreview = true
	dst := Default()
	dst.MaxRetries = 2
	if err := OverlayNonDefault(dst, src); err != nil {
		t.Fatal(err)
	}
	if dst.MaxRetries != 9 || !dst.CaptureBodyPreview {
		t.Fatalf("merged retries=%d preview=%v", dst.MaxRetries, dst.CaptureBodyPreview)
	}
	src.MaxRetries = Default().MaxRetries
	dst = Default()
	dst.MaxRetries = 2
	if err := OverlayNonDefault(dst, src); err != nil {
		t.Fatal(err)
	}
	if dst.MaxRetries != 2 {
		t.Fatalf("default backup retries overwrote live: %d", dst.MaxRetries)
	}
}

func TestDiffKeysNamesModifiedSettings(t *testing.T) {
	a := Default()
	b := Default()
	if keys := DiffKeys(a, b); len(keys) != 0 {
		t.Fatalf("identical configs: %v", keys)
	}
	b.MaxRetries = 9
	keys := DiffKeys(b, a)
	if len(keys) != 1 || keys[0] != "max_retries" {
		t.Fatalf("diff = %v", keys)
	}
}

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

// Default() must populate every tunable (no zero-valued config that code then
// has to second-guess). This is the single-source-of-truth guarantee.
func TestDefaultHasNoZeroTunables(t *testing.T) {
	c := Default()
	checks := map[string]bool{
		"listen":                  c.Listen != "",
		"db_path":                 c.DBPath != "",
		"history_size":            c.HistorySize > 0,
		"shutdown_timeout":        c.ShutdownTimeout > 0,
		"restart_drain_timeout":   c.RestartDrainTimeout > 0,
		"max_request_bytes":       c.MaxRequestBytes > 0,
		"upstream_timeout":        c.UpstreamTimeout > 0,
		"max_conns_per_host":      c.MaxConnsPerHost > 0,
		"max_idle_conns":          c.MaxIdleConns > 0,
		"max_idle_conns_per_host": c.MaxIdleConnsPerHost > 0,
		"idle_conn_timeout":       c.IdleConnTimeout > 0,
		"max_retries":             c.MaxRetries > 0,
		"quality_retries":         c.QualityRetries > 0,
		"queue_retry_after":       c.QueueRetryAfter > 0,
		"base_backoff":            c.BaseBackoff > 0,
		"max_backoff":             c.MaxBackoff > 0,
		"convo_idle_gap":          c.ConversationIdleGap > 0,
		"convo_max_open":          c.ConversationMaxOpen > 0,
		"anthropic_max_tok":       c.AnthropicDefaultMaxTokens > 0,
		"cursor_park_ttl":         c.CursorParkTTL > 0,
		"cursor_heartbeat":        c.CursorHeartbeatInterval > 0,
		"sse_keepalive":           c.SSEKeepaliveInterval > 0,
		"dash_log_rows":           c.DashLogRows > 0,
		"dash_poll":               c.DashPollInterval > 0,
		"dash_chart":              c.DashChartRefresh > 0,
		"dash_explorer":           c.DashExplorerStale > 0,
		"storage_wchan":           c.StorageWriteChanCap > 0,
		"storage_batch":           c.StorageBatchCap > 0,
		"storage_flush":           c.StorageFlushInterval > 0,
		"storage_qto":             c.StorageQueryTimeout > 0,
		"backup_max_bytes":        c.BackupMaxBytes > 0,
		"read_header":             c.ReadHeaderTimeout > 0,
		"idle_timeout":            c.IdleTimeout > 0,
	}
	for k, ok := range checks {
		if !ok {
			t.Errorf("Default().%s is zero/empty - every tunable must have a default", k)
		}
	}
	// The queue caps are intentionally zero (0 = unlimited); pin that so an
	// accidental default-zero on a real tunable isn't masked, and the trio's
	// documented zero semantics are asserted somewhere.
	zeroByDesign := map[string]bool{
		"max_concurrent": c.MaxConcurrent == 0,
		"max_queue_size": c.MaxQueueSize == 0,
		"max_queue_wait": c.MaxQueueWait == 0,
	}
	for k, ok := range zeroByDesign {
		if !ok {
			t.Errorf("Default().%s should stay 0 (0 = unlimited by design)", k)
		}
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

func TestExampleProfilesAreIsolated(t *testing.T) {
	example, pristine, defaults := Example(), Example(), Default()
	if err := example.Validate(); err != nil {
		t.Fatalf("shipped example is invalid: %v", err)
	}
	if len(defaults.Providers) != 0 || len(example.Providers) != 2 {
		t.Fatal("only the shipped example should enable the two provider profiles")
	}
	withoutProfiles := example.Clone()
	withoutProfiles.Providers = nil
	if !reflect.DeepEqual(withoutProfiles.Map(), defaults.Map()) {
		t.Fatal("example duplicates or changes server defaults")
	}
	for _, label := range []string{"cursor.sh", "x.ai"} {
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

// upstream_timeout is the one field where 0 is meaningful (disables the cap):
// an explicit 0s in YAML must override the non-zero default, while omitting the
// key keeps the default. A bare integer is not a duration string.
func TestUpstreamTimeoutExplicitZero(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, []byte("upstream_timeout: 0s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.UpstreamTimeout != 0 {
		t.Errorf("explicit upstream_timeout: 0s = %v, want 0 (disabled)", c.UpstreamTimeout)
	}
	if err := os.WriteFile(p, []byte("upstream_timeout: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c0 := loadFileOK(t, "upstream_timeout: 0\n")
	if c0.UpstreamTimeout != Default().UpstreamTimeout {
		t.Fatalf("upstream_timeout: 0 (bare integer) applied %v, want default", c0.UpstreamTimeout)
	}
	// Omitting the key keeps the default.
	c2, err := LoadFile(filepath.Join(dir, "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c2.UpstreamTimeout != 5*time.Minute {
		t.Errorf("absent upstream_timeout = %v, want default 5m", c2.UpstreamTimeout)
	}
}

// Settings WriteYAML writes every Schema key, including explicit 0. LoadFile
// must keep those zeros (pool unlimited, no retries, empty db_path, …) instead
// of treating them as "absent - keep Default()".
func TestWriteYAMLExplicitZerosSurviveLoadFile(t *testing.T) {
	cur := Default()
	if err := cur.Apply(map[string]any{
		"max_conns_per_host":    0,
		"max_idle_conns":        0,
		"idle_conn_timeout":     FormatDuration(0),
		"max_retries":           0,
		"queue_retry_after":     FormatDuration(0),
		"quality_retries":       0,
		"upstream_timeout":      FormatDuration(0),
		"restart_drain_timeout": FormatDuration(0),
		"db_path":               "",
		"capture_body_preview":  false,
	}); err != nil {
		t.Fatal(err)
	}
	if err := cur.Validate(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := WriteFile(p, cur); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxConnsPerHost != 0 || got.MaxIdleConns != 0 {
		t.Errorf("pool sizes = %d/%d, want 0 (unlimited)", got.MaxConnsPerHost, got.MaxIdleConns)
	}
	if got.MaxIdleConnsPerHost != Default().MaxIdleConnsPerHost {
		t.Errorf("max_idle_conns_per_host = %d, want default %d (0 is not unlimited in net/http)", got.MaxIdleConnsPerHost, Default().MaxIdleConnsPerHost)
	}
	if got.IdleConnTimeout != 0 {
		t.Errorf("idle_conn_timeout = %v, want 0", got.IdleConnTimeout)
	}
	if got.MaxRetries != 0 {
		t.Errorf("max_retries = %d, want 0", got.MaxRetries)
	}
	if got.QueueRetryAfter != 0 {
		t.Errorf("queue_retry_after = %v, want 0", got.QueueRetryAfter)
	}
	if got.QualityRetries != 0 {
		t.Errorf("quality_retries = %d, want 0", got.QualityRetries)
	}
	if got.UpstreamTimeout != 0 {
		t.Errorf("upstream_timeout = %v, want 0", got.UpstreamTimeout)
	}
	if got.RestartDrainTimeout != 0 {
		t.Errorf("restart_drain_timeout = %v, want 0 (wait indefinitely)", got.RestartDrainTimeout)
	}
	if got.DBPath != "" {
		t.Errorf("db_path = %q, want empty (durability off)", got.DBPath)
	}
	if got.CaptureBodyPreview {
		t.Error("capture_body_preview still true")
	}
	// Untouched keys still match Default().
	if got.HistorySize != Default().HistorySize {
		t.Errorf("history_size = %d, want default %d", got.HistorySize, Default().HistorySize)
	}
}

func TestLoadFilePartialZeros(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, []byte("max_retries: 0\nmax_conns_per_host: 0\nqueue_retry_after: 0s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxRetries != 0 {
		t.Errorf("max_retries = %d, want 0", c.MaxRetries)
	}
	if c.MaxConnsPerHost != 0 {
		t.Errorf("max_conns_per_host = %d, want 0", c.MaxConnsPerHost)
	}
	if c.QueueRetryAfter != 0 {
		t.Errorf("queue_retry_after = %v, want 0", c.QueueRetryAfter)
	}
	if c.QualityRetries != 1 {
		t.Errorf("absent quality_retries = %d, want default 1", c.QualityRetries)
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

// quality_retries is the second field where an explicit 0 is meaningful
// (disables the degenerate-200 quality retry / cursor re-ask): the presence
// probe must let a written 0 override the default 1.
func TestQualityRetriesExplicitZero(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, []byte("quality_retries: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.QualityRetries != 0 {
		t.Errorf("explicit quality_retries: 0 = %d, want 0 (disabled)", c.QualityRetries)
	}
	c2, err := LoadFile(filepath.Join(dir, "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c2.QualityRetries != 1 {
		t.Errorf("absent quality_retries = %d, want default 1", c2.QualityRetries)
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

func TestLoadFileRepairDropsBrokenKeys(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	raw := "max_retries: 3\nmax_retriez: 9\nhistory_size: 0\n"
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	c, skipped, err := LoadFileRepair(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxRetries != 3 || c.HistorySize != Default().HistorySize {
		t.Fatalf("repaired config max_retries=%d history_size=%d", c.MaxRetries, c.HistorySize)
	}
	joined := strings.Join(skipped, ",")
	if !strings.Contains(joined, "max_retriez") || !strings.Contains(joined, "history_size") {
		t.Fatalf("skipped = %v", skipped)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte("max_retriez")) || bytes.Contains(body, []byte("history_size: 0\n")) {
		t.Fatalf("repaired file still contains dropped keys:\n%s", body)
	}
	if !bytes.Contains(body, []byte("max_retries: 3\n")) {
		t.Fatalf("repaired file lost the valid override:\n%s", body)
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

// Invalid overlay values are skipped at LoadFile (boot must succeed). Validate
// still rejects a Config that actually holds those values.
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
		{"sse keepalive zero", "sse_keepalive_interval: 0s"},
		{"dash_log_rows too small", "dash_log_rows: 1"},
		{"dash_poll zero", "dash_poll_interval: 0s"},
		{"batch cap exceeds chan cap", "storage_write_chan_cap: 100\nstorage_batch_cap: 200"},
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
		{"listen integer", "listen: 8080"},
		{"backup too small", "backup_max_bytes: 10"},
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
	c = loadFileOK(t, "listen: 8080\nmax_conns_per_host: 10\nmax_idle_conns_per_host: 8\n")
	if c.Listen != Default().Listen || c.MaxConnsPerHost != 10 || c.MaxIdleConnsPerHost != 8 {
		t.Fatalf("invalid listen split the pool: listen=%q pool=%d/%d", c.Listen, c.MaxConnsPerHost, c.MaxIdleConnsPerHost)
	}
	c = loadFileOK(t, "max_conns_per_host: 10\nmax_idle_conns_per_host: 8\nbase_backoff: 10m\nmax_backoff: 1m\n")
	if c.MaxConnsPerHost != 10 || c.MaxIdleConnsPerHost != 8 {
		t.Fatalf("conflicting backoff split the pool: %d/%d", c.MaxConnsPerHost, c.MaxIdleConnsPerHost)
	}
	c = loadFileOK(t, "listen: 8080\nallowed_base_urls:\n  - api.openai.com\nmax_conns_per_host: 10\nmax_idle_conns_per_host: 8\n")
	if c.MaxConnsPerHost != 10 || c.MaxIdleConnsPerHost != 8 {
		t.Fatalf("two independent invalid keys wiped the pool: %d/%d", c.MaxConnsPerHost, c.MaxIdleConnsPerHost)
	}
	c = loadFileOK(t, "listen: 8080\nallowed_base_urls:\n  - api.openai.com\nbase_backoff: 5m\nmax_backoff: 10m\nmax_conns_per_host: 10\nmax_idle_conns_per_host: 8\n")
	if c.BaseBackoff != 5*time.Minute || c.MaxBackoff != 10*time.Minute || c.MaxConnsPerHost != 10 || c.MaxIdleConnsPerHost != 8 {
		t.Fatalf("retry pass lost related keys: backoff=%v/%v pool=%d/%d", c.BaseBackoff, c.MaxBackoff, c.MaxConnsPerHost, c.MaxIdleConnsPerHost)
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

func TestLoadFileSkipsDurationInteger(t *testing.T) {
	assertOverlaySkipped(t, "shutdown_timeout: 5\n", "shutdown_timeout")
	assertOverlaySkipped(t, "upstream_timeout: 5\n", "upstream_timeout")
	assertOverlaySkipped(t, "queue_retry_after: 2\n", "queue_retry_after")
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
		{"boundary values", "history_size: 1\nconversation_max_open: 1\nqueue_retry_after: 0s"},
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
func TestAllowedBaseURLsValidated(t *testing.T) {
	bad := []struct{ name, entry, wantErr string }{
		{"missing scheme", "api.openai.com",
			`allowed_base_urls: "api.openai.com" must be an absolute http:// or https:// URL`},
		{"unsupported scheme", "ftp://api.openai.com",
			`allowed_base_urls: "ftp://api.openai.com" must be an absolute http:// or https:// URL`},
		{"empty host", "https://",
			`allowed_base_urls: "https://" must be an absolute http:// or https:// URL`},
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
		{"uppercase scheme and host", "HTTP://API.OPENAI.COM",
			`allowed_base_urls: "HTTP://API.OPENAI.COM": host must be lowercase (the runtime prefix match is byte-sensitive); use "http://api.openai.com"`},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			yaml := "allowed_base_urls:\n  - " + strconv.Quote(tc.entry) + "\n"
			p := filepath.Join(t.TempDir(), "c.yaml")
			if err := os.WriteFile(p, []byte(yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			c := loadFileOK(t, yaml)
			if len(c.AllowedBaseURLs) != 0 {
				t.Fatalf("LoadFile applied bad allowed_base_urls entry %q: %v", tc.entry, c.AllowedBaseURLs)
			}
		})
	}

	// Well-formed entries load cleanly: both spellings from the proxy.yaml
	// example (an https host and an http host:port), a path suffix, and an
	// IPv6 host:port (its bracketed host is already canonical lowercase).
	good := "allowed_base_urls:\n  - \"https://api.example.com\"\n  - \"http://10.0.0.5:8000\"\n  - \"https://gw.example.com/v1\"\n  - \"http://[::1]:8080\"\n"
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile(good allowed_base_urls) = %v, want nil", err)
	}
	if len(c.AllowedBaseURLs) != 4 {
		t.Errorf("allowed_base_urls = %v, want 4 entries", c.AllowedBaseURLs)
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
