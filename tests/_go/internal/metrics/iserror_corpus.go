package metrics

// isErrorCorpus is the ONE truth table for Record outcome semantics. IsError
// (a genuine upstream failure, final or absorbed by a retry) and HasRateLimit
// (a final or retried HTTP 429, counted once per request) are two predicates
// over the same request outcomes, so one corpus feeds both the buffer suite's
// error-contract test and the rate-limit suite's affected-request test.
// The contract it pins: 429 is flow control and 499 is the LOCAL client's own
// cancellation, so neither counts as an error on its own; a genuine failure
// counts whether it is the final outcome OR an absorbed retry attempt the
// client never saw; a rate limit reaches the metric from the final status or
// any attempt, never from RateLimited alone (queue waits and 503 pacing set
// that flag too).
type isErrorRow struct {
	name        string
	rec         Record
	wantErr     bool // Record.IsError
	wantLimited bool // Record.HasRateLimit
}

var isErrorCorpus = []isErrorRow{
	{"final 200, no attempts", Record{StatusCode: 200}, false, false},
	{"final 429 rate limit", Record{StatusCode: 429, ErrorType: "rate_limit_error", RateLimited: true}, false, true},
	{"final 499 client closed", Record{StatusCode: StatusClientClosedRequest, ClientDisconnected: true}, false, false},
	{"final 499 after an absorbed 5xx", Record{StatusCode: StatusClientClosedRequest, ClientDisconnected: true, Attempts: []RetryAttempt{{StatusCode: 503}}}, true, false},
	{"final 500", Record{StatusCode: 500}, true, false},
	{"final 502", Record{StatusCode: 502}, true, false},
	{"final 400 (4xx other than 429)", Record{StatusCode: 400}, true, false},
	{"structured error, status 0", Record{StatusCode: 0, ErrorType: "upstream_unreachable"}, true, false},
	{"recovered after absorbed 5xx", Record{StatusCode: 200, Attempts: []RetryAttempt{{StatusCode: 502}}}, true, false},
	{"recovered after absorbed 429 only", Record{StatusCode: 200, RateLimited: true, Attempts: []RetryAttempt{{StatusCode: 429, ErrorType: "rate_limit"}}}, false, true},
	{"final 429 after an absorbed 5xx", Record{StatusCode: 429, RateLimited: true, Attempts: []RetryAttempt{{StatusCode: 503}}}, true, true},
	{"queue wait is not429", Record{StatusCode: 200, QueueWaitMs: 10, RateLimited: true}, false, false},
	{"typed final429 without broad flag", Record{StatusCode: 429, ErrorType: "insufficient_quota"}, false, true},
	{"recovered429 twice", Record{StatusCode: 200, Attempts: []RetryAttempt{{StatusCode: 429}, {StatusCode: 429}}}, false, true},
	{"final429 plus retries", Record{StatusCode: 429, Attempts: []RetryAttempt{{StatusCode: 429}, {StatusCode: 429}}}, false, true},
	{"recovered503", Record{StatusCode: 200, RateLimited: true, Attempts: []RetryAttempt{{StatusCode: 503}}}, true, false},
	{"final429 after503", Record{StatusCode: 429, Attempts: []RetryAttempt{{StatusCode: 503}}}, true, true},
	{"final500 after429", Record{StatusCode: 500, Attempts: []RetryAttempt{{StatusCode: 429}}}, true, true},
	{"pending after429", Record{Attempts: []RetryAttempt{{StatusCode: 429}}}, false, true},
	{"cancel after429", Record{StatusCode: 499, Attempts: []RetryAttempt{{StatusCode: 429}}}, false, true},
}
