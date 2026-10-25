package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
