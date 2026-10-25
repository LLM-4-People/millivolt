package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/LLM-4-People/millivolt/internal/metrics"
)

// Totals is the since-inception cross-request aggregate the dashboard's KPI
// surface reads. The single writer goroutine maintains it exactly: only rows
// that have been durably committed are counted, so a dashboard aggregate that
// merges totals with the un-flushed ring records (deduped via the written-id
// set) matches the durable history byte-for-byte with no flush-lag window.
// IsError semantics come from the canonical metrics.Record.IsError.
type Totals struct {
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	Cost      float64 `json:"cost"`
	InputTok  int64   `json:"input_tokens"`
	OutputTok int64   `json:"output_tokens"`
	CacheRead int64   `json:"cache_read_tokens"`
	Reasoning int64   `json:"reasoning_tokens"`
	TTFTSum   int64   `json:"-"`
	TTFTN     int64   `json:"-"`
	TPSSum    float64 `json:"-"`
	TPSN      int64   `json:"-"`
	// Cost-bearing denominators for the KPI's blended unit economics: the
	// count of requests that reported cost > 0 and their in+out tokens.
	// Blended $/req and $/Mtok divide by these, so an unpriced provider
	// (no cost in its response → 0) can never dilute the rate.
	CostReqs  int64   `json:"-"`
	CostInOut float64 `json:"-"` // ratios are floating point; in+out need not fit int64
	err       error
}

// Err reports invalid arithmetic without exposing partial or wrapped totals.
func (t Totals) Err() error { return t.err }

// valid reports whether every field is representable: counts are non-negative
// and floats are finite and non-negative. Hot folds use AddCount/AddValue,
// whose induction is "start at zero and fold admitted contributions", so a
// persisted document is the one base they cannot re-derive; loadTotals rejects
// an out-of-range document and rebuilds from the authoritative rows instead of
// seeding an accumulator that can no longer detect the corruption.
func (t Totals) valid() bool {
	if t.err != nil {
		return false
	}
	for _, v := range [...]int64{t.Requests, t.Errors, t.InputTok, t.OutputTok,
		t.CacheRead, t.Reasoning, t.TTFTSum, t.TTFTN, t.TPSN, t.CostReqs} {
		if _, err := metrics.SumCounts(v); err != nil {
			return false
		}
	}
	for _, v := range [...]float64{t.Cost, t.TPSSum, t.CostInOut} {
		if _, err := metrics.SumValues(v); err != nil {
			return false
		}
	}
	return true
}

// AvgTTFT returns the mean over captured TTFT samples (ttft_ms > 0), or null
// when no sample exists - mirrors the dashboard's "- not 0" rule.
func (t Totals) AvgTTFT() *float64 {
	if t.TTFTN == 0 {
		return nil
	}
	return metrics.ScaledRatio(float64(t.TTFTSum), float64(t.TTFTN), 1)
}

// AvgTPS returns the mean over captured generation-speed samples (tps > 0).
func (t Totals) AvgTPS() *float64 {
	if t.TPSN == 0 {
		return nil
	}
	return metrics.ScaledRatio(t.TPSSum, float64(t.TPSN), 1)
}

// Add is the shared durable/ring accounting owner. Arithmetic failure stays
// attached to the aggregate until a rebuild, never masquerading as zero.
func (t *Totals) Add(r *metrics.Record) {
	if r == nil || t.err != nil {
		return
	}
	t.Requests++
	if r.IsError() {
		t.Errors++
	}
	// Contributions are admitted per record and every step's overflow is
	// checked, so the durable accumulator is a checked sum by induction and is
	// never re-validated here. t.err keeps the fail-closed latch.
	if err := metrics.AddValue(&t.Cost, r.Cost); err != nil {
		t.err = err
		return
	}
	// Only cost-reporting requests feed the blended denominators - a 0-cost
	// request (provider sent no price) adds tokens but must not drag the
	// blended rate down.
	if r.Cost > 0 {
		t.CostReqs++
		if err := metrics.AddValue(&t.CostInOut, float64(r.Usage.InputTokens)); err != nil {
			t.err = err
			return
		}
		if err := metrics.AddValue(&t.CostInOut, float64(r.Usage.OutputTokens)); err != nil {
			t.err = err
			return
		}
	}
	if err := metrics.AddCount(&t.InputTok, r.Usage.InputTokens); err != nil {
		t.err = err
		return
	}
	if err := metrics.AddCount(&t.OutputTok, r.Usage.OutputTokens); err != nil {
		t.err = err
		return
	}
	if err := metrics.AddCount(&t.CacheRead, r.Usage.CacheReadTokens); err != nil {
		t.err = err
		return
	}
	if err := metrics.AddCount(&t.Reasoning, r.Usage.ReasoningTokens); err != nil {
		t.err = err
		return
	}
	// TTFTMs == 0 means "absent", not "0 ms" - no-token requests must not
	// dilute the mean (same gate as the client's truthy r.ttft_ms).
	if r.TTFTMs > 0 {
		if err := metrics.AddCount(&t.TTFTSum, r.TTFTMs); err != nil {
			t.err = err
			return
		}
		t.TTFTN++
	}
	if tps := r.DecodeTPS; tps > 0 {
		if err := metrics.AddValue(&t.TPSSum, tps); err != nil {
			t.err = err
			return
		}
		t.TPSN++
	}
}

// computeTotals rebuilds the aggregate from the durable rows (startup +
// post-merge). One full scan; runs synchronously at Open or while the caller
// holds the sole writer connection.
func (s *Store) computeTotals(ctx context.Context) error {
	return s.computeTotalsWith(ctx, nil)
}

// computeTotalsWith is computeTotals with a test seam between the consistent
// read snapshot and the writer-side epoch check. Production passes nil.
func (s *Store) computeTotalsWith(ctx context.Context, afterRead func() error) error {
	const attempts = 3
	for attempt := 0; attempt < attempts; attempt++ {
		t, epoch, highRowID, err := s.readTotalsSnapshot(ctx)
		if err != nil {
			return err
		}
		if afterRead != nil {
			if err := afterRead(); err != nil {
				return err
			}
		}
		ok, err := s.persistTotalsIfCurrent(ctx, t, epoch, highRowID)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return fmt.Errorf("totals snapshot changed while rebuilding")
}

// readTotalsSnapshot reads the rows and their mutation witness from one
// consistent read transaction. The epoch and high rowid are the same witness
// the writer-side recheck compares against.
func (s *Store) readTotalsSnapshot(ctx context.Context) (Totals, int64, int64, error) {
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Totals{}, 0, 0, err
	}
	defer tx.Rollback()
	t, err := readTotals(ctx, tx)
	if err != nil {
		return Totals{}, 0, 0, err
	}
	epoch, err := projectionEpoch(ctx, tx)
	if err != nil {
		return Totals{}, 0, 0, err
	}
	highRowID, err := projectionHighRowID(ctx, tx)
	if err != nil {
		return Totals{}, 0, 0, err
	}
	return t, epoch, highRowID, nil
}

// persistTotalsIfCurrent commits the document only when both mutation
// witnesses still match the read snapshot: the epoch covers rewrites,
// removals and out-of-order rowids, and the high rowid covers ordinary
// appends that leave the epoch unchanged. Holding the sole writer connection
// through installTotals prevents an in-process commit from landing between the
// check and publication; the next boot rebuilds if an unsupported external
// writer commits after the check.
func (s *Store) persistTotalsIfCurrent(ctx context.Context, t Totals, epoch, highRowID int64) (bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	current, err := projectionEpoch(ctx, tx)
	if err != nil {
		return false, err
	}
	currentHigh, err := projectionHighRowID(ctx, tx)
	if err != nil {
		return false, err
	}
	if current != epoch || currentHigh != highRowID {
		return false, nil
	}
	if err := saveTotals(ctx, tx, t, false, current, currentHigh); err != nil {
		return false, fmt.Errorf("persist totals: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.installTotals(t, current, currentHigh)
	return true, nil
}

func (s *Store) installTotals(t Totals, epoch, highRowID int64) {
	s.aggMu.Lock()
	s.totals = t
	s.totalsBuilt = true
	s.totalsWitness = totalsWitness{epoch: epoch, highRowID: highRowID}
	s.totalsDegraded = false
	s.accountingRevision.Add(1)
	s.aggMu.Unlock()
}

// TotalsDegraded reports whether the boot-time totals scan failed, leaving
// the aggregate a truthful subset (rows committed since the failed scan)
// instead of since-inception totals.
func (s *Store) TotalsDegraded() bool {
	s.aggMu.Lock()
	defer s.aggMu.Unlock()
	return s.totalsDegraded
}

// totalsMetaKey is the durable since-inception aggregate. It is written in the
// same transaction as every insert, full purge, filtered purge and merge, so a
// reboot loads one row instead of scanning all history. Version gates the
// document: a semantics change bumps it and forces one rebuilding scan.
//
// v3: TPS means the single first-token-to-last-token rate (gen_tps). The old
// document's tps_sum mixed overall_tps and gen_tps, so it is rebuilt.
const totalsMetaKey = "totals"

const totalsMetaVersion = 3

// totalsWitness is the projection state the in-memory totals were read or
// computed against. A batch may only extend a base whose witness is still
// current; otherwise it rebuilds from durable rows instead of blessing an
// undercount with newer witnesses. Protected by aggMu.
type totalsWitness struct {
	epoch     int64
	highRowID int64
}

// totalsWitnessNewer reports whether a describes a later projection mutation
// than b. The epoch advances on rewrites/removals; within one epoch the high
// rowid advances on appends.
func totalsWitnessNewer(a, b totalsWitness) bool {
	if a.epoch != b.epoch {
		return a.epoch > b.epoch
	}
	return a.highRowID > b.highRowID
}

// totalsDoc is the persisted shape. Totals' JSON tags are the dashboard API
// contract; this type owns the storage contract separately.
type totalsDoc struct {
	V int `json:"v"`
	// Epoch is the projection mutation epoch observed after the same
	// transaction's request triggers. Rewrites, removals and out-of-order
	// rowids advance it, so a stale document is detected at boot and rebuilt
	// instead of trusted over authoritative SQLite rows.
	Epoch int64 `json:"epoch"`
	// HighRowID is the projection high-water mark observed in the same
	// transaction. An ordinary external append keeps the epoch unchanged but
	// always advances this mark, so it catches the one mutation the epoch
	// deliberately leaves out of the fast path.
	HighRowID int64   `json:"high_rowid"`
	Invalid   bool    `json:"invalid,omitempty"`
	Requests  int64   `json:"requests"`
	Errors    int64   `json:"errors"`
	Cost      float64 `json:"cost"`
	InputTok  int64   `json:"input_tokens"`
	OutputTok int64   `json:"output_tokens"`
	CacheRead int64   `json:"cache_read_tokens"`
	Reasoning int64   `json:"reasoning_tokens"`
	TTFTSum   int64   `json:"ttft_sum"`
	TTFTN     int64   `json:"ttft_n"`
	TPSSum    float64 `json:"tps_sum"`
	TPSN      int64   `json:"tps_n"`
	CostReqs  int64   `json:"cost_reqs"`
	CostInOut float64 `json:"cost_in_out"`
}

func totalsDocument(t Totals, invalid bool, epoch, highRowID int64) totalsDoc {
	return totalsDoc{
		V: totalsMetaVersion, Epoch: epoch, HighRowID: highRowID, Invalid: invalid || t.err != nil,
		Requests: t.Requests, Errors: t.Errors, Cost: t.Cost,
		InputTok: t.InputTok, OutputTok: t.OutputTok,
		CacheRead: t.CacheRead, Reasoning: t.Reasoning,
		TTFTSum: t.TTFTSum, TTFTN: t.TTFTN,
		TPSSum: t.TPSSum, TPSN: t.TPSN,
		CostReqs: t.CostReqs, CostInOut: t.CostInOut,
	}
}

func (d totalsDoc) totals() Totals {
	return Totals{
		Requests: d.Requests, Errors: d.Errors, Cost: d.Cost,
		InputTok: d.InputTok, OutputTok: d.OutputTok,
		CacheRead: d.CacheRead, Reasoning: d.Reasoning,
		TTFTSum: d.TTFTSum, TTFTN: d.TTFTN,
		TPSSum: d.TPSSum, TPSN: d.TPSN,
		CostReqs: d.CostReqs, CostInOut: d.CostInOut,
	}
}

// saveTotals writes the aggregate and both mutation witnesses on the caller's
// transaction/connection. It must share the mutation's transaction: a totals
// row ahead of or behind the request rows would turn a crash into silently
// wrong since-inception KPIs, and a witness read outside that transaction
// could bless an aggregate that never saw its rows.
func saveTotals(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, t Totals, invalid bool, epoch, highRowID int64) error {
	b, err := json.Marshal(totalsDocument(t, invalid, epoch, highRowID))
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT OR REPLACE INTO meta(key, value) VALUES (?, ?)`, totalsMetaKey, string(b))
	return err
}

// loadTotals returns the persisted aggregate and the projection witness it was
// written against. found is false for an absent, malformed, invalid or
// older-version document, in which case the caller rebuilds from the durable
// rows and rewrites the document.
func loadTotals(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (t Totals, witness totalsWitness, found bool, err error) {
	var raw string
	var epoch, highRowID int64
	// One statement reads the document and both mutation witnesses from the
	// same SQLite snapshot, so a concurrent commit cannot land between them.
	err = db.QueryRowContext(ctx, `SELECT m.value, s.epoch, s.high_rowid
		FROM meta AS m CROSS JOIN request_projection_state AS s
		WHERE m.key = ? AND s.singleton = 1`, totalsMetaKey).Scan(&raw, &epoch, &highRowID)
	if errors.Is(err, sql.ErrNoRows) {
		return Totals{}, totalsWitness{}, false, nil
	}
	if err != nil {
		return Totals{}, totalsWitness{}, false, err
	}
	var d totalsDoc
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		log.Printf("storage: persisted totals malformed (%v); rebuilding", err)
		return Totals{}, totalsWitness{}, false, nil
	}
	if d.V != totalsMetaVersion || d.Invalid {
		return Totals{}, totalsWitness{}, false, nil
	}
	if epoch != d.Epoch || highRowID != d.HighRowID {
		// Authoritative rows changed outside this store's transaction (the
		// projection triggers fired); the cached aggregate is no longer exact.
		return Totals{}, totalsWitness{}, false, nil
	}
	t = d.totals()
	if !t.valid() {
		log.Printf("storage: persisted totals outside their representable range; rebuilding")
		return Totals{}, totalsWitness{}, false, nil
	}
	return t, totalsWitness{epoch: epoch, highRowID: highRowID}, true, nil
}

// projectionEpoch reads the current mutation witness on the caller's
// transaction/connection. Triggers keep it current within the transaction.
func projectionEpoch(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int64, error) {
	var epoch int64
	err := db.QueryRowContext(ctx, `SELECT epoch FROM request_projection_state WHERE singleton = 1`).Scan(&epoch)
	return epoch, err
}

// projectionHighRowID reads the append witness next to the epoch on the
// caller's transaction/connection. Ordinary inserts advance it without
// touching the epoch, so a persisted totals document must record both.
func projectionHighRowID(ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int64, error) {
	var highRowID int64
	err := db.QueryRowContext(ctx, `SELECT high_rowid FROM request_projection_state WHERE singleton = 1`).Scan(&highRowID)
	return highRowID, err
}

func readTotals(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (Totals, error) {
	rows, err := db.QueryContext(ctx, `SELECT status_code, error_type, attempts,
		input_tokens, output_tokens, cache_read_tokens, reasoning_tokens,
		cost, ttft_ms, gen_tps
		FROM requests`)
	if err != nil {
		return Totals{}, err
	}
	defer rows.Close()
	var t Totals
	for rows.Next() {
		var status, ttft int64
		var errorType string
		var attemptsJSON []byte
		var in, out, cacheR, reason int64
		var cost, gen float64
		if err := rows.Scan(&status, &errorType, &attemptsJSON,
			&in, &out, &cacheR, &reason, &cost, &ttft, &gen); err != nil {
			return Totals{}, err
		}
		atts, err := DecodeAttemptsColumn(attemptsJSON)
		if err != nil {
			log.Printf("storage: totals: decode attempts: %v", err)
		}
		r := metrics.Record{StatusCode: int(status), ErrorType: errorType, Attempts: atts,
			TTFTMs: ttft, DecodeTPS: gen,
		}
		r.Usage.InputTokens = in
		r.Usage.OutputTokens = out
		r.Usage.CacheReadTokens = cacheR
		r.Usage.ReasoningTokens = reason
		r.Cost = cost
		t.Add(&r)
	}
	if err := rows.Err(); err != nil {
		return Totals{}, err
	}
	return t, nil
}
