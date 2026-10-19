package main

import (
	"errors"
	"os"
	"testing"
	"time"
)

// waitReloadLock polls the global reload lock without blocking. It returns
// true with the lock held by the caller when it became free.
func waitReloadLock(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if reloadMu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// TestHandoffReloadFenceBlocksUntilResume pins the restart flush barrier: from
// the moment the handoff takes the fence, no reload can acquire the lock; a
// failed handoff closes the resume fence and releases it so serving can
// continue with normal reloads.
func TestHandoffReloadFenceBlocksUntilResume(t *testing.T) {
	rs := newRestarter(&restartDeps{})
	rs.inHandoff.Store(true)
	fenceReloadsForHandoff(rs)

	fenced := !reloadMu.TryLock()
	if !fenced {
		reloadMu.Unlock()
	}
	// Always resolve the handoff so the watcher releases the lock, even if
	// the assertion below fails.
	rs.finishFailure(errors.New("fixture handoff failure"))
	released := waitReloadLock(2 * time.Second)
	if released {
		reloadMu.Unlock()
	}
	if !fenced {
		t.Fatal("handoff fence left the reload lock acquirable during the flush window")
	}
	if !released {
		t.Fatal("failed handoff did not release the reload fence")
	}
}

// TestTryReloadConfigSkipsWhenLockHeld pins the signal-loop non-blocking
// contract: a SIGHUP reload must not wait behind a Settings save, restore, or
// handoff fence (SIGTERM/SIGINT still reach the loop). Once the lock is free,
// the same call applies the config normally.
func TestTryReloadConfigSkipsWhenLockHeld(t *testing.T) {
	liveReloadFixture(t)
	if err := os.WriteFile(liveConfigPath, []byte("max_retries: 5\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reloadMu.Lock()
	skipped, ran, err := tryReloadConfig()
	reloadMu.Unlock()
	if ran || err != nil || skipped != nil {
		t.Fatalf("tryReloadConfig while locked = skipped=%v ran=%v err=%v, want a non-blocking skip", skipped, ran, err)
	}
	if liveCfg.MaxRetries != bootCfg.MaxRetries {
		t.Fatalf("skipped reload applied max_retries=%d", liveCfg.MaxRetries)
	}

	skipped, ran, err = tryReloadConfig()
	if err != nil || !ran || len(skipped) != 0 {
		t.Fatalf("tryReloadConfig after release = skipped=%v ran=%v err=%v", skipped, ran, err)
	}
	if liveCfg.MaxRetries != 5 {
		t.Fatalf("released reload did not apply max_retries=5 (got %d)", liveCfg.MaxRetries)
	}
}
