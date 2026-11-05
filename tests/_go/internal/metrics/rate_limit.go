package metrics

import "testing"

// TestRateLimitAffectedRequest pins the affected-request contract over the
// shared isErrorCorpus truth table (also feeding the buffer suite's error
// contract): a request counts once when its final response or any absorbed or
// in-band attempt carries a rate-limit class - a 429 or a recognized
// rate-limit type/code - and never from RateLimited alone (queue waits and
// 503 pacing set that flag without a rate limit).
func TestRateLimitAffectedRequest(t *testing.T) {
	for _, tc := range isErrorCorpus {
		if got := tc.rec.HasRateLimit(); got != tc.wantLimited {
			t.Errorf("%s: HasRateLimit = %v, want %v", tc.name, got, tc.wantLimited)
		}
	}
}
