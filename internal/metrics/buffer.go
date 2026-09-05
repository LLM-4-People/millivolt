package metrics

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Buffer is an in-memory, lock-free-ish ring buffer of recent records. It
// supports O(1) append and a snapshot query for the live dashboard. Skew is
// tolerated for reads: readers see a consistent snapshot at the moment they
// start but may miss concurrent appends.
type Buffer struct {
	mu      sync.RWMutex
	buf     []*Record
	seqs    []int64 // sequence number of each ring slot (parallel to buf)
	head    int
	size    int
	maxSize int

	// nextSeq is the process-lifetime feed cursor: every record (live or
	// backfilled) gets a strictly increasing sequence number, and clients
	// resume the feed with ?since= / Last-Event-ID against these numbers.
	// It is never reset (not even by Reset) so a stale client cursor always
	// lands below the current range and triggers a full resync instead of
	// silently colliding with re-numbered records.
	nextSeq int64
	// Aggregate revisions track content, independently of the replay cursor:
	// purges do not rewind nextSeq, and pending events have no sequence at all.
	// Updated inside the existing mutation lock; no extra hot-path work beyond
	// an increment. Readers can validate memoized finalized/pending aggregates.
	finalRevision   uint64
	pendingRevision uint64

	// feedID identifies the current replay epoch, guarded by mu. Restarts and
	// removals mint a fresh pin: deltas cannot communicate deleted records.
	feedID string

	// Live counters, exposed to the dashboard as a process-wide gauge.
	inFlight atomic.Int64
	totalReq atomic.Int64
	totalErr atomic.Int64

	// subChans holds live SSE stream subscribers for instant push updates.
	subsMu sync.Mutex
	subs   map[chan RingEvent]struct{}

	// liveSubs holds SSE subscribers that also want per-request lifecycle
	// events (begin/end) so the dashboard can render a request the moment it
	// starts and watch it stream - not only after it completes. Lifecycle events
	// are ephemeral: they go only to these subscribers, never into the ring or
	// the durable store (which must persist exactly one finalized record).
	liveSubs map[chan *LiveEvent]struct{}

	// pending holds the current in-flight requests (id → the begin/update
	// snapshot that the live fan-out also published). Snapshot payloads carry
	// it so a dashboard that loads mid-request still renders the streaming row
	// (a fresh page missed the original "begin" event, which is never
	// replayed). Ephemeral like the lifecycle events themselves: never the
	// ring, never the store. Guarded by mu; PublishLive maintains it.
	pending       map[string]*Record
	modelObserver func(bool, ...[]*Record) any // full snapshot vs event metadata; guarded by mu

	// feedDone is selected on by every HandleStream so a graceful restart can
	// end the dashboard SSE feeds without touching proxied LLM streams (the
	// feed is ephemeral and resumable - clients reconnect and resync via
	// feed_id). CloseFeeds closes the current channel and installs a fresh
	// one, so a handoff that fails and resumes serving keeps working; a
	// handler holds the channel it subscribed with, so a swap can never
	// strand a live stream on a closed channel it did not see. Guarded by mu,
	// so a deletion, its new feed pin, and invalidation are one transition.
	feedDone chan struct{}

	// snapLimit caps the record count of full snapshots (0 = unlimited).
	// Applied by cmd/proxy's applySnapshotLimit at boot and re-applied on
	// every reload (dash_log_rows hot-reloads), reset to 0 when the store
	// is off; read atomically by SnapshotSince.
	snapLimit atomic.Int64
}

// RingEvent is a finalized record pushed to ring subscribers (the SSE
// "record" event) together with its feed sequence number - the dashboard's
// resume cursor, also sent as the SSE event id for Last-Event-ID replay.
type RingEvent struct {
	Record *Record
	Seq    int64
	feedID string // mutation epoch, not a separate wire field
}

// LiveEvent is a per-request lifecycle event: Phase is "begin" (the request was
// accepted; the record exists but is not yet finalized - no status/TTFT/end),
// "update" (a mid-flight progress signal - e.g. an absorbed retry attempt - the
// record is still live and not final; carries the current snapshot so the
// dashboard can show retries in real time), or "end" (the finalized record - the
// same authoritative record that also lands in the ring + store). "begin"
// carries a lightweight snapshot (id, start, client, provider, model, stream) so
// the dashboard can show a pending row immediately. Every event also carries the
// process-wide in-flight gauge (post-transition), so the dashboard KPI band can
// react the moment a stream starts or ends.
type LiveEvent struct {
	Phase           string  `json:"phase"` // "begin" | "update" | "end"
	Record          *Record `json:"record"`
	InFlight        int64   `json:"in_flight"`
	PendingRevision uint64  `json:"pending_revision"`
	feedID          string  // mutation epoch, not a separate wire field
}

// NewBuffer creates a ring buffer that holds at most maxSize records.
func NewBuffer(maxSize int) *Buffer {
	if maxSize < 1 {
		// Defensive floor only (a ring needs >= 1 slot); the real default is
		// HistorySize in config.Default() - never substitute a value here.
		maxSize = 1
	}
	return &Buffer{
		buf:      make([]*Record, maxSize),
		seqs:     make([]int64, maxSize),
		maxSize:  maxSize,
		nextSeq:  0, // pre-incremented on each append, so the first record gets seq 1
		feedID:   newFeedID(),
		feedDone: make(chan struct{}),
	}
}

func newFeedID() string {
	var idBytes [8]byte
	rand.Read(idBytes[:])
	return hex.EncodeToString(idBytes[:])
}

// CloseFeeds ends every dashboard SSE stream: each HandleStream selects on
// the channel it subscribed with and returns (its defers unsubscribe). The
// current channel is replaced with a fresh one, so feeds opened after the
// close keep streaming - a restart handoff that fails and resumes serving
// does not leave the dashboard dead. Idempotent; never touches the ring,
// the counters, or proxied LLM streams.
func (b *Buffer) CloseFeeds() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closeFeedsLocked()
}

func (b *Buffer) closeFeedsLocked() {
	close(b.feedDone)
	b.feedDone = make(chan struct{})
}

// invalidateFeedLocked makes all existing cursors foreign and wakes streams
// before the mutation lock is released. It never changes sequence numbers.
func (b *Buffer) invalidateFeedLocked() {
	b.feedID = newFeedID()
	b.closeFeedsLocked()
}

// feedChan snapshots the current feed-done channel for a handler's lifetime.
func (b *Buffer) feedChan() <-chan struct{} {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.feedDone
}

// Record is the hot-path append. The existing subscriber lock orders sequence
// assignment with publication; otherwise concurrent callers could publish N+1
// before N and a reconnect at N+1 would silently skip N. Lock order is subsMu
// then mu; lifecycle publishers release mu before acquiring subsMu.
func (b *Buffer) Record(r *Record) {
	if r == nil {
		return // never store a nil record
	}
	b.subsMu.Lock()
	b.mu.Lock()
	b.totalReq.Add(1)
	if r.IsError() {
		b.totalErr.Add(1)
	}
	b.nextSeq++
	b.finalRevision++
	seq := b.nextSeq
	feed := b.feedID
	b.buf[b.head] = r
	b.seqs[b.head] = seq
	b.head = (b.head + 1) % b.maxSize
	if b.size < b.maxSize {
		b.size++
	}
	// Completion and pending retirement share the purge fence. Publishing end
	// separately after Record could resurrect a deleted final as a pending row
	// or tag its delayed end with a replacement feed epoch.
	var ended *LiveEvent
	if _, pending := b.pending[r.ID]; pending {
		delete(b.pending, r.ID)
		b.pendingRevision++
		b.inFlight.Store(int64(len(b.pending)))
		ended = &LiveEvent{Phase: "end", Record: r, InFlight: b.inFlight.Load(), PendingRevision: b.pendingRevision, feedID: feed}
	}
	b.mu.Unlock()

	// Push to any live subscribers (dashboard) non-blockingly.
	for ch := range b.subs {
		select {
		case ch <- RingEvent{Record: r, Seq: seq, feedID: feed}:
		default:
			// A later ID must never advance past this lost event. Retire only
			// this slow subscriber; reconnect replays from its last delivered ID.
			delete(b.subs, ch)
			close(ch)
		}
	}
	if ended != nil {
		b.publishLiveLocked(ended)
	}
	b.subsMu.Unlock()
}

// subChanCap buffers records per SSE subscriber so a slow dashboard never
// blocks the hot-path Record fan-out (overflow closes it for replay). Internal
// guardrail, not user-tunable.
const subChanCap = 256

// Subscribe returns a channel that receives new records (with their feed
// sequence numbers) as they arrive. The channel is buffered (subChanCap) and
// never blocks the writer.
func (b *Buffer) Subscribe() chan RingEvent {
	ch := make(chan RingEvent, subChanCap)
	b.subsMu.Lock()
	if b.subs == nil {
		b.subs = make(map[chan RingEvent]struct{})
	}
	b.subs[ch] = struct{}{}
	b.subsMu.Unlock()
	return ch
}

// Unsubscribe removes a subscription channel.
func (b *Buffer) Unsubscribe(ch chan RingEvent) {
	b.subsMu.Lock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
	b.subsMu.Unlock()
}

// SubscribeLive registers for per-request lifecycle events (begin/update/end).
// Overflow closes the buffered channel so reconnect restores the pending state.
func (b *Buffer) SubscribeLive() chan *LiveEvent {
	ch := make(chan *LiveEvent, subChanCap)
	b.subsMu.Lock()
	if b.liveSubs == nil {
		b.liveSubs = make(map[chan *LiveEvent]struct{})
	}
	b.liveSubs[ch] = struct{}{}
	b.subsMu.Unlock()
	return ch
}

// UnsubscribeLive removes a lifecycle subscription channel.
func (b *Buffer) UnsubscribeLive(ch chan *LiveEvent) {
	b.subsMu.Lock()
	if _, ok := b.liveSubs[ch]; ok {
		delete(b.liveSubs, ch)
		close(ch)
	}
	b.subsMu.Unlock()
}

// PublishLive publishes begin/update snapshots. Record atomically owns end,
// pending retirement, and finalized recording, including the in-flight gauge.
// A request's begin→Record pair is exactly its in-flight window,
// so the counter is maintained here even when no dashboard is connected (the
// gauge is process-wide, not per-subscriber). The "update" phase does not move
// the gauge - it is a mid-flight progress signal inside the same begin→end
// window (e.g. an absorbed retry attempt), so the in-flight count must not
// change. Each event carries the authoritative current in-flight value. It never
// touches the ring buffer or the durable store, and never blocks the hot path
// (close-on-overflow, same policy as Record's fan-out). The record is
// shallow-copied so a "begin"/"update" snapshot stays a stable pending view -
// the live record keeps mutating as the request streams, and subscribers must
// not see those in-place writes.
func (b *Buffer) PublishLive(phase string, r *Record) {
	if r == nil || (phase != "begin" && phase != "update") {
		return
	}
	snap := *r // shallow copy: scalars + slice/map headers (fine for a pending view)
	// An "update" is published mid-flight while the proxy may be concurrently
	// appending to rec.Attempts; a shared slice header could read a torn slice.
	// Copy the attempts so subscribers get a consistent point-in-time view.
	// (Cheap: attempts are few, and only on the update path.)
	if phase == "update" && r.Attempts != nil {
		att := make([]RetryAttempt, len(r.Attempts))
		copy(att, r.Attempts)
		snap.Attempts = att
	}
	// Maintain the pending registry - the single owner of "what is in flight
	// right now". It lets a dashboard that (re)loads mid-request still see the
	// streaming row: snapshot payloads carry these pending snapshots alongside
	// the finalized ring, since lifecycle events are ephemeral and a fresh
	// page missed the original "begin". The registry mirrors the live fan-out
	// exactly (begin adds, update refreshes, end removes) and never touches the
	// ring or the durable store - each request still persists exactly one
	// finalized record there.
	b.mu.Lock()
	_, exists := b.pending[snap.ID]
	if (phase == "begin" && exists) || (phase == "update" && !exists) {
		b.mu.Unlock()
		return // duplicate begins and updates after completion cannot reopen a request
	}
	if b.pending == nil {
		b.pending = make(map[string]*Record)
	}
	b.pending[snap.ID] = &snap
	b.pendingRevision++
	// The registry owns the gauge: snapshot, revision and count are one
	// transition under mu, including the first begin. No separate Begin/End API.
	b.inFlight.Store(int64(len(b.pending)))
	ev := &LiveEvent{Phase: phase, Record: &snap, InFlight: b.inFlight.Load(), PendingRevision: b.pendingRevision, feedID: b.feedID}
	b.mu.Unlock()
	b.subsMu.Lock()
	b.publishLiveLocked(ev)
	b.subsMu.Unlock()
}

func (b *Buffer) publishLiveLocked(ev *LiveEvent) {
	for ch := range b.liveSubs {
		select {
		case ch <- ev:
		default:
			// Reconnect snapshots restore the current ephemeral pending state.
			delete(b.liveSubs, ch)
			close(ch)
		}
	}
}

// Counters returns the current live process-wide counters.
func (b *Buffer) Counters() Counters {
	return Counters{
		InFlight: b.inFlight.Load(),
		TotalReq: b.totalReq.Load(),
		TotalErr: b.totalErr.Load(),
	}
}

// ringIndex maps an oldest-first logical index to its physical slot. Records
// occupy [0:size] until full, then wrap at head. The logical end (size) is
// allowed for an empty window. Callers must hold b.mu (read or write).
func (b *Buffer) ringIndex(logical int) int {
	if b.size < b.maxSize {
		return logical
	}
	physical := b.head + logical
	if physical >= b.maxSize {
		physical -= b.maxSize
	}
	return physical
}

// copyRingWindow copies only the requested logical window, starting at its
// physical slot, with at most two contiguous copies. Records and their paired
// sequences share this one traversal; no full-ring allocation is needed for
// a capped snapshot or small delta. Callers must hold the ring's lock.
func copyRingWindow[T any](ring []T, start, count int) []T {
	out := make([]T, count)
	n := copy(out, ring[start:])
	copy(out[n:], ring)
	return out
}

// Snapshot returns a copy of the current buffer contents from oldest to
// newest. The returned slice is safe to mutate.
func (b *Buffer) Snapshot() []*Record {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return copyRingWindow(b.buf, b.ringIndex(0), b.size)
}

// Revisions returns finalized and pending content revisions atomically. The
// finalized revision also covers eviction, backfill, and removal; unlike the
// feed cursor it therefore invalidates aggregate results after a purge.
func (b *Buffer) Revisions() (finalized, pending uint64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.finalRevision, b.pendingRevision
}

// Backfill loads existing records into the ring buffer at startup to restore
// history from durable storage. It broadcasts nothing to SSE subscribers, but
// each restored record is added to the cumulative counters: RemoveWhere (a
// filtered purge) symmetrically subtracts purged records from those tallies,
// so a purge can never drive them negative against records this process never
// counted (a shipped bug: backfilled history was purged without ever being
// counted, leaving total_requests/total_errors negative).
func (b *Buffer) Backfill(records []*Record) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range records {
		if r == nil {
			continue // never store a nil record
		}
		b.totalReq.Add(1)
		if r.IsError() {
			b.totalErr.Add(1)
		}
		b.nextSeq++
		b.finalRevision++
		b.buf[b.head] = r
		b.seqs[b.head] = b.nextSeq
		b.head = (b.head + 1) % b.maxSize
		if b.size < b.maxSize {
			b.size++
		}
	}
}

// Len returns the current number of records in the buffer.
func (b *Buffer) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.size
}

// Reset clears the ring buffer and the cumulative counters, returning the
// dashboard to a clean slate. In-flight is left untouched (live streams are
// still open). The feed epoch invalidates old cursors and notifies subscribers.
// nextSeq is deliberately not reset: stale client cursors from before the
// wipe must fall below the resumed range and force a full resync - re-using
// low sequence numbers would silently collide with the post-reset records.
func (b *Buffer) Reset() {
	b.mu.Lock()
	b.finalRevision++
	b.buf = make([]*Record, b.maxSize)
	b.seqs = make([]int64, b.maxSize)
	b.head = 0
	b.size = 0
	b.totalReq.Store(0)
	b.totalErr.Store(0)
	b.invalidateFeedLocked()
	b.mu.Unlock()
}

// RemovalBoundary captures a finalized-record fence, including cumulative
// counters for a full wipe. In-flight lifecycle state is deliberately excluded.
type RemovalBoundary struct {
	seq, requests, errors int64
}

func (b *Buffer) RemovalBoundary() RemovalBoundary {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return RemovalBoundary{b.nextSeq, b.totalReq.Load(), b.totalErr.Load()}
}

// RemoveThrough removes only records finalized at or before the boundary.
// A nil predicate means a full wipe through that boundary; completions arriving
// during a durable delete retain both their rows and their cumulative counters.
func (b *Buffer) RemoveThrough(boundary RemovalBoundary, match func(*Record) bool) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.removeWhereLocked(&boundary, match)
}

// RemoveWhere drops every buffered record for which match returns true,
// compacting the ring in place. Used by filtered purge so the live view drops
// the same records the database deleted. Returns the number removed.
func (b *Buffer) RemoveWhere(match func(*Record) bool) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.removeWhereLocked(nil, match)
}

func (b *Buffer) removeWhereLocked(boundary *RemovalBoundary, match func(*Record) bool) int {
	// Use the same logical traversal as Snapshot for records and their cursors.
	start := b.ringIndex(0)
	cur := copyRingWindow(b.buf, start, b.size)
	seqs := copyRingWindow(b.seqs, start, b.size)
	kept := make([]*Record, 0, len(cur))
	keptSeqs := make([]int64, 0, len(cur))
	removed := 0
	removedErr := 0
	for i, r := range cur {
		if r == nil {
			continue
		}
		if (boundary == nil || seqs[i] <= boundary.seq) && (match == nil || match(r)) {
			removed++
			if r.IsError() {
				removedErr++
			}
			continue
		}
		kept = append(kept, r)
		keptSeqs = append(keptSeqs, seqs[i]) // sequences survive; the feed pin changes
	}
	// Rebuild the ring from the kept records (oldest first). head must point at
	// the next write slot under the not-full invariant (head == size), never
	// at 0: resetting it to 0 made every post-purge Record overwrite the oldest
	// kept record while size kept growing - live null slots appeared at the
	// tail of every snapshot (the dashboard JSON then carried `null` records,
	// crashing buildIndexes) and the oldest survivors vanished from the live
	// view. The modulo keeps head == size when the ring is not full and wraps
	// to the canonical 0 (the rebuilt buf[0] is the oldest record) when kept
	// still fills the ring.
	b.buf = make([]*Record, b.maxSize)
	b.seqs = make([]int64, b.maxSize)
	b.head = len(kept) % b.maxSize
	b.size = len(kept)
	copy(b.buf, kept)
	copy(b.seqs, keptSeqs)
	// Counters are cumulative process-wide tallies (Record increments them even
	// for records that later evict from a full ring), so subtract the removed
	// delta rather than re-deriving from the ring - re-deriving would silently
	// discard all pre-eviction history once the ring has wrapped.
	if boundary != nil && match == nil {
		b.finalRevision++
		b.totalReq.Add(-boundary.requests)
		b.totalErr.Add(-boundary.errors)
	} else if removed > 0 {
		b.finalRevision++
		b.totalReq.Add(-int64(removed))
		b.totalErr.Add(-int64(removedErr))
	}
	if boundary != nil || removed > 0 {
		// A durable deletion may affect only archive rows outside this ring.
		// RemoveThrough follows its commit, so even then subscribers resync.
		b.invalidateFeedLocked()
	}
	return removed
}

// ---------------------------------------------------------------------------
// Live aggregation helpers (computed from the buffer snapshot)
// ---------------------------------------------------------------------------

// AggResult holds aggregate counters and histograms over a snapshot.
type AggResult struct {
	Err   error `json:"-"`
	Total int   `json:"total"`
	// ByProvider holds per-provider TTFT samples (ms) - only records with a
	// captured TTFT (TTFTMs > 0), so no-token requests never skew percentiles.
	ByProvider  map[string][]int64 `json:"by_provider"`
	ByStatus    map[int]int        `json:"by_status"`
	TotalInput  int64              `json:"total_input_tokens"`
	TotalOutput int64              `json:"total_output_tokens"`
	TotalCache  int64              `json:"total_cache_read"`
	ErrorCount  int                `json:"error_count"`
	ToolCalls   int                `json:"tool_calls"`
}

// Aggregate computes lightweight aggregates over the given snapshot. Results
// are O(n) in snapshot size.
func Aggregate(records []*Record) *AggResult {
	a := &AggResult{
		ByProvider: make(map[string][]int64),
		ByStatus:   make(map[int]int),
	}
	a.Total = len(records)
	for _, r := range records {
		if r == nil {
			// Buffer.Record/Backfill still drop nils; this guards Prometheus
			// and other Aggregate callers. The dashboard bootstrap does not call Aggregate.
			a.Total--
			continue
		}
		a.ByStatus[r.StatusCode]++
		// TTFTMs == 0 means "absent" (no token ever arrived), not "0 ms":
		// no-token requests must not dilute TTFT aggregates or percentiles.
		if r.TTFTMs > 0 {
			a.ByProvider[r.Provider] = append(a.ByProvider[r.Provider], r.TTFTMs)
		}
		for _, term := range [...]struct {
			dst   *int64
			value int64
		}{{&a.TotalInput, r.Usage.InputTokens}, {&a.TotalOutput, r.Usage.OutputTokens}, {&a.TotalCache, r.Usage.CacheReadTokens}} {
			var err error
			*term.dst, err = SumCounts(*term.dst, term.value)
			if err != nil {
				a.Err = err
			}
		}
		if r.IsError() {
			a.ErrorCount++
		}
		a.ToolCalls += r.ToolCalls
	}
	return a
}

// Percentile computes the p-th percentile (0-100) over a sorted int64 slice.
// Uses the nearest-rank method: index = ceil(p/100 * n) - 1.
func Percentile(sorted []int64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return float64(sorted[0])
	}
	if p >= 100 {
		return float64(sorted[len(sorted)-1])
	}
	idx := int(math.Ceil(float64(len(sorted)) * p / 100.0))
	if idx < 1 {
		idx = 1
	}
	if idx > len(sorted) {
		idx = len(sorted)
	}
	return float64(sorted[idx-1])
}

// Snapshot is the live-feed payload: finalized ring records, the in-flight
// pending registry, live counters, and the cursor metadata a client needs to
// resume (seq / oldest_seq / feed_id). Embedded wholesale into the dashboard
// bootstrap payload; HandleStream marshals it directly.
type Snapshot struct {
	Records         []*Record `json:"records"`
	InFlightRecords []*Record `json:"in_flight_records"`
	PendingRevision uint64    `json:"pending_revision"`
	BufferSize      int       `json:"buffer_size"`
	Counters        Counters  `json:"counters"`
	Seq             int64     `json:"seq"`
	OldestSeq       int64     `json:"oldest_seq"`
	FeedID          string    `json:"feed_id"`
	Incremental     bool      `json:"incremental"`
}

// SetSnapshotLimit caps how many records a full snapshot carries (0 =
// unlimited, the default). The cap is applied by cmd/proxy's
// applySnapshotLimit at boot and re-applied on every hot reload
// (dash_log_rows reloads live), and explicitly reset to 0 when the store
// is off: a cursor-less client gets the newest window instead of a
// multi-megabyte full-ring dump, and older history pages from the store
// via /metrics/agg/log on scroll - never a silent miss (a cursor resume is
// a delta and is never truncated). Not a tunable: derived from dash_log_rows
// by the caller.
func (b *Buffer) SetSnapshotLimit(n int) { b.snapLimit.Store(int64(n)) }

// FeedID returns the current replay-epoch pin. A client whose
// cursor was minted by a different feed must take a full snapshot.
func (b *Buffer) FeedID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.feedID
}

// SnapshotSince builds a payload from a cursor already bound to this epoch.
// HTTP callers use SnapshotRequest to atomically validate that binding.
// since == 0 means "no cursor" → the full buffer. An in-range cursor
// (oldest_seq <= since <= latest_seq) yields only the newer records; a
// cursor outside the current range (ring eviction - since < oldest_seq - or
// a foreign/stale cursor from before a restart or reset) yields a full
// snapshot (`Incremental: false`) so a client can never silently miss
// records. Every payload carries the cursor metadata: `seq` (the newest
// sequence number), `oldest_seq`, and `feed_id` - a client that sees a
// changed feed_id must discard its cursor and keep the snapshot wholesale.
// A full snapshot is capped to the newest snapshot-limit records (a
// cursor-less client only needs the first log pages; older history pages
// from the store); an incremental delta is delivered verbatim - capping one
// would be a silent miss.
func (b *Buffer) SnapshotSince(since int64) Snapshot {
	b.mu.RLock()
	snap := b.snapshotSinceLocked(since)
	b.mu.RUnlock()
	sortPendingRecords(snap.InFlightRecords)
	return snap
}

func (b *Buffer) snapshotSinceLocked(since int64) Snapshot {
	size := b.size
	var oldest, latest int64
	if size > 0 {
		oldest, latest = b.seqs[b.ringIndex(0)], b.seqs[b.ringIndex(size-1)]
	}
	full := since <= 0 || since < oldest || since > latest
	start := 0
	if full {
		if limit := int(b.snapLimit.Load()); limit > 0 && size > limit {
			start = size - limit
		}
	} else {
		// Sequences stay increasing in logical order, including purge gaps.
		// Find the first newer record in O(log size), or O(1) at the feed tip.
		start = size
		if since < latest {
			start = sort.Search(size, func(i int) bool { return b.seqs[b.ringIndex(i)] > since })
		}
	}
	records := copyRingWindow(b.buf, b.ringIndex(start), size-start)
	inFlight := b.copyPendingLocked()
	return Snapshot{
		Records:         records,
		InFlightRecords: inFlight,
		PendingRevision: b.pendingRevision,
		BufferSize:      size,
		Counters:        b.Counters(),
		Seq:             latest,
		OldestSeq:       oldest,
		FeedID:          b.feedID,
		Incremental:     !full,
	}
}

// SnapshotJSONSince returns the live-payload JSON for the given cursor.
func (b *Buffer) SnapshotJSONSince(since int64) ([]byte, error) {
	return json.Marshal(b.SnapshotSince(since))
}

// PendingRecords returns the current in-flight begin/update snapshots
// (oldest first). Safe to call on a nil Buffer. The slice is a copy of the
// map; the records themselves are the pending snapshots (do not mutate).
func (b *Buffer) PendingRecords() []*Record {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	out := b.copyPendingLocked()
	b.mu.RUnlock()
	sortPendingRecords(out)
	return out
}

// copyPendingLocked captures the current immutable begin/update snapshots.
// Callers hold b.mu only for the map copy; ordering happens after unlock so a
// dashboard's sort never holds up Record or PublishLive.
func (b *Buffer) copyPendingLocked() []*Record {
	if len(b.pending) == 0 {
		return nil
	}
	out := make([]*Record, 0, len(b.pending))
	for _, r := range b.pending {
		if r != nil {
			out = append(out, r)
		}
	}
	return out
}

// sortPendingRecords gives every pending snapshot the same start-time/ID order.
// The slice is owned by the caller; pending record fields are immutable.
func sortPendingRecords(records []*Record) {
	slices.SortFunc(records, func(a, b *Record) int {
		if order := a.Start.Compare(b.Start); order != 0 {
			return order
		}
		return strings.Compare(a.ID, b.ID)
	})
}
