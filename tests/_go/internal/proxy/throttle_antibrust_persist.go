package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// TestThrottleAntiBurstPersistsAndRestores drives a real policy through the
// shared persist/restore choreography and asserts both ramp fields survive the
// durable document, not just the live scheduler.
func TestThrottleAntiBurstPersistsAndRestores(t *testing.T) {
	p2 := persistRestoreProxy(t, "throttle.db", func(p *Server) *httptest.ResponseRecorder {
		return postThrottle(t, p, `{"provider":"alpha.example","concurrency":4,"anti_burst":true,"ramp_duration":"10s"}`)
	})
	got := schedThrottleFor(p2.scheduler, "alpha.example").Limit
	if !got.AntiBurst || got.RampDuration != 10*time.Second || got.Concurrency != 4 {
		t.Fatalf("restored ramp = %+v", got)
	}
}

// TestThrottleAntiBurstRestoreDocument covers the documents a real store can
// already hold: a pre-feature entry without the fields, the explicit zero
// spelling, and an invalid duration that must be skipped and logged without
// dropping its valid neighbours.
func TestThrottleAntiBurstRestoreDocument(t *testing.T) {
	for _, tc := range []struct {
		name     string
		document string
		invalid  bool
	}{
		{
			name:     "legacy",
			document: `{"throttles":[{"provider":"legacy.example","concurrency":4}]}`,
		},
		{
			name:     "zero-spelling",
			document: `{"throttles":[{"provider":"legacy.example","concurrency":4,"ramp_duration":"0s"}]}`,
		},
		{
			name:     "invalid-skipped",
			document: `{"throttles":[{"provider":"legacy.example","concurrency":4},{"provider":"bad.example","concurrency":4,"anti_burst":true,"ramp_duration":"500ms"},{"provider":"wide.example","concurrency":4,"anti_burst":true,"ramp_duration":"10s"}]}`,
			invalid:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "throttle.db")
			store, _ := openProxyTestStore(t, path)
			t.Cleanup(func() { _ = store.Close() })
			if err := store.SaveThrottle(context.Background(), []byte(tc.document)); err != nil {
				t.Fatal(err)
			}

			var logged bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logged)
			p := New(config.Default(), metrics.Noop{})
			p.AttachPausePersist(store)
			log.SetOutput(previous)

			got := schedThrottleFor(p.scheduler, "legacy.example").Limit
			if got.AntiBurst || got.RampDuration != 0 || got.Concurrency != 4 {
				t.Fatalf("restored legacy policy = %+v", got)
			}
			if !tc.invalid {
				return
			}
			if text := logged.String(); !strings.Contains(text, "skip bad.example") {
				t.Fatalf("invalid stored ramp was not logged as skipped: %q", text)
			}
			if schedThrottleFor(p.scheduler, "bad.example").Limit.Active() {
				t.Fatal("invalid stored ramp was restored")
			}
			wide := schedThrottleFor(p.scheduler, "wide.example").Limit
			if !wide.AntiBurst || wide.RampDuration != 10*time.Second {
				t.Fatalf("valid neighbour lost its ramp: %+v", wide)
			}
		})
	}
}

// TestPersistedThrottleRampOffKeepsLegacyBytes pins the compatibility contract:
// with both ramp fields off the document is byte-identical to the pre-feature
// shape, so no stored document gains a key it never opted into.
func TestPersistedThrottleRampOffKeepsLegacyBytes(t *testing.T) {
	th := scheduler.Throttle{
		Provider: "alpha.example",
		Limit: scheduler.Limit{
			Concurrency: 4,
			Requests:    30,
			ReqWindow:   time.Minute,
		},
		Source:    scheduler.ThrottleSourceUI,
		UpdatedBy: "dashboard",
		UpdatedAt: time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC),
	}
	raw, err := json.Marshal(throttleToPersisted(th))
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"provider":"alpha.example","concurrency":4,"requests":30,"request_window":"1m","source":"ui","updated_by":"dashboard","updated_at":"2026-01-02T03:04:05Z"}`
	if string(raw) != want {
		t.Fatalf("off-ramp document changed\n got: %s\nwant: %s", raw, want)
	}
}

// TestThrottlePersistDocumentBoundWithRampFields pins the 8 MiB safety net with
// every per-entry field at its own maximum, including the anti-burst pair at
// its longest persisted spelling. It also pins the pair's own growth so the
// bound comment's per-entry allowance cannot drift silently.
func TestThrottlePersistDocumentBoundWithRampFields(t *testing.T) {
	const gates = 4096 // scheduler.maxProviderGates
	provider := strings.Repeat("a", throttleProviderLabelMaxBytes)
	client := strings.Repeat("b", metrics.RetainedClientMax)
	at := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	// The longest formatting of any duration inside the shared band.
	worstRamp := maxLimitWindow - time.Nanosecond

	build := func(ramp bool) []byte {
		items := make([]persistedThrottle, 0, gates)
		for i := 0; i < gates; i++ {
			limit := scheduler.Limit{
				Concurrency: maxLimitConcurrency,
				Requests:    maxLimitRequests,
				ReqWindow:   maxLimitWindow,
				Tokens:      maxLimitTokens,
				TokWindow:   maxLimitWindow,
			}
			if ramp {
				limit.AntiBurst = true
				limit.RampDuration = worstRamp
			}
			items = append(items, throttleToPersisted(scheduler.Throttle{
				Provider:  provider,
				Limit:     limit,
				Source:    scheduler.ThrottleSourceHeader,
				UpdatedBy: client,
				UpdatedAt: at,
			}))
		}
		raw, err := json.Marshal(persistedThrottleDoc{Throttles: items})
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}

	on := build(true)
	if len(on) >= throttlePersistDocMaxBytes {
		t.Fatalf("maximal document with ramp fields = %d bytes, cap %d", len(on), throttlePersistDocMaxBytes)
	}
	if grow := len(on) - len(build(false)); grow > gates*56 {
		t.Fatalf("anti-burst pair grew the document by %d bytes, allowance %d", grow, gates*56)
	}
}
