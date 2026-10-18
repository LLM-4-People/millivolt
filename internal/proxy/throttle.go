package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// Persistent provider-wide caps. Set from the dashboard or from
// X-Proxy-Limit-* headers: one client writes, every client of that
// provider is limited until the cap is changed or turned off.

const (
	hdrLimitConcurrency = "X-Proxy-Limit-Concurrency"
	hdrLimitRequests    = "X-Proxy-Limit-Requests"
	hdrLimitTokens      = "X-Proxy-Limit-Tokens"
)

// Safety guardrails on a header/UI-supplied cap. Not user-tunable: they
// bound a hostile or typo'd value so it cannot stall the provider forever
// or overflow the token bucket. Window is 1s..24h (same ceiling as
// scheduler.MaxRetryHint - a daily quota window).
const (
	maxLimitConcurrency = 100_000
	maxLimitRequests    = 1_000_000_000
	maxLimitTokens      = 1_000_000_000_000
	minLimitWindow      = time.Second
	maxLimitWindow      = 24 * time.Hour
)

// Durable throttle-policy bounds and background-writer cadence. These are
// internal safety/implementation constants, not user tunables.
//
// throttleProviderLabelMaxBytes covers the routing boundary's DNS hostname
// bound (253 bytes) plus an IP authority's brackets and port. The scheduler
// already refuses a new header-created gate past 4096 entries; combined with
// this label bound it also bounds the persisted document.
//
// throttlePersistDocMaxBytes is a final safety net. With 4096 labels of 255
// bytes at JSON's worst-case six-byte escape per byte, plus per-entry fields,
// the document stays below 8 MiB. A document at the cap is a bug, not normal
// operator state, so refusing it keeps the dirty generation retrying rather
// than writing an unbounded blob.
const (
	throttleProviderLabelMaxBytes = 255
	throttlePersistDocMaxBytes    = 8 << 20

	throttlePersistDebounce = 250 * time.Millisecond
	throttlePersistRetryMin = 100 * time.Millisecond
	throttlePersistRetryMax = 30 * time.Second
)

type throttlePersist interface {
	SaveThrottle(ctx context.Context, raw []byte) error
	LoadThrottle(ctx context.Context) ([]byte, error)
}

type persistedThrottle struct {
	Provider    string    `json:"provider"`
	Concurrency int       `json:"concurrency,omitempty"`
	Requests    int64     `json:"requests,omitempty"`
	ReqWindow   string    `json:"request_window,omitempty"`
	Tokens      int64     `json:"tokens,omitempty"`
	TokWindow   string    `json:"token_window,omitempty"`
	Source      string    `json:"source,omitempty"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
}

type persistedThrottleDoc struct {
	Throttles []persistedThrottle `json:"throttles,omitempty"`
}

type throttleRuntime struct {
	persist throttlePersist

	// mu owns the generation counters and the writer lifecycle flags. The
	// scheduler owns policy; this lock never wraps a scheduler call.
	mu           sync.Mutex
	persistGen   uint64
	persistedGen uint64
	started      bool
	closed       bool

	// persistMu lets concurrent persistence loops (operator edits and the one
	// background writer) overlap, while a flush takes the exclusive side so no
	// stale write can land after it. Every write is bounded by the store query
	// timeout, which also bounds the flush's wait for in-flight loops.
	persistMu sync.RWMutex
	// wake is the bounded dirty signal. At most one pending notification is
	// retained; the writer reads the latest generation rather than a queue of
	// changes, so a burst collapses into the newest policy.
	wake chan struct{}
	stop chan struct{}
	done chan struct{}

	// debounce coalesces a burst before the writer snapshots. retryMin and
	// retryMax bound the backoff after a failed write.
	debounce time.Duration
	retryMin time.Duration
	retryMax time.Duration
}

// init sizes the lifecycle channels and cadence. Called once from New; tests
// may override debounce/retryMin/retryMax before the first dirty signal.
func (t *throttleRuntime) init() {
	t.wake = make(chan struct{}, 1)
	t.stop = make(chan struct{})
	t.done = make(chan struct{})
	t.debounce = throttlePersistDebounce
	t.retryMin = throttlePersistRetryMin
	t.retryMax = throttlePersistRetryMax
}

func (s *Server) attachThrottlePersist(p throttlePersist) {
	s.throttle.mu.Lock()
	s.throttle.persist = p
	s.throttle.mu.Unlock()
	s.restoreThrottlePolicies(p)
	s.startThrottleWriterIfDirty()
}

// startThrottleWriterIfDirty launches the one background writer when a policy
// generation predates the attach (for example a test attaches after changes).
// Normal startup has no dirty generation and starts the writer on the first
// header change instead.
func (s *Server) startThrottleWriterIfDirty() {
	s.throttle.mu.Lock()
	start := !s.throttle.closed && !s.throttle.started &&
		s.throttle.persist != nil && s.throttle.persistGen != s.throttle.persistedGen
	if start {
		s.throttle.started = true
	}
	s.throttle.mu.Unlock()
	if start {
		go s.runThrottlePersister()
		s.throttle.signal()
	}
}

func (s *Server) restoreThrottlePolicies(p throttlePersist) {
	ctx, cancel := s.storeQueryCtx()
	defer cancel()
	raw, err := p.LoadThrottle(ctx)
	if err != nil {
		log.Printf("throttle: load: %v", err)
		return
	}
	items := decodeThrottles(raw)
	if len(items) == 0 {
		return
	}
	var ts []scheduler.Throttle
	for _, it := range items {
		t, err := persistedToThrottle(it)
		if err != nil {
			log.Printf("throttle: skip %s: %v", it.Provider, err)
			continue
		}
		s.pause.providers.add(t.Provider)
		ts = append(ts, t)
	}
	s.scheduler.RestoreThrottles(ts)
	if len(ts) > 0 {
		log.Printf("throttle restored (%d provider caps)", len(ts))
	}
}

func decodeThrottles(raw []byte) []persistedThrottle {
	if len(raw) == 0 {
		return nil
	}
	var doc persistedThrottleDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		log.Printf("throttle: decode: %v", err)
		return nil
	}
	return doc.Throttles
}

func persistedToThrottle(it persistedThrottle) (scheduler.Throttle, error) {
	if strings.TrimSpace(it.Provider) == "" {
		return scheduler.Throttle{}, fmt.Errorf("provider required")
	}
	if err := validateThrottleProvider(it.Provider); err != nil {
		return scheduler.Throttle{}, err
	}
	lim := scheduler.Limit{Concurrency: it.Concurrency, Requests: it.Requests, Tokens: it.Tokens}
	if it.Requests > 0 {
		d, err := parseLimitWindow(it.ReqWindow)
		if err != nil {
			return scheduler.Throttle{}, err
		}
		lim.ReqWindow = d
	}
	if it.Tokens > 0 {
		d, err := parseLimitWindow(it.TokWindow)
		if err != nil {
			return scheduler.Throttle{}, err
		}
		lim.TokWindow = d
	}
	if err := validateLimit(lim); err != nil {
		return scheduler.Throttle{}, err
	}
	return scheduler.Throttle{
		Provider:  it.Provider,
		Limit:     lim,
		Source:    it.Source,
		UpdatedBy: it.UpdatedBy,
		UpdatedAt: it.UpdatedAt,
	}, nil
}

func throttleToPersisted(t scheduler.Throttle) persistedThrottle {
	it := persistedThrottle{
		Provider:    t.Provider,
		Concurrency: t.Limit.Concurrency,
		Requests:    t.Limit.Requests,
		Tokens:      t.Limit.Tokens,
		Source:      t.Source,
		UpdatedBy:   t.UpdatedBy,
		UpdatedAt:   t.UpdatedAt,
	}
	if t.Limit.ReqWindow > 0 {
		it.ReqWindow = config.FormatDuration(t.Limit.ReqWindow)
	}
	if t.Limit.TokWindow > 0 {
		it.TokWindow = config.FormatDuration(t.Limit.TokWindow)
	}
	return it
}

func validateThrottleProvider(provider string) error {
	if len(provider) > throttleProviderLabelMaxBytes {
		return fmt.Errorf("provider label must be at most %d bytes", throttleProviderLabelMaxBytes)
	}
	return nil
}

func (s *Server) applyThrottle(t scheduler.Throttle) error {
	return s.updateThrottle(t.Provider, func(scheduler.Throttle) scheduler.Throttle { return t })
}

// updateThrottle is the synchronous operator edit path. It applies the policy,
// then waits for the write that covers that policy generation so the endpoint
// can report a persistence warning against live state.
func (s *Server) updateThrottle(provider string, update func(scheduler.Throttle) scheduler.Throttle) error {
	if err := validateThrottleProvider(provider); err != nil {
		return err
	}
	if !s.scheduler.UpdateThrottle(provider, update) {
		return nil
	}
	s.pause.providers.add(provider)
	return s.persistThrottleSync()
}

// updateThrottleFromHeaders applies a validated client header patch without
// durable I/O. The coalescing writer owns persistence, so inference latency is
// independent of the number of gates and of the store.
func (s *Server) updateThrottleFromHeaders(provider string, update func(scheduler.Throttle) scheduler.Throttle) error {
	if err := validateThrottleProvider(provider); err != nil {
		return err
	}
	if !s.scheduler.UpdateThrottle(provider, update) {
		return nil
	}
	s.pause.providers.add(provider)
	s.markThrottleDirty()
	return nil
}

// latestThrottleSnap captures the scheduler's provider caps in the persisted
// shape. Occupancy is ephemeral and never stored, so this uses the policy-only
// listing rather than the dashboard's live-occupancy scan.
func (s *Server) latestThrottleSnap() []persistedThrottle {
	items := s.scheduler.ThrottlePolicies()
	snap := make([]persistedThrottle, 0, len(items))
	for _, t := range items {
		snap = append(snap, throttleToPersisted(t))
	}
	return snap
}

// persistThrottleSync advances the policy generation and writes the latest
// snapshot. The generation guard means a concurrent header change is either
// included in this write or written by the next iteration; only the latest
// generation can finish successfully.
func (s *Server) persistThrottleSync() error {
	s.throttle.mu.Lock()
	s.throttle.persistGen++
	s.throttle.mu.Unlock()
	if err := s.persistThrottleLatest(context.Background()); err != nil {
		return &operatorPersistenceError{err}
	}
	return nil
}

// markThrottleDirty advances the generation and wakes the one lifecycle-owned
// writer. It never waits for durable I/O and never starts a goroutine per
// request: the writer is started at most once and the wake channel holds one
// pending signal, however many changes arrive.
func (s *Server) markThrottleDirty() {
	t := &s.throttle
	t.mu.Lock()
	t.persistGen++
	start := false
	if !t.closed && !t.started && t.persist != nil {
		t.started = true
		start = true
	}
	t.mu.Unlock()
	if start {
		go s.runThrottlePersister()
	}
	t.signal()
}

func (t *throttleRuntime) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// persistThrottleLatest writes the newest policy generation. Concurrent
// loops may overlap; the generation check after each write means a stale
// snapshot is always followed by the latest policy before that loop returns.
// A flush takes the exclusive side of persistMu, so it runs only after every
// in-flight loop has converged.
func (s *Server) persistThrottleLatest(ctx context.Context) error {
	s.throttle.persistMu.RLock()
	defer s.throttle.persistMu.RUnlock()
	return s.persistThrottleLatestLocked(ctx)
}

func (s *Server) persistThrottleLatestLocked(ctx context.Context) error {
	t := &s.throttle
	// A loop that writes a snapshot superseded by a newer generation must
	// write the latest policy again before returning. Without this, two
	// overlapping writers could leave the older snapshot as the last durable
	// value even though persistedGen already named the newer generation.
	rewrite := false
	for {
		t.mu.Lock()
		gen := t.persistGen
		if gen == t.persistedGen && !rewrite {
			t.mu.Unlock()
			return nil
		}
		target := t.persist
		t.mu.Unlock()
		if target == nil {
			// Memory-only policy: leave the generation dirty so a later attach
			// can persist it, and report success to callers.
			return nil
		}
		snap := s.latestThrottleSnap()
		err := s.writeThrottleSnap(ctx, target, snap)
		t.mu.Lock()
		if t.persistGen == gen {
			if err == nil {
				t.persistedGen = gen
			}
			t.mu.Unlock()
			return err
		}
		// The generation moved while this write was in flight: the snapshot
		// (and any error) is superseded. Refetch and write the latest policy.
		rewrite = true
		t.mu.Unlock()
	}
}

// runThrottlePersister is the single background writer. It waits for a dirty
// signal, coalesces a burst for the debounce window, then persists until the
// latest generation is durable. A failed write is retried with exponential
// backoff; the dirty generation is never cleared by a failure. The stop
// channel is the only exit path, so Close can join it without a busy loop.
func (s *Server) runThrottlePersister() {
	t := &s.throttle
	defer close(t.done)
	for {
		select {
		case <-t.stop:
			return
		case <-t.wake:
		}
		if !s.waitThrottleQuiet() {
			return
		}
		backoff := t.retryMin
		for {
			err := s.persistThrottleLatest(context.Background())
			if err == nil {
				break
			}
			// persistOperatorDocCtx already logged the store failure. Wait the
			// full backoff even when new changes wake us, so a failing store
			// cannot be hit once per request.
			if !s.waitThrottleRetry(&backoff) {
				return
			}
		}
	}
}

// waitThrottleQuiet restarts the debounce window on every dirty signal and
// returns true once the burst has been quiet for the configured window.
func (s *Server) waitThrottleQuiet() bool {
	t := &s.throttle
	timer := time.NewTimer(t.debounce)
	defer timer.Stop()
	for {
		select {
		case <-t.stop:
			return false
		case <-t.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(t.debounce)
		case <-timer.C:
			return true
		}
	}
}

// waitThrottleRetry waits one backoff step and doubles it up to retryMax. A
// new dirty wake is left pending; the next persist attempt reads the latest
// generation, and the backoff keeps a failing sink from being hammered.
func (s *Server) waitThrottleRetry(backoff *time.Duration) bool {
	t := &s.throttle
	timer := time.NewTimer(*backoff)
	defer timer.Stop()
	select {
	case <-t.stop:
		return false
	case <-timer.C:
		*backoff *= 2
		if *backoff > t.retryMax {
			*backoff = t.retryMax
		}
		return true
	}
}

func (s *Server) writeThrottleSnap(ctx context.Context, p throttlePersist, items []persistedThrottle) error {
	return s.persistOperatorDocCtx(ctx, "throttle",
		persistedThrottleDoc{Throttles: items}, throttlePersistDocMaxBytes, p.SaveThrottle)
}

// FlushThrottles persists the latest policy within the bounded storage
// timeout. Restart handoff calls it before the child opens the database; it
// leaves the background writer running so a failed handoff can resume.
func (s *Server) FlushThrottles(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg().StorageQueryTimeout)
	defer cancel()
	s.throttle.persistMu.Lock()
	defer s.throttle.persistMu.Unlock()
	return s.persistThrottleLatestLocked(ctx)
}

// CloseThrottlePersist stops the writer and flushes the last generation before
// storage closes. It is idempotent and safe to call concurrently: the stop
// channel is closed once, every caller joins the same done channel, and the
// final write takes the exclusive flush side. Both the join and the write are
// bounded by StorageQueryTimeout, so a wedged store delays shutdown by at most
// two configured query timeouts instead of forever.
func (s *Server) CloseThrottlePersist(ctx context.Context) error {
	t := &s.throttle
	t.mu.Lock()
	if !t.closed {
		t.closed = true
		close(t.stop)
	}
	started := t.started
	t.mu.Unlock()

	if started {
		waitCtx, cancel := context.WithTimeout(ctx, s.cfg().StorageQueryTimeout)
		defer cancel()
		select {
		case <-t.done:
		case <-waitCtx.Done():
			return fmt.Errorf("throttle persister stop: %w", waitCtx.Err())
		}
	}
	flushCtx, cancel := context.WithTimeout(ctx, s.cfg().StorageQueryTimeout)
	defer cancel()
	s.throttle.persistMu.Lock()
	defer s.throttle.persistMu.Unlock()
	if err := s.persistThrottleLatestLocked(flushCtx); err != nil {
		return fmt.Errorf("throttle persist flush: %w", err)
	}
	return nil
}

// applyThrottleHeaders parses X-Proxy-Limit-* on this request. Absent
// headers leave the current cap alone. Any present header that is
// malformed rejects the request WITHOUT applying the others (atomic).
// A well-formed set merges into the provider cap immediately; the
// coalescing writer owns durable persistence.
func (s *Server) applyThrottleHeaders(r *http.Request, provider, client string) error {
	if provider == "" {
		return nil
	}
	hasC := headerPresent(r, hdrLimitConcurrency)
	hasR := headerPresent(r, hdrLimitRequests)
	hasT := headerPresent(r, hdrLimitTokens)
	if !hasC && !hasR && !hasT {
		return nil
	}
	// Parse/validate the patch before taking the policy lock. Applying it below
	// merges against the latest cap, so concurrent requests changing different
	// dimensions cannot overwrite each other's updates.
	var next scheduler.Limit
	if hasC {
		n, off, err := parseLimitInt(r.Header.Get(hdrLimitConcurrency), maxLimitConcurrency)
		if err != nil {
			return errInvalidHeader(hdrLimitConcurrency, r.Header.Get(hdrLimitConcurrency))
		}
		if off {
			next.Concurrency = 0
		} else {
			next.Concurrency = int(n)
		}
	}
	if hasR {
		if err := applyRateLimit(r.Header.Get(hdrLimitRequests), hdrLimitRequests,
			maxLimitRequests, &next.Requests, &next.ReqWindow); err != nil {
			return err
		}
	}
	if hasT {
		if err := applyRateLimit(r.Header.Get(hdrLimitTokens), hdrLimitTokens,
			maxLimitTokens, &next.Tokens, &next.TokWindow); err != nil {
			return err
		}
	}
	if err := validateLimit(next); err != nil {
		return err
	}
	// The validated cap is active immediately. Durable persistence is the
	// coalescing writer's job, so a store failure cannot reject inference or
	// add a write to this request path.
	return s.updateThrottleFromHeaders(provider, func(t scheduler.Throttle) scheduler.Throttle {
		if hasC {
			t.Limit.Concurrency = next.Concurrency
		}
		if hasR {
			t.Limit.Requests, t.Limit.ReqWindow = next.Requests, next.ReqWindow
		}
		if hasT {
			t.Limit.Tokens, t.Limit.TokWindow = next.Tokens, next.TokWindow
		}
		t.Source = scheduler.ThrottleSourceHeader
		t.UpdatedBy = client
		t.UpdatedAt = time.Time{}
		return t
	})
}

// applyRateLimit parses one rate-dimension header (X-Proxy-Limit-Requests or
// -Tokens) into its count/window pair: "off" zeroes both dimensions, a
// malformed value rejects as an invalid header carrying the raw value.
// applyThrottleHeaders applies the whole patch atomically, so a rejected
// dimension discards the others too.
func applyRateLimit(raw, name string, max int64, count *int64, window *time.Duration) error {
	n, win, off, err := parseLimitRate(raw, max)
	if err != nil {
		return errInvalidHeader(name, raw)
	}
	if off {
		*count, *window = 0, 0
	} else {
		*count, *window = n, win
	}
	return nil
}

func validateLimit(l scheduler.Limit) error {
	if l.Concurrency < 0 || l.Concurrency > maxLimitConcurrency {
		return fmt.Errorf("concurrency must be 0..%d", maxLimitConcurrency)
	}
	if l.Requests < 0 || l.Requests > maxLimitRequests {
		return fmt.Errorf("requests must be 0..%d", maxLimitRequests)
	}
	if l.Tokens < 0 || l.Tokens > maxLimitTokens {
		return fmt.Errorf("tokens must be 0..%d", maxLimitTokens)
	}
	if l.Requests > 0 {
		if l.ReqWindow < minLimitWindow || l.ReqWindow > maxLimitWindow {
			return fmt.Errorf("request window must be %s..%s", config.FormatDuration(minLimitWindow), config.FormatDuration(maxLimitWindow))
		}
	}
	if l.Tokens > 0 {
		if l.TokWindow < minLimitWindow || l.TokWindow > maxLimitWindow {
			return fmt.Errorf("token window must be %s..%s", config.FormatDuration(minLimitWindow), config.FormatDuration(maxLimitWindow))
		}
	}
	return nil
}

func parseLimitInt(s string, max int64) (n int64, off bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false, fmt.Errorf("empty")
	}
	if isLimitOff(s) {
		return 0, true, nil
	}
	n, err = strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > max {
		return 0, false, fmt.Errorf("out of range")
	}
	return n, false, nil
}

func parseLimitRate(s string, max int64) (n int64, window time.Duration, off bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, false, fmt.Errorf("empty")
	}
	if isLimitOff(s) {
		return 0, 0, true, nil
	}
	countStr, winStr, ok := strings.Cut(s, "/")
	if !ok {
		return 0, 0, false, fmt.Errorf("want COUNT/WINDOW")
	}
	n, err = strconv.ParseInt(strings.TrimSpace(countStr), 10, 64)
	if err != nil || n <= 0 || n > max {
		return 0, 0, false, fmt.Errorf("count out of range")
	}
	window, err = parseLimitWindow(strings.TrimSpace(winStr))
	if err != nil {
		return 0, 0, false, err
	}
	return n, window, false, nil
}

func isLimitOff(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "0", "off", "none", "unlimited":
		return true
	}
	return false
}

func parseLimitWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("window required")
	}
	// Accept 1d / 2d as 24h units (time.ParseDuration has no 'd').
	if len(s) > 1 && (s[len(s)-1] == 'd' || s[len(s)-1] == 'D') {
		n, err := strconv.ParseFloat(s[:len(s)-1], 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid window %q", s)
		}
		d := time.Duration(n * float64(24*time.Hour))
		if d < minLimitWindow || d > maxLimitWindow {
			return 0, fmt.Errorf("window out of range")
		}
		return d, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid window %q", s)
	}
	if d < minLimitWindow || d > maxLimitWindow {
		return 0, fmt.Errorf("window out of range")
	}
	return d, nil
}

// estimateTokens is the Acquire-time reservation: prompt chars/4 plus
// req_max_tokens when the client sent one. Zero means "don't reserve;
// settle actual usage when the response ends".
func estimateTokens(rec *metrics.Record) int64 {
	if rec == nil {
		return 0
	}
	chars := rec.CharsSystem + rec.CharsUser + rec.CharsAssistant + rec.CharsTool
	est := int64(chars / 4)
	if rec.ReqMaxTokens != nil && *rec.ReqMaxTokens > 0 {
		est += int64(*rec.ReqMaxTokens)
	}
	return est
}

// settleTokens is the actual usage charged against the token window.
// Prefer the provider's total; otherwise input+output. Reasoning is a
// subset of output and is not added. Zero refunds the reservation. Impossible
// usage remains explicitly unavailable and retains the admitted reservation.
func settleTokens(rec *metrics.Record) int64 {
	if rec == nil {
		return 0
	}
	if rec.ErrorCode == metrics.CodeInvalidUsage {
		return scheduler.TokensUnavailable
	}
	n, err := metrics.SumCounts(rec.Usage.InputTokens, rec.Usage.OutputTokens)
	if err != nil || rec.Usage.TotalTokens < 0 {
		invalidateUsage(rec, metrics.ErrMetricRange)
		return scheduler.TokensUnavailable
	}
	if rec.Usage.TotalTokens > 0 {
		return rec.Usage.TotalTokens
	}
	return n
}

// invalidateUsage owns the explicit unavailable signal shared by protocol
// adapters and final quota settlement. Existing failure detail is preserved.
func invalidateUsage(rec *metrics.Record, err error) {
	rec.Usage = metrics.Usage{}
	rec.GenTokens, rec.AnswerTokens = 0, 0
	rec.ErrorCode = metrics.CodeInvalidUsage
	if rec.ErrorType == "" {
		rec.ErrorType = metrics.CodeInvalidUsage
	}
	if rec.ErrorMsg == "" {
		rec.ErrorMsg = err.Error()
	}
}

// HandleThrottle is GET/POST /admin/throttle. GET returns every active
// cap plus known providers (history ∪ current caps) so the dashboard can
// set a limit on a seen provider. POST requires {"provider"} (deny by
// default). clear:true drops the cap. Otherwise any supplied dimension
// merges; omitted dimensions keep their current value; 0 / empty window
// turns that dimension off.
func (s *Server) HandleThrottle(w http.ResponseWriter, r *http.Request) {
	if !s.operatorStateGet(w, r, s.ThrottleSnapshot) {
		return
	}
	var body struct {
		Provider    string  `json:"provider"`
		Clear       bool    `json:"clear"`
		Concurrency *int    `json:"concurrency"`
		Requests    *int64  `json:"requests"`
		ReqWindow   *string `json:"request_window"`
		Tokens      *int64  `json:"tokens"`
		TokWindow   *string `json:"token_window"`
	}
	if err := adminjson.Decode(w, r, &body); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	provider := strings.TrimSpace(body.Provider)
	if provider == "" {
		adminjson.WriteError(w, http.StatusBadRequest, "provider required")
		return
	}
	if err := validateThrottleProvider(provider); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Clear {
		s.resetOperatorState(w, s.ThrottleSnapshot,
			func() error { return s.applyThrottle(scheduler.Throttle{Provider: provider}) },
			"throttle cleared (%s)", provider)
		return
	}
	if body.Concurrency == nil && body.Requests == nil && body.Tokens == nil &&
		body.ReqWindow == nil && body.TokWindow == nil {
		adminjson.WriteError(w, http.StatusBadRequest, "concurrency, requests, or tokens required")
		return
	}
	var next scheduler.Limit
	var updateErr error
	persistErr := s.updateThrottle(provider, func(cur scheduler.Throttle) scheduler.Throttle {
		next = cur.Limit
		if body.Concurrency != nil {
			if *body.Concurrency < 0 {
				updateErr = fmt.Errorf("concurrency must be >= 0")
				return cur
			}
			next.Concurrency = *body.Concurrency
		}
		if body.Requests != nil || body.ReqWindow != nil {
			var n int64
			if body.Requests != nil {
				n = *body.Requests
			} else {
				n = next.Requests
			}
			if n < 0 {
				updateErr = fmt.Errorf("requests must be >= 0")
				return cur
			}
			if n == 0 {
				next.Requests = 0
				next.ReqWindow = 0
			} else {
				win := next.ReqWindow
				if body.ReqWindow != nil {
					d, err := parseLimitWindow(*body.ReqWindow)
					if err != nil {
						updateErr = fmt.Errorf("invalid request_window")
						return cur
					}
					win = d
				}
				if win <= 0 {
					updateErr = fmt.Errorf("request_window required")
					return cur
				}
				next.Requests = n
				next.ReqWindow = win
			}
		}
		if body.Tokens != nil || body.TokWindow != nil {
			var n int64
			if body.Tokens != nil {
				n = *body.Tokens
			} else {
				n = next.Tokens
			}
			if n < 0 {
				updateErr = fmt.Errorf("tokens must be >= 0")
				return cur
			}
			if n == 0 {
				next.Tokens = 0
				next.TokWindow = 0
			} else {
				win := next.TokWindow
				if body.TokWindow != nil {
					d, err := parseLimitWindow(*body.TokWindow)
					if err != nil {
						updateErr = fmt.Errorf("invalid token_window")
						return cur
					}
					win = d
				}
				if win <= 0 {
					updateErr = fmt.Errorf("token_window required")
					return cur
				}
				next.Tokens = n
				next.TokWindow = win
			}
		}
		if err := validateLimit(next); err != nil {
			updateErr = err
			return cur
		}
		return scheduler.Throttle{
			Provider:  provider,
			Limit:     next,
			Source:    scheduler.ThrottleSourceUI,
			UpdatedBy: "dashboard",
		}
	})
	if updateErr != nil {
		adminjson.WriteError(w, http.StatusBadRequest, updateErr.Error())
		return
	}
	log.Printf("throttle set (%s conc=%d req=%d/%s tok=%d/%s)", provider,
		next.Concurrency, next.Requests, config.FormatDuration(next.ReqWindow),
		next.Tokens, config.FormatDuration(next.TokWindow))
	writeOperatorState(w, s.ThrottleSnapshot(), persistErr)
}

// ThrottleSnapshot builds the GET /admin/throttle state document - the
// single owner of that shape, shared by the HTTP handler and the dashboard
// bootstrap payload (so the two can never disagree).
func (s *Server) ThrottleSnapshot() map[string]any {
	infos := s.scheduler.ListThrottles()
	throttles := make([]map[string]any, 0, len(infos))
	for _, inf := range infos {
		throttles = append(throttles, map[string]any{
			"provider":           inf.Provider,
			"concurrency":        inf.Limit.Concurrency,
			"requests":           inf.Limit.Requests,
			"request_window":     durationOrEmpty(inf.Limit.ReqWindow),
			"tokens":             inf.Limit.Tokens,
			"token_window":       durationOrEmpty(inf.Limit.TokWindow),
			"source":             inf.Source,
			"updated_by":         inf.UpdatedBy,
			"in_flight":          inf.InFlight,
			"queued":             inf.Queued,
			"requests_remaining": inf.ReqRemaining,
			"tokens_remaining":   inf.TokRemaining,
			"requests_capacity":  inf.ReqCapacity,
			"tokens_capacity":    inf.TokCapacity,
		})
	}
	known := sanitizeNameList(append(s.pause.providers.list(), throttleProviders(infos)...))
	return map[string]any{
		"ok":              true,
		"throttles":       throttles,
		"known_providers": nullSlice(known),
		"active":          len(throttles) > 0,
	}
}

func throttleProviders(infos []scheduler.ThrottleInfo) []string {
	out := make([]string, 0, len(infos))
	for _, inf := range infos {
		if inf.Provider != "" {
			out = append(out, inf.Provider)
		}
	}
	return out
}

func durationOrEmpty(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return config.FormatDuration(d)
}
