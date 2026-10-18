package metrics

import "time"

// TimeBucket is the server-local display/scope owner. A wire timestamp's
// original location must not change its classification after persistence.
func TimeBucket(t time.Time) string {
	t = t.In(time.Local)
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return "weekend"
	}
	switch h := t.Hour(); {
	case h < 8:
		return "night"
	case h < 16:
		return "work"
	default:
		return "evening"
	}
}

// ObservedRecord adds derived observer data without copying/mutating a stored
// record or adding a custom per-record JSON encoder. Metadata is generated at
// HTTP serialization boundaries, never by the proxy recorder.
type ObservedRecord struct {
	*Record
	TimeBucket string `json:"time_bucket"`
	ModelCanon any    `json:"model_canon,omitempty"`
}

func ObserveRecords(records []*Record) []ObservedRecord {
	if records == nil {
		return nil
	}
	out := make([]ObservedRecord, len(records))
	for i, r := range records {
		if r != nil {
			out[i] = ObservedRecord{Record: r, TimeBucket: TimeBucket(r.Start)}
		}
	}
	return out
}

// modelObserverFunc is the installed canonical-name provider. It is immutable
// per installed value; SetModelObserver swaps the whole holder atomically.
type modelObserverFunc func(bool, ...[]*Record) any

// SetModelObserver installs the startup-wired canonical-name provider. The
// metrics package deliberately knows no configuration or regex implementation.
// The holder is lock-free: SSE record/end serialization must never contend with
// Record's ring lock, and the provider is installed once at startup (or on an
// explicit reload), not per event.
func (b *Buffer) SetModelObserver(fn func(bool, ...[]*Record) any) {
	if fn == nil {
		b.modelObserver.Store(nil)
		return
	}
	installed := modelObserverFunc(fn)
	b.modelObserver.Store(&installed)
}

func (b *Buffer) observerModels(full bool, records ...[]*Record) any {
	fn := b.modelObserver.Load()
	if fn == nil {
		return nil
	}
	return (*fn)(full, records...)
}

type ObservedSnapshot struct {
	Snapshot
	Records         []ObservedRecord `json:"records"`
	InFlightRecords []ObservedRecord `json:"in_flight_records"`
	ModelCanon      any              `json:"model_canon,omitempty"`
}

func ObserveSnapshot(s Snapshot, modelCanon any) ObservedSnapshot {
	return ObservedSnapshot{Snapshot: s, Records: ObserveRecords(s.Records),
		InFlightRecords: ObserveRecords(s.InFlightRecords), ModelCanon: modelCanon}
}

func (b *Buffer) observeRecord(r *Record) ObservedRecord {
	return ObservedRecord{Record: r, TimeBucket: TimeBucket(r.Start), ModelCanon: b.observerModels(false, []*Record{r})}
}

func (b *Buffer) observeLive(ev *LiveEvent) any {
	return struct {
		*LiveEvent
		Record     ObservedRecord `json:"record"`
		ModelCanon any            `json:"model_canon,omitempty"`
	}{ev, ObservedRecord{Record: ev.Record, TimeBucket: TimeBucket(ev.Record.Start)}, b.observerModels(false, []*Record{ev.Record})}
}
