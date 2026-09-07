package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQueryLimitConfigPersistenceAndBounds(t *testing.T) {
	load := func(raw string) (*Config, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		return LoadFile(path)
	}
	defaults := Default()
	got, err := load("{}")
	if err != nil || got.StorageQueryMaxBytes != defaults.StorageQueryMaxBytes || got.StorageQueryMaxRows != defaults.StorageQueryMaxRows {
		t.Fatalf("defaults=%+v err=%v", got, err)
	}
	for _, field := range []struct {
		key      string
		min, max int
	}{
		{"storage_query_max_bytes", StorageQueryMaxBytesMin, StorageQueryMaxBytesMax},
		{"storage_query_max_rows", StorageQueryMaxRowsMin, StorageQueryMaxRowsMax},
	} {
		for _, invalid := range []string{"-1", "0", fmt.Sprint(field.min - 1), fmt.Sprint(field.max + 1), "1.5", "bad"} {
			if _, err := load(field.key + ": " + invalid); err == nil {
				t.Errorf("accepted %s=%s", field.key, invalid)
			}
		}
		for _, valid := range []int{field.min, field.max} {
			if _, err := load(fmt.Sprintf("%s: %d", field.key, valid)); err != nil {
				t.Errorf("boundary %s=%d: %v", field.key, valid, err)
			}
		}
		c := Default()
		if field.key == "storage_query_max_bytes" {
			c.StorageQueryMaxBytes = ByteSize(field.max + 1)
		} else {
			c.StorageQueryMaxRows = field.max + 1
		}
		if err := c.Validate(); err == nil {
			t.Errorf("Validate accepted %s overflow", field.key)
		}
	}
	changed := Default()
	changed.StorageQueryMaxBytes = StorageQueryMaxBytesMin
	changed.StorageQueryMaxRows = StorageQueryMaxRowsMin
	var buf bytes.Buffer
	if err := WriteYAML(&buf, changed); err != nil {
		t.Fatal(err)
	}
	got, err = load(buf.String())
	if err != nil || got.StorageQueryMaxBytes != changed.StorageQueryMaxBytes || got.StorageQueryMaxRows != changed.StorageQueryMaxRows {
		t.Fatalf("roundtrip=%+v err=%v", got, err)
	}
	for _, f := range Schema() {
		if strings.HasPrefix(f.Key, "storage_query_max_") && f.HotReload {
			t.Errorf("%s incorrectly hot-reloads", f.Key)
		}
	}
	if _, err := load("storage_query_max_byte: 8192"); err == nil {
		t.Fatal("unknown query setting accepted")
	}
	for _, key := range []string{"history_size", "max_conns_per_host", "max_request_bytes"} {
		for _, raw := range []string{"1.5", "8192.5", "8192.0", "null"} {
			if _, err := load(key + ": " + raw); err == nil {
				t.Errorf("shared integer boundary accepted %s=%s", key, raw)
			}
		}
	}
}
