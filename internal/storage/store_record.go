package storage

import (
	"errors"
	"log"
	"time"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

type queuedRecord struct {
	record *metrics.Record
	seq    uint64
}

// Recorder couples the live ring and durable enqueue at their shared, short
// publication fence. It does not wait for storage: channel overflow still drops
// only the durable copy. Lifecycle events remain exclusively in the live buffer.
func (s *Store) Recorder(buffer *metrics.Buffer) metrics.Recorder {
	return liveRecorder{s, buffer}
}

type liveRecorder struct {
	store  *Store
	buffer *metrics.Buffer
}

func (r liveRecorder) Record(record *metrics.Record) { r.store.record(record, r.buffer) }
func (r liveRecorder) PublishLive(phase string, record *metrics.Record) {
	r.buffer.PublishLive(phase, record)
}

// Record implements metrics.Recorder. It never blocks: if the channel is
// full, the record is dropped (metrics are best-effort, not lossless).
// Records arriving after Close are dropped too: a send on the closed channel
// panics, which we recover and count as a drop rather than crash the proxy.
// The shared retention boundary runs here as well: Store.Record is a valid
// Recorder without a ring, and nothing may persist unbounded client text.
func (s *Store) Record(r *metrics.Record) {
	s.record(r, nil)
}

func (s *Store) record(r *metrics.Record, buffer *metrics.Buffer) {
	if r == nil {
		return
	}
	// Recover is a last-resort belt: the sendMu/shut guard below already makes
	// send-on-closed-channel impossible, so a panic here would be a real bug.
	defer func() {
		if recover() != nil {
			s.incrDrops()
		}
	}()
	// Bound exactly once at the shared publication fence. The ring path's
	// Buffer.Record applies the retention owner before enqueueing this same
	// record and must stay under sendMu: Clear captures the accepted count and
	// the ring removal boundary in one critical section, so ring publication
	// and the enqueue fence cannot straddle a purge. A direct Store.Record has
	// no ring and applies retention here.
	// Hold sendMu so Close cannot close ch between the shut check and the send
	// (the select never blocks: the channel is buffered and default drops).
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if buffer != nil {
		buffer.Record(r)
	} else {
		metrics.RetainRecord(r)
	}
	if s.shut {
		s.incrDrops()
		return
	}
	select {
	case s.ch <- queuedRecord{record: r, seq: s.accepted + 1}:
		s.accepted++
	case <-s.done:
		s.incrDrops()
	default:
		// Drop on overflow to protect the hot path. This is intentional:
		// the ring buffer already holds the most recent records in memory.
		s.incrDrops()
	}
}

func (s *Store) incrDrops() {
	// Atomic: the overload path must not serialize producers on a mutex
	// exactly when records are already being lost.
	s.drops.Add(1)
}

// Dropped returns the number of records dropped due to a full write channel.
func (s *Store) Dropped() uint64 { return s.drops.Load() }

func (s *Store) writer() {
	defer func() { close(s.writerDone); s.wg.Done() }()
	batch := make([]*metrics.Record, 0, s.opts.BatchCap)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.insertBatch(batch)
		if err != nil {
			if s.writerErr == nil {
				s.writerErr = err
			}
			log.Printf("storage: insert batch: %v", err)
			// Count flush failures as drops so durable-write loss is visible
			// alongside channel-overflow drops (metrics are best-effort, but
			// never silently lost).
			for range batch {
				s.incrDrops()
			}
		}
		clear(batch)
		batch = batch[:0]
		return err
	}
	var consumed uint64
	appendRecord := func(r queuedRecord) error {
		consumed = r.seq
		batch = append(batch, r.record)
		if len(batch) >= cap(batch) {
			return flush()
		}
		return nil
	}
	// A command is published at an exact accepted-record boundary. Inspect it
	// before consuming any later record, including synchronous flush/shutdown
	// drains. Only the writer waits for SQL; producers merely publish the fence.
	// A nil check must not acquire sendMu: contending request producers can
	// otherwise starve this consumer while its bounded channel overflows.
	// Clear publishes under sendMu before later enqueues; receiving such an
	// enqueue and then loading this pointer observes that command. A command
	// published after this load still includes the already-received record in
	// its accepted boundary. Thus lock-free inspection cannot cross the fence.
	servicePurge := func(held *queuedRecord) (bool, error) {
		cmd := s.pendingPurge.Load()
		if cmd == nil {
			return false, nil
		}
		var firstErr error
		usedHeld := held != nil && held.seq <= cmd.accepted
		if usedHeld {
			firstErr = appendRecord(*held)
		}
		for consumed < cmd.accepted {
			r, ok := <-s.ch
			if !ok {
				firstErr = errors.New("storage: closed before purge boundary")
				break
			}
			if err := appendRecord(r); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		if err := flush(); err != nil && firstErr == nil {
			firstErr = err
		}
		writeErr := firstErr
		var result PurgeResult
		if firstErr == nil {
			result, firstErr = s.purgeTransaction(cmd)
		}
		// Clear holds purgeMu until this result is received, so another command
		// cannot replace this one before the writer has retired it. Retire with
		// compare-and-swap anyway: clearing an exact command can never discard a
		// newer published command.
		s.pendingPurge.CompareAndSwap(cmd, nil)
		cmd.result <- purgeOutcome{result, firstErr}
		return usedHeld, writeErr
	}
	accept := func(r queuedRecord) error {
		used, err := servicePurge(&r)
		if used {
			return err
		}
		return errors.Join(err, appendRecord(r))
	}
	ticker := time.NewTicker(s.opts.FlushInterval)
	defer ticker.Stop()
	// The continuation cadence is independent of the configured flush interval
	// so a large expired sidecar set still drains promptly when that interval
	// is long.
	debugTicker := time.NewTicker(debugExpireRetryInterval)
	defer debugTicker.Stop()
	for {
		select {
		case r, ok := <-s.ch:
			if !ok {
				// Channel closed and fully drained; final flush.
				servicePurge(nil)
				flush()
				return
			}
			accept(r)
		case <-s.purgeWake:
			servicePurge(nil)
		case ret := <-s.flushCh:
			// Synchronous flush (restart handoff): absorb everything already
			// buffered, commit it, then reply. The writer stays alive - unlike
			// Close, the pipeline keeps accepting records afterwards.
			_, firstErr := servicePurge(nil)
		drain:
			for {
				select {
				case r, ok := <-s.ch:
					if !ok {
						break drain
					}
					err := accept(r)
					if err != nil && firstErr == nil {
						firstErr = err
					}
				default:
					break drain
				}
			}
			if _, err := servicePurge(nil); err != nil && firstErr == nil {
				firstErr = err
			}
			if err := flush(); err != nil && firstErr == nil {
				firstErr = err
			}
			ret <- firstErr
		case <-ticker.C:
			flush()
			s.expireDebugCapturesIfDue()
		case <-debugTicker.C:
			s.continueDebugExpire()
		case <-s.done:
			// Shutdown: Close closes s.ch right after s.done, so range drains
			// every buffered record - including any a producer raced in after
			// the done check - before the final flush.
			for r := range s.ch {
				accept(r)
			}
			servicePurge(nil)
			flush()
			return
		}
	}
}

// Flush drains every record buffered in the write channel and commits it,
// then returns - without closing the pipeline (records sent afterwards are
// written by the writer as usual). This is the restart-handoff primitive:
// the draining process makes all finalized records durable BEFORE the fresh
// process opens the database, so the child's totals scan and ring backfill
// see the complete history while the old process keeps a working store for
// a failed-handoff resume. Concurrent Close waits for the actual final drain,
// including its error; a sealed input channel alone does not prove durability.
func (s *Store) Flush() error {
	ret := make(chan error, 1)
	select {
	case s.flushCh <- ret:
	case <-s.done:
		<-s.writerDone
		return s.writerErr
	}
	select {
	case err := <-ret:
		return err
	case <-s.done:
		<-s.writerDone
		return s.writerErr
	}
}

// markWrittenLocked records id as durably written, evicting the oldest
// tracked id once the cap is reached so the written set stays bounded. The
// caller must hold s.aggMu; duplicate guards stay with each caller.
func (s *Store) markWrittenLocked(id string) {
	if s.writtenCap > 0 && len(s.writtenOrder) >= s.writtenCap {
		delete(s.written, s.writtenOrder[0])
		s.writtenOrder = s.writtenOrder[1:]
	}
	s.written[id] = struct{}{}
	s.writtenOrder = append(s.writtenOrder, id)
}

// account folds a committed batch into the written-id set and totals. When
// next is non-nil it is the exact aggregate persisted in the batch's own
// transaction; installing it (instead of re-adding) keeps memory and disk
// identical. Called by the single writer goroutine, so it never races another
// account().
func (s *Store) account(records []*metrics.Record, next *Totals, witness totalsWitness, degraded bool) {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	if next != nil {
		// A maintenance install (merge, purge, totals rebuild) can acquire the
		// writer connection after this batch commits but before account runs.
		// Such an install already counted this batch from durable rows, so a
		// newer witness wins: never overwrite it with the batch's older base.
		if !s.totalsBuilt || !totalsWitnessNewer(s.totalsWitness, witness) {
			s.totals = *next
			s.totalsBuilt = true
			s.totalsWitness = witness
			// The degraded flag describes a rebuild that never saw older rows.
			// Post-boot accounting never clears it; only a later boot's
			// successful full scan (or an explicit maintenance rebuild) does. A
			// failed in-transaction rebuild sets it so the subset is never
			// presented as since-inception.
			s.totalsDegraded = s.totalsDegraded || degraded
		}
	} else {
		if !s.totalsBuilt {
			// Degraded mode: the initial rebuild failed. Count new rows from
			// zero so live aggregates at least track post-start traffic.
			s.totals = Totals{}
			s.totalsBuilt = true
		}
		for _, r := range records {
			s.totals.Add(r)
		}
	}
	for _, r := range records {
		if _, ok := s.written[r.ID]; !ok {
			s.markWrittenLocked(r.ID)
		}
	}
	// Every successful account call follows a durable commit boundary. Publish
	// a new revision even when a duplicate record leaves the folded values
	// unchanged: a reader may have captured the old key before this commit.
	s.accountingRevision.Add(1)
}

// totalsBase returns the in-memory aggregate and the projection witness it
// was read against. The caller holds the sole writer connection (Begin has
// returned), so no merge/purge can publish between this read and the commit;
// insertBatch still compares the witness inside its transaction because an
// external restart-handoff writer can commit between boot and this batch.
func (s *Store) totalsBase() (Totals, bool, bool, totalsWitness) {
	s.aggMu.Lock()
	next := s.totals
	built := s.totalsBuilt
	degraded := s.totalsDegraded
	witness := s.totalsWitness
	s.aggMu.Unlock()
	if !built {
		next = Totals{}
	}
	return next, built, degraded, witness
}

// AccountingRevision returns the durable accounting witness used to validate
// exact KPI fold memos. It advances under aggMu whenever totals or the
// written-id set changes; reading it never takes that lock. A fold whose
// before/after reads are equal observed one coherent durable state.
func (s *Store) AccountingRevision() uint64 { return s.accountingRevision.Load() }

// TotalsAndUnwritten captures totals and filters the caller-owned ring slice
// under one accounting lock. A racing commit cannot remove a row from both
// sides of the merge. Only the bounded ring is probed; the written set is not
// copied, and record folding happens after the lock is released.
func (s *Store) TotalsAndUnwritten(ring []*metrics.Record) (Totals, []*metrics.Record) {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	out := ring[:0]
	for _, r := range ring {
		if r == nil {
			continue
		}
		if _, written := s.written[r.ID]; !written {
			out = append(out, r)
		}
	}
	return s.totals, out
}

// FilterUnwritten takes a set of record ids and returns only those not in the
// written set - the exact dedupe boundary for ring/stored merges. Takes one
// aggMu hold instead of copying the full written map, so the caller pays
// O(ring) probes instead of O(written) allocation.
func (s *Store) FilterUnwritten(ids []string) []string {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	out := ids[:0] // reuse the caller's slice to avoid allocation
	for _, id := range ids {
		if _, ok := s.written[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// Totals returns the since-inception aggregate (durably committed rows). For
// ring-merge consumers prefer TotalsAndUnwritten (atomic pair).
func (s *Store) Totals() Totals {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	return s.totals
}

// MarkWritten primes the written-id set with ids that were persisted BEFORE
// this process started (the startup ring backfill). Without it a fresh
// process would treat every backfilled record as "not yet written" and the
// dashboard aggregates would double-count the whole ring against the store.
func (s *Store) MarkWritten(ids []string) {
	if len(ids) == 0 {
		return
	}
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	changed := false
	for _, id := range ids {
		if _, ok := s.written[id]; ok {
			continue
		}
		s.markWrittenLocked(id)
		changed = true
	}
	if changed {
		s.accountingRevision.Add(1)
	}
}
