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
			PolicyKey: strings.Join(c.StormStatusCodes, ",") + ":" + strconv.FormatBool(c.StormTransportErrors) + ":" + strconv.FormatBool(c.StormStreamErrors) + ":" + strconv.Itoa(c.StormMaxRetries),
		},
	}
}

// stormRequestState belongs to one handler goroutine, like its metrics record.
// It holds no additional request body or credentials. Initial admission waits
// for readiness; only actual sends acquire exclusive recovery permits.
// Successful HTTP responses settle after relay, so an in-band failure is one
// failed upstream attempt rather than a success followed by a second sample.
type stormRequestState struct {
	rec         *metrics.Record
	response    *scheduler.StormPermit
	responseCtx context.Context
	extra       int
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
	if failed && (!s.cfg().StormStreamErrors || isNonRetryableQuotaErr(state.rec.ErrorType, state.rec.ErrorCode)) {
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

func (s *Server) writeStormQueueError(w http.ResponseWriter, rec *metrics.Record, err error) bool {
	if !errors.Is(err, scheduler.ErrStormQueueFull) && !errors.Is(err, scheduler.ErrStormMaxWait) && !errors.Is(err, scheduler.ErrStormCapacity) {
		return false
	}
	rec.ErrorType = "storm_queue_full"
	rec.ErrorMsg = err.Error()
	header := http.Header{}
	header.Set("Retry-After", strconv.Itoa(s.cfg().QueueRetryAfter))
	writeClientErrorHdr(w, rec, "rate_limit_error", "error storm protection: "+err.Error(), http.StatusTooManyRequests, header)
	return true
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
