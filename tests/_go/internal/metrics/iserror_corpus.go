package metrics

// isErrorCorpus is the ONE truth table for Record outcome semantics. IsError
// (a genuine upstream failure, final or absorbed by a retry) and HasRateLimit
// (a rate limit the request hit, final or retried, counted once per request)
// are two predicates over the same request outcomes, so one corpus feeds both
// the buffer suite's error-contract test and the rate-limit suite's
// affected-request test.
// The contract it pins: the two ask different questions. IsError asks "did
// this request fail": a final 429 is the flow-control handshake the client can
// retry (not a failure), a plain 499 is the LOCAL client's own cancellation
// (not a failure), and a recorded structured error on any other status IS a
// failure - including a rate-limit class carried by a 400 or by a committed
// 200 whose keepalive hid the upstream status. HasRateLimit asks "did it hit a
// rate limit", which is the CLASS, not the status: any rate-limit spelling on
// the final outcome or in any attempt counts, and a durable quota 429 stays a
// rate-limit event (its class is quota, and quota_pause_mode owns the
// reaction). A genuine failure counts whether it is the final outcome OR an
// absorbed retry attempt the client never saw; a rate limit never comes from
// RateLimited alone (queue waits and 503 pacing set that flag too).
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
	{"recovered after absorbed 500 (the >= 500 boundary)", Record{StatusCode: 200, Attempts: []RetryAttempt{{StatusCode: 500}}}, true, false},
	{"recovered after absorbed 429 only", Record{StatusCode: 200, RateLimited: true, Attempts: []RetryAttempt{{StatusCode: 429, ErrorType: "rate_limit"}}}, false, true},
	{"final 429 after an absorbed 5xx", Record{StatusCode: 429, RateLimited: true, Attempts: []RetryAttempt{{StatusCode: 503}}}, true, true},
	{"429 after an absorbed 500 still counts (flow-arm boundary)", Record{StatusCode: 429, RateLimited: true, Attempts: []RetryAttempt{{StatusCode: 500}}}, true, true},
	{"queue wait is not429", Record{StatusCode: 200, QueueWaitMs: 10, RateLimited: true}, false, false},
	{"typed final429 without broad flag", Record{StatusCode: 429, ErrorType: "insufficient_quota"}, false, true},
	{"recovered429 twice", Record{StatusCode: 200, Attempts: []RetryAttempt{{StatusCode: 429}, {StatusCode: 429}}}, false, true},
	{"final429 plus retries", Record{StatusCode: 429, Attempts: []RetryAttempt{{StatusCode: 429}, {StatusCode: 429}}}, false, true},
	{"recovered503", Record{StatusCode: 200, RateLimited: true, Attempts: []RetryAttempt{{StatusCode: 503}}}, true, false},
	{"final429 after503", Record{StatusCode: 429, Attempts: []RetryAttempt{{StatusCode: 503}}}, true, true},
	{"final500 after429", Record{StatusCode: 500, Attempts: []RetryAttempt{{StatusCode: 429}}}, true, true},
	{"pending after429", Record{Attempts: []RetryAttempt{{StatusCode: 429}}}, false, true},
	{"cancel after429", Record{StatusCode: 499, Attempts: []RetryAttempt{{StatusCode: 429}}}, false, true},
	{"final 499 with in-band error", Record{StatusCode: StatusClientClosedRequest, ClientDisconnected: true, ErrorType: "api_error", ErrorCode: "internal_error"}, true, false},
	// A rate limit the client aborted around: the class is rate_limit (the
	// request hit a provider limit), and because the client was mid-flight
	// when the upstream failed it is also a recorded failure - the health
	// surface counts the structured error, exactly like its 200 twin.
	{"final 499 with in-band rate limit error", Record{StatusCode: StatusClientClosedRequest, ClientDisconnected: true, ErrorType: "rate_limit_error"}, true, true},
	// The 499 arm reads the FINAL outcome's own type/code, never the attempt
	// list: a structured error was still observed on the wire
	// (the upstream failed before the client left), so it counts as an error
	// even though an absorbed 429 also keeps the request in the rate-limit
	// series. An earlier version of the JS mirror consulted the attempt list
	// here and got this row wrong.
	{"final 499 with in-band error after an absorbed 429", Record{StatusCode: StatusClientClosedRequest, ClientDisconnected: true, ErrorType: "api_error", ErrorCode: "internal_error", Attempts: []RetryAttempt{{StatusCode: 429}}}, true, true},
	// Same rule at a committed 200: the client was told "rate limit" on a
	// status line the keepalive had already committed, so the request failed
	// (an error) AND hit a rate limit.
	{"committed 200 with an in-band 429", Record{StatusCode: 200, ErrorType: "rate_limit_error", Attempts: []RetryAttempt{{StatusCode: 429, InBand: true}}}, true, true},
	// A rate limit announced by a code on a non-429 status (a gateway answering
	// `tier_limit_exceeded` with 400) counts in the rate-limit series - the case
	// a status-only predicate got wrong - while the request itself still failed
	// and stays in the error rate. The two questions are deliberately separate.
	{"400 carrying a rate-limit code", Record{StatusCode: 400, ErrorType: "tier_limit_exceeded", ErrorCode: "tier_limit_exceeded"}, true, true},
	{"400 carrying a durable quota code", Record{StatusCode: 400, ErrorType: "insufficient_quota"}, true, false},
	// A client-fault TYPE outranks a transient-looking CODE, so a gateway that
	// wraps a capacity code in invalid_request_error is still the caller's
	// fault, matching the in-band rescue precedence.
	{"400 client-fault type with a transient code", Record{StatusCode: 400, ErrorType: "invalid_request_error", ErrorCode: "internal_error"}, true, false},
	// The proxy creates only in-band 429 entries, but Go, the dashboard and the
	// published SQL must agree on any entry shape, so this synthetic 5xx is
	// locked as an absorbed failure.
	{"synthetic in-band 500 counts as an absorbed failure", Record{StatusCode: 200, Attempts: []RetryAttempt{{StatusCode: 500, InBand: true}}}, true, false},
}
