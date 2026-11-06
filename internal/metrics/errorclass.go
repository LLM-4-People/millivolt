package metrics

import (
	"slices"
	"sort"
	"strings"
	"sync/atomic"
)

// ClassExtensions are the operator's additive classification spellings, one
// list per operator-extension setting (rate_limit_error_classes,
// quota_error_classes, retryable_error_classes). They are installed as one
// value by the process's config owner (proxy.New / proxy.Reload) and read by
// every vocabulary predicate, so the retry ladder, the health predicates, the
// published SQL and the dashboard vocabulary always answer from the same
// effective lists. An entry can only ADD a classification - the built-in
// vocabularies stay authoritative, and config validation rejects an entry that
// names a built-in spelling of a different class, so an operator cannot move a
// spelling between classes or weaken a built-in.
//
// Entries are normalized (lowercased, trimmed) on install.
type ClassExtensions struct {
	RateLimit []string
	Quota     []string
	Retryable []string
}

// classExtensions holds the installed extensions. Nil means the built-ins
// alone, which is the state of every process that never installed any and of
// every test that resets them.
var classExtensions atomic.Pointer[ClassExtensions]

// classExtensionRevision advances only when the installed vocabulary actually
// changes. Classification depends on it, so any cache that retains a derived
// has429/error/retryable answer (the dashboard's durable projection and its
// aggregate memos) must key freshness on this witness, not on the database
// version: a reload changes the classifier without changing a stored row.
var classExtensionRevision atomic.Uint64

// ClassExtensionRevision returns the current vocabulary witness. It changes
// whenever an install swaps in a different set of extensions, and never on a
// no-op install, so a cache can compare it cheaply.
func ClassExtensionRevision() uint64 {
	return classExtensionRevision.Load()
}

func sameClassExtensions(a, b ClassExtensions) bool {
	return slices.Equal(a.RateLimit, b.RateLimit) &&
		slices.Equal(a.Quota, b.Quota) &&
		slices.Equal(a.Retryable, b.Retryable)
}

func emptyClassExtensions(e ClassExtensions) bool {
	return len(e.RateLimit) == 0 && len(e.Quota) == 0 && len(e.Retryable) == 0
}

// InstallClassExtensions replaces the operator extensions. The proxy's config
// owner installs them on construction and on every reload; tests install them
// directly. A nil or empty value restores the built-ins. The swap is atomic,
// so a concurrent request classifies against one coherent vocabulary, and the
// last install wins - one process must have one owner.
func InstallClassExtensions(ext ClassExtensions) {
	normalized := ClassExtensions{}
	normalized.RateLimit = normalizeExtension(ext.RateLimit)
	normalized.Quota = normalizeExtension(ext.Quota)
	normalized.Retryable = normalizeExtension(ext.Retryable)
	for {
		old := classExtensions.Load()
		if old != nil && sameClassExtensions(*old, normalized) {
			return
		}
		if old == nil && emptyClassExtensions(normalized) {
			// The built-ins are already effective. Publish the canonical
			// empty value without advancing the revision: a no-op install
			// must not invalidate every classification-derived cache.
			if classExtensions.CompareAndSwap(nil, &normalized) {
				return
			}
			continue
		}
		if classExtensions.CompareAndSwap(old, &normalized) {
			classExtensionRevision.Add(1)
			return
		}
	}
}

// installedClassExtensions returns the current extensions (never nil).
func installedClassExtensions() ClassExtensions {
	if p := classExtensions.Load(); p != nil {
		return *p
	}
	return ClassExtensions{}
}

// normalizeExtension lowercases, trims, drops empties and dedupes an extension
// list, preserving order. Config validation already rejects those shapes, but
// the classifier must not depend on its caller having validated.
func normalizeExtension(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extensionMatches reports whether the normalized spelling matches an
// installed extension entry on the type or the code.
func extensionMatches(ext []string, typ, code string) bool {
	for _, v := range ext {
		if v == typ || v == code {
			return true
		}
	}
	return false
}

// ErrorClass is the canonical classification of one upstream or proxy outcome.
// It is the SINGLE answer to four questions every surface used to answer
// independently: is this a failure, is it retryable, is it a rate limit, and
// what does the client see. The retry ladder, the error-storm observer, the
// health predicates, the operator gate vocabulary and the client error writers
// all read this one classification, so a provider spelling can never be
// retryable in one path and final in another.
//
// Classification is NOT status-only. A provider may announce a rate limit with
// a non-429 status, and several real ones do: `tier_limit_exceeded`,
// `rate_limit_exceeded` and `resource_exhausted` arrive as HTTP 400 from some
// gateways while meaning exactly what a 429 means. Consulting the code
// vocabulary only on 429 is what let such an error surface to the client
// unretried while the operator's own request budget was still enforcing a
// stricter limit. The class therefore considers the status, the provider's own
// type/code, and the transport outcome together.
type ErrorClass string

const (
	// ClassNone is a usable outcome. It never appears on a failed attempt.
	ClassNone ErrorClass = ""

	// ClassRateLimit is transient flow control: the provider is asking for a
	// slower rate. Retryable, and counted in the rate-limit series. The
	// class's own predicate reads it as a flow event (a 429 alone is flow
	// control); the health surface decides separately - a rate-limit class
	// carried by a non-429 status is still a failed request (OutcomeIsError).
	ClassRateLimit ErrorClass = "rate_limit"

	// ClassQuota is a durable account/billing condition (credits exhausted,
	// spend or hard limits reached). Waiting cannot clear it, so it is never
	// retried by the ordinary ladder; quota_pause_mode owns the reaction. It
	// is an error.
	ClassQuota ErrorClass = "quota"

	// ClassUnavailable is transient upstream capacity or server failure: 5xx
	// and the provider spellings that mean the same thing. Retryable, an
	// error.
	ClassUnavailable ErrorClass = "unavailable"

	// ClassUnreachable is a transport failure with no usable response
	// (connection refused/reset, DNS failure, premature EOF). Retryable, an
	// error.
	ClassUnreachable ErrorClass = "unreachable"

	// ClassTimeout is an upstream that accepted the request and then ran out
	// of time. Retryable, an error.
	ClassTimeout ErrorClass = "timeout"

	// ClassContent is a 2xx that carried nothing usable (empty completion,
	// truncated stream, reasoning without an answer). Retried on the
	// quality/thinking budgets rather than the transient one. An error.
	ClassContent ErrorClass = "content"

	// ClassClientError is the caller's fault: malformed, unauthorized,
	// forbidden, absent, too large. Final, an error.
	ClassClientError ErrorClass = "client_error"

	// ClassProxy is a failure inside millivolt itself (buffer budget,
	// translation, oversized response). Never retried upstream, an error.
	ClassProxy ErrorClass = "proxy"

	// ClassCanceled is the local client going away (499). Flow event, not an
	// error.
	ClassCanceled ErrorClass = "canceled"

	// ClassUpstream is an upstream failure this build cannot classify more
	// precisely. Final by default (fail closed), an error.
	ClassUpstream ErrorClass = "upstream"
)

// ErrorFacts is the observable evidence one classification reads. Every field
// is something a caller already holds at the point of decision; the classifier
// derives everything else. Retryable is NOT an input: that is the classifier's
// answer, so there is no second authority for it.
type ErrorFacts struct {
	// Status is the HTTP status the provider returned, or 0 for a transport
	// failure with no response.
	Status int
	// Type and Code are the provider's own structured error members, or a
	// proxy-minted class. Either may be empty.
	Type string
	Code string
	// Transport marks a failure with no HTTP response at all. Which transport
	// failures are worth another attempt stays the transport layer's judgment
	// (isRetryableTransportErr); this only says there was no response to
	// classify.
	Transport bool
	// Local marks a proxy-minted failure, so a provider spelling can never be
	// attributed to the provider.
	Local bool
}

// clientFaultClasses are the documented client-fault types: the request itself
// is wrong, so a re-send produces the identical failure. The TYPE is the
// authoritative class field, so a transient-looking code mixed under one of
// these does not make the request retryable - a provider that mixes them is
// ambiguous, not transient. engine_overloaded is the documented exception: it
// names a capacity condition that gateways wrap in the generic
// invalid_request_error spelling.
var clientFaultClasses = map[string]bool{
	"invalid_request_error":   true,
	"authentication_error":    true,
	"permission_error":        true,
	"not_found_error":         true,
	"request_too_large":       true,
	"billing_error":           true,
	"context_length_exceeded": true,
}

// overridesClientFault are the capacity codes that name a genuine transient
// condition even when wrapped in a client-fault type.
var overridesClientFault = map[string]bool{
	"engine_overloaded": true,
}

// isClientFault reports whether the envelope names the caller's own fault. The
// type is authoritative; a capacity code may override the generic
// invalid_request_error wrapper, matching the in-band rescue composition
// (InBandRetryable) so the two paths cannot disagree about the same envelope.
// Inputs must already be lowercased and trimmed. The operator's
// retryable_error_classes extension outranks this deny - both callers check
// the extension first - so a gateway that mislabels a transient failure can be
// opted back in without a code change.
func isClientFault(typ, code string) bool {
	if !clientFaultClasses[typ] {
		return false
	}
	if typ == "invalid_request_error" && overridesClientFault[code] {
		return false
	}
	return true
}

// Classify returns the canonical class for the given evidence.
//
// Precedence, most specific first:
//  1. a clean success (2xx with nothing structured to report);
//  2. durable quota/billing, which outranks every transient reading (a billing
//     block is never fixed by waiting);
//  3. rate limit, which outranks the status: a rate-limit code carried by a
//     400 is a rate limit, and the 429 status is itself a rate limit;
//  4. the caller's own fault, whose type outranks a transient-looking code;
//  5. a proxy-minted class;
//  6. transport failure;
//  7. content (2xx with nothing usable);
//  8. timeout, before the generic availability class so the client sees the
//     narrower reason;
//  9. transient upstream availability;
//  10. cancellation;
//  11. anything else that failed.
func Classify(f ErrorFacts) ErrorClass {
	typ, code := strings.ToLower(strings.TrimSpace(f.Type)), strings.ToLower(strings.TrimSpace(f.Code))

	// 1. A clean success carries no structured failure to classify. This runs
	// first so a proxy-minted code (which may arrive with no type at all)
	// cannot turn a successful 2xx into a proxy failure.
	if f.Status >= 200 && f.Status < 300 && typ == "" && code == "" && !f.Transport {
		return ClassNone
	}
	// 2. Durable account/billing conditions outrank everything: no retry and no
	// rate-limit accounting can clear them.
	if NonRetryableQuotaClass(typ, code) != "" {
		return ClassQuota
	}
	// 3. Rate limits, whether announced by status or by code. The code check is
	// deliberately NOT gated on a 429 status: providers that answer a rate
	// limit with 400 are common, and gating on the status is what surfaced
	// `tier_limit_exceeded` to clients unretried.
	if f.Status == 429 || IsRateLimitClass(typ, code) {
		return ClassRateLimit
	}
	// 4. The caller's own fault, before any transient reading - unless the
	// operator explicitly listed the spelling in retryable_error_classes: that
	// list is authoritative and overrides the built-in deny, exactly as it
	// does on the in-band rescue path (InBandRetryable).
	if isClientFault(typ, code) && !extensionMatches(installedClassExtensions().Retryable, typ, code) {
		return ClassClientError
	}
	// 5. Proxy-minted classes, before any provider reading.
	if f.Local {
		if c, ok := localClass(typ, code); ok {
			return c
		}
		return ClassProxy
	}
	// 6. Transport failures.
	if f.Transport {
		if isTimeoutClass(typ, code) {
			return ClassTimeout
		}
		return ClassUnreachable
	}
	// 7. A 2xx that carried nothing usable.
	if f.Status >= 200 && f.Status < 300 {
		if c, ok := contentClass(typ, code); ok {
			return c
		}
		// A structured failure inside a 200 that no vocabulary recognizes:
		// fail closed and let the caller relay it verbatim.
		return ClassUpstream
	}
	// 8. Timeouts, before the generic availability class.
	if isTimeoutClass(typ, code) {
		return ClassTimeout
	}
	// 9. Transient upstream availability. Any status at or above 500 is a
	// server-side availability failure, including a non-standard 6xx-9xx from
	// a hostile or broken gateway: nothing above 500 is a clean success.
	if f.Status >= 500 {
		return ClassUnavailable
	}
	if IsRetryableClass(typ, code) {
		return ClassUnavailable
	}
	// 10. Cancellation. A plain 499 is the local client's own abort. A 499 that
	// carries a structured error observed the upstream's failure before the
	// client left, so the envelope - not the status - decides its class,
	// exactly like the committed-200 twin it aborted around.
	if f.Status == 499 && typ == "" && code == "" {
		return ClassCanceled
	}
	// 11. The caller's fault by status, then anything else that failed. A 499
	// with a structured error deliberately skips the by-status arm: the status
	// describes the client, not the failure.
	if f.Status >= 400 && f.Status <= 499 && f.Status != 499 {
		return ClassClientError
	}
	if typ != "" || code != "" {
		return ClassUpstream
	}
	return ClassNone
}

// Retryable reports whether the ordinary transient ladder may re-send a class.
// Rate limits, unavailable capacity, unreachable transports and timeouts are
// retryable; a durable quota, a client fault, a proxy fault and a cancellation
// are not. Content is retried on its own quality/thinking budgets rather than
// this one, so it is not retryable here.
func (c ErrorClass) Retryable() bool {
	switch c {
	case ClassRateLimit, ClassUnavailable, ClassUnreachable, ClassTimeout:
		return true
	}
	return false
}

// IsError reports whether the class itself reads as a failure: a rate limit
// and a cancellation are flow events, every other class is a failure. The
// health surface answers a different question (OutcomeIsError) by reading the
// status and the error type rather than the class, so the two deliberately
// diverge in the documented families; neither is derived from the other.
func (c ErrorClass) IsError() bool {
	switch c {
	case ClassNone, ClassRateLimit, ClassCanceled:
		return false
	}
	return true
}

// IsRateLimit reports whether the class participates in rate-limit health and
// pacing accounting.
func (c ErrorClass) IsRateLimit() bool { return c == ClassRateLimit }

// String returns the wire token. It is the same spelling every client surface
// emits, so an operator and a client agree on the class by construction.
func (c ErrorClass) String() string { return string(c) }

// rateLimitClasses are the provider type/code spellings that denote transient
// rate limiting. They are honored on ANY status, because gateways commonly
// answer a rate limit with 400 or 403 rather than 429. Never message text:
// matching prose would let an unrelated sentence change retry behavior.
var rateLimitClasses = map[string]bool{
	"rate_limit_error":             true, // OpenAI / Anthropic type
	"rate_limit_exceeded":          true, // Responses API code
	"rate_limit":                   true, // gateway spelling
	"ratelimiterror":               true,
	"too_many_requests":            true, // gateway spelling
	"too_many_requests_error":      true,
	"tier_limit_exceeded":          true, // membership/tier RPM and TPM caps
	"membership_limit_exceeded":    true,
	"requests_per_minute_exceeded": true,
	"rpm_limit_exceeded":           true,
	"tpm_limit_exceeded":           true,
	"tokens_per_minute_exceeded":   true,
	"resource_exhausted":           true, // gRPC-family rate limit
	"quota_exceeded":               true, // transient window quota (durable classes win first)
	"slow_down":                    true,
}

// retryableClasses are the provider type/code spellings that denote transient
// upstream availability - the classes the provider's own documentation says to
// retry. The durable and client-fault vocabularies are never here.
var retryableClasses = map[string]bool{
	"server_error":              true, // OpenAI 500
	"service_unavailable":       true, // OpenAI 503 type
	"service_unavailable_error": true, // OpenAI 503 type (current docs)
	"server_is_overloaded":      true, // OpenAI 503 code
	"api_error":                 true, // Anthropic 500
	"internal_error":            true, // gateway spelling
	"internal_server_error":     true, // gateway spelling
	"overloaded_error":          true, // Anthropic 529
	"engine_overloaded":         true, // vLLM capacity code
	"upstream_error":            true, // millivolt's own transient class
	"bad_gateway":               true,
	"gateway_timeout":           true,
}

// timeoutClasses are the spellings that denote a timeout specifically, so the
// client sees the narrower class instead of the generic unavailable one.
var timeoutClasses = map[string]bool{
	"timeout_error":     true, // Anthropic 540
	"timeout":           true,
	"request_timeout":   true,
	"deadline_exceeded": true,
	"gateway_timeout":   true,
}

// contentClasses are a 2xx outcome that carried nothing usable. They are
// retried on the quality and thinking budgets.
var contentClasses = map[string]bool{
	CodeEmptyCompletion: true,
	CodeEmptyToolCall:   true,
	CodeTruncated:       true,
	CodeReasoningOnly:   true,
	"empty_turn":        true, // cursor void turn
}

// Proxy-minted error type spellings. These are the ONE owner of the literals:
// the proxy's error writers and its localErrorType predicate reference these
// names instead of repeating the strings, so a new proxy class is added here
// once and immediately participates in classification.
const (
	TypeTransformError      = "transform_error"
	TypeTranslationError    = "translation_error"
	TypeStreamReadError     = "stream_read_error"
	TypeResponseTooLarge    = "response_too_large"
	TypeResponseBudget      = "response_buffer_budget"
	TypeQueueFull           = "queue_full"
	TypeStormQueueFull      = "storm_queue_full"
	TypeQueueDrained        = "queue_drained"
	TypeUpstreamError       = "upstream_error"
	TypeUpstreamUnreachable = "upstream_unreachable"
	TypeAPIError            = "api_error"
	TypeInvalidRequest      = "invalid_request_error"
	TypeProviderError       = "provider_error"
	TypeRateLimit           = "rate_limit_error"
	TypeTransport           = "transport"
)

// proxyClasses are the classes millivolt mints itself. This map and the
// constants above are the single vocabulary: IsLocalErrorType answers "is this
// ours" and Classify answers what it means.
var proxyClasses = map[string]bool{
	TypeTransformError:   true,
	TypeTranslationError: true,
	TypeStreamReadError:  true,
	TypeResponseTooLarge: true,
	TypeResponseBudget:   true,
	TypeQueueFull:        true,
	TypeStormQueueFull:   true,
	TypeQueueDrained:     true,
	CodeInvalidUsage:     true,
}

// IsLocalErrorType reports whether an error type was minted by the proxy rather
// than parsed from a provider envelope. Provider classifications keep their own
// code when usage is invalidated after the failure. An empty type is local by
// this definition (there is no provider claim to preserve), matching the
// historical inline predicate.
func IsLocalErrorType(t string) bool {
	if t == "" {
		return true
	}
	return proxyClasses[t] || t == TypeUpstreamUnreachable
}

func localClass(typ, code string) (ErrorClass, bool) {
	if proxyClasses[typ] || proxyClasses[code] {
		// queue_drained is a retryable capacity answer, not a proxy fault: the
		// response is a 503 (or an in-band rate_limit_error) with Retry-After,
		// so classifying it as ClassProxy would tell clients and SDKs not to
		// retry the very response that asks them to.
		if typ == TypeQueueDrained || code == TypeQueueDrained {
			return ClassUnavailable, true
		}
		return ClassProxy, true
	}
	if contentClasses[typ] || contentClasses[code] {
		return ClassContent, true
	}
	return "", false
}

func contentClass(typ, code string) (ErrorClass, bool) {
	if contentClasses[typ] || contentClasses[code] {
		return ClassContent, true
	}
	return "", false
}

func isTimeoutClass(typ, code string) bool {
	return timeoutClasses[typ] || timeoutClasses[code]
}

// IsRateLimitClass reports whether a provider type or code denotes transient
// rate limiting, independent of the HTTP status that carried it. This is the
// single owner of that vocabulary (the built-ins plus the operator's
// rate_limit_error_classes extension): the classifier, the health predicates,
// the published SQL and the client-facing class all consult it.
func IsRateLimitClass(typ, code string) bool {
	typ, code = strings.ToLower(strings.TrimSpace(typ)), strings.ToLower(strings.TrimSpace(code))
	if rateLimitClasses[typ] || rateLimitClasses[code] {
		return true
	}
	return extensionMatches(installedClassExtensions().RateLimit, typ, code)
}

// IsRetryableClass reports whether a provider type or code denotes transient
// upstream availability: the built-in availability vocabulary plus the
// operator's retryable_error_classes extension.
func IsRetryableClass(typ, code string) bool {
	typ, code = strings.ToLower(strings.TrimSpace(typ)), strings.ToLower(strings.TrimSpace(code))
	if retryableClasses[typ] || retryableClasses[code] {
		return true
	}
	return extensionMatches(installedClassExtensions().Retryable, typ, code)
}

// BuiltinClass names the built-in class a spelling belongs to - "quota",
// "rate_limit", "retryable", "timeout", "content", "proxy" or "client_error" -
// or "" when no built-in vocabulary knows it. Config validation uses it to
// reject an operator extension that names a built-in spelling of a DIFFERENT
// class: extensions may only add, never move a spelling between classes.
// Gateway overlaps resolve to the first matching family (gateway_timeout is
// both retryable and a timeout, so it reports "retryable").
func BuiltinClass(spelling string) string {
	spelling = strings.ToLower(strings.TrimSpace(spelling))
	switch {
	case nonRetryableQuotaClasses[spelling]:
		return "quota"
	case rateLimitClasses[spelling]:
		return "rate_limit"
	case retryableClasses[spelling]:
		return "retryable"
	case timeoutClasses[spelling]:
		return "timeout"
	case contentClasses[spelling]:
		return "content"
	case proxyClasses[spelling] || spelling == TypeUpstreamUnreachable:
		return "proxy"
	case clientFaultClasses[spelling]:
		return "client_error"
	}
	return ""
}

// InBandRetryable reports whether a structured error carried inside a 2xx
// authorizes a transparent re-send - the ONE composition the proxy's in-band
// rescue reads. Precedence: a durable quota/billing condition never re-sends
// (the quota pause owns it); the operator's retryable_error_classes extension
// is authoritative and may override the built-in client-fault deny; a
// documented client-fault type is final against the built-in vocabulary; the
// built-in availability and timeout vocabularies match either member (a
// timeout is transient, so it authorizes the rescue exactly as a 5xx class
// does). Rate-limit spellings stay false: an in-band rate limit carries no
// Retry-After and the scheduler owns pacing, so it is relayed verbatim.
func InBandRetryable(typ, code string) bool {
	typ, code = strings.ToLower(strings.TrimSpace(typ)), strings.ToLower(strings.TrimSpace(code))
	if IsNonRetryableQuotaErr(typ, code) {
		return false
	}
	if extensionMatches(installedClassExtensions().Retryable, typ, code) {
		return true
	}
	if isClientFault(typ, code) {
		return false
	}
	return retryableClasses[typ] || retryableClasses[code] ||
		timeoutClasses[typ] || timeoutClasses[code]
}

// RateLimitExtensions returns the installed operator extension list for rate
// limits (nil when none). The dashboard publishes it alongside the built-in
// vocabulary so the browser's mirror classifies exactly like the server.
func RateLimitExtensions() []string {
	return installedClassExtensions().RateLimit
}

// BuiltinRateLimitClassVocabulary returns the built-in rate-limit spellings
// WITHOUT operator extensions. The dashboard's static JS mirror ships exactly
// this list; extensions travel in the bootstrap payload instead, so the static
// mirror never has to be regenerated by an operator.
func BuiltinRateLimitClassVocabulary() []string {
	return sortedVocabulary(rateLimitClasses)
}

// RateLimitClassVocabulary returns the effective rate-limit spellings: the
// built-ins plus the operator's extensions, sorted. Returned as a copy so no
// caller can mutate the classifier's vocabulary.
func RateLimitClassVocabulary() []string {
	return mergedVocabulary(rateLimitClasses, installedClassExtensions().RateLimit)
}

// sortedVocabulary renders a vocabulary set in the deterministic order the SQL
// renderers and the JS parity pins expect.
func sortedVocabulary(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mergedVocabulary renders a built-in set plus its extension list, sorted and
// deduped, as the effective vocabulary.
func mergedVocabulary(builtin map[string]bool, ext []string) []string {
	seen := make(map[string]bool, len(builtin)+len(ext))
	out := make([]string, 0, len(builtin)+len(ext))
	for k := range builtin {
		seen[k] = true
		out = append(out, k)
	}
	for _, v := range ext {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// RateLimitClassListSQL renders the classifier's rate-limit vocabulary as a
// comma-separated SQL literal list, sorted for a deterministic string.
//
// It lives here, beside the vocabulary it renders, because the two published
// SQL predicates that mirror this classifier (internal/mcp's describe document
// and internal/storage's purge filter) both have to name the same spellings.
// Generating them from the vocabulary is what keeps a documented query and the
// Go predicate it claims to mirror from silently drifting apart; hand-copying
// the list into SQL is the failure this exists to prevent.
func RateLimitClassListSQL() string {
	vocab := RateLimitClassVocabulary()
	parts := make([]string, 0, len(vocab))
	for _, v := range vocab {
		parts = append(parts, "'"+strings.ReplaceAll(v, "'", "''")+"'")
	}
	return strings.Join(parts, ", ")
}

// RateLimitClassSQL renders the "one of these SQL expressions names a
// rate-limit class" predicate, using the same vocabulary. Each argument is a
// nullable text expression (a column, or a json_extract path); NULL is treated
// as empty, matching the record's empty-string zero, and the comparison trims
// and lowercases like the Go matcher, so a spelling stored with surrounding
// whitespace answers the same on both sides.
func RateLimitClassSQL(exprs ...string) string {
	list := RateLimitClassListSQL()
	parts := make([]string, 0, len(exprs))
	for _, e := range exprs {
		parts = append(parts, "lower(trim(coalesce("+e+", ''))) IN ("+list+")")
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// absorbed5xxSQL is the SQL twin of absorbedServerFailure: a genuine upstream
// 5xx an earlier attempt saw, even when a later attempt recovered.
const absorbed5xxSQL = `EXISTS (SELECT 1 FROM json_each(requests.attempts) WHERE json_extract(value, '$.status_code') >= 500)`

// IsErrorSQL returns the SQL predicate equivalent to Record.IsError, and
// HasRateLimitSQL the equivalent of Record.HasRateLimit. Both are the ONE
// rendering of those predicates: the MCP describe document and the durable
// purge filter call these instead of carrying their own copies, so a published
// query and the dashboard it claims to mirror cannot drift.
//
// The predicates are written against the durable `requests` table (columns
// status_code, error_type, error_code, attempts). The attempts column is JSON;
// json_each scans it.
//
// IsErrorSQL is status-and-type based, matching OutcomeIsError: a 429 is flow
// control (not an error) and a 499 is the client's own abort unless it carries
// a structured error; anything else needs a structured error or an error
// status. A rate-limit class on a non-429 status is still an error here - the
// class answers "why", and HasRateLimitSQL counts it in the rate-limit series.
func IsErrorSQL() string {
	return `(
  (status_code = 429 AND ` + absorbed5xxSQL + `)
  OR (status_code = 499 AND (error_type != '' OR ` + absorbed5xxSQL + `))
  OR (status_code <> 429 AND status_code <> 499 AND (error_type != '' OR status_code >= 400))
  OR (status_code <> 429 AND status_code <> 499 AND status_code < 400 AND ` + absorbed5xxSQL + `)
)`
}

// HasRateLimitSQL is the SQL twin of Record.HasRateLimit. The rate-limit
// vocabulary is rendered from its owner (metrics), so an operator extension
// reaches the published query automatically.
func HasRateLimitSQL() string {
	return `(status_code = 429 OR ` + RateLimitClassSQL("error_type", "error_code") + ` OR EXISTS (
  SELECT 1 FROM json_each(requests.attempts)
  WHERE json_extract(value, '$.status_code') = 429
     OR ` + RateLimitClassSQL("json_extract(value, '$.error_type')", "json_extract(value, '$.error_code')") + `
))`
}
