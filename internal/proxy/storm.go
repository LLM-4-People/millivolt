package proxy

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

func schedulerOptions(c *config.Config) scheduler.Options {
	return scheduler.Options{
		MaxConcurrent: c.MaxConcurrent, MaxQueueSize: c.MaxQueueSize,
		MaxWait: c.MaxQueueWait, BaseBackoff: c.BaseBackoff, MaxBackoff: c.MaxBackoff,
		Storm: scheduler.StormOptions{
			Enabled: c.StormEnabled, ProviderEnabled: c.StormProviderEnabled, ModelEnabled: c.StormModelEnabled,
			Window: c.StormWindow, MinSamples: c.StormMinSamples, ErrorPercent: c.StormErrorPercent,
			InitialBackoff: c.StormInitialBackoff, MaxBackoff: c.StormMaxBackoff,
			BackoffMultiplier: c.StormBackoffMultiplier, JitterPercent: c.StormJitterPercent,
			RecoverySuccesses: c.StormRecoverySuccesses, MaxQueue: c.StormMaxQueue,
			MaxWait: c.StormMaxWait, MaxScopes: c.StormMaxScopes,
			QuotaEnabled:           c.QuotaPauseMode == config.QuotaPauseRetry,
			QuotaRecoverySuccesses: c.QuotaPauseRecoverySuccesses,
			PolicyKey:              c.QuotaPauseMode + ":" + strings.Join(c.StormStatusCodes, ",") + ":" + strconv.FormatBool(c.StormTransportErrors) + ":" + strconv.FormatBool(c.StormStreamErrors) + ":" + strconv.Itoa(c.StormMaxRetries),
		},
	}
}

// stormRequestState belongs to one handler goroutine, like its metrics record.
// It holds no additional request body or credentials. Initial admission waits
// for readiness; only actual sends acquire exclusive recovery permits.
// Successful HTTP responses settle after relay, so an in-band failure is one
// failed upstream attempt rather than a success followed by a second sample.
// format is the target's wire format: the in-band quota branch of
// finishStormResponse gates the quota pause on the OpenAI-wire family.
type stormRequestState struct {
	rec         *metrics.Record
	response    *scheduler.StormPermit
	responseCtx context.Context
	extra       int
	format      string
	observation scheduler.StormObservation
}

type stormContextKey struct{}

func stormState(ctx context.Context) *stormRequestState {
	state, _ := ctx.Value(stormContextKey{}).(*stormRequestState)
	return state
}

func stormObservation(ctx context.Context) *scheduler.StormObservation {
	if state := stormState(ctx); state != nil {
		return &state.observation
	}
	return nil
}

func (s *Server) waitStorm(ctx context.Context, hooks scheduler.WaiterHooks) (*scheduler.StormPermit, error) {
	return s.waitStormGate(ctx, hooks, false)
}

func (s *Server) waitStormGate(ctx context.Context, hooks scheduler.WaiterHooks, admission bool) (*scheduler.StormPermit, error) {
	state := stormState(ctx)
	model := ""
	if state != nil {
		model = state.rec.Model
	}
	start := time.Now()
	waited := false
	onWait := func() {
		waited = true
		if hooks.OnWait != nil {
			hooks.OnWait()
		}
		if state != nil {
			state.rec.Throttled = true
			s.publishUpdate(state.rec)
		}
	}
	var p *scheduler.StormPermit
	var err error
	if admission {
		err = s.scheduler.WaitStormReady(ctx, hooks.Provider, model, onWait)
	} else {
		p, err = s.scheduler.WaitStorm(ctx, hooks.Provider, model, onWait)
	}
	if waited && state != nil {
		state.rec.Throttled = false
		state.rec.QueueWaitMs += time.Since(start).Milliseconds()
		s.publishUpdate(state.rec)
	}
	return p, err
}

// observeStormHTTP accepts only configured transient statuses. The existing
// bounded quota-envelope classifier must have run first for a 429. Error
// reasons are protocol labels, never potentially sensitive provider messages.
func (s *Server) observeStormHTTP(ctx context.Context, p *scheduler.StormPermit, resp *http.Response, retryable bool) bool {
	if retryable && slices.Contains(s.cfg().StormStatusCodes, strconv.Itoa(resp.StatusCode)) {
		return p.Observe(true, "HTTP "+strconv.Itoa(resp.StatusCode), parseRetryAfter(resp), stormObservation(ctx))
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if state := stormState(ctx); state != nil {
			state.response = p
			state.responseCtx = nil // Native runs have no per-send body deadline.
		} else {
			p.Observe(false, "", 0)
		}
	} else {
		p.Cancel()
	}
	return false
}

func (s *Server) observeStormTransport(ctx context.Context, p *scheduler.StormPermit, retryable bool) bool {
	if retryable && s.cfg().StormTransportErrors {
		return p.Observe(true, "transport error", 0, stormObservation(ctx))
	}
	p.Cancel()
	return false
}

func (s *Server) finishStormResponse(ctx context.Context, qualityFailure bool) {
	state := stormState(ctx)
	if state == nil || state.response == nil {
		return
	}
	p := state.response
	state.response = nil
	// Cleanup cancellation is ordinary. A fired per-send deadline remains
	// DeadlineExceeded even after cleanup and is not outage evidence.
	deadlineFired := state.responseCtx != nil && state.responseCtx.Err() == context.DeadlineExceeded
	state.responseCtx = nil
	if ctx.Err() != nil || state.rec.ClientDisconnected || deadlineFired {
		p.Cancel()
		return
	}
	failed := qualityFailure || state.rec.ErrorType != ""
	if failed && isNonRetryableQuotaErr(state.rec.ErrorType, state.rec.ErrorCode) {
		// An in-band durable quota error inside a 200 stream arms the
		// provider pause like its 429 twin, but nothing is parked for this
		// request: its response already reached the client verbatim. The
		// settling permit may be this gate's probe; on unarmed targets the
		// release is the historical Cancel.
		s.settleQuotaFailure(p, state.format, state.rec.Client, state.rec.Provider,
			metrics.NonRetryableQuotaClass(state.rec.ErrorType, state.rec.ErrorCode), 0)
		return
	}
	if failed && !s.cfg().StormStreamErrors {
		p.Cancel()
		return
	}
	reason := ""
	if failed {
		reason = "response error"
	}
	p.Observe(failed, reason, 0, &state.observation)
}

// Ordinary retries keep their existing budget. Extra storm retries are shared
// by every send/quality re-ask in this logical request and remain finite even
// across settings reloads or repeated response-quality attempts.
func (s *Server) allowStormRetry(ctx context.Context, active bool) bool {
	state := stormState(ctx)
	cfg := s.cfg()
	if !active || !cfg.StormEnabled || state == nil || ctx.Err() != nil || state.extra >= cfg.StormMaxRetries {
		return false
	}
	state.extra++
	return true
}

// isStormQueueRejection reports whether err is an operator storm-queue
// rejection - the classes stormQueueErrorRecord stamps as 429.
func isStormQueueRejection(err error) bool {
	return errors.Is(err, scheduler.ErrStormQueueFull) ||
		errors.Is(err, scheduler.ErrStormMaxWait) ||
		errors.Is(err, scheduler.ErrStormCapacity)
}

// stormQueueErrorRecord stamps the record for a storm-queue rejection with
// the decided HTTP status (429 - the status the proxy sent, or would have
// sent on a committed socket) and returns the in-band error envelope. The 429
// classifies the record as rate-limited (HasRateLimit true; IsError false
// because metrics short-circuits on 429 before the error type - 429 alone is
// not an error - unless an absorbed attempt was a genuine 5xx, which metrics
// still honors), the same class as the request plane's queue-full rejection.
// stormQueueErrorRecord is the single owner of the envelope's type and
// message: writeStormQueueError writes it as an HTTP error (pre-commit
// sockets), while relay failure paths on an already committed socket emit it
// in-band via emitErrorSSE. ok=false when err is not a storm-queue rejection.
func (s *Server) stormQueueErrorRecord(rec *metrics.Record, err error) (typ, msg string, ok bool) {
	if !isStormQueueRejection(err) {
		return "", "", false
	}
	rec.StatusCode = http.StatusTooManyRequests
	rec.ErrorType = "storm_queue_full"
	rec.ErrorMsg = err.Error()
	return "rate_limit_error", "error storm protection: " + err.Error(), true
}

func (s *Server) writeStormQueueError(w http.ResponseWriter, rec *metrics.Record, err error) bool {
	typ, msg, ok := s.stormQueueErrorRecord(rec, err)
	if !ok {
		return false
	}
	writeClientErrorHdr(w, rec, typ, msg, http.StatusTooManyRequests, s.retryAfterHeader())
	return true
}

// writeTransportFailure renders the shared epilogue when a fresh upstream
// send failed with no HTTP response: an operator storm-queue rejection
// (rate_limit_error + Retry-After) wins when it applies, otherwise the client
// receives the canonical upstream_unreachable 502. The generic doWithRetry
// epilogue and the cursor bidi open - whose five-statement blocks were
// byte-identical - both route through here, so the two can never drift. The
// relay's quality re-send epilogues do NOT join - each surface sinks the
// failure differently (classifyResendFailure owns their shared record
// stamps): the non-streaming loop's status line is still open, so it answers
// the client with an api_error JSON http.Error; the streaming loop's status
// line is already committed, so it emits the same upstream_unreachable
// classification in-band on the SSE socket (relay.go).
func (s *Server) writeTransportFailure(w http.ResponseWriter, rec *metrics.Record, err error) {
	if s.writeStormQueueError(w, rec, err) {
		return
	}
	writeClientError(w, rec, typeUpstreamUnreachable, "upstream error: "+transportErrText(err), http.StatusBadGateway)
}

// StormSnapshot shares scheduler-authoritative status with embedded bootstrap
// and ordinary dashboard refreshes. It does no history or storage work.
func (s *Server) StormSnapshot() map[string]any {
	cfg := s.cfg()
	return map[string]any{
		"enabled": cfg.StormEnabled, "banner_enabled": cfg.StormBannerEnabled,
		"storms": s.scheduler.StormSnapshot(),
	}
}
