package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// TestPauseQueuesNewRequestsUntilUnpause is the end-to-end operator-pause
// contract: an in-flight upstream call finishes, a request that arrives
// while paused never hits upstream until resume.
func TestPauseDurationKeysMatchFormatDuration(t *testing.T) {
	for k, d := range pauseDurations {
		if k == "" {
			if d != 0 {
				t.Errorf("empty pause duration = %s, want 0", d)
			}
			continue
		}
		if got := config.FormatDuration(d); got != k {
			t.Errorf("pauseDurations[%q] FormatDuration = %q", k, got)
		}
	}
	err := pauseDurationError()
	if err == nil || !strings.Contains(err.Error(), "15m") || !strings.Contains(err.Error(), "24h") {
		t.Fatalf("pauseDurationError = %v", err)
	}
}

func TestPauseTimerUnpauses(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.applyHolds([]persistedPause{{All: true, Until: time.Now().Add(50 * time.Millisecond)}})
	if !p.scheduler.Paused() {
		t.Fatal("expected hold")
	}
	waitFor(t, func() bool { return !p.scheduler.Paused() })
	if p.scheduler.Paused() {
		t.Fatal("timer did not clear the hold")
	}
}

func TestPausePersistsAndRestores(t *testing.T) {
	p2 := persistRestoreProxy(t, "pause.db", func(p *Server) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		p.HandlePause(rr, httptest.NewRequest(http.MethodPost, "/admin/pause",
			strings.NewReader(`{"paused":true,"clients":["client-a"],"duration":"1h"}`)))
		return rr
	})
	if !p2.scheduler.Holds("client-a") {
		t.Fatal("restored scheduler did not hold the client")
	}
	if p2.scheduler.Holds("other") {
		t.Fatal("restored hold leaked to other clients")
	}
	if len(p2.pause.holds) != 1 || p2.pause.holds[0].Duration != "1h" {
		t.Fatalf("restored holds = %+v, want duration 1h", p2.pause.holds)
	}
}

func TestOneHoldTimerLeavesTheOther(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.applyHolds([]persistedPause{
		{Clients: []string{"short"}, Until: time.Now().Add(50 * time.Millisecond)},
		{Clients: []string{"long"}, Duration: "1h", Until: time.Now().Add(time.Hour)},
	})
	waitFor(t, func() bool { return !p.scheduler.Holds("short") })
	if p.scheduler.Holds("short") {
		t.Fatal("short hold timer did not fire")
	}
	if !p.scheduler.Holds("long") {
		t.Fatal("long hold was cleared with the short timer")
	}
}

func TestSameUntilTimersClearBoth(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	until := time.Now().Add(50 * time.Millisecond)
	p.applyHolds([]persistedPause{
		{Clients: []string{"a"}, Until: until},
		{Clients: []string{"b"}, Until: until},
	})
	waitFor(t, func() bool { return !p.scheduler.Paused() })
	if p.scheduler.Paused() || p.scheduler.Holds("a") || p.scheduler.Holds("b") {
		t.Fatal("same-Until timers did not clear both holds")
	}
}

func TestGetOmitsExpiredHold(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	p.scheduler.RestoreHolds([]scheduler.Hold{{ID: "live", Clients: []string{"y"}}})
	p.pause.mu.Lock()
	p.pause.holds = []persistedPause{
		{ID: "dead", Clients: []string{"x"}, Until: time.Now().Add(-time.Second)},
		{ID: "live", Clients: []string{"y"}},
	}
	p.pause.mu.Unlock()
	rr := httptest.NewRecorder()
	p.HandlePause(rr, httptest.NewRequest(http.MethodGet, "/admin/pause", nil))
	st := pauseJSON(t, rr)
	holds, _ := st["holds"].([]any)
	if len(holds) != 1 {
		t.Fatalf("holds = %v, want only the live one", st["holds"])
	}
	h := holds[0].(map[string]any)
	if h["id"] != "live" {
		t.Fatalf("hold id = %v", h["id"])
	}
}
