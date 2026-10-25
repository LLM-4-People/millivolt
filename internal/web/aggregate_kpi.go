package web

import (
	"encoding/json"
	"sync"

	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/storage"
)

// ---------- KPI aggregate endpoint ----------

type kpiPayload struct {
	err      error
	Requests int64   `json:"requests"`
	Errors   int64   `json:"errors"`
	InFlight int64   `json:"in_flight"`
	Cost     float64 `json:"cost"`
	// Unit economics for the KPI band (server-derived, nil on a zero
	// denominator - the dashboard renders "-", never a fake 0):
	// blended $/req and $/Mtok over input+output tokens.
	CostPerReq  *float64 `json:"cost_per_req"`
	CostPerMTok *float64 `json:"cost_per_mtok"`
	InputTok    int64    `json:"input_tokens"`
	OutputTok   int64    `json:"output_tokens"`
	CacheRead   int64    `json:"cache_read_tokens"`
	Reasoning   int64    `json:"reasoning_tokens"`
	AvgTTFT     *float64 `json:"avg_ttft_ms"`
	AvgTPS      *float64 `json:"avg_tps"`
}

func (p kpiPayload) MarshalJSON() ([]byte, error) {
	if p.err != nil {
		return nil, p.err
	}
	type payload kpiPayload
	return json.Marshal(payload(p))
}

// kpiMemoKey identifies one exact KPI fold input state: the ring's finalized
// content revision plus the store's durable accounting revision. Both owners
// advance their counter inside the mutation lock before publishing, so a fold
// bracketed by equal keys observed one coherent state. The accounting value is
// zero when no store is configured.
type kpiMemoKey struct {
	finalized  uint64
	accounting uint64
}

type kpiMemoEntry struct {
	key    kpiMemoKey
	totals storage.Totals
}

// kpiMemo is the single bounded slot for the bootstrap KPI fold. No TTL and no
// per-view map: the exact key decides reuse. Only the folded totals are
// retained; live counters are read at serve time.
//
// Cold folds are coalesced by fold: the first caller for a key computes while
// later same-key callers wait, then re-check the exact key and reuse the
// leader's result. A memo hit never takes fold, so a published key stays
// immediately servable while another key folds.
type kpiMemo struct {
	mu    sync.Mutex
	entry *kpiMemoEntry
	// fold serializes cold folds. It is never held for a memo hit.
	fold sync.Mutex
	// ring is the cold fold's retained ring-copy scratch. fold serializes cold
	// folds, so exactly one fold owns it at a time; keeping it here (rather
	// than borrowing a pooled buffer) means the 1M-row ring copy is allocated
	// once for the process instead of once per poll.
	ring []*metrics.Record
	// testFold is nil in production. Package tests install it to hold one cold
	// fold in flight while same-key callers arrive; it runs under fold after
	// folds is incremented.
	testFold func(key kpiMemoKey)
	// folds counts cold-fold entries. It is written under fold; tests read it
	// after joining the callers that can reach it.
	folds int
}

func (m *kpiMemo) get(key kpiMemoKey) (storage.Totals, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entry != nil && m.entry.key == key {
		return m.entry.totals, true
	}
	return storage.Totals{}, false
}

func (m *kpiMemo) put(key kpiMemoKey, totals storage.Totals) {
	m.mu.Lock()
	m.entry = &kpiMemoEntry{key: key, totals: totals}
	m.mu.Unlock()
}

// foldKPI runs one cold fold owner. A caller that missed the fast memo waits
// here, re-checks the same key, and reuses the leader's result. Different keys
// are not coalesced: each computes once, serialized by this owner.
func (m *kpiMemo) foldKPI(key kpiMemoKey, compute func() (storage.Totals, bool)) storage.Totals {
	m.fold.Lock()
	defer m.fold.Unlock()
	if totals, ok := m.get(key); ok {
		return totals
	}
	m.folds++
	if m.testFold != nil {
		m.testFold(key)
	}
	totals, publish := compute()
	if publish {
		m.put(key, totals)
	}
	return totals
}

// kpiMemoKey reads the two content witnesses. The ring revision covers every
// finalized append, eviction, backfill, removal and reset; the store revision
// covers durable totals and written-id changes, including direct Record
// commits that never touch the ring.
func (a *AggAPI) kpiMemoKey() kpiMemoKey {
	finalized, _ := a.buf.Revisions()
	key := kpiMemoKey{finalized: finalized}
	if a.store != nil {
		key.accounting = a.store.AccountingRevision()
	}
	return key
}

// kpi computes the global since-inception KPI aggregate (Totals + un-flushed
// ring records, exact written-id dedupe) plus the inline unit-economics
// fields. The ONE KPI surface - served inside /metrics/bootstrap. The folded
// totals are memoized on the exact ring/durable revisions; the ring snapshot
// itself and the live in-flight gauge are never memoized. A cold miss takes
// the fold owner so concurrent callers for the same key fold once.
func (a *AggAPI) kpi() kpiPayload {
	key := a.kpiMemoKey()
	if totals, ok := a.kpiMemo.get(key); ok {
		return a.kpiFromTotals(totals)
	}
	return a.kpiFromTotals(a.kpiMemo.foldKPI(key, func() (storage.Totals, bool) {
		t := storage.Totals{}
		// The cold fold is serialized by foldKPI, so the retained scratch is
		// owned here. TotalsAndUnwritten may shorten the view in place; the
		// memo keeps the original backing array for the next fold.
		ring := a.buf.SnapshotInto(a.kpiMemo.ring)
		a.kpiMemo.ring = ring[:0]
		if a.store != nil {
			t, ring = a.store.TotalsAndUnwritten(ring)
		}
		for _, r := range ring {
			t.Add(r)
		}
		// Publishing only when the witnesses are unchanged keeps a concurrent
		// commit's data from being labeled with a stale revision (the same
		// before/after discipline as aggregateUnchanged).
		return t, a.kpiMemoKey() == key
	}))
}

func (a *AggAPI) kpiFromTotals(t storage.Totals) kpiPayload {
	// Unit economics derive inline from the folded totals (no storage change);
	// nil when the denominator is 0. Both denominators count only
	// cost-REPORTING requests (Totals.CostReqs / CostInOut, maintained by the
	// shared Totals.Add), so unpriced providers never dilute the rate.
	return kpiPayload{
		err:         t.Err(),
		Requests:    t.Requests,
		Errors:      t.Errors,
		InFlight:    a.buf.Counters().InFlight,
		Cost:        t.Cost,
		CostPerReq:  metrics.ScaledRatio(t.Cost, float64(t.CostReqs), 1),
		CostPerMTok: metrics.ScaledRatio(t.Cost, t.CostInOut, 1e6),
		InputTok:    t.InputTok,
		OutputTok:   t.OutputTok,
		CacheRead:   t.CacheRead,
		Reasoning:   t.Reasoning,
		AvgTTFT:     t.AvgTTFT(),
		AvgTPS:      t.AvgTPS(),
	}
}
