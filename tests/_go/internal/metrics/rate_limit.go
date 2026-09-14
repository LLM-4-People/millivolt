package metrics

import "testing"

// TestRateLimitAffectedRequest pins the affected-request contract over the
// shared isErrorCorpus truth table (also feeding the buffer suite's error
// contract): a request counts once when its final status or any absorbed
// attempt is a 429, and never from RateLimited alone (queue waits and 503
// pacing set that flag without a 429).
func TestRateLimitAffectedRequest(t *testing.T) {
	for _, tc := range isErrorCorpus {
		if got := tc.rec.HasRateLimit(); got != tc.wantLimited {
			t.Errorf("%s: HasRateLimit = %v, want %v", tc.name, got, tc.wantLimited)
		}
	}
}
