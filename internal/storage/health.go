package storage

import (
	"fmt"
	"io"
	"sort"
	"sync/atomic"
	"time"
)

// Drop causes. Every discard from the durable write pipeline is counted under
// exactly one of them: the sum is the old single counter, and the label is what
// tells an operator whether the cause was overload, shutdown, a panic or a
// failing database.
const (
	// DropOverflow: the write channel was full (overload).
	DropOverflow = "overflow"
	// DropAfterClose: the record arrived after Close sealed the pipeline.
	DropAfterClose = "after_close"
	// DropPanic: the publication fence recovered a panic.
	DropPanic = "panic"
	// DropFlushFailed: the batch containing the record failed to commit.
	DropFlushFailed = "flush_failed"
)

// dropCauses is the closed set the exposition always renders, so a series is
// present and zero before anything goes wrong.
var dropCauses = [...]string{DropOverflow, DropAfterClose, DropPanic, DropFlushFailed}

// wedgeWindowFloor is the shortest window allowed before a non-empty write
// queue with no successful flush counts as wedged. The live window is four
// configured flush intervals, so a normal slow tick is absorbed; the floor
// keeps a very short interval from flapping.
const wedgeWindowFloor = 30 * time.Second

// health is the one owner of the durable pipeline's health counters. It is
// separate from Store so the accounting rules live in one place and the writer
// cannot forget a cause.
type health struct {
	drops  [len(dropCauses)]atomic.Uint64
	failed atomic.Uint64
	// lastFlushUnixNano is zero until the first successful commit.
	lastFlushUnixNano atomic.Int64
}

func (h *health) drop(cause string) { h.dropN(cause, 1) }

// dropN books n discarded records under one cause: a failed batch drops every
// record it held, and one label lookup covers the whole batch.
func (h *health) dropN(cause string, n int) {
	if n <= 0 {
		return
	}
	if i := causeIndex(cause); i >= 0 {
		h.drops[i].Add(uint64(n))
	}
}

// recordFlush books one flush attempt: a success advances the last-commit
// stamp the wedge test reads, a failure makes readiness sticky.
func (h *health) recordFlush(err error, at time.Time) {
	if err != nil {
		h.failed.Add(1)
		return
	}
	h.lastFlushUnixNano.Store(at.UnixNano())
}

func causeIndex(cause string) int {
	for i, c := range dropCauses {
		if c == cause {
			return i
		}
	}
	return -1
}

// Health is one coherent read of the durable write pipeline, for the
// exposition and the readiness answer.
type Health struct {
	// Enabled is false when the store is absent; readiness then never fails
	// for storage.
	Enabled bool
	// QueueDepth/QueueCapacity are the current buffered records and the bound.
	QueueDepth    int
	QueueCapacity int
	// Drops counts discarded records by cause since boot.
	Drops map[string]uint64
	// FailedFlushes counts failed commits since boot; it is sticky, so a
	// single lost batch keeps readiness red until an operator looks.
	FailedFlushes uint64
	// LastFlush is the last successful commit, zero if none succeeded.
	LastFlush time.Time
	// Wedged reports a non-empty queue with no successful flush inside the
	// wedge window: the writer goroutine is stuck or gone.
	Wedged bool
}

// Health reads the pipeline state. It is safe on a nil store (storage off): the
// answer is a disabled, all-zero document.
func (s *Store) Health() Health {
	h := Health{Drops: make(map[string]uint64, len(dropCauses))}
	for _, cause := range dropCauses {
		h.Drops[cause] = 0
	}
	if s == nil {
		return h
	}
	now := time.Now()
	h.Enabled = true
	h.QueueCapacity = cap(s.ch)
	h.QueueDepth = len(s.ch)
	h.FailedFlushes = s.health.failed.Load()
	for i, cause := range dropCauses {
		h.Drops[cause] = s.health.drops[i].Load()
	}
	if ns := s.health.lastFlushUnixNano.Load(); ns != 0 {
		h.LastFlush = time.Unix(0, ns)
	}
	window := max(4*s.opts.FlushInterval, wedgeWindowFloor)
	h.Wedged = h.QueueDepth > 0 && (h.LastFlush.IsZero() || now.Sub(h.LastFlush) > window)
	return h
}

// Ready answers the readiness probe. It fails on a wedged writer and on any
// failed flush since boot, and names the reason for the operator.
func (s *Store) Ready() (bool, string) {
	if s == nil {
		return true, ""
	}
	h := s.Health()
	if h.FailedFlushes > 0 {
		return false, fmt.Sprintf("%d durable flush(es) failed since boot", h.FailedFlushes)
	}
	if h.Wedged {
		return false, fmt.Sprintf("write queue holds %d of %d records with no successful flush since %s",
			h.QueueDepth, h.QueueCapacity, h.LastFlush.UTC().Format(time.RFC3339))
	}
	return true, ""
}

// valueless guarantees every cause series is present (at zero when unseen), so
// the exposition has one shape and a scraper can rate() a cause before the
// first drop.
func (h Health) valueless() Health {
	if h.Drops == nil {
		h.Drops = make(map[string]uint64, len(dropCauses))
	}
	for _, cause := range dropCauses {
		if _, ok := h.Drops[cause]; !ok {
			h.Drops[cause] = 0
		}
	}
	return h
}

// WritePrometheus renders the storage block of the exposition. It is produced
// per scrape rather than with the ring body: queue depth and the drop counters
// move without a finalized record, so the ring-revision memo would freeze them.
// A disabled store renders only the enabled flag, because the pipeline series
// would all be meaningless zeros.
func (h Health) WritePrometheus(w io.Writer) {
	h = h.valueless()
	fmt.Fprintf(w, "# HELP llm_proxy_storage_enabled Durable storage is configured.\n")
	fmt.Fprintf(w, "# TYPE llm_proxy_storage_enabled gauge\n")
	fmt.Fprintf(w, "llm_proxy_storage_enabled %d\n", boolGauge(h.Enabled))
	if !h.Enabled {
		return
	}
	fmt.Fprintf(w, "# HELP llm_proxy_storage_dropped_total Records discarded from the durable pipeline since boot, by cause.\n")
	fmt.Fprintf(w, "# TYPE llm_proxy_storage_dropped_total counter\n")
	causes := make([]string, 0, len(h.Drops))
	for cause := range h.Drops {
		causes = append(causes, cause)
	}
	sort.Strings(causes)
	for _, cause := range causes {
		fmt.Fprintf(w, "llm_proxy_storage_dropped_total{cause=%q} %d\n", cause, h.Drops[cause])
	}
	fmt.Fprintf(w, "# HELP llm_proxy_storage_flush_failed_total Failed durable commits since boot.\n")
	fmt.Fprintf(w, "# TYPE llm_proxy_storage_flush_failed_total counter\n")
	fmt.Fprintf(w, "llm_proxy_storage_flush_failed_total %d\n", h.FailedFlushes)
	fmt.Fprintf(w, "# HELP llm_proxy_storage_queue_depth Records buffered between the proxy and the writer.\n")
	fmt.Fprintf(w, "# TYPE llm_proxy_storage_queue_depth gauge\n")
	fmt.Fprintf(w, "llm_proxy_storage_queue_depth %d\n", h.QueueDepth)
	fmt.Fprintf(w, "# HELP llm_proxy_storage_queue_capacity Records the buffer can hold before drops.\n")
	fmt.Fprintf(w, "# TYPE llm_proxy_storage_queue_capacity gauge\n")
	fmt.Fprintf(w, "llm_proxy_storage_queue_capacity %d\n", h.QueueCapacity)
	fmt.Fprintf(w, "# HELP llm_proxy_storage_last_flush_timestamp_seconds Unix time of the last successful durable commit.\n")
	fmt.Fprintf(w, "# TYPE llm_proxy_storage_last_flush_timestamp_seconds gauge\n")
	if h.LastFlush.IsZero() {
		fmt.Fprintf(w, "llm_proxy_storage_last_flush_timestamp_seconds 0\n")
	} else {
		fmt.Fprintf(w, "llm_proxy_storage_last_flush_timestamp_seconds %d\n", h.LastFlush.Unix())
	}
	fmt.Fprintf(w, "# HELP llm_proxy_storage_wedged The write queue is non-empty with no successful flush inside the wedge window.\n")
	fmt.Fprintf(w, "# TYPE llm_proxy_storage_wedged gauge\n")
	fmt.Fprintf(w, "llm_proxy_storage_wedged %d\n", boolGauge(h.Wedged))
}

// WritePrometheus is the store method the exposition provider calls: a nil
// store still renders the enabled=0 document.
func (s *Store) WritePrometheus(w io.Writer) {
	if s == nil {
		Health{}.valueless().WritePrometheus(w)
		return
	}
	s.Health().WritePrometheus(w)
}

func boolGauge(b bool) int {
	if b {
		return 1
	}
	return 0
}
