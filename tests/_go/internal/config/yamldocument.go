package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestDuplicateTopLevelYAMLKeysRejected pins the shared mapping decoder: a
// duplicate top-level key is ambiguous, so it is a parse error naming the key
// and line instead of a last-wins overwrite (or a default when the last value
// is invalid).
func TestDuplicateTopLevelYAMLKeysRejected(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		key  string
		line int
	}{
		{"valid then invalid", "max_retries: 2\nmax_retries: nope\n", "max_retries", 2},
		{"valid then valid", "max_retries: 2\nmax_retries: 9\n", "max_retries", 2},
		{"duplicate listen", "listen: \":9999\"\nlisten: \":8081\"\n", "listen", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, skipped, err := LoadBytes([]byte(tc.body))
			if err == nil {
				t.Fatalf("LoadBytes() = %+v, %v, want a duplicate-key parse error", cfg, skipped)
			}
			if cfg != nil || skipped != nil {
				t.Fatalf("LoadBytes() returned a config/skipped list with the error: %+v %v", cfg, skipped)
			}
			var parseErr *configParseError
			if !errors.As(err, &parseErr) {
				t.Fatalf("error %v is not a configParseError", err)
			}
			for _, needle := range []string{
				`duplicate key "` + tc.key + `"`,
				"line " + strconv.Itoa(tc.line),
			} {
				if !strings.Contains(err.Error(), needle) {
					t.Fatalf("error %q must contain %q", err, needle)
				}
			}
		})
	}
}

// TestDuplicateYAMLKeyRepairDoesNotRewrite pins the repair boundary: an
// ambiguous duplicate is refused without renaming the source or writing a
// default over it, so the operator's first value is still on disk.
func TestDuplicateYAMLKeyRepairDoesNotRewrite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.yaml")
	raw := []byte("listen: \":9999\"\nlisten: \":8081\"\n")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, skipped, err := LoadFileRepair(p)
	if err == nil {
		t.Fatalf("LoadFileRepair() = %+v, %v, want the duplicate rejected", cfg, skipped)
	}
	got, readErr := os.ReadFile(p)
	if readErr != nil || !bytes.Equal(got, raw) {
		t.Fatalf("repair rewrote the duplicate document: %v %q", readErr, got)
	}
	if _, statErr := os.Stat(p + invalidConfigSuffix); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("repair moved the duplicate document aside: %v", statErr)
	}
}

// TestLoadFileRejectsOversizedDocument pins the read bound: the input document
// is stat-ed against the same 16 MiB envelope as the rendered document before
// it is read or parsed, so a huge file cannot allocate first.
func TestLoadFileRejectsOversizedDocument(t *testing.T) {
	p := filepath.Join(t.TempDir(), "huge.yaml")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(int64(ConfigDocumentMaxBytes) + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = LoadFile(p)
	if err == nil || !strings.Contains(err.Error(), "above") || !strings.Contains(err.Error(), FormatByteSize(ConfigDocumentMaxBytes)) {
		t.Fatalf("LoadFile() = %v, want the document-size rejection", err)
	}
	if strings.Contains(err.Error(), "parse config") {
		t.Fatalf("error %q came from parsing, not the size check", err)
	}

	cfg, skipped, repairErr := LoadFileRepair(p)
	if repairErr == nil {
		t.Fatalf("LoadFileRepair() = %+v, %v, want the oversized file rejected", cfg, skipped)
	}
	info, statErr := os.Stat(p)
	if statErr != nil || info.Size() != int64(ConfigDocumentMaxBytes)+1 {
		t.Fatalf("repair changed the oversized source: %v %+v", statErr, info)
	}
	if _, statErr := os.Stat(p + invalidConfigSuffix); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("repair moved the oversized source aside: %v", statErr)
	}
}

// TestLoadFileAcceptsDocumentAtInputCap pins the inclusive bound: a valid
// document exactly at ConfigDocumentMaxBytes still loads normally.
func TestLoadFileAcceptsDocumentAtInputCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "cap.yaml")
	raw := bytes.Repeat([]byte("#\n"), ConfigDocumentMaxBytes/2)
	if len(raw) != ConfigDocumentMaxBytes {
		t.Fatalf("fixture = %d bytes, want %d", len(raw), ConfigDocumentMaxBytes)
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(p)
	if err != nil {
		t.Fatalf("LoadFile(at cap) = %v, want nil", err)
	}
	if c.Listen != Default().Listen {
		t.Fatalf("at-cap document listen = %q, want the default", c.Listen)
	}
}
