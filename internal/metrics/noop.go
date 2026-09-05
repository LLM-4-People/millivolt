package metrics

// Noop is a Recorder that discards records. Used as the default until a real
// sink is configured.
type Noop struct{}

func (Noop) Record(*Record) {}

// PublishLive is a no-op (Noop has no live subscribers).
func (Noop) PublishLive(string, *Record) {}

// MultiRecorder fans each record out to multiple recorders (e.g. the live
// ring buffer and durable storage).
type MultiRecorder []Recorder

func (m MultiRecorder) Record(r *Record) {
	for _, rec := range m {
		rec.Record(r)
	}
}

// PublishLive forwards a lifecycle event to any member that publishes live
// events (the ring buffer). Members that don't (the durable store) are skipped
// - a store must persist exactly one finalized record per request, so lifecycle
// events never reach it.
func (m MultiRecorder) PublishLive(phase string, r *Record) {
	for _, rec := range m {
		if lp, ok := rec.(interface {
			PublishLive(string, *Record)
		}); ok {
			lp.PublishLive(phase, r)
		}
	}
}
