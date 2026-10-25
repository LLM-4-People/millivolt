package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
		"thinking_retries":      0,
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
	if got.ThinkingRetries != 0 {
		t.Errorf("thinking_retries = %d, want 0", got.ThinkingRetries)
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

// thinking_retries is the third explicitly-zero-meaningful int (0 disables
// the mid-thinking rescues); the same absent-vs-explicit pin as its sibling.
func TestThinkingRetriesExplicitZero(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(p, []byte("thinking_retries: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.ThinkingRetries != 0 {
		t.Errorf("explicit thinking_retries: 0 = %d, want 0 (disabled)", c.ThinkingRetries)
	}
	c2, err := LoadFile(filepath.Join(dir, "absent.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c2.ThinkingRetries != 1 {
		t.Errorf("absent thinking_retries = %d, want default 1", c2.ThinkingRetries)
	}
}

func TestLoadFileSkipsDurationInteger(t *testing.T) {
	assertOverlaySkipped(t, "shutdown_timeout: 5\n", "shutdown_timeout")
	assertOverlaySkipped(t, "upstream_timeout: 5\n", "upstream_timeout")
	assertOverlaySkipped(t, "queue_retry_after: 2\n", "queue_retry_after")
}
