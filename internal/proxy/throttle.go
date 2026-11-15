package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/logging"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// Persistent provider-wide caps. Set from the dashboard or from
// X-Proxy-Limit-* headers: one client writes, every client of that
// provider is limited until the cap is changed or turned off.

const (
	hdrLimitConcurrency = "X-Proxy-Limit-Concurrency"
	hdrLimitRequests    = "X-Proxy-Limit-Requests"
	hdrLimitTokens      = "X-Proxy-Limit-Tokens"
)

// Safety guardrails on a header/UI-supplied cap. Not user-tunable: they bound a
// hostile or typo'd value so it cannot stall the provider forever or overflow
// the token bucket. The bands themselves live in scheduler, which this package
// and the MCP validation surface both import, so there is one declaration
// rather than a copy per consumer. The window ceiling is MaxRetryHint's: a daily
// quota window is the longest span either feature reasons about, and the
// anti-burst ramp reuses the same band.
const (
	maxLimitConcurrency = scheduler.LimitConcurrencyMax
	maxLimitRequests    = scheduler.LimitRequestsMax
	maxLimitTokens      = scheduler.LimitTokensMax
	minLimitWindow      = scheduler.LimitWindowMin
	maxLimitWindow      = scheduler.LimitWindowMax
)

// Durable throttle-policy bounds and background-writer cadence. These are
// internal safety/implementation constants, not user tunables.
//
// throttleProviderLabelMaxBytes covers the routing boundary's DNS hostname
// bound (253 bytes) plus an IP authority's brackets and port. The scheduler
// already refuses a new header-created gate past 4096 entries; combined with
// this label bound it also bounds the persisted document.
//
// throttleProviderLabelMaxBytes bounds one provider label. The document bound
// itself is shared with every other operator document
// (operatorPersistDocMaxBytes): the gate count is bounded at 4096 and a label
// at 255 bytes, and the anti-burst pair adds at most 56 bytes per entry.
const (
	throttleProviderLabelMaxBytes = 255

	throttlePersistDebounce = 250 * time.Millisecond
	throttlePersistRetryMin = 100 * time.Millisecond
	throttlePersistRetryMax = 30 * time.Second
)

type throttlePersist interface {
	SaveThrottle(ctx context.Context, raw []byte) error
	LoadThrottle(ctx context.Context) ([]byte, error)
}

type persistedThrottle struct {
	// Provider is the legacy single-provider mirror. It is written only for a
	// provider-only scope and read as that scope's provider when Providers is
	// absent, so a document written before scoped policies still restores.
	Provider      string    `json:"provider,omitempty"`
	Providers     []string  `json:"providers,omitempty"`
	Models        []string  `json:"models,omitempty"`
	Clients       []string  `json:"clients,omitempty"`
	KeyHashes     []string  `json:"key_hashes,omitempty"`
	Conversations []string  `json:"conversations,omitempty"`
	Days          []int     `json:"days,omitempty"`
	StartMinute   int       `json:"start_minute,omitempty"`
	EndMinute     int       `json:"end_minute,omitempty"`
	Concurrency   int       `json:"concurrency,omitempty"`
	Requests      int64     `json:"requests,omitempty"`
	ReqWindow     string    `json:"request_window,omitempty"`
	Tokens        int64     `json:"tokens,omitempty"`
	TokWindow     string    `json:"token_window,omitempty"`
	AntiBurst     bool      `json:"anti_burst,omitempty"`
	RampDuration  string    `json:"ramp_duration,omitempty"`
	Source        string    `json:"source,omitempty"`
	UpdatedBy     string    `json:"updated_by,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
}

// throttleDocVersion is the persisted document's schema version. Version 1
// held provider-only policies; version 2 adds selector and schedule fields.
// A version 1 document restores unchanged, and a version 2 document read by a
// version 1 binary would silently drop its scopes, so downgrading with scoped
// policies installed requires clearing them first.
const throttleDocVersion = 2

type persistedThrottleDoc struct {
	Version   int                 `json:"version,omitempty"`
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

	// restoreDegraded records that a durable cap document existed but could not
	// be decoded, so this process booted with none of the caps it had before the
	// restart. Reported rather than only logged: a silently empty cap list reads
	// as "no provider is capped", which is the opposite of what happened.
	restoreDegraded bool

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
		logging.Printf("", "throttle: load: %v", err)
		return
	}
	items, degraded := decodeThrottles(raw)
	if degraded {
		s.throttle.mu.Lock()
		s.throttle.restoreDegraded = true
		s.throttle.mu.Unlock()
	}
	if len(items) == 0 {
		return
	}
	var ts []scheduler.Throttle
	for _, it := range items {
		t, err := persistedToThrottle(it)
		if err != nil {
			logging.Printf("", "throttle: skip %s: %v", it.Provider, err)
			continue
		}
		s.notePolicyScope(t)
		ts = append(ts, t)
	}
	s.scheduler.RestoreThrottles(ts)
	// A restored policy scopes its names before any traffic, so pin them now
	// instead of waiting for the next operator write.
	s.syncNamePins()
	if len(ts) > 0 {
		logging.Printf("", "throttle restored (%d provider caps)", len(ts))
	}
}

// decodeThrottles reads the persisted cap document. The second result reports a
// document that existed but could not be decoded: an empty store and a corrupt
// one both yield no caps, and only the caller can tell the operator that the
// difference means caps were lost rather than never installed.
func decodeThrottles(raw []byte) (items []persistedThrottle, degraded bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var doc persistedThrottleDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		logging.Printf("", "throttle: decode: %v", err)
		return nil, true
	}
	return doc.Throttles, false
}

func persistedToThrottle(it persistedThrottle) (scheduler.Throttle, error) {
	providers := it.Providers
	if len(providers) == 0 && strings.TrimSpace(it.Provider) != "" {
		providers = []string{it.Provider}
	}
	scope := scheduler.NormalizeLimitScope(scheduler.LimitScope{
		Providers:     providers,
		Models:        it.Models,
		Clients:       it.Clients,
		KeyHashes:     it.KeyHashes,
		Conversations: it.Conversations,
	})
	if err := validateThrottleScope(scope); err != nil {
		return scheduler.Throttle{}, err
	}
	schedule := scheduler.LimitSchedule{
		Days:     it.Days,
		StartMin: it.StartMinute,
		EndMin:   it.EndMinute,
	}
	if err := scheduler.ValidateLimitSchedule(schedule); err != nil {
		return scheduler.Throttle{}, err
	}
	schedule = scheduler.NormalizeLimitSchedule(schedule)
	lim := scheduler.Limit{
		Concurrency: it.Concurrency,
		Requests:    it.Requests,
		Tokens:      it.Tokens,
		AntiBurst:   it.AntiBurst,
	}
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
	if it.RampDuration != "" {
		d, err := parseRampDuration(it.RampDuration)
		if err != nil {
			return scheduler.Throttle{}, err
		}
		lim.RampDuration = d
	}
	if err := validateLimit(lim); err != nil {
		return scheduler.Throttle{}, err
	}
	if (lim.AntiBurst || lim.RampDuration > 0) && (!scope.ProviderOnly() || !schedule.Empty()) {
		return scheduler.Throttle{}, fmt.Errorf("anti_burst requires a single-provider policy without a schedule")
	}
	return scheduler.Throttle{
		ID:        scheduler.PolicyID(scope, schedule),
		Scope:     scope,
		Schedule:  schedule,
		Limit:     lim,
		Source:    it.Source,
		UpdatedBy: it.UpdatedBy,
		UpdatedAt: it.UpdatedAt,
	}, nil
}

func throttleToPersisted(t scheduler.Throttle) persistedThrottle {
	it := persistedThrottle{
		Days:        t.Schedule.Days,
		StartMinute: t.Schedule.StartMin,
		EndMinute:   t.Schedule.EndMin,
		Concurrency: t.Limit.Concurrency,
		Requests:    t.Limit.Requests,
		Tokens:      t.Limit.Tokens,
		AntiBurst:   t.Limit.AntiBurst,
		Source:      t.Source,
		UpdatedBy:   t.UpdatedBy,
		UpdatedAt:   t.UpdatedAt,
	}
	// A provider-only policy keeps the version 1 shape: one provider field and
	// no selector arrays. That makes an unchanged policy byte-identical to the
	// document a previous binary wrote, and the legacy field is a projection of
	// the scope, never a second fact. Every other selector shape writes the
	// explicit arrays.
	if t.Scope.ProviderOnly() {
		it.Provider = t.Scope.PrimaryProvider()
	} else {
		it.Providers = t.Scope.Providers
		it.Models = t.Scope.Models
		it.Clients = t.Scope.Clients
		it.KeyHashes = t.Scope.KeyHashes
		it.Conversations = t.Scope.Conversations
	}
	if t.Limit.ReqWindow > 0 {
		it.ReqWindow = config.FormatDuration(t.Limit.ReqWindow)
	}
	if t.Limit.TokWindow > 0 {
		it.TokWindow = config.FormatDuration(t.Limit.TokWindow)
	}
	if t.Limit.RampDuration > 0 {
		it.RampDuration = config.FormatDuration(t.Limit.RampDuration)
	}
	return it
}

// validateThrottleScope is the proxy-side owner of selector bounds. A provider
// label keeps the routing boundary's DNS bound; every value is bounded so one
// policy cannot consume the operator document budget.
func validateThrottleScope(scope scheduler.LimitScope) error {
	if err := scheduler.ValidateLimitScope(scope); err != nil {
		return err
	}
	for _, provider := range scope.Providers {
		if err := validateThrottleProvider(provider); err != nil {
			return err
		}
	}
	for _, group := range [][]string{scope.Models, scope.Clients, scope.Conversations} {
		for _, value := range group {
			if len(value) > throttleScopeValueMaxBytes {
				return fmt.Errorf("scope value must be at most %d bytes", throttleScopeValueMaxBytes)
			}
		}
	}
	return nil
}

const throttleScopeValueMaxBytes = 512

func validateThrottleProvider(provider string) error {
	if strings.TrimSpace(provider) == "" {
		return fmt.Errorf("provider required")
	}
	if len(provider) > throttleProviderLabelMaxBytes {
		return fmt.Errorf("provider label must be at most %d bytes", throttleProviderLabelMaxBytes)
	}
	return nil
}

// errThrottleConflict reports an edit whose new selector already belongs to
// another policy. Deny by default: replacing a neighbour would lose its
// counters and metadata.
var errThrottleConflict = fmt.Errorf("a limit with this selector already exists")

// updateThrottle is the synchronous operator edit path. It applies the policy,
// then waits for the write that covers that policy generation so the endpoint
// can report a persistence warning against live state.
func (s *Server) updateThrottle(id string, update func(scheduler.Throttle) scheduler.Throttle) error {
	var applied scheduler.Throttle
	changed, conflict := s.scheduler.UpdatePolicy(id, func(cur scheduler.Throttle) scheduler.Throttle {
		applied = update(cur)
		return applied
	})
	if conflict {
		return errThrottleConflict
	}
	if !changed {
		return nil
	}
	if applied.Limit.Active() {
		s.notePolicyScope(applied)
	}
	s.syncNamePins()
	return s.persistThrottleSync()
}

// updateThrottleFromHeaders applies a validated client header patch without
// durable I/O. The Coalescing writer owns persistence, so inference latency is
// independent of the number of policies and of the store. Header limits are
// always the provider-wide, unscheduled policy: a header cannot name a model,
// client, key or conversation, and silently overwriting an operator's scoped
// rules would be a wider change than the caller asked for.
func (s *Server) updateThrottleFromHeaders(provider string, update func(scheduler.Throttle) scheduler.Throttle) error {
	if err := validateThrottleProvider(provider); err != nil {
		return err
	}
	id := scheduler.PolicyID(scheduler.LimitScope{Providers: []string{provider}}, scheduler.LimitSchedule{})
	changed, _ := s.scheduler.UpdatePolicy(id, func(cur scheduler.Throttle) scheduler.Throttle {
		if cur.Scope.Empty() {
			cur.Scope = scheduler.LimitScope{Providers: []string{provider}}
		}
		return update(cur)
	})
	if !changed {
		return nil
	}
	// The provider is a live operator-document scope, so it must be pinned,
	// not only observed: an observed name is evictable from the bounded
	// vocabulary while the header limit still scopes it.
	s.pause.providers.add(provider)
	s.syncNamePins()
	s.markThrottleDirty()
	return nil
}

// notePolicyScope records every operator-chosen name in the shared known-name
// vocabularies, so a policy's scope stays selectable in the console even after
// its traffic stops.
func (s *Server) notePolicyScope(t scheduler.Throttle) {
	for _, provider := range t.Scope.Providers {
		s.pause.providers.add(provider)
	}
	for _, client := range t.Scope.Clients {
		s.pause.clients.add(client)
	}
	for _, model := range t.Scope.Models {
		s.pause.models.add(canonicalModel(model))
	}
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
//
// The whole convergence attempt is bounded by one StorageQueryTimeout, not one
// store call per iteration: sustained header churn must not keep an operator
// POST open indefinitely. A bounded failure leaves the dirty generation in
// place for the background writer or a later flush; the policy is already
// applied in memory.
func (s *Server) persistThrottleSync() error {
	s.throttle.mu.Lock()
	s.throttle.persistGen++
	s.throttle.mu.Unlock()
	ctx, cancel := s.storeQueryCtx()
	defer cancel()
	if err := s.persistThrottleLatest(ctx); err != nil {
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

// persistThrottleSnapshot writes the policy generation it observed, exactly
// once, and never chases a newer one.
//
// This is the background writer's owner. The converging loop below is for
// callers that need a barrier - an operator edit that must be durable before it
// returns, or a restart handoff that must flush before the child opens the
// database. A background writer has no such caller: chasing a moving generation
// means one whole-document write per change that lands during a write, so under
// sustained header traffic it wrote as fast as traffic arrived and, because it
// ran on context.Background(), had no bound at all. A newer change now leaves
// the generation dirty and its own wake signal drives the next write, so one
// wake produces at most one document.
// persistThrottleSnapshot writes the policy generation it observed, exactly
// once, and never chases a newer one.
//
// It takes the EXCLUSIVE side of persistMu, not the shared one, because the
// document must never land out of order. With a shared lock a background write
// that started before an operator edit could land after it: measured with a
// held first write, the operator's second policy was overwritten by the stale
// snapshot, persistedGen still named the newer generation, so neither a retry
// nor the shutdown flush rewrote it and the edit was lost for good. Serializing
// the two paths costs one wait bounded by StorageQueryTimeout and keeps the
// one-document-per-wake bound, because the generation is read under the same
// lock that orders the writes.
func (s *Server) persistThrottleSnapshot(ctx context.Context) error {
	s.throttle.persistMu.Lock()
	defer s.throttle.persistMu.Unlock()
	t := &s.throttle
	t.mu.Lock()
	gen := t.persistGen
	if gen == t.persistedGen {
		t.mu.Unlock()
		return nil
	}
	target := t.persist
	t.mu.Unlock()
	if target == nil {
		// Memory-only policy: leave the generation dirty so a later attach can
		// persist it, and report success to callers.
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	snap := s.latestThrottleSnap()
	if err := s.writeThrottleSnap(ctx, target, snap); err != nil {
		return err
	}
	t.mu.Lock()
	// Only claim the generation this write actually captured. A change that
	// landed mid-write keeps the generation dirty and is picked up by the wake
	// it raised.
	if t.persistGen == gen {
		t.persistedGen = gen
	}
	t.mu.Unlock()
	return nil
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
		// Check the caller's bound before every write, not only inside the
		// store call: a generation that keeps moving between writes would
		// otherwise restart this loop past the deadline. The dirty generation
		// stays for the background writer.
		if err := ctx.Err(); err != nil {
			return err
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
// signal, coalesces a burst for the debounce window, then writes the policy
// generation it then observes - one document per wake. A failed write is
// retried with exponential backoff; the dirty generation is never cleared by a
// failure. The stop channel is the only exit path, so Close can join it
// without a busy loop.
//
// It writes through persistThrottleSnapshot, not persistThrottleLatest: the
// converging loop belongs to callers that need a barrier (an operator edit, a
// restart handoff), while a background writer that chased a moving generation
// wrote one whole document per change arriving mid-write and had no bound. A
// change that lands during a write raises its own wake, so the next pass picks
// it up and a quiet policy still ends durable.
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
			err := s.persistThrottleSnapshot(context.Background())
			if err == nil {
				break
			}
			// A closed database cannot answer again in this process, so the
			// ladder is pointless: leave the generation dirty and stop instead
			// of hammering the dead handle until shutdown. A busy store stays
			// on the ladder.
			if throttleStoreGone(err) {
				logging.Printf("", "throttle: persist stopped, store is closed: %v", err)
				return
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

// throttleStoreGone reports the closed-handle class through the storage owner:
// a closed *sql.DB answers with database/sql's own "database is closed" error,
// which is not sql.ErrConnDone, so only the shared classifier stops the ladder
// on it. A busy store keeps its transient error and keeps retrying.
func throttleStoreGone(err error) bool {
	return storage.IsClosed(err)
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
		persistedThrottleDoc{Version: throttleDocVersion, Throttles: items}, operatorPersistDocMaxBytes, p.SaveThrottle)
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
	if l.RampDuration != 0 {
		if l.RampDuration < minLimitWindow || l.RampDuration > maxLimitWindow {
			return fmt.Errorf("ramp duration must be 0s or %s..%s", config.FormatDuration(minLimitWindow), config.FormatDuration(maxLimitWindow))
		}
	}
	return nil
}

func parseLimitInt(s string, max int) (n int, off bool, err error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false, fmt.Errorf("empty")
	}
	if isLimitOff(s) {
		return 0, true, nil
	}
	// Atoi is the int-sized parser, so the header value can never narrow
	// through a wider temporary on a 32-bit target.
	n, err = strconv.Atoi(s)
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

// parseRampDuration parses the anti-burst ramp duration. The empty string and
// the two zero spellings mean "off"; every other value reuses parseLimitWindow,
// so the duration grammar and the 1s..24h band stay single-owned.
func parseRampDuration(s string) (time.Duration, error) {
	switch strings.TrimSpace(s) {
	case "", "0", "0s":
		return 0, nil
	}
	return parseLimitWindow(s)
}

// estimateTokens is the Acquire-time reservation: prompt characters / 4.
//
// The client's output ceiling (max_tokens / max_completion_tokens /
// max_output_tokens) is deliberately NOT part of the reservation. It is a
// ceiling, not expected usage, and reserving it turned a token *budget* into a
// worst-case concurrency cap: a 500K/6h budget admitted only a handful of
// requests that each declared a 32K output limit, so every later request
// queued even though actual usage was tiny. Actual usage is charged at
// settlement (`settleTokens`), so the bucket still enforces the window and can
// go into debt on a real burst; the documented contract is a token budget that
// permits oversized reservations, while the request budget is a strict sliding
// window.
func estimateTokens(rec *metrics.Record) int64 {
	if rec == nil {
		return 0
	}
	chars := rec.CharsSystem + rec.CharsUser + rec.CharsAssistant + rec.CharsTool
	return int64(chars / 4)
}

// settleTokens is the actual usage charged against the token window.
// Prefer the provider's total; otherwise input+output. Reasoning is a
// subset of output and is not added. Zero refunds the reservation. Impossible
// usage remains explicitly unavailable and retains the admitted reservation.
func settleTokens(rec *metrics.Record) int64 {
	if rec == nil {
		return 0
	}
	if rec.UsageInvalid || rec.ErrorCode == metrics.CodeInvalidUsage {
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
	rec.UsageInvalid = true
	rec.Usage = metrics.Usage{}
	rec.GenTokens, rec.AnswerTokens = 0, 0
	// A provider error already recorded owns the type/code pair; stamping
	// invalid_usage over its code would produce a mismatched classification.
	// A proxy-local failure has no provider code to preserve, so the
	// invalid-usage code still describes the accounting outcome.
	if localErrorType(rec.ErrorType) {
		if rec.ErrorType == "" {
			rec.ErrorType = metrics.CodeInvalidUsage
		}
		rec.ErrorCode = metrics.CodeInvalidUsage
		if rec.ErrorMsg == "" {
			rec.ErrorMsg = err.Error()
		}
	}
}

// localErrorType reports whether an error type was minted by the proxy rather
// than parsed from a provider envelope. Provider classifications keep their
// own code when usage is invalidated after the failure. The vocabulary is
// owned by metrics (the classifier's owner) so the two cannot drift.
func localErrorType(t string) bool {
	return metrics.IsLocalErrorType(t)
}

// throttleBody is the strict POST /admin/throttle document. Pointer fields
// distinguish "not supplied" (keep the stored value) from an explicit empty
// value (clear it); slice fields make the same distinction through nil versus
// an empty array.
type throttleBody struct {
	ID            *string  `json:"id"`
	Provider      *string  `json:"provider"`
	Providers     []string `json:"providers"`
	Models        []string `json:"models"`
	Clients       []string `json:"clients"`
	KeyHashes     []string `json:"key_hashes"`
	Conversations []string `json:"conversations"`
	Days          []int    `json:"days"`
	StartMinute   *int     `json:"start_minute"`
	EndMinute     *int     `json:"end_minute"`
	Clear         bool     `json:"clear"`
	Concurrency   *int     `json:"concurrency"`
	Requests      *int64   `json:"requests"`
	ReqWindow     *string  `json:"request_window"`
	Tokens        *int64   `json:"tokens"`
	TokWindow     *string  `json:"token_window"`
	AntiBurst     *bool    `json:"anti_burst"`
	RampDuration  *string  `json:"ramp_duration"`
}

func (b *throttleBody) hasScope() bool {
	return b.Provider != nil || b.Providers != nil || b.Models != nil ||
		b.Clients != nil || b.KeyHashes != nil || b.Conversations != nil ||
		b.Days != nil || b.StartMinute != nil || b.EndMinute != nil
}

func (b *throttleBody) hasLimit() bool {
	return b.Concurrency != nil || b.Requests != nil || b.ReqWindow != nil ||
		b.Tokens != nil || b.TokWindow != nil || b.AntiBurst != nil || b.RampDuration != nil
}

// mergeThrottleScope applies a body's selector fields to a base. Omitted
// dimensions keep the stored value; an explicit empty array clears it.
func mergeThrottleScope(base scheduler.LimitScope, body *throttleBody) scheduler.LimitScope {
	if body.Provider != nil {
		base.Providers = []string{*body.Provider}
	}
	if body.Providers != nil {
		base.Providers = body.Providers
	}
	if body.Models != nil {
		base.Models = body.Models
	}
	if body.Clients != nil {
		base.Clients = body.Clients
	}
	if body.KeyHashes != nil {
		base.KeyHashes = body.KeyHashes
	}
	if body.Conversations != nil {
		base.Conversations = body.Conversations
	}
	return scheduler.NormalizeLimitScope(base)
}

// mergeThrottleSchedule applies the optional window to a base. Days and the
// minute pair are independent so a caller can re-time a rule without resending
// its days, or re-day it without resending the clock. It deliberately does not
// normalize: validation must see the raw values, and normalization would
// silently clamp an out-of-band day or minute into a different window.
func mergeThrottleSchedule(base scheduler.LimitSchedule, body *throttleBody) scheduler.LimitSchedule {
	if body.Days != nil {
		base.Days = body.Days
	}
	if body.StartMinute != nil {
		base.StartMin = *body.StartMinute
	}
	if body.EndMinute != nil {
		base.EndMin = *body.EndMinute
	}
	return base
}

// HandleThrottle is GET/POST /admin/throttle. GET returns every active policy
// plus the known selector vocabularies. POST requires an id or a selector
// (deny by default). clear:true removes the resolved policy. Otherwise any
// supplied limit dimension merges; omitted dimensions keep their current
// value; 0 / empty window turns that dimension off. The anti-burst flag and
// ramp duration require a single-provider policy without a schedule, because
// a ramp is defined over the provider-plus-key groups the scheduler owns.
func (s *Server) HandleThrottle(w http.ResponseWriter, r *http.Request) {
	if !s.operatorStateGet(w, r, s.ThrottleSnapshot) {
		return
	}
	var body throttleBody
	if err := adminjson.Decode(w, r, &body); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.Method == http.MethodPost {
		q, err := adminjson.StrictQuery(r)
		if err != nil {
			adminjson.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
		if err := adminjson.UnknownQueryKey(q); err != nil {
			adminjson.WriteError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if body.Provider != nil && body.Providers != nil {
		adminjson.WriteError(w, http.StatusBadRequest,
			"provider and providers cannot both be supplied")
		return
	}
	if body.ID == nil && !body.hasScope() {
		adminjson.WriteError(w, http.StatusBadRequest, "id or selector required")
		return
	}
	// Resolve the policy being edited. An id names it exactly. Otherwise the
	// supplied selector names it: re-applying the same scope is an edit, not a
	// second rule with the same meaning.
	targetID := ""
	if body.ID != nil {
		targetID = strings.TrimSpace(*body.ID)
		if targetID == "" {
			adminjson.WriteError(w, http.StatusBadRequest, "id must not be empty")
			return
		}
		if _, ok := s.scheduler.PolicyByID(targetID); !ok {
			adminjson.WriteError(w, http.StatusNotFound, "limit not found")
			return
		}
	} else {
		preview := mergeThrottleScope(scheduler.LimitScope{}, &body)
		previewSchedule := mergeThrottleSchedule(scheduler.LimitSchedule{}, &body)
		if !preview.Empty() || !previewSchedule.Empty() {
			targetID = scheduler.PolicyID(preview, previewSchedule)
		}
	}
	if body.Clear {
		// Clear is a whole-policy action. Accepting a dimension beside it
		// would drop the field and report success, so the caller could
		// believe a cap is active when none is.
		if body.hasLimit() {
			adminjson.WriteError(w, http.StatusBadRequest,
				"clear cannot be combined with limit dimensions")
			return
		}
		if targetID == "" {
			adminjson.WriteError(w, http.StatusBadRequest, "selector required to clear")
			return
		}
		s.resetOperatorState(w, s.ThrottleSnapshot,
			func() error { return s.removeThrottle(targetID) },
			"throttle cleared (%s)", targetID)
		return
	}
	if !body.hasLimit() {
		adminjson.WriteError(w, http.StatusBadRequest,
			"concurrency, requests, tokens, anti_burst, or ramp_duration required")
		return
	}
	var next scheduler.Limit
	var updateErr error
	var applied scheduler.Throttle
	persistErr := s.updateThrottle(targetID, func(cur scheduler.Throttle) scheduler.Throttle {
		scope := mergeThrottleScope(cur.Scope, &body)
		schedule := mergeThrottleSchedule(cur.Schedule, &body)
		if err := validateThrottleScope(scope); err != nil {
			updateErr = err
			return cur
		}
		if err := scheduler.ValidateLimitSchedule(schedule); err != nil {
			updateErr = err
			return cur
		}
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
		if body.AntiBurst != nil {
			next.AntiBurst = *body.AntiBurst
		}
		if body.RampDuration != nil {
			d, err := parseRampDuration(*body.RampDuration)
			if err != nil {
				updateErr = fmt.Errorf("invalid ramp_duration")
				return cur
			}
			next.RampDuration = d
		}
		// A ramp only shapes an active cap: the scheduler drops an inactive
		// limit, so accepting one here would answer 200 while storing nothing.
		// Only an explicit on-ramp is refused, so false / empty / 0s can still
		// ride along with the clear of the last cap.
		rampOn := (body.AntiBurst != nil && *body.AntiBurst) ||
			(body.RampDuration != nil && next.RampDuration > 0)
		if !next.Active() && rampOn {
			updateErr = fmt.Errorf("anti_burst and ramp_duration require an active limit")
			return cur
		}
		if rampOn && (!scope.ProviderOnly() || !schedule.Empty()) {
			updateErr = fmt.Errorf("anti_burst requires a single-provider limit without a schedule")
			return cur
		}
		if err := validateLimit(next); err != nil {
			updateErr = err
			return cur
		}
		applied = scheduler.Throttle{
			Scope:     scope,
			Schedule:  schedule,
			Limit:     next,
			Source:    scheduler.ThrottleSourceUI,
			UpdatedBy: "dashboard",
		}
		return applied
	})
	if updateErr != nil {
		adminjson.WriteError(w, http.StatusBadRequest, updateErr.Error())
		return
	}
	if errors.Is(persistErr, errThrottleConflict) {
		adminjson.WriteError(w, http.StatusConflict, persistErr.Error())
		return
	}
	if applied.Limit.Active() {
		logging.Printf("", "throttle set (%s conc=%d req=%d/%s tok=%d/%s burst=%t ramp=%s)", applied.ID,
			next.Concurrency, next.Requests, config.FormatDuration(next.ReqWindow),
			next.Tokens, config.FormatDuration(next.TokWindow),
			next.AntiBurst, config.FormatDuration(next.RampDuration))
	}
	writeOperatorState(w, s.ThrottleSnapshot(), persistErr)
}

// removeThrottle clears one policy by identity and persists the new document.
func (s *Server) removeThrottle(id string) error {
	if !s.scheduler.RemovePolicy(id) {
		return nil
	}
	s.syncNamePins()
	return s.persistThrottleSync()
}

// ThrottleSnapshot builds the GET /admin/throttle state document - the
// single owner of that shape, shared by the HTTP handler and the dashboard
// bootstrap payload (so the two can never disagree).
func (s *Server) ThrottleSnapshot() map[string]any {
	infos := s.scheduler.ListThrottles()
	throttles := make([]map[string]any, 0, len(infos))
	for _, inf := range infos {
		throttles = append(throttles, map[string]any{
			"id":                 inf.ID,
			"scope":              scopeDocument(inf.Scope),
			"schedule":           scheduleDocument(inf.Schedule),
			"provider":           inf.Scope.PrimaryProvider(),
			"concurrency":        inf.Limit.Concurrency,
			"requests":           inf.Limit.Requests,
			"request_window":     durationOrEmpty(inf.Limit.ReqWindow),
			"tokens":             inf.Limit.Tokens,
			"token_window":       durationOrEmpty(inf.Limit.TokWindow),
			"anti_burst":         inf.Limit.AntiBurst,
			"ramp_duration":      durationOrEmpty(inf.Limit.RampDuration),
			"source":             inf.Source,
			"updated_by":         inf.UpdatedBy,
			"in_flight":          inf.InFlight,
			"queued":             inf.Queued,
			"requests_remaining": inf.ReqRemaining,
			"tokens_remaining":   inf.TokRemaining,
			"requests_capacity":  inf.ReqCapacity,
			"tokens_capacity":    inf.TokCapacity,
			// The server owns whether a schedule is open right now; the
			// dashboard must not re-derive it from the stored minutes.
			"scheduled": inf.Scheduled,
		})
	}
	known := sanitizeNameList(append(s.pause.providers.list(), throttleProviders(infos)...))
	s.throttle.mu.Lock()
	restoreDegraded := s.throttle.restoreDegraded
	s.throttle.mu.Unlock()
	return map[string]any{
		"ok":              true,
		"throttles":       throttles,
		"known_providers": nullSlice(known),
		"known_clients":   nullSlice(sanitizeNameList(s.pause.clients.list())),
		"known_models":    nullSlice(sanitizeNameList(s.pause.models.list())),
		// A durable cap document existed but could not be read, so no cap from
		// before the restart is enforcing. Reported apart from an empty list,
		// which would read as a deliberate, uncapped configuration.
		"restore_degraded": restoreDegraded,
		"active":           len(throttles) > 0,
	}
}

// scopeDocument projects a selector into the wire shape, omitting empty
// dimensions so the dashboard's "any value" reading stays unambiguous.
func scopeDocument(scope scheduler.LimitScope) map[string]any {
	doc := map[string]any{}
	if len(scope.Providers) > 0 {
		doc["providers"] = scope.Providers
	}
	if len(scope.Models) > 0 {
		doc["models"] = scope.Models
	}
	if len(scope.Clients) > 0 {
		doc["clients"] = scope.Clients
	}
	if len(scope.KeyHashes) > 0 {
		doc["key_hashes"] = scope.KeyHashes
	}
	if len(scope.Conversations) > 0 {
		doc["conversations"] = scope.Conversations
	}
	return doc
}

// scheduleDocument projects a schedule; an always-active window is null rather
// than an object with zeroed fields.
func scheduleDocument(schedule scheduler.LimitSchedule) any {
	if schedule.Empty() {
		return nil
	}
	return map[string]any{
		"days":         schedule.Days,
		"start_minute": schedule.StartMin,
		"end_minute":   schedule.EndMin,
	}
}

func throttleProviders(infos []scheduler.ThrottleInfo) []string {
	out := make([]string, 0, len(infos))
	for _, inf := range infos {
		out = append(out, inf.Scope.Providers...)
	}
	return out
}

func durationOrEmpty(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return config.FormatDuration(d)
}
