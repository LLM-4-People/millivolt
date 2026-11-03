package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestTranslatedResponseMaxBytesBoundsAndPersistence(t *testing.T) {
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
	if err != nil || got.TranslatedResponseMaxBytes != defaults.TranslatedResponseMaxBytes {
		t.Fatalf("defaults: %+v %v", got, err)
	}
	for _, invalid := range []string{"0", "-1", "1", fmt.Sprint(TranslatedResponseMaxBytesMax + 1), "4096.5", "null", "bad"} {
		got, err := load("translated_response_max_bytes: " + invalid)
		if err != nil {
			t.Errorf("%s: %v", invalid, err)
		} else if !reflect.DeepEqual(got.Map()["translated_response_max_bytes"], defaults.Map()["translated_response_max_bytes"]) {
			t.Errorf("applied invalid translated_response_max_bytes=%s", invalid)
		}
	}
	for _, valid := range []int64{TranslatedResponseMaxBytesMin, TranslatedResponseMaxBytesMax} {
		if _, err := load(fmt.Sprintf("translated_response_max_bytes: %d", valid)); err != nil {
			t.Errorf("boundary %d: %v", valid, err)
		}
	}
	for _, tc := range []struct {
		value int64
		want  string
	}{
		{TranslatedResponseMaxBytesMin - 1, "translated_response_max_bytes: must be 4KiB..1GiB, got 4095B"},
		{TranslatedResponseMaxBytesMax + 1, "translated_response_max_bytes: must be 4KiB..1GiB, got 1073741825B"},
	} {
		c := Default()
		c.TranslatedResponseMaxBytes = ByteSize(tc.value)
		if err := c.Validate(); err == nil || err.Error() != tc.want {
			t.Errorf("Validate error = %v, want %q", err, tc.want)
		}
	}
	f := FieldByKey("translated_response_max_bytes")
	if f == nil || f.Category != "upstream" || f.Kind != KindBytes || !f.HotReload {
		t.Fatalf("schema = %+v", f)
	}
	if f.Min == nil || *f.Min != float64(TranslatedResponseMaxBytesMin) || f.Max == nil || *f.Max != float64(TranslatedResponseMaxBytesMax) {
		t.Fatalf("schema band = %v..%v", f.Min, f.Max)
	}
	if !strings.Contains(f.Help, "502") {
		t.Fatalf("schema help does not state the failure mode: %q", f.Help)
	}
}
