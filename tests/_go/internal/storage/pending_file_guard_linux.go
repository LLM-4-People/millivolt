//go:build linux

package storage

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestOpenRejectsFIFOPendingSnapshot is the boot-hang regression: the pending
// slot was opened before its file type was checked, so a FIFO blocked Open
// forever. The shared guard must reject and quarantine it without reaching
// os.Open.
func TestOpenRejectsFIFOPendingSnapshot(t *testing.T) {
	dir := t.TempDir()
	dstPath := filepath.Join(dir, "dst.db")
	openLiveRow(t, dstPath, "keep-old")
	incoming := PendingSnapshotPath(dstPath)
	if err := syscall.Mkfifo(incoming, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		s, err := Open(dstPath, testOpts)
		if s != nil {
			_ = s.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Open after FIFO pending snapshot: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open blocked on a FIFO pending snapshot")
	}
	requireLiveRow(t, dstPath, "keep-old")
	info, err := os.Lstat(incoming + pendingSnapshotInvalidSuffix)
	if err != nil {
		t.Fatalf("FIFO pending snapshot was not quarantined: %v", err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("quarantined FIFO mode = %v, want a named pipe", info.Mode())
	}
}
