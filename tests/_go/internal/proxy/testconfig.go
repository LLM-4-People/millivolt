package proxy

import (
	"testing"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
)

// testConfig is config.Default() with every retry ladder bounded to
// single-digit milliseconds. Use it in tests whose subject is retry
// accounting, attempt ordering or client-visible surfacing: the test must
// observe that a ladder ran, never how long production pacing takes.
// config.Default() starts at 1s and doubles to a 2min cap, so an accounting
// test using it spends up to half a minute waiting for nothing it asserts.
//
// Pacing is the behavior under test in a small set that must keep its real
// durations instead of this helper:
//
//   - TestExhaustedRetriesPaceNextFirstSend,
//     TestConsecutiveExhausted429sDoubleBackoff, TestConsecutiveFailuresDoubleBackoff,
//     TestRecoveredRequestResetsBackoff and TestRetryAfterNotClampedToMaxBackoff
//     (scheduler.go) distinguish armed 420ms-2.5s pacing bands.
//   - TestShortRetryAfterDoesNotResetAttemptBackoff and
//     TestRetryTTFTIsFinalAttempt (scheduler.go) distinguish hundreds of
//     milliseconds of ladder from a reset or an attempt-anchored TTFT.
//   - The quota pacing tests (TestQuota429EndsRetryLoop,
//     TestQuota429OnLastAttemptDoesNotPace, TestQuota429DoesNotPaceSibling)
//     keep their explicit 300ms-1s bases so a broken group pace would exceed
//     their 200ms follow-up bound.
//   - The storm cursor pacing tests (storm_cursor.go) observe the storm
//     ladder and keep their own 150ms-1s fixtures.
//   - TestCursorFiredSendTimeoutIsNotPaced (pause.go) keeps a one-second
//     base so any FailSend retry would be unmistakable.
func testConfig() *config.Config {
	c := config.Default()
	c.BaseBackoff = 5 * time.Millisecond
	c.MaxBackoff = 10 * time.Millisecond
	c.StormInitialBackoff = 5 * time.Millisecond
	c.StormMaxBackoff = 10 * time.Millisecond
	return c
}

func TestTestConfigBoundsEveryLadder(t *testing.T) {
	c := testConfig()
	for name, value := range map[string]time.Duration{
		"BaseBackoff":         c.BaseBackoff,
		"MaxBackoff":          c.MaxBackoff,
		"StormInitialBackoff": c.StormInitialBackoff,
		"StormMaxBackoff":     c.StormMaxBackoff,
	} {
		if value <= 0 || value > 10*time.Millisecond {
			t.Errorf("%s = %v, want a real ladder bounded at 10ms", name, value)
		}
	}
	if c.BaseBackoff > c.MaxBackoff || c.StormInitialBackoff > c.StormMaxBackoff {
		t.Errorf("testConfig base exceeds its cap: %+v", c)
	}
}
