package proxy

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// throttleRow finds one provider's row in the GET /admin/throttle document -
// the same shape the dashboard bootstrap consumes.
func throttleRow(t *testing.T, p *Server, provider string) map[string]any {
	t.Helper()
	list, _ := getThrottleState(t, p)["throttles"].([]any)
	for _, raw := range list {
		row, ok := raw.(map[string]any)
		if ok && row["provider"] == provider {
			return row
		}
	}
	t.Fatalf("throttle row for %s missing", provider)
	return nil
}

// TestThrottleAntiBurstRoundTripAndPartialMerge pins the pointer-merge
// contract on the operator API: an omitted field preserves the current value,
// explicit false / empty / 0s turns off only that field, and a patch that
// never names the ramp leaves the stored pair intact.
func TestThrottleAntiBurstRoundTripAndPartialMerge(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})

	if rr := postThrottle(t, p, `{"provider":"alpha.example","concurrency":4,"anti_burst":true,"ramp_duration":"10s"}`); rr.Code != http.StatusOK {
		t.Fatalf("set ramp: %d %s", rr.Code, rr.Body.String())
	}
	got := schedThrottleFor(p.scheduler, "alpha.example").Limit
	if !got.AntiBurst || got.RampDuration != 10*time.Second {
		t.Fatalf("stored ramp = %+v", got)
	}
	row := throttleRow(t, p, "alpha.example")
	if row["anti_burst"] != true || row["ramp_duration"] != "10s" {
		t.Fatalf("GET row = %+v", row)
	}

	// A rate patch that never names the ramp preserves both fields.
	if rr := postThrottle(t, p, `{"provider":"alpha.example","requests":30,"request_window":"1m"}`); rr.Code != http.StatusOK {
		t.Fatalf("rate patch: %d %s", rr.Code, rr.Body.String())
	}
	got = schedThrottleFor(p.scheduler, "alpha.example").Limit
	if !got.AntiBurst || got.RampDuration != 10*time.Second || got.Requests != 30 {
		t.Fatalf("rate patch reset the ramp: %+v", got)
	}

	// Explicit false turns only the flag off.
	if rr := postThrottle(t, p, `{"provider":"alpha.example","anti_burst":false}`); rr.Code != http.StatusOK {
		t.Fatalf("flag off: %d %s", rr.Code, rr.Body.String())
	}
	got = schedThrottleFor(p.scheduler, "alpha.example").Limit
	if got.AntiBurst || got.RampDuration != 10*time.Second {
		t.Fatalf("flag off disturbed the duration: %+v", got)
	}

	// Both documented zero spellings disable only the duration.
	for _, off := range []string{
		`{"provider":"alpha.example","ramp_duration":"0s"}`,
		`{"provider":"alpha.example","ramp_duration":""}`,
	} {
		if rr := postThrottle(t, p, `{"provider":"alpha.example","concurrency":4,"anti_burst":true,"ramp_duration":"10s"}`); rr.Code != http.StatusOK {
			t.Fatalf("reset ramp: %d %s", rr.Code, rr.Body.String())
		}
		if rr := postThrottle(t, p, off); rr.Code != http.StatusOK {
			t.Fatalf("duration off %s: %d %s", off, rr.Code, rr.Body.String())
		}
		got = schedThrottleFor(p.scheduler, "alpha.example").Limit
		if !got.AntiBurst || got.RampDuration != 0 || got.Concurrency != 4 {
			t.Fatalf("duration off %s = %+v", off, got)
		}
		if row := throttleRow(t, p, "alpha.example"); row["ramp_duration"] != "" {
			t.Fatalf("GET ramp after %s = %v", off, row["ramp_duration"])
		}
	}
}

// TestThrottleHeaderPreservesAntiBurstPolicy pins that the header merge closure
// keeps the anti-burst fields: only the three cap dimensions are patched, so a
// header-written policy must retain the operator's ramp in memory and in the
// persisted document.
func TestThrottleHeaderPreservesAntiBurstPolicy(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	deferThrottleWriter(p)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

	if rr := postThrottle(t, p, `{"provider":"alpha.example","concurrency":4,"anti_burst":true,"ramp_duration":"10s"}`); rr.Code != http.StatusOK {
		t.Fatalf("seed ramp: %d %s", rr.Code, rr.Body.String())
	}
	applyThrottleHeader(t, p, "alpha.example", hdrLimitConcurrency, "6")

	got := schedThrottleFor(p.scheduler, "alpha.example")
	if got.Limit.Concurrency != 6 || !got.Limit.AntiBurst || got.Limit.RampDuration != 10*time.Second {
		t.Fatalf("header merge dropped the ramp: %+v", got.Limit)
	}
	if got.Source != scheduler.ThrottleSourceHeader {
		t.Fatalf("source = %s, want header", got.Source)
	}
	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := store.LoadThrottle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	items := decodeThrottles(raw)
	if len(items) != 1 {
		t.Fatalf("persisted document = %s", raw)
	}
	restored, err := persistedToThrottle(items[0])
	if err != nil {
		t.Fatalf("persisted ramp did not round trip: %v (%s)", err, raw)
	}
	if restored.Limit.Concurrency != 6 || !restored.Limit.AntiBurst || restored.Limit.RampDuration != 10*time.Second {
		t.Fatalf("persisted header policy = %+v", restored.Limit)
	}
}

// TestThrottleRampDurationGrammar drives every documented spelling through the
// operator API. The accepted set is the shared 1s..24h window grammar plus the
// three off spellings; a rejected spelling must change neither the live policy
// nor the durable document.
func TestThrottleRampDurationGrammar(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want time.Duration
		ok   bool
	}{
		{"empty", "", 0, true},
		{"zero", "0", 0, true},
		{"zero-seconds", "0s", 0, true},
		{"floor", "1s", time.Second, true},
		{"ten-seconds", "10s", 10 * time.Second, true},
		{"ceiling", "24h", 24 * time.Hour, true},
		{"day-unit", "1d", 24 * time.Hour, true},
		{"sub-second", "500ms", 0, false},
		{"over-double-day", "48h", 0, false},
		{"over-ceiling", "25h", 0, false},
		{"negative", "-1s", 0, false},
		{"garbage", "soon", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(config.Default(), metrics.Noop{})
			store := &countingThrottlePersist{}
			p.attachThrottlePersist(store)
			deferThrottleWriter(p)
			t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

			if rr := postThrottle(t, p, `{"provider":"alpha.example","concurrency":4}`); rr.Code != http.StatusOK {
				t.Fatalf("seed: %d %s", rr.Code, rr.Body.String())
			}
			before := schedThrottleFor(p.scheduler, "alpha.example")
			saves := store.saveCount()
			rr := postThrottle(t, p, `{"provider":"alpha.example","ramp_duration":`+strconv.Quote(tc.raw)+`}`)
			if !tc.ok {
				if rr.Code != http.StatusBadRequest {
					t.Fatalf("ramp %q = %d %s, want 400", tc.raw, rr.Code, rr.Body.String())
				}
				if after := schedThrottleFor(p.scheduler, "alpha.example"); after != before || store.saveCount() != saves {
					t.Fatalf("rejected ramp %q changed the policy: %+v (saves %d -> %d)", tc.raw, after, saves, store.saveCount())
				}
				return
			}
			if rr.Code != http.StatusOK {
				t.Fatalf("ramp %q = %d %s, want 200", tc.raw, rr.Code, rr.Body.String())
			}
			after := schedThrottleFor(p.scheduler, "alpha.example").Limit
			if after.RampDuration != tc.want || after.Concurrency != 4 || after.AntiBurst {
				t.Fatalf("ramp %q = %+v, want duration %s", tc.raw, after, tc.want)
			}
			if row := throttleRow(t, p, "alpha.example"); row["ramp_duration"] != durationOrEmpty(tc.want) {
				t.Fatalf("GET ramp for %q = %v, want %q", tc.raw, row["ramp_duration"], durationOrEmpty(tc.want))
			}
		})
	}
}

// TestValidateLimitRampDuration pins the range check at the validator, the
// single owner the POST and restore paths share. An inert combination (a
// one-slot target, or both fields off) is preconfiguration, not an error.
func TestValidateLimitRampDuration(t *testing.T) {
	for _, tc := range []struct {
		name string
		ramp time.Duration
		ok   bool
	}{
		{"off", 0, true},
		{"floor", time.Second, true},
		{"ten-seconds", 10 * time.Second, true},
		{"ceiling", 24 * time.Hour, true},
		{"negative", -time.Second, false},
		{"sub-second", 500 * time.Millisecond, false},
		{"over-ceiling", 24*time.Hour + time.Nanosecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLimit(scheduler.Limit{Concurrency: 4, RampDuration: tc.ramp})
			if tc.ok && err != nil {
				t.Fatalf("ramp %s rejected: %v", tc.ramp, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ramp %s accepted", tc.ramp)
			}
		})
	}
	// The scheduler defines these as inert, so the validator must not reject
	// an operator who preconfigures them before raising the cap.
	if err := validateLimit(scheduler.Limit{Concurrency: 1, AntiBurst: true, RampDuration: 10 * time.Second}); err != nil {
		t.Fatalf("inert ramp rejected: %v", err)
	}
	if err := validateLimit(scheduler.Limit{AntiBurst: true}); err != nil {
		t.Fatalf("inert anti-burst flag rejected: %v", err)
	}
}

// TestThrottleAntiBurstDefaultsOff pins that the new fields stay off unless
// named, are absent from GET, and are omitted from the persisted document, so
// a policy that never opted in keeps its pre-feature bytes.
func TestThrottleAntiBurstDefaultsOff(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	deferThrottleWriter(p)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

	if rr := postThrottle(t, p, `{"provider":"alpha.example","concurrency":4}`); rr.Code != http.StatusOK {
		t.Fatalf("set: %d %s", rr.Code, rr.Body.String())
	}
	got := schedThrottleFor(p.scheduler, "alpha.example").Limit
	if got.AntiBurst || got.RampDuration != 0 {
		t.Fatalf("default ramp = %+v", got)
	}
	row := throttleRow(t, p, "alpha.example")
	if row["anti_burst"] != false || row["ramp_duration"] != "" {
		t.Fatalf("GET default ramp = %+v", row)
	}
	if err := p.FlushThrottles(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := store.LoadThrottle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("anti_burst")) || bytes.Contains(raw, []byte("ramp_duration")) {
		t.Fatalf("off ramp fields leaked into the document: %s", raw)
	}
}

// TestThrottleAntiBurstRequiresActiveLimit pins the guard that keeps a 200
// from promising a policy the scheduler drops: an explicit on-ramp needs a live
// cap, while the off spellings may travel with the clear of the last cap.
func TestThrottleAntiBurstRequiresActiveLimit(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	store := &countingThrottlePersist{}
	p.attachThrottlePersist(store)
	deferThrottleWriter(p)
	t.Cleanup(func() { _ = p.CloseThrottlePersist(context.Background()) })

	for _, body := range []string{
		`{"provider":"new.example","anti_burst":true}`,
		`{"provider":"new.example","ramp_duration":"10s"}`,
		`{"provider":"new.example","anti_burst":false,"ramp_duration":"10s"}`,
	} {
		if rr := postThrottle(t, p, body); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s, want 400", body, rr.Code, rr.Body.String())
		}
		if schedThrottleFor(p.scheduler, "new.example").Limit.Active() {
			t.Fatalf("%s stored an inactive policy", body)
		}
	}
	if saves := store.saveCount(); saves != 0 {
		t.Fatalf("rejected ramp attempts persisted %d documents", saves)
	}

	// The off spellings stay allowed so the last cap can be cleared with them.
	for _, body := range []string{
		`{"provider":"new.example","anti_burst":false}`,
		`{"provider":"new.example","ramp_duration":"0s"}`,
		`{"provider":"new.example","ramp_duration":""}`,
	} {
		if rr := postThrottle(t, p, body); rr.Code != http.StatusOK {
			t.Fatalf("%s = %d %s, want 200", body, rr.Code, rr.Body.String())
		}
	}
	if schedThrottleFor(p.scheduler, "new.example").Limit.Active() {
		t.Fatal("off-spelling patch stored a policy")
	}

	if rr := postThrottle(t, p, `{"provider":"new.example","concurrency":2,"anti_burst":true}`); rr.Code != http.StatusOK {
		t.Fatalf("active ramp: %d %s", rr.Code, rr.Body.String())
	}
	got := schedThrottleFor(p.scheduler, "new.example").Limit
	if !got.AntiBurst || got.Concurrency != 2 {
		t.Fatalf("active ramp = %+v", got)
	}
}

// TestThrottleAntiBurstStrictDecode pins the trust boundary: an unknown name,
// a wrong type, and the case alias of the new field are all rejected before
// any policy changes.
func TestThrottleAntiBurstStrictDecode(t *testing.T) {
	p := New(config.Default(), metrics.Noop{})
	for _, body := range []string{
		`{"provider":"alpha.example","concurrency":4,"anti_bursts":true}`,
		`{"provider":"alpha.example","concurrency":4,"ramp_duration":10}`,
		`{"provider":"alpha.example","concurrency":4,"anti_burst":"yes"}`,
		`{"provider":"alpha.example","concurrency":4,"anti_burst":true,"AntiBurst":false}`,
	} {
		if rr := postThrottle(t, p, body); rr.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s, want 400", body, rr.Code, rr.Body.String())
		}
	}
	if schedThrottleFor(p.scheduler, "alpha.example").Limit.Active() {
		t.Fatal("rejected document changed the policy")
	}
}
