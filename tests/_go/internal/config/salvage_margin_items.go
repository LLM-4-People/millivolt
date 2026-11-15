package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestSalvageReadsColumnZeroSequenceItems pins R1-42: a fail-closed block can
// spell its sequence at the document margin, which is valid YAML. A column-zero
// "- " line can only belong to the key above, so ending the block there kept
// the bare key and lost the allowlist: salvage then failed closed on
// "must be a list of strings" instead of recovering the origins.
func TestSalvageReadsColumnZeroSequenceItems(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{
			name: "column zero items",
			raw:  "allowed_base_urls:\n- https://a.example\nnext: 1\n",
			want: "allowed_base_urls:\n- https://a.example",
		},
		{
			name: "document marker ends the document",
			raw:  "allowed_base_urls:\n- https://a.example\n---\n- https://b.example\n",
			want: "allowed_base_urls:\n- https://a.example",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(topLevelFailClosedBlocks([]byte(tc.raw))["allowed_base_urls"]); got != tc.want {
				t.Fatalf("salvaged block = %q, want %q", got, tc.want)
			}
		})
	}

	raw := "listen: 127.0.0.1:9090\nallowed_base_urls:\n- https://a.example\n- https://b.example\nmax_retries: [broken\n"
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _, err := LoadFileRepair(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"https://a.example", "https://b.example"}
	if !reflect.DeepEqual(c.AllowedBaseURLs, want) {
		t.Fatalf("salvaged allowlist = %v, want %v", c.AllowedBaseURLs, want)
	}
	// The rewritten document is valid and still carries both origins.
	got, err := LoadFile(p)
	if err != nil {
		t.Fatalf("rewritten config is unreadable: %v", err)
	}
	if !reflect.DeepEqual(got.AllowedBaseURLs, want) {
		t.Fatalf("rewritten config lost the salvaged origins: %v", got.AllowedBaseURLs)
	}
}
