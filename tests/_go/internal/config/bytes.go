package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseByteSizeGrammar(t *testing.T) {
	cases := []struct {
		in   any
		want int64
	}{
		{int64(1024), 1024},
		{"1024", 1024},
		{"1KiB", 1024},
		{"1kib", 1024},
		{"32MiB", 32 << 20},
		{" 8 MiB ", 8 << 20},
		{"1GiB", 1 << 30},
		{"4KiB", 4 << 10},
		{"0B", 0},
		{float64(4096), 4096},
	}
	for _, tc := range cases {
		got, err := parseByteSize(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("parseByteSize(%v) = %d, %v, want %d", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []any{"", "32MB", "1.5MiB", "KiB", "32MiBextra", true} {
		if _, err := parseByteSize(bad); err == nil {
			t.Errorf("parseByteSize(%v) succeeded, want error", bad)
		}
	}
	// Bare integer strings remain counts of bytes (existing YAML).
	got, err := parseByteSize("33554432")
	if err != nil || got != 32<<20 {
		t.Errorf("bare integer bytes = %d, %v", got, err)
	}
}

func TestApplyAndLoadByteSizeString(t *testing.T) {
	c := Default()
	if err := c.Apply(map[string]any{"max_request_bytes": "2MiB"}); err != nil {
		t.Fatal(err)
	}
	if c.MaxRequestBytes != 2<<20 {
		t.Errorf("Apply 2MiB = %d", c.MaxRequestBytes)
	}
	path := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(path, []byte("max_request_bytes: 4KiB\nstorage_query_max_bytes: 8MiB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRequestBytes != 4<<10 {
		t.Errorf("YAML 4KiB = %d", got.MaxRequestBytes)
	}
	if got.StorageQueryMaxBytes != 8<<20 {
		t.Errorf("YAML 8MiB = %d", got.StorageQueryMaxBytes)
	}
}

func TestFormatDuration(t *testing.T) {
	if FormatDuration(0) != "0s" {
		t.Errorf("0 = %s", FormatDuration(0))
	}
	if FormatDuration(HeartbeatIntervalMin) != "1ns" {
		t.Errorf("heartbeat min = %s", FormatDuration(HeartbeatIntervalMin))
	}
	if FormatDuration(HeartbeatIntervalMax) != "10m" {
		t.Errorf("heartbeat max = %s", FormatDuration(HeartbeatIntervalMax))
	}
	if FormatDuration(Default().QueueRetryAfter) != "2s" {
		t.Errorf("queue_retry_after = %s", FormatDuration(Default().QueueRetryAfter))
	}
	if FormatDuration(2*time.Minute) != "2m" {
		t.Errorf("2m = %s", FormatDuration(2*time.Minute))
	}
	if FormatDuration(24*time.Hour) != "24h" {
		t.Errorf("24h = %s", FormatDuration(24*time.Hour))
	}
	if FormatDuration(5*time.Minute) != "5m" {
		t.Errorf("5m = %s", FormatDuration(5*time.Minute))
	}
	if FormatDuration(Default().BaseBackoff) != "1s" {
		t.Errorf("base_backoff = %s", FormatDuration(Default().BaseBackoff))
	}
	if FormatDuration(Default().MaxBackoff) != "2m" {
		t.Errorf("max_backoff = %s", FormatDuration(Default().MaxBackoff))
	}
	if FormatDuration(Default().StorageFlushInterval) != "500ms" {
		t.Errorf("storage_flush_interval = %s", FormatDuration(Default().StorageFlushInterval))
	}
}

func TestHeartbeatRange(t *testing.T) {
	want := FormatDuration(HeartbeatIntervalMin) + ".." + FormatDuration(HeartbeatIntervalMax)
	if heartbeatRange() != want {
		t.Errorf("heartbeatRange = %q, want %q", heartbeatRange(), want)
	}
	if heartbeatRange() != "1ns..10m" {
		t.Errorf("heartbeatRange = %q, want 1ns..10m", heartbeatRange())
	}
}

func TestFormatByteSize(t *testing.T) {
	if FormatByteSize(0) != "0B" {
		t.Errorf("0 = %s", FormatByteSize(0))
	}
	if FormatByteSize(MaxRequestBytesMin) != "1KiB" {
		t.Errorf("min request bytes = %s", FormatByteSize(MaxRequestBytesMin))
	}
	if FormatByteSize(int64(Default().MaxRequestBytes)) != "32MiB" {
		t.Errorf("default max_request_bytes = %s", FormatByteSize(int64(Default().MaxRequestBytes)))
	}
	if FormatByteSize(1000) != "1000B" {
		t.Errorf("1000 = %s", FormatByteSize(1000))
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	c := Default()
	if c.RetryAfterSeconds() != int(c.QueueRetryAfter/time.Second) {
		t.Errorf("RetryAfterSeconds = %d", c.RetryAfterSeconds())
	}
	c.QueueRetryAfter = 0
	if c.RetryAfterSeconds() != 0 {
		t.Errorf("zero RetryAfterSeconds = %d", c.RetryAfterSeconds())
	}
}
