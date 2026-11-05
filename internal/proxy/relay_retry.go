package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// doWithRetry executes the upstream request, transparently absorbing transient
// failures: 429 and any 5xx are retried with backoff while the client has not
// yet received any bytes, so the client only ever sees the final outcome. It
// parses provider retry hints (Retry-After, x-ratelimit-reset) as a floor on
// the wait; adaptive exponential backoff still grows. Any retry (429, 5xx,
// transport) trips the provider+key group: that request owns the
// next send. After success or exhaust, exactly one sibling is the probe; a
// clean first-attempt success restores full concurrency, another retry keeps
// it at one-at-a-time. Exhausted retryable failures double a request-level
// backoff (base_backoff → max_backoff); a success resets it. WaitSend also
// honors operator pause on retries and remaining nextAllowedAt across pause.
// When retries are exhausted the last upstream response is returned as-is
// (failures are never masked). A 429 whose error envelope is a durable
// quota/billing condition (metrics.IsNonRetryableQuotaErr) follows
// quota_pause_mode: unarmed targets surface it immediately (waiting cannot
// clear it), armed OpenAI-wire targets park the provider and this request
// behind the quota gate or hold until a re-send resolves 2xx - those quota
// re-sends never count against the transient retry budget.
//
// firstSendIsRetry marks an invocation whose OPENING send is itself a retry
// (the degenerate-200 quality re-run): its first WaitSend honors operator
// holds exactly like a 429/5xx retry - "a retry is a new send and waits" -
// instead of the first-attempt exemption that lets an already-acquired
// in-flight request finish.
//
// The returned CancelFunc belongs to the FINAL response's per-send deadline
// (X-Proxy-Timeout-Ms): the caller must defer it past the body relay so the
// deadline keeps governing the body exactly like the request-lifetime wrap it
// replaced, and so absorbed attempts' contexts (canceled inside, right after
// their bodies are drained) never leak. It is always non-nil on success.
func (s *Server) doWithRetry(ctx context.Context, groupKey string, r *http.Request, t *target, key string, body []byte, rec *metrics.Record, hooks scheduler.WaiterHooks, firstSendIsRetry bool) (*http.Response, context.CancelFunc, error) {
	// MaxRetries comes from config (single source of truth for the default).
	maxRetries := s.cfg().MaxRetries
	own := false
	retried := false
	// ordinary counts only transient absorb attempts (429/5xx/transport).
	// Quota-pause re-sends park for an account condition, not a transient
	// failure, and must never exhaust the transient budget.
	ordinary := 0
	end := func(failed bool) {
		if failed {
			// Exhausted retryable: double request backoff, keep one probe.
			s.scheduler.FailSend(groupKey, own)
			return
		}
		if own {
			// Retried → one next probe. Clean first-attempt → full concurrency.
			s.scheduler.EndSend(groupKey, retried)
		}
	}
	// permit and attemptStart are declared outside the loop so the admission
	// call can assign them (and own) with a plain = : a := would shadow the
	// outer own inside the loop body and silently drop the Trip-claimed send
	// token between attempts.
	var (
		permit       *scheduler.StormPermit
		attemptStart time.Time
	)
	for attempt := 0; ; attempt++ {
		var err error
		own, permit, attemptStart, err = s.admitSendAttempt(ctx, groupKey, hooks, firstSendIsRetry, attempt, own)
		if err != nil {
			end(false)
			return nil, nil, err
		}
		// FinalAttemptAt anchors TTFT at the adopted attempt's send start.
		// Stamp every attempt, including the first: TTFT is always measured
		// from the upstream send, never from Start (which also covers
		// queueing and request parsing). Later attempts re-stamp their own
		// start, so the surviving value always describes the attempt whose
		// response is adopted. Only a request that never sent an upstream
		// attempt keeps the zero value (TTFT is then absent).
		rec.FinalAttemptAt = attemptStart
		// Per-send deadline: X-Proxy-Timeout-Ms bounds this ONE send (the
		// Do below and the returned body), anchored on the attemptStart the
		// admission helper stamped - a fresh budget per attempt, never eaten
		// by the waits above. The attempt's httptrace decomposition rides the
		// same context, so an abandoned attempt's trace dies with it (see
		// upstreamTiming).
		timing := upstreamTiming{start: attemptStart}
		attemptCtx, attemptCancel := withSendTimeout(httptrace.WithClientTrace(ctx, timing.trace(t.tlsPossible())), t.timeout)
		// Rebuild the upstream request (body can't be reused across retries).
		upstream, err := s.buildUpstreamRequest(attemptCtx, r, t, key, body)
		if err != nil {
			permit.Cancel()
			attemptCancel()
			end(false)
			return nil, nil, err
		}
		resp, err := s.httpClient().Do(upstream)
		if err != nil {
			// Observe cancellation before cleanup: canceling our own send
			// context must not turn a transient transport error into an abort.
			attemptErr := attemptCtx.Err()
			attemptCancel() // no response body was received; nothing to drain
			// Caller aborted (client disconnect / Ctrl+C) or the per-send
			// deadline fired - never retry; surface verbatim.
			if ctx.Err() != nil || attemptErr != nil {
				permit.Cancel()
				end(false)
				return nil, nil, err
			}
			// Genuine transport failure (no response received). Retry the
			// transient ones (header timeout, conn reset/refused, EOF, DNS
			// timeout) transparently; give up on permanent ones (NXDOMAIN,
			// TLS errors) or when the retry budget is spent.
			retryable := isRetryableTransportErr(err)
			active := s.observeStormTransport(ctx, permit, retryable)
			if !retryable || ordinary >= maxRetries && !s.allowStormRetry(ctx, active) {
				end(retryable)
				return nil, nil, err
			}
			own = s.absorbTransportRetry(rec, groupKey, transportErrText(err), own)
			ordinary++
			retried = true
			continue
		}

		// Guard the response body at receipt, before any consumer reads it.
		// The durable-quota peek, the absorbed-attempt error-body drain, and
		// the caller's capture/relay all read resp.Body; a provider that sends
		// headers and then goes silent must not pin the handler, the upstream
		// connection and the inflight reservation outside the shared idle
		// bound. Cancelling this attempt's context is what aborts a blocked
		// transport read; Close stops the timer and releases the context.
		resp.Body = s.wrapUpstreamBody(resp.Body, attemptCancel)

		// A 429 can be transient flow control OR the provider announcing a
		// durable account condition (quota/credits/spend limits) that waiting
		// can never clear, and only the body's structured type/code tells them
		// apart. The same body decides every other class on the response: a
		// rate limit, a genuine client fault, or transient availability. So
		// the bounded envelope is peeked ONCE here, before any decision, and
		// the classification runs off it - a status-only test cannot tell a
		// 400 that means "rate limit" from a 400 that means "malformed
		// request", and treating them alike is what surfaced
		// `tier_limit_exceeded` to clients unretried.
		//
		// With the quota pause armed for this target, a durable 429 parks the
		// provider and this request: the attempt is absorbed,
		// settleQuotaFailure opens the recovery gate (retry mode) or the
		// provider hold (manual mode), and the next admission below waits out
		// the pause - the loop re-sends until a send resolves 2xx and the
		// queue drains. Unarmed targets surface the 429 immediately - no retry
		// budget burned, no pacing of the group, the absorbed-attempt log
		// stays clean (nothing was absorbed).
		//
		// This relay's overflow policy: a peek that OVERFLOWS the cap leaves
		// the body open (peekLeftOpen), so the final relay can serve
		// prefix + live remainder verbatim and an oversized error keeps normal
		// retry semantics. A peek whose read FAILED (peekErr) closed the body
		// under the whole-document bound: the final relay re-serves the
		// captured prefix and keeps the failed read propagating through the
		// untrustworthy remainder, so a translated or buffered owner fails
		// closed instead of asserting the upstream status over a truncated
		// envelope.
		var errBody []byte
		peekLeftOpen := false
		var errTyp, errCode string
		var peekErr error
		if resp.StatusCode >= 400 {
			// The peek is part of THIS send: the attempt context carries the
			// per-send deadline (X-Proxy-Timeout-Ms) when one was requested
			// and falls back to the shared whole-document bound otherwise.
			errBody, errTyp, errCode, peekLeftOpen, peekErr = s.peekErrorEnvelope(attemptCtx, resp)
			// A 429 whose envelope names a durable account condition is the
			// documented quota-pause trigger. The pause reacts to a 429; a
			// durable code on another status still classifies as quota (so it
			// is never retried) but does not open the provider gate.
			if resp.StatusCode == http.StatusTooManyRequests && isNonRetryableQuotaErr(errTyp, errCode) {
				if !s.settleQuotaFailure(permit, t.format, hooks.Client, hooks.Provider, metrics.NonRetryableQuotaClass(errTyp, errCode), parseRetryAfter(resp)) {
					end(false)
					// This response reaches the client: adopt this attempt's
					// transport timing (the failed attempts before it never
					// touched the record; FinalAttemptAt was already stamped at
					// this attempt's admission above).
					timing.adopt(rec)
					// Re-serve the captured bytes so the client still gets the
					// full error body verbatim.
					resp.Body = io.NopCloser(bytes.NewReader(errBody))
					return resp, attemptCancel, nil
				}
				// Parked by the quota pause: book the absorbed attempt (with
				// the provider's Retry-After hint for the record) and loop.
				// Quota waits never burn the ordinary retry budget - only
				// transient attempts count against maxRetries.
				attemptCancel() // the in-cap body was drained by the peek
				retryAfter := s.absorbHTTPRetry(rec, resp, errBody, t.authHeader, metrics.ClassQuota)
				rec.RetryAfterMs = int(retryAfter.Milliseconds())
				// A durable quota condition is an account state, not flow
				// control: RateLimited stays unset for the same reason the
				// unarmed path does.
				rec.ErrorType = ""
				rec.ErrorMsg = ""
				s.publishUpdate(rec)
				retried = true
				continue
			}
		}

		// Only retry transient conditions: a rate limit, an upstream/server
		// failure, or a transport-level retryable error. The decision reads the
		// provider's OWN type/code as well as the status, through the canonical
		// classifier: some providers answer a rate limit with HTTP 400 (for
		// example `tier_limit_exceeded` / `membership rpm limit exceeded`), and
		// a status-only test surfaced those to the client unretried. When
		// retries are exhausted the last upstream response is surfaced
		// verbatim, so the client sees the real error - retries are
		// transparent, failures are not masked. A durable quota/billing class
		// is never retried by this ladder: the quota pause owns it.
		class := outcomeClass(resp, errTyp, errCode)
		durableQuota := class == metrics.ClassQuota
		retryable := class.Retryable() && !durableQuota
		active := s.observeStormHTTP(ctx, permit, resp, retryable)
		if state := stormState(ctx); state != nil && state.response == permit {
			state.responseCtx = attemptCtx
		}
		if !retryable || ordinary >= maxRetries && !s.allowStormRetry(ctx, active) {
			// This is the attempt whose response reaches the client: TTFT is
			// measured from its send start (FinalAttemptAt was stamped at its
			// admission above), so a retried request shows the successful
			// attempt's responsiveness, not the accumulated retry delay. The
			// httptrace decomposition is adopted the same way: the record
			// keeps this attempt's dial/handshake/header-wait facts, and the
			// absorbed attempts' traces died with their contexts.
			timing.adopt(rec)
			end(retryable)
			// Stamp the parsed classification so the record, the live ring and
			// the health predicates all read the SAME class the retry ladder
			// just decided on. Without this a rate limit carried by a 400
			// reaches the record as a bare 400: it still counts as a failed
			// request by status, but it stays out of the rate-limit series and
			// the client-visible class reads as a plain client fault.
			if errTyp != "" || errCode != "" {
				rec.ErrorType, rec.ErrorCode = errTyp, errCode
				if msg, _, _ := parseErrorBody(errBody); msg != "" {
					rec.ErrorMsg = msg
				}
			}
			switch {
			case class.IsRateLimit():
				rec.RateLimited = true
			case class == metrics.ClassQuota:
				// A durable account condition is not an error-storm event and
				// not flow control: leave the storm evidence and the pacing
				// flag alone, exactly as the 429 path does (a queue wait or an
				// earlier absorbed 503 may already have set RateLimited for the
				// scheduler, and this response neither adds nor clears it).
			}
			if peekErr != nil {
				// The bounded envelope read failed mid-body (the
				// whole-document bound fired, the shared idle guard canceled
				// the attempt, or the connection reset): the captured prefix
				// is re-served, but the record must name the read failure
				// exactly as the live relay loop does for a body read it
				// cannot finish. Only the SURFACED attempt marks: an absorbed
				// read failure just truncates the discarded error detail.
				markStreamErr(ctx, rec, peekErr)
			}
			if errBody != nil {
				if peekLeftOpen || peekErr != nil {
					// Overflow: relay the COMPLETE body - captured prefix +
					// live remainder (byte-for-byte, never a truncated prefix);
					// Close() closes the original body. Read failure: the rest
					// is the broken/closed body the peek abandoned - only the
					// captured prefix is trustworthy, so the re-served chain
					// yields the prefix and lets the read failure propagate
					// into the owner's bounded read (fail closed).
					resp.Body = &prefixRemainderBody{prefix: errBody, rest: resp.Body}
				} else {
					resp.Body = io.NopCloser(bytes.NewReader(errBody))
				}
			}
			// The body was armed with the shared idle guard when this attempt's
			// response arrived (and re-served prefixes above retain the live
			// remainder's guard), so the caller keeps a bounded read.
			return resp, attemptCancel, nil
		}

		// Read the failed attempt's error body (bounded) so the proxy logs the
		// full failure detail even though the client never sees it. The body is
		// then discarded - it is never forwarded. Best-effort: a read failure
		// just means less error detail for this absorbed attempt. The envelope
		// peek above captured the in-cap prefix (io.ReadAll never returns nil)
		// and closed the body on a read failure; an OVERSIZED body was left
		// open, so close it here - the remainder is discarded with the rest of
		// the attempt.
		if peekLeftOpen {
			resp.Body.Close()
		}
		// The absorbed attempt's send is over (body drained): release its
		// per-send deadline before the next attempt mints a fresh one.
		attemptCancel()

		// Record the absorbed attempt so the dashboard/drawer show the errors
		// that preceded the final (e.g. 200) outcome. The caller's parsed
		// envelope is passed in so the retry-hint decision reuses the class it
		// already computed instead of decoding the body a second time.
		retryAfter := s.absorbHTTPRetry(rec, resp, errBody, t.authHeader, class)
		s.publishUpdate(rec)
		// The canonical class decides the rate-limit flag, so a rate limit
		// announced by a code on a non-429 status is counted exactly like a
		// 429. 503 keeps its documented scheduler-pacing meaning (a provider
		// shedding load), which is broader than the health predicate
		// Record.HasRateLimit; a plain 502 stays unlabelled.
		if class.IsRateLimit() || resp.StatusCode == http.StatusServiceUnavailable {
			rec.RateLimited = true
		}
		rec.ErrorType = ""
		rec.ErrorMsg = ""
		rec.RetryAfterMs = int(retryAfter.Milliseconds())

		ordinary++
		retried = true
		own = s.scheduler.Trip(groupKey, s.scheduler.BackoffFor(groupKey, retryAfter)) || own
	}
}

// outcomeClass is the canonical classification of one received HTTP response,
// including its parsed provider envelope. It is the ONE call site that turns
// status + type + code into a class; the retry ladder, storm observation and
// the record all read its result instead of re-deriving it.
func outcomeClass(resp *http.Response, typ, code string) metrics.ErrorClass {
	return metrics.Classify(metrics.ErrorFacts{
		Status: resp.StatusCode,
		Type:   typ,
		Code:   code,
	})
}

// isRetryableTransportErr reports whether a client.Do transport error (no
// HTTP response received) is a transient failure worth retrying. The caller
// must have already ruled out caller-abort via ctx.Err() - this only classifies
// genuine upstream/network failures. Retryable: header/i/o timeouts, connection
// reset/refused, unexpected EOF, DNS timeout/temporary. Not retryable: DNS
// NXDOMAIN (host doesn't exist), TLS/cert errors, and other permanent failures.
func isRetryableTransportErr(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// Retry transient DNS (timeout/temporary-servfail); NXDOMAIN is permanent.
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	// Timeout-class net errors (e.g. "timeout awaiting response headers").
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	// Syscall-level dial/read/write failures (connection refused/reset).
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	// Connection dropped mid-handshake.
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// transportErrText renders a transport error for storage/display without the
// *url.Error wrapper, whose message embeds the full request URL - including
// any client query string, which may carry sensitive values. Only the
// underlying network error text is surfaced.
func transportErrText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// parseRetryAfter extracts a delay from provider retry hints. Checks
// Retry-After (seconds or HTTP-date), then OpenAI's x-ratelimit-reset-requests
// (e.g. "2s" or a duration string), then anthropic-ratelimit-reset-requests.
// Every path is bounded to [0, scheduler.MaxRetryHint]: the stored hint lands
// in the record's RetryAfterMs and feeds BackoffFor, whose pathological-hint
// clamp only catches positives - an overflow-wrapped negative or an
// implementation-defined non-finite float conversion must never get stored.
func parseRetryAfter(resp *http.Response) time.Duration {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			switch {
			case secs <= 0:
				return 0
			case secs > int(scheduler.MaxRetryHint/time.Second):
				return scheduler.MaxRetryHint
			default:
				// Within the ceiling the seconds product can no longer wrap.
				return time.Duration(secs) * time.Second
			}
		}
		if ts, err := http.ParseTime(v); err == nil {
			return scheduler.ClampRetryHint(time.Until(ts))
		}
	}
	for _, h := range []string{
		"X-Ratelimit-Reset-Requests",
		"X-Ratelimit-Reset-Tokens",
		"anthropic-ratelimit-requests-reset",
		"anthropic-ratelimit-tokens-reset",
	} {
		if v := resp.Header.Get(h); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				return scheduler.ClampRetryHint(d)
			}
			// NaN and +/-Inf are malformed like any parse failure: fall
			// through to the next candidate. A finite value is range-checked
			// before the float-to-Duration conversion so it can never be
			// out of range.
			if secs, err := strconv.ParseFloat(v, 64); err == nil &&
				!math.IsNaN(secs) && !math.IsInf(secs, 0) {
				switch {
				case secs <= 0:
					return 0
				case secs > scheduler.MaxRetryHint.Seconds():
					return scheduler.MaxRetryHint
				default:
					return time.Duration(secs * float64(time.Second))
				}
			}
		}
	}
	return 0
}

// retryHintFromBody finds an RFC3339 reset instant in a provider error body.
// A 429 often explains the wait in its message ("... resets
// 2026-11-03T23:11:00Z") while sending no Retry-After header; honoring that
// instant lets the retry ladder wait for the window instead of exhausting in
// seconds. Only a future instant bounded by MaxRetryHint is honored; a body
// without a parseable timestamp contributes nothing.
func retryHintFromBody(body []byte) time.Duration {
	for i := 0; i+20 <= len(body); i++ {
		if body[i] < '0' || body[i] > '9' ||
			body[i+4] != '-' || body[i+7] != '-' || body[i+10] != 'T' {
			continue
		}
		ts, err := time.Parse(time.RFC3339, string(body[i:i+20]))
		if err != nil {
			continue
		}
		if d := time.Until(ts); d > 0 {
			return scheduler.ClampRetryHint(d)
		}
		return 0
	}
	return 0
}
