package metrics

import (
	"encoding/json"
	"sync"
)

// eventCache retains the observed JSON payload of one published ring or
// lifecycle event. It is event-owned: Record and PublishLive attach one cache
// only when the event has more than one subscriber at publication, and every
// channel copy of that event shares the pointer. The first stream to consume
// the event encodes the current observer result; every other subscriber of
// that same event reuses the immutable bytes, so observer work and JSON
// encoding run at most once per event instead of once per attached tab. A
// shared encode failure is retained too, so every subscriber observes the same
// failure and closes/resyncs instead of retrying with a different observer
// snapshot.
//
// The cache dies with its event: subscriber channels are bounded, nothing is
// stored globally, no cache crosses events or feed epochs, and zero- or
// one-subscriber publications keep the direct encode path without allocating
// a cache.
type eventCache struct {
	once sync.Once
	data []byte
	err  error
}

// sharedEventCache owns the cache-allocation threshold: zero or one
// subscriber encodes directly, more than one shares one event cache.
func sharedEventCache(subscribers int) *eventCache {
	if subscribers < 2 {
		return nil
	}
	return &eventCache{}
}

// encode invokes marshal at most once and retains both its bytes and its
// error. sync.Once publishes the retained result to concurrent consumers.
func (c *eventCache) encode(marshal func() ([]byte, error)) ([]byte, error) {
	c.once.Do(func() {
		c.data, c.err = marshal()
	})
	return c.data, c.err
}

// encodeRingEvent is the one encoder for the "record" SSE payload. A shared
// event encodes the observer snapshot at its first consumer; an uncached event
// (zero or one subscriber at publish) keeps the direct marshal path. The
// returned bytes are retained: callers must not mutate them.
func (b *Buffer) encodeRingEvent(ev RingEvent) ([]byte, error) {
	if ev.cache == nil {
		return json.Marshal(b.observeRecord(ev.Record))
	}
	return ev.cache.encode(func() ([]byte, error) {
		return json.Marshal(b.observeRecord(ev.Record))
	})
}

// encodeLiveEvent is the one encoder for begin/update/end SSE payloads. It
// follows the same shared-cache contract as encodeRingEvent.
func (b *Buffer) encodeLiveEvent(ev *LiveEvent) ([]byte, error) {
	if ev.cache == nil {
		return json.Marshal(b.observeLive(ev))
	}
	return ev.cache.encode(func() ([]byte, error) {
		return json.Marshal(b.observeLive(ev))
	})
}
