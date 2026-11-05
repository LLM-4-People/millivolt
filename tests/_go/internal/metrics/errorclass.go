package metrics

import (
	"strings"
	"testing"
)

// TestClassifyDefault pins the canonical classifier's precedence on the
// boundary rows of every class. The classifier is the single answer to "is
// this a failure, is it retryable, is it a rate limit, what does the client
// see", so a precedence change here silently changes the retry ladder, the
// health counts and the client-visible class at once; the rows below name each
// boundary explicitly.
func TestClassifyDefault(t *testing.T) {
	cases := []struct {
		name  string
		facts ErrorFacts
		want  ErrorClass
	}{
		// Clean successes: nothing structured, nothing failed.
		{"clean 200", ErrorFacts{Status: 200}, ClassNone},
		{"clean 204", ErrorFacts{Status: 204}, ClassNone},
		{"redirect without structured error", ErrorFacts{Status: 302}, ClassNone},
		{"status 0 with no evidence", ErrorFacts{}, ClassNone},

		// Rate limits are classified by status OR by the provider's own
		// spelling on ANY status - a gateway answering one with 400 is the
		// case a status-only predicate got wrong.
		{"429 alone", ErrorFacts{Status: 429}, ClassRateLimit},
		{"400 with tier_limit_exceeded type", ErrorFacts{Status: 400, Type: "tier_limit_exceeded"}, ClassRateLimit},
		{"403 with resource_exhausted code", ErrorFacts{Status: 403, Code: "resource_exhausted"}, ClassRateLimit},
		{"200 with rate_limit_error", ErrorFacts{Status: 200, Type: "rate_limit_error"}, ClassRateLimit},

		// Durable account/billing conditions outrank transient readings and
		// are never retryable. The class of a durable 429 is quota even though
		// the health predicate treats the 429 as flow control (pinned
		// separately).
		{"400 with insufficient_quota", ErrorFacts{Status: 400, Type: "insufficient_quota"}, ClassQuota},
		{"429 with insufficient_quota", ErrorFacts{Status: 429, Type: "insufficient_quota"}, ClassQuota},
		{"429 with a billing code", ErrorFacts{Status: 429, Code: "billing_hard_limit_reached"}, ClassQuota},

		// The caller's own fault: the type is authoritative, but a documented
		// capacity code may override the generic wrapper.
		{"400 invalid_request_error", ErrorFacts{Status: 400, Type: "invalid_request_error"}, ClassClientError},
		{"401 authentication_error", ErrorFacts{Status: 401, Type: "authentication_error"}, ClassClientError},
		{"404 without a type", ErrorFacts{Status: 404}, ClassClientError},
		{"400 client-fault type with a transient code", ErrorFacts{Status: 400, Type: "invalid_request_error", Code: "internal_error"}, ClassClientError},
		{"400 invalid_request_error with engine_overloaded", ErrorFacts{Status: 400, Type: "invalid_request_error", Code: "engine_overloaded"}, ClassUnavailable},

		// Transport failures have no response to read.
		{"transport timeout", ErrorFacts{Transport: true, Type: "timeout"}, ClassTimeout},
		{"transport bare", ErrorFacts{Transport: true}, ClassUnreachable},

		// 2xx content failures.
		{"200 with empty_completion", ErrorFacts{Status: 200, Type: CodeEmptyCompletion}, ClassContent},
		{"200 with an unknown structured error", ErrorFacts{Status: 200, Type: "who_knows_this_thing"}, ClassUpstream},

		// Proxy-minted failures.
		{"proxy transform_error", ErrorFacts{Status: 500, Type: TypeTransformError, Local: true}, ClassProxy},
		{"proxy queue_full", ErrorFacts{Status: 503, Type: TypeQueueFull, Local: true}, ClassProxy},
		{"proxy unknown local type", ErrorFacts{Status: 500, Type: "mystery_local", Local: true}, ClassProxy},
		// A queue drain is the proxy asking the client to retry (503 plus
		// Retry-After), so it is a retryable capacity answer, not a proxy
		// fault the client should abandon.
		{"proxy queue_drained", ErrorFacts{Status: 503, Type: TypeQueueDrained, Local: true}, ClassUnavailable},

		// Timeouts before the generic availability class.
		{"500 with timeout_error", ErrorFacts{Status: 500, Type: "timeout_error"}, ClassTimeout},
		{"504 alone", ErrorFacts{Status: 504}, ClassUnavailable},
		{"503 with service_unavailable", ErrorFacts{Status: 503, Type: "service_unavailable"}, ClassUnavailable},
		{"500 alone", ErrorFacts{Status: 500}, ClassUnavailable},
		{"502 with api_error", ErrorFacts{Status: 502, Type: "api_error"}, ClassUnavailable},
		// A non-standard status from a broken or hostile gateway is still a
		// server-side availability failure, never a clean success.
		{"620 from a broken gateway", ErrorFacts{Status: 620}, ClassUnavailable},

		// Cancellation: only a BARE 499 is the client's own abort; a 499
		// carrying a structured error is classified by the envelope.
		{"bare 499", ErrorFacts{Status: 499}, ClassCanceled},
		{"499 with api_error", ErrorFacts{Status: 499, Type: "api_error"}, ClassUnavailable},
		{"499 with an unknown type", ErrorFacts{Status: 499, Type: "some_error"}, ClassUpstream},

		// Unclassifiable failures fail closed, never open. A 200 carrying a
		// structured error is never "transient availability" - the in-band
		// rescue path (internal/proxy) owns that decision, so the HTTP class
		// stays the fail-closed upstream class.
		{"400 with an unknown type", ErrorFacts{Status: 400, Type: "some_error"}, ClassClientError},
		{"418 with an unknown code", ErrorFacts{Status: 418, Code: "teapot"}, ClassClientError},
		{"200 with upstream_error", ErrorFacts{Status: 200, Type: "upstream_error"}, ClassUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.facts); got != tc.want {
				t.Fatalf("Classify(%+v) = %q, want %q", tc.facts, got, tc.want)
			}
		})
	}
}

// TestErrorClassPredicates pins the three predicates every class consumer
// reads: Retryable (the ordinary transient ladder), IsError (the health
// surface) and IsRateLimit (rate-limit accounting). They are deliberately
// different answers over the same vocabulary, so each class is listed with all
// three.
func TestErrorClassPredicates(t *testing.T) {
	cases := []struct {
		class                       ErrorClass
		retryable, isError, isLimit bool
	}{
		{ClassNone, false, false, false},
		{ClassRateLimit, true, false, true},
		{ClassQuota, false, true, false},
		{ClassUnavailable, true, true, false},
		{ClassUnreachable, true, true, false},
		{ClassTimeout, true, true, false},
		{ClassContent, false, true, false},
		{ClassClientError, false, true, false},
		{ClassProxy, false, true, false},
		{ClassCanceled, false, false, false},
		{ClassUpstream, false, true, false},
	}
	for _, tc := range cases {
		if got := tc.class.Retryable(); got != tc.retryable {
			t.Errorf("%q.Retryable() = %v, want %v", tc.class, got, tc.retryable)
		}
		if got := tc.class.IsError(); got != tc.isError {
			t.Errorf("%q.IsError() = %v, want %v", tc.class, got, tc.isError)
		}
		if got := tc.class.IsRateLimit(); got != tc.isLimit {
			t.Errorf("%q.IsRateLimit() = %v, want %v", tc.class, got, tc.isLimit)
		}
	}
}

// TestInBandRetryable pins the in-band rescue composition the streaming and
// non-streaming rescue sites share, including the engine_overloaded override
// of the generic invalid_request_error wrapper and the durable-quota deny.
func TestInBandRetryable(t *testing.T) {
	cases := []struct {
		typ, code string
		want      bool
	}{
		{"server_error", "", true},
		{"API_Error", "internal_error", true},
		{"service_unavailable", "server_is_overloaded", true},
		{"overloaded_error", "", true},
		{"timeout_error", "", true},
		{"engine_overloaded", "", true},
		{"invalid_request_error", "engine_overloaded", true},
		{"invalid_request_error", "internal_error", false},
		{"authentication_error", "server_error", false},
		{"rate_limit_error", "", false},
		{"insufficient_quota", "", false},
		{"model_warming_up", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if got := InBandRetryable(tc.typ, tc.code); got != tc.want {
			t.Errorf("InBandRetryable(%q, %q) = %v, want %v", tc.typ, tc.code, got, tc.want)
		}
	}
}

// TestClassExtensionsAddButNeverMove pins the operator extension mechanism
// end to end: an installed spelling classifies exactly like the class it was
// listed under (on every vocabulary consumer at once), the retryable extension
// overrides the built-in client-fault deny, BuiltinClass reports the built-in
// class so validation can reject a remap, and resetting restores the
// built-ins. The installer is process-wide by design (one owner), so the test
// restores the empty state even on failure.
func TestClassExtensionsAddButNeverMove(t *testing.T) {
	t.Cleanup(func() { InstallClassExtensions(ClassExtensions{}) })

	InstallClassExtensions(ClassExtensions{
		RateLimit: []string{"  Gateway_Throttle ", "gateway_throttle"},
		Quota:     []string{"account_depleted"},
		Retryable: []string{"warming_up"},
	})

	// Rate-limit extension: the class, HasRateLimit and the SQL rate-limit
	// predicate all move together, on any status, while the health predicate
	// keeps counting the failed request (its class says why).
	if got := Classify(ErrorFacts{Status: 400, Type: "gateway_throttle"}); got != ClassRateLimit {
		t.Fatalf("rate-limit extension class = %q, want %q", got, ClassRateLimit)
	}
	if !OutcomeIsError(400, "gateway_throttle", "") {
		t.Fatal("a non-429 rate-limit class is still a failed request")
	}
	if !IsRateLimitClass("gateway_throttle", "") {
		t.Fatal("IsRateLimitClass must honor the extension")
	}
	extended := Record{StatusCode: 400, ErrorType: "gateway_throttle"}
	if !extended.HasRateLimit() {
		t.Fatal("the extension spelling must count in the rate-limit series")
	}
	if !strings.Contains(RateLimitClassListSQL(), "'gateway_throttle'") {
		t.Fatal("the published SQL must carry the extension spelling")
	}

	// Quota extension: durable, never retryable in-band, and an error.
	if got := Classify(ErrorFacts{Status: 400, Type: "account_depleted"}); got != ClassQuota {
		t.Fatalf("quota extension class = %q, want %q", got, ClassQuota)
	}
	if InBandRetryable("account_depleted", "") {
		t.Fatal("a quota extension spelling must never re-send in-band")
	}
	if !OutcomeIsError(400, "account_depleted", "") {
		t.Fatal("a quota extension spelling on a non-429 status is an error")
	}
	if got := NonRetryableQuotaClass("account_depleted", ""); got != "account_depleted" {
		t.Fatalf("NonRetryableQuotaClass = %q, want the matched extension token", got)
	}

	// Retryable extension: authorizes the in-band rescue even under a built-in
	// client-fault wrapper, and the classifier says the same thing.
	if !InBandRetryable("warming_up", "") {
		t.Fatal("the retryable extension must authorize an in-band re-send")
	}
	if got := Classify(ErrorFacts{Status: 400, Type: "warming_up"}); got != ClassUnavailable {
		t.Fatalf("retryable extension class = %q, want %q", got, ClassUnavailable)
	}
	InstallClassExtensions(ClassExtensions{Retryable: []string{"invalid_request_error"}})
	if got := Classify(ErrorFacts{Status: 400, Type: "invalid_request_error"}); got != ClassUnavailable {
		t.Fatalf("an operator-overridden client-fault type class = %q, want %q (the extension outranks the built-in deny)", got, ClassUnavailable)
	}
	if !InBandRetryable("invalid_request_error", "") {
		t.Fatal("the retryable extension must outrank the client-fault deny in-band too")
	}

	// BuiltinClass names the built-in a spelling belongs to, so validation can
	// reject a remap; the extension itself never changes those answers, and
	// the coverage spans every family, not only the three extension lists.
	for spelling, want := range map[string]string{
		"tier_limit_exceeded":   "rate_limit",
		"insufficient_quota":    "quota",
		"server_error":          "retryable",
		"bad_gateway":           "retryable",
		"timeout_error":         "timeout",
		TypeQueueFull:           "proxy",
		TypeUpstreamUnreachable: "proxy",
		CodeEmptyCompletion:     "content",
		TypeInvalidRequest:      "client_error",
		"warming_up":            "",
	} {
		if got := BuiltinClass(spelling); got != want {
			t.Errorf("BuiltinClass(%q) = %q, want %q", spelling, got, want)
		}
	}

	// Resetting restores the built-ins alone.
	InstallClassExtensions(ClassExtensions{})
	if got := Classify(ErrorFacts{Status: 400, Type: "gateway_throttle"}); got != ClassClientError {
		t.Fatalf("after reset, an unknown spelling class = %q, want %q", got, ClassClientError)
	}
}

// TestOutcomeIsErrorIsClassBlind pins the exact relationship between the health
// predicate and the class across the whole corpus: the two answer differently
// only in the two documented families, and any other divergence among these
// rows is a bug.
//
//   - a 429 is flow control by status, so the health surface never counts it -
//     even when its body names a durable quota/billing condition (the class is
//     ClassQuota there, and quota_pause_mode owns the reaction);
//   - a rate-limit class on any OTHER status is a real failure for the health
//     surface (the class says why the request failed), so a 400 carrying
//     `tier_limit_exceeded`, an in-band 429 behind a committed 200, or a 499
//     carrying a rate-limit error all count as errors while HasRateLimit also
//     counts them in the rate-limit series.
//
// One latent shape is not corpus-exercised: Classify also reads error_code,
// while the health predicate reads the type alone (the envelope parser pairs
// every code with a type, so a code-only spelling does not reach a record in
// production).
func TestOutcomeIsErrorIsClassBlind(t *testing.T) {
	quota429, rateLimitOther := 0, 0
	for _, row := range isErrorCorpus {
		facts := ErrorFacts{Status: row.rec.StatusCode, Type: row.rec.ErrorType, Code: row.rec.ErrorCode}
		class := Classify(facts)
		classErr := class.IsError()
		healthErr := OutcomeIsError(row.rec.StatusCode, row.rec.ErrorType, row.rec.ErrorCode)
		differs := (class == ClassQuota && row.rec.StatusCode == 429) ||
			(class == ClassRateLimit && row.rec.StatusCode != 429)
		if differs {
			switch {
			case class == ClassQuota:
				quota429++
				if classErr != true || healthErr != false {
					t.Errorf("row %q: a durable quota 429 must be class-quota/health-flow-control, got class=%v health=%v", row.name, classErr, healthErr)
				}
			default:
				rateLimitOther++
				if classErr != false || healthErr != true {
					t.Errorf("row %q: a non-429 rate-limit class must be health-error/class-flow-control, got class=%v health=%v", row.name, classErr, healthErr)
				}
			}
			continue
		}
		if classErr != healthErr {
			t.Errorf("row %q: class says IsError=%v, health predicate says %v (class %q, status %d)", row.name, classErr, healthErr, class, row.rec.StatusCode)
		}
	}
	if quota429 == 0 || rateLimitOther == 0 {
		t.Fatalf("the corpus does not exercise both documented divergences (quota429=%d, rateLimitOther=%d)", quota429, rateLimitOther)
	}
}

// TestRecordIsErrorComposesOutcomeIsErrorAndAbsorbed5xx pins the public
// predicate's exact composition, so a future edit cannot quietly fold the
// absorbed-5xx arm into the outcome predicate (which the error explorer reads
// per attempt) or drop it from the record predicate.
func TestRecordIsErrorComposesOutcomeIsErrorAndAbsorbed5xx(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
		want bool
	}{
		{"clean 200", Record{StatusCode: 200}, false},
		{"final 500", Record{StatusCode: 500}, true},
		{"200 with an absorbed 500", Record{StatusCode: 200, Attempts: []RetryAttempt{{StatusCode: 500}}}, true},
		{"200 with an absorbed 429", Record{StatusCode: 200, Attempts: []RetryAttempt{{StatusCode: 429}}}, false},
		{"400 rate-limit code with an absorbed 5xx", Record{StatusCode: 400, ErrorType: "tier_limit_exceeded", Attempts: []RetryAttempt{{StatusCode: 502}}}, true},
		{"durable quota 429", Record{StatusCode: 429, ErrorType: "insufficient_quota"}, false},
		{"durable quota 400", Record{StatusCode: 400, ErrorType: "insufficient_quota"}, true},
		// The class says rate_limit; the health surface still counts the
		// failed request. The two questions are deliberately separate.
		{"400 with a rate-limit code", Record{StatusCode: 400, ErrorType: "tier_limit_exceeded"}, true},
		{"committed 200 with an in-band rate limit", Record{StatusCode: 200, ErrorType: "rate_limit_error"}, true},
		{"499 with a rate-limit error", Record{StatusCode: StatusClientClosedRequest, ErrorType: "rate_limit_error"}, true},
	}
	for _, tc := range cases {
		if got := tc.rec.IsError(); got != tc.want {
			t.Errorf("%s: Record.IsError() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
