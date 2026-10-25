package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// parseErrorBody extracts the provider's structured error detail (type, code,
// message) from a response body via the canonical envelope decoder
// (metrics.ParseErrorEnvelope), defaulting the type when absent. Shared by the
// final-response path and the per-retry-attempt log so both parse identically.
//
// The message is bounded (metrics.TruncatePreview) because a provider's error
// message can echo user-supplied content and is otherwise arbitrary
// provider-controlled bytes - the record stays bounded regardless of
// capture_body_preview. When the (bounded) body doesn't parse (e.g. it was
// truncated mid-token), fall back to a bounded raw prefix so the record keeps a
// classifiable signature instead of a blank one.
func parseErrorBody(b []byte) (typ, code, msg string) {
	if len(b) == 0 {
		return "", "", ""
	}
	if typ, code, msg = metrics.ParseErrorEnvelope(b); typ != "" {
		return typ, code, msg
	}
	if json.Valid(b) {
		// Decodable but null/empty error value: classify, no message.
		return "provider_error", "", ""
	}
	// Unparseable (e.g. an oversized body truncated mid-token): keep a
	// bounded raw prefix so the error is still classifiable.
	return "provider_error", "", metrics.TruncatePreview(string(b))
}

// upstreamMeta is the per-response provider metadata read from response
// headers. readUpstreamMeta is the single owner of the header-name
// vocabulary - the request-id aliases, server, processing time, echoed model
// and rate-limit state - shared by the record's final-response capture and
// the per-attempt capture, so every attempt stores exactly the same
// information. The has* flags preserve the record path's only-when-present
// semantics: a re-capture never zeroes a field the response did not carry.
type upstreamMeta struct {
	RequestID          string
	Server             string
	Model              string
	ProcessingMs       int
	RateLimitRemaining int
	RateLimitLimit     int
	hasProcessingMs    bool
	hasRemaining       bool
	hasLimit           bool
}

// readUpstreamMeta extracts one response's upstream metadata (request id,
// server, processing time, echoed model, rate-limit state) from its headers.
func readUpstreamMeta(h http.Header) upstreamMeta {
	m := upstreamMeta{
		RequestID: firstNonEmpty(
			h.Get("X-Request-Id"),
			h.Get("X-Openai-Request-Id"),
			h.Get("X-Api-Request-Id"),
			h.Get("Request-Id"),
		),
		Server: h.Get("Server"),
		Model:  h.Get("X-Model"),
	}
	m.ProcessingMs, m.hasProcessingMs = headerIntValue(h, "X-Openai-Processing-Ms", "anthropic-processing-ms")
	m.RateLimitRemaining, m.hasRemaining = headerIntValue(h, "X-Ratelimit-Remaining-Requests", "anthropic-ratelimit-requests-remaining")
	m.RateLimitLimit, m.hasLimit = headerIntValue(h, "X-Ratelimit-Limit-Requests", "anthropic-ratelimit-requests-limit")
	return m
}

// captureUpstreamHeaders refreshes the record's upstream header metadata -
// the redacted audit headers, provider request id/server/processing time and
// echoed model, rate-limit state, and the transport's auto-decompress fact -
// from the given response. ServeHTTP captures for the first response; the
// quality loops re-capture after every successful re-send so the record
// describes the attempt whose body the client actually received.
func captureUpstreamHeaders(resp *http.Response, rec *metrics.Record, authHeader string) {
	rec.ResponseHeaders = captureHeaders(resp.Header, authHeader)
	// The wire-compression fact lives only here: on auto-decompress the
	// transport deletes Content-Encoding and Content-Length from the header
	// map, so the captured headers cannot show it. resp.Uncompressed is set
	// by the transport exactly when it decoded a gzip body, and it is read
	// before the relay consumes the body.
	rec.UpstreamGzip = resp.Uncompressed
	m := readUpstreamMeta(resp.Header)
	rec.ProviderRequestID = m.RequestID
	rec.ProviderServer = m.Server
	if m.Model != "" {
		rec.ProviderModel = m.Model
	}
	if m.hasProcessingMs {
		rec.ProcessingMs = m.ProcessingMs
	}
	if m.hasRemaining {
		rec.RateLimitRemaining = m.RateLimitRemaining
	}
	if m.hasLimit {
		rec.RateLimitLimit = m.RateLimitLimit
	}
}

// copyFlushing relays a body with one Flush after each successful write, so a
// long-lived event-stream error body reaches the client as it arrives. The
// stack read buffer avoids io.Copy's per-call allocation.
func copyFlushing(w io.Writer, body io.Reader, flusher http.Flusher) error {
	var buf [32 * 1024]byte
	for {
		n, rerr := body.Read(buf[:])
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
	}
}

// captureUpstreamMeta stores the same upstream-response metadata on an
// absorbed attempt - the attempt-log twin of captureUpstreamHeaders
// (readUpstreamMeta owns the vocabulary). The attempt is built fresh for
// this response, so absent headers stay zero; there is no previous value to
// preserve and nothing re-captures an attempt later.
func captureUpstreamMeta(at *metrics.RetryAttempt, resp *http.Response, authHeader string) {
	at.ResponseHeaders = captureHeaders(resp.Header, authHeader)
	m := readUpstreamMeta(resp.Header)
	at.ProviderRequestID = m.RequestID
	at.ProviderServer = m.Server
	at.ProviderModel = m.Model
	at.ProcessingMs = m.ProcessingMs
	at.RateLimitRemaining = m.RateLimitRemaining
	at.RateLimitLimit = m.RateLimitLimit
}

// captureErrorFromResponse reads a bounded prefix of an error response body
// under the shared whole-document bound, then re-serves the captured prefix
// followed by the live remainder so the client still receives the full
// original body. The bound is what keeps a provider that holds or drips a
// small error body from delaying the handler past whole_document_read_timeout
// before the translated or buffered owner performs its own bounded read. A
// timed-out read closes the body; the captured prefix is still re-served and
// the owner's read then fails closed.
func (s *Server) captureErrorFromResponse(ctx context.Context, resp *http.Response, rec *metrics.Record) {
	b, err := s.readWholeDocument(ctx, resp.Body, maxErrBodyBytes)
	if err == nil && len(b) > 0 {
		rec.ErrorType, rec.ErrorCode, rec.ErrorMsg = parseErrorBody(b)
	}
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(b), resp.Body))
}

// prefixRemainderBody re-serves a captured body prefix followed by the live
// remainder of the original body, closing that original body exactly once on
// Close. Used when a peeked error body overflowed the capture cap: the final
// response must relay byte-for-byte ("an exhausted budget surfaces the last
// upstream response verbatim"), so the unread remainder stays live instead of
// being truncated away.
type prefixRemainderBody struct {
	prefix []byte
	rest   io.ReadCloser
}

func (b *prefixRemainderBody) Read(p []byte) (int, error) {
	if len(b.prefix) > 0 {
		n := copy(p, b.prefix)
		b.prefix = b.prefix[n:]
		return n, nil
	}
	return b.rest.Read(p)
}

func (b *prefixRemainderBody) Close() error { return b.rest.Close() }

// quota429Peek is the single owner of the bounded durable-quota peek on a 429
// response: it reads at most maxErrBodyBytes+1 bytes of the error body and
// classifies the in-cap envelope through the canonical parser
// (metrics.ParseErrorEnvelope + isNonRetryableQuotaErr). The caller arms the
// shared upstream idle guard on resp.Body at receipt (doWithRetry and
// openCursorHTTP), so a tarpit that sends headers and stalls cannot hold this
// read past upstream_stream_idle_timeout. An in-cap body is closed and reported
// together with its durable classification and the envelope's protocol tokens
// (type, code - the quota-pause reason label); an overflowed body (longer than
// maxErrBodyBytes) is deliberately left OPEN and unclassified
// (durable=false, leftOpen=true) - the classification stays bounded by the
// cap. Each transport then applies its own overflow policy at the call site:
// the generic relay re-serves prefix + live remainder verbatim, the cursor
// transport closes and re-wraps only the truncated prefix.
func quota429Peek(resp *http.Response) (durable bool, typ, code string, peeked []byte, leftOpen bool) {
	peeked, _ = io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes+1))
	if len(peeked) > maxErrBodyBytes {
		return false, "", "", peeked, true
	}
	resp.Body.Close()
	typ, code, _ = metrics.ParseErrorEnvelope(peeked)
	return isNonRetryableQuotaErr(typ, code), typ, code, peeked, false
}

// withSendTimeout derives the per-attempt upstream send context: the
// X-Proxy-Timeout-Ms routing header bounds ONE send (upstream headers + that
// body's relay phase), so a fresh deadline anchors at each attempt's Do -
// waits that precede the send (operator holds, group serialization, backoff,
// Retry-After windows) run on the base request context and never burn the
// budget. With no caller deadline the attempt still gets its own cancelable
// child context: the shared upstream body idle guard cancels that context to
// unblock a stalled transport read, so the no-timeout path must not degrade to
// a no-op cancel. Callers always defer the returned func.
func withSendTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

// upstreamTiming decomposes ONE upstream send attempt's transport time via
// net/http/httptrace: the dial phase (ConnectStart to ConnectDone), the TLS
// handshake (TLSHandshakeStart to TLSHandshakeDone), the wait for the first
// byte of response headers (the attempt's own send start to
// GotFirstResponseByte), and the pool-hit fact (GotConn's
// GotConnInfo.Reused). start is the attempt's send start - attemptStart,
// stamped by admitSendAttempt after every admission gate - so
// upstream_ttfb_ms anchors exactly where FinalAttemptAt/TTFT anchor.
//
// The struct lives on the stack of ONE attempt and is wired into that
// attempt's own context: an abandoned attempt's trace dies with its context,
// so the record is only ever stamped by adopt at the points where a send
// driver returns the attempt whose response the client will receive - the
// same adoption FinalAttemptAt follows. Attempts absorbed inside the drivers
// (429/5xx/transport) never reach an adoption point, so their observations
// are dropped wholesale and the fields keep describing the last adopted
// attempt.
//
// Both transports in use fire these hooks: the stdlib net/http transport
// fires ConnectStart/ConnectDone (via the nettrace context value
// httptrace.WithClientTrace installs), the TLS handshake pair, GotConn with
// GotConnInfo.Reused and GotFirstResponseByte; x/net/http2 (the cursor
// bidi transport) reads ContextClientTrace and fires GotConn with its own
// reused flag and GotFirstResponseByte.
type upstreamTiming struct {
	// mu guards every field: httptrace fires the connect hooks on the
	// transport's dial goroutines, and a Happy-Eyeballs losing dial can stamp
	// ConnectDone while the request goroutine has already adopted the winning
	// connection's observations. Dial events are rare per attempt, so a small
	// mutex is the obviously-correct owner.
	mu        sync.Mutex
	start     time.Time // the attempt's send start (post-admission)
	connectS  time.Time // first ConnectStart (Happy Eyeballs can dial more than once)
	connectD  time.Time // last ConnectDone: the whole dial phase, fallbacks included
	tlsS      time.Time
	tlsD      time.Time
	firstByte time.Time
	reused    bool
}

// trace returns the httptrace.ClientTrace whose closures stamp this
// attempt's observations. Each hook is a plain timestamp: a hook that never
// fires (no dial on a pooled connection, no handshake on plaintext, no
// headers on a transport failure) simply leaves its zero value, and adopt
// renders that as "not observed".
//
// tlsPossible comes from the target's own scheme (target.tlsPossible), so the
// handshake pair is installed only where a handshake can happen. Redirects are
// never followed (preserveUpstreamRedirect), so one attempt's request always
// carries its target's scheme. An absent hook is indistinguishable from a hook
// that cannot fire: both leave the zero value adopt renders as "not observed",
// so a plaintext target saves the two closures and shifts no timing semantics.
func (u *upstreamTiming) trace(tlsPossible bool) *httptrace.ClientTrace {
	trace := &httptrace.ClientTrace{
		ConnectStart: func(network, addr string) {
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.connectS.IsZero() {
				u.connectS = time.Now()
			}
		},
		ConnectDone: func(network, addr string, err error) {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.connectD = time.Now()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.reused = info.Reused
		},
		GotFirstResponseByte: func() {
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.firstByte.IsZero() {
				u.firstByte = time.Now()
			}
		},
	}
	if tlsPossible {
		trace.TLSHandshakeStart = func() {
			u.mu.Lock()
			defer u.mu.Unlock()
			if u.tlsS.IsZero() {
				u.tlsS = time.Now()
			}
		}
		trace.TLSHandshakeDone = func(state tls.ConnectionState, err error) {
			u.mu.Lock()
			defer u.mu.Unlock()
			u.tlsD = time.Now()
		}
	}
	return trace
}

// adopt stamps the record with this attempt's four observations. It runs
// only when the attempt's response is the one the relay commits to, so the
// persisted fields describe the final adopted attempt. Every observed
// positive interval rounds up to 1ms like FinalizeRecord's TTFT: 0 is
// reserved for "not observed", and an interval so short the clock resolved
// it to zero still means the hooks fired - observed, never absent.
func (u *upstreamTiming) adopt(rec *metrics.Record) {
	u.mu.Lock()
	defer u.mu.Unlock()
	rec.UpstreamConnectMs = observedMs(u.connectS, u.connectD)
	rec.UpstreamTLSMs = observedMs(u.tlsS, u.tlsD)
	rec.UpstreamTTFBMs = observedMs(u.start, u.firstByte)
	rec.UpstreamConnReused = u.reused
}

// observedMs renders one hook-pair interval as integer milliseconds: 0 only
// when either timestamp is missing (the hook never fired - "not observed");
// a stamped pair is an observation, so it never reports below 1ms.
func observedMs(start, end time.Time) int64 {
	if start.IsZero() || end.IsZero() {
		return 0
	}
	if ms := end.Sub(start).Milliseconds(); ms > 0 {
		return ms
	}
	return 1
}

// admitSendAttempt gates one upstream send attempt on the retry-driver
// admission policy shared by the generic relay and the cursor bidi driver:
// the WaitSend hold rule (the opening send honors operator holds when it is
// itself a retry - "a retry is a new send and waits"; later attempts always
// do), the storm permit, and the attemptStart stamp taken only after every
// admission gate so waits never burn send deadlines. own is WaitSend's
// returned token (unchanged on error). The caller keeps its own
// end-of-request choreography.
func (s *Server) admitSendAttempt(ctx context.Context, groupKey string, hooks scheduler.WaiterHooks, firstSendIsRetry bool, attempt int, own bool) (bool, *scheduler.StormPermit, time.Time, error) {
	own, err := s.scheduler.WaitSend(ctx, groupKey, hooks, attempt > 0 || firstSendIsRetry, own)
	if err != nil {
		return own, nil, time.Time{}, err
	}
	permit, err := s.waitStorm(ctx, hooks)
	if err != nil {
		return own, nil, time.Time{}, err
	}
	return own, permit, time.Now(), nil
}

// absorbTransportRetry books one absorbed transport-level failure: the
// attempt-log entry (transport class, the caller's rendered message), the
// retry counter with a live publish, and the Trip that paces the group and
// keeps the send token claimed. Which transport errors are retryable, the
// message rendering, and the give-up condition stay per-transport. Returns
// the send-token ownership after Trip.
func (s *Server) absorbTransportRetry(rec *metrics.Record, groupKey, msg string, own bool) bool {
	rec.Attempts = append(rec.Attempts, metrics.RetryAttempt{
		StatusCode: 0,
		ErrorType:  "transport",
		ErrorMsg:   msg,
		At:         time.Now(),
	})
	rec.Retries++
	s.publishUpdate(rec)
	return s.scheduler.Trip(groupKey, s.scheduler.BackoffFor(groupKey, 0)) || own
}

// absorbHTTPRetry books one absorbed retryable HTTP response: the provider
// retry hint (parseRetryAfter) and the attempt-log entry (status, bounded
// error-body envelope via parseErrorBody, hint milliseconds, the response's
// upstream metadata via captureUpstreamMeta), appended with the retry
// counter. Returns the hint - the pacing floor for the caller's Trip and
// RetryAfterMs stamp. The bounded body read, per-send cleanup, record
// stamps and publish order stay per-transport: the generic relay interplays
// with the durable-quota 429 peek and clears stale error fields after its
// publish; the cursor driver closes its duplex pipe and stamps before
// publishing.
func (s *Server) absorbHTTPRetry(rec *metrics.Record, resp *http.Response, errBody []byte, authHeader string) time.Duration {
	retryAfter := parseRetryAfter(resp)
	at := metrics.RetryAttempt{
		StatusCode:   resp.StatusCode,
		RetryAfterMs: int(retryAfter.Milliseconds()),
		At:           time.Now(),
	}
	at.ErrorType, at.ErrorCode, at.ErrorMsg = parseErrorBody(errBody)
	captureUpstreamMeta(&at, resp, authHeader)
	rec.Attempts = append(rec.Attempts, at)
	rec.Retries++
	return retryAfter
}
