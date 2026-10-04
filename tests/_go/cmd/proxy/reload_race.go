package main

// Round-5 race finding, made permanent: reloadConfig used to end with
// `*liveCfg = *cloned` - an in-place whole-struct write to the SAME object
// main handed to proxy.New at boot (the server's immutable cfgSnap
// snapshot, read unlocked on request goroutines via s.cfg()). The Reload
// call swaps the server's snapshot to a fresh clone, but the in-place write
// mutates the boot object with no happens-before edge to requests still
// reading it. The regression parks one request mid-flight holding the
// unlocked request-entry config reads it already made (resolveTarget at
// proxy.go and the readRequest size/preview arguments), runs the real
// reloadConfig while it is parked, then completes the request. Under -race
// the pre-fix tree reports the publication write against those parked
// reads; the fixed tree publishes a NEW liveCfg object and the boot
// snapshot is never written again, so the same scenario must stay clean.
//
// Shape notes for future editors, both empirically verified on the pre-fix
// tree (a regression that silently stops firing is worse than none):
//   - The park is a blocked request BODY, not a blocked models-discovery
//     upstream. A parked upstream fetch cannot fire here: the fetch touches
//     the upstream client's connection pool, and reloadConfig's liveProxy
//     Reload call closes the old transport's idle connections, acquiring
//     the parked reader's clock through the pool lock - the race detector
//     then sees the publication write as ordered. The body park performs
//     the same unlocked config reads with no transport interaction at all.
//   - The reloader waits out a fixed sleep, never a handshake with the
//     request goroutine. Any reader-to-reloader channel would hand the
//     reader's clock to the reloader and order the very accesses under
//     test. The sleep only needs the request to have STARTED - its entry
//     config reads happen microseconds after launch - so the wide margin
//     cannot mask the race.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// parkedBody blocks the request goroutine inside readRequest's body read
// until the reloader unblocks it, exercising the mid-request park against
// the publication step.
type parkedBody struct{ release <-chan struct{} }

func (b parkedBody) Read([]byte) (int, error) {
	<-b.release
	return 0, errors.New("client body never finished")
}

// TestReloadPublishesNewConfigObject drives the production wiring end to
// end: liveReloadFixture aliases liveCfg and the proxy's boot snapshot to
// one object exactly like main does (proxy.New(cfg) then liveCfg = cfg),
// one scheduler-free request parks mid-flight holding the boot snapshot,
// and the real reloadConfig runs while it is parked.
func TestReloadPublishesNewConfigObject(t *testing.T) {
	liveReloadFixture(t)
	// One hot-reloadable change: the reload must succeed and reach its
	// publication step while the request is parked.
	if err := os.WriteFile(liveConfigPath, []byte("max_retries: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{}, 1)
	// Idempotent unblock so every exit path (including t.Fatal) frees the
	// parked request goroutine.
	unblock := func() {
		select {
		case release <- struct{}{}:
		default:
		}
	}
	defer unblock()

	done := make(chan *httptest.ResponseRecorder, 1)
	// The base URL points at a loopback address that is never dialed: the
	// request parks at its own body read long before any upstream send.
	req, _ := http.NewRequest(http.MethodPost, "http://proxy.invalid/v1/chat/completions", parkedBody{release: release})
	req.Header.Set("X-Proxy-Base-URL", "http://127.0.0.1:1")
	req.Header.Set("Authorization", "Bearer sk-test")
	go func() {
		w := httptest.NewRecorder()
		liveProxy.ServeHTTP(w, req)
		done <- w
	}()

	// No handshake with the parked goroutine (see the file comment): let it
	// reach its entry config reads and park, then publish past them.
	time.Sleep(300 * time.Millisecond)
	if _, err := reloadConfig(); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case w := <-done:
		if w.Code != http.StatusBadRequest {
			t.Fatalf("parked request = %d %s, want the 400 body-read failure", w.Code, w.Body.Bytes())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("parked request did not finish after unblock")
	}
}
