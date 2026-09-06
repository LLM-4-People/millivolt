package metrics

import "testing"

func TestRateLimitAffectedRequest(t *testing.T) {
	for _, test := range []struct {
		name             string
		record           Record
		limited, failure bool
	}{
		{"clean", Record{StatusCode: 200}, false, false},
		{"queue wait is not429", Record{StatusCode: 200, QueueWaitMs: 10, RateLimited: true}, false, false},
		{"typed final429 without broad flag", Record{StatusCode: 429, ErrorType: "insufficient_quota"}, true, false},
		{"recovered429 twice", Record{StatusCode: 200, Attempts: []RetryAttempt{{StatusCode: 429}, {StatusCode: 429}}}, true, false},
		{"final429 plus retries", Record{StatusCode: 429, Attempts: []RetryAttempt{{StatusCode: 429}, {StatusCode: 429}}}, true, false},
		{"recovered503", Record{StatusCode: 200, RateLimited: true, Attempts: []RetryAttempt{{StatusCode: 503}}}, false, true},
		{"final429 after503", Record{StatusCode: 429, Attempts: []RetryAttempt{{StatusCode: 503}}}, true, true},
		{"final500 after429", Record{StatusCode: 500, Attempts: []RetryAttempt{{StatusCode: 429}}}, true, true},
		{"pending after429", Record{Attempts: []RetryAttempt{{StatusCode: 429}}}, true, false},
		{"cancel after429", Record{StatusCode: 499, Attempts: []RetryAttempt{{StatusCode: 429}}}, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := test.record.HasRateLimit(); got != test.limited {
				t.Fatalf("HasRateLimit = %v, want %v", got, test.limited)
			}
			if got := test.record.IsError(); got != test.failure {
				t.Fatalf("IsError = %v, want %v", got, test.failure)
			}
		})
	}
}
