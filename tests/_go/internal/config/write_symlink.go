package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileFollowsSymlink pins the shared writer contract at the config
// owner: a symlinked runtime config stays a symlink and its target receives the
// new document, so a Settings save cannot replace the link with a regular file
// and leave the deployment's target stale.
func TestWriteFileFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	if err := os.WriteFile(target, []byte("listen: \"127.0.0.1:8081\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "proxy.yaml")
	if err := os.Symlink("real.yaml", link); err != nil {
		t.Fatal(err)
	}
	c := Default()
	c.Listen = "127.0.0.1:9999"
	if err := WriteFile(link, c); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(link); err != nil {
		t.Fatal(err)
	} else if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("config write replaced the symlink instead of following it")
	}
	loaded, err := LoadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Listen != "127.0.0.1:9999" {
		t.Fatalf("loaded listen = %q, want the written value", loaded.Listen)
	}
}
