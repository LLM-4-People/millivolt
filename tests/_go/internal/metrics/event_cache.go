package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sameBacking reports whether two non-empty slices point at the same retained
// bytes. Shared events must return one backing array, not copies.
func sameBacking(a, b []byte) bool {
	return len(a) > 0 && len(b) > 0 && &a[0] == &b[0]
}

// eventRecord is a fixed-time record for byte comparisons: time_bucket is
// derived from Start, so time.Now() could cross a bucket boundary between the
// shared encode and the direct-marshal comparison.
func eventRecord(id string) *Record {
	return &Record{ID: id, Provider: "p", StatusCode: 200, Start: time.UnixMilli(1_800_000_000_000)}
}

func TestEventCacheEncode(t *testing.T) {
	t.Run("marshal once and share bytes", func(t *testing.T) {
		cache := &eventCache{}
		want := []byte(`{"record":"shared"}`)
		calls := 0
		marshal := func() ([]byte, error) {
			calls++
			return want, nil
		}
		first, err := cache.encode(marshal)
		if err != nil {
			t.Fatalf("first encode: %v", err)
		}
		second, err := cache.encode(marshal)
		if err != nil {
			t.Fatalf("second encode: %v", err)
		}
		if calls != 1 {
			t.Fatalf("marshal calls = %d, want 1", calls)
		}
		if !bytes.Equal(first, want) || !bytes.Equal(second, want) {
			t.Fatalf("encoded bytes = %q / %q, want %q", first, second, want)
		}
		if !sameBacking(first, second) {
			t.Fatal("cached encodes returned different backing arrays")
		}
	})

	t.Run("error is retained", func(t *testing.T) {
		cache := &eventCache{}
		boom := errors.New("observer result is not JSON")
		calls := 0
		marshal := func() ([]byte, error) {
			calls++
			return nil, boom
		}
		_, first := cache.encode(marshal)
		_, second := cache.encode(marshal)
		if calls != 1 {
			t.Fatalf("marshal calls = %d, want 1", calls)
		}
		if !errors.Is(first, boom) || !errors.Is(second, boom) {
			t.Fatalf("retained errors = %v / %v, want %v", first, second, boom)
		}
	})
}

// sharedEventCache owns the allocation threshold: an event with zero or one
// consumer keeps the direct encode path and must not allocate a cache; two or
// more consumers share one.
func TestSharedEventCacheAllocationPolicy(t *testing.T) {
	for _, subscribers := range []int{0, 1} {
		if cache := sharedEventCache(subscribers); cache != nil {
			t.Fatalf("sharedEventCache(%d) = %p, want nil", subscribers, cache)
		}
	}
	if cache := sharedEventCache(2); cache == nil {
		t.Fatal("sharedEventCache(2) = nil, want one event cache")
	}
}

func TestRingEventCachePublication(t *testing.T) {
	t.Run("single subscriber encodes directly", func(t *testing.T) {
		b := NewBuffer(4)
		var observes atomic.Int64
		b.SetModelObserver(func(full bool, _ ...[]*Record) any {
			if !full {
				observes.Add(1)
			}
			return nil
		})
		sub := b.Subscribe()
		defer b.Unsubscribe(sub)

		rec := eventRecord("solo")
		b.Record(rec)
		ev := <-sub
		if ev.cache != nil {
			t.Fatal("one subscriber received a shared event cache")
		}
		first, err := b.encodeRingEvent(ev)
		if err != nil {
			t.Fatal(err)
		}
		second, err := b.encodeRingEvent(ev)
		if err != nil {
			t.Fatal(err)
		}
		if got := observes.Load(); got != 2 {
			t.Fatalf("observer calls = %d, want 2 (nil cache must encode independently)", got)
		}
		if !bytes.Equal(first, second) {
			t.Fatalf("direct encodes differ: %q / %q", first, second)
		}
	})

	t.Run("two subscribers share one cache and one encode", func(t *testing.T) {
		b := NewBuffer(4)
		var observes atomic.Int64
		b.SetModelObserver(func(full bool, _ ...[]*Record) any {
			if !full {
				observes.Add(1)
			}
			return map[string]string{"model": "canonical"}
		})
		firstSub := b.Subscribe()
		secondSub := b.Subscribe()
		defer b.Unsubscribe(firstSub)
		defer b.Unsubscribe(secondSub)

		rec := eventRecord("shared")
		b.Record(rec)
		firstEv, secondEv := <-firstSub, <-secondSub
		if firstEv.cache == nil || secondEv.cache == nil {
			t.Fatalf("shared ring events have nil caches: %p / %p", firstEv.cache, secondEv.cache)
		}
		if firstEv.cache != secondEv.cache {
			t.Fatalf("channel copies do not share one cache: %p / %p", firstEv.cache, secondEv.cache)
		}
		firstData, err := b.encodeRingEvent(firstEv)
		if err != nil {
			t.Fatal(err)
		}
		secondData, err := b.encodeRingEvent(secondEv)
		if err != nil {
			t.Fatal(err)
		}
		if got := observes.Load(); got != 1 {
			t.Fatalf("observer calls = %d, want 1", got)
		}
		if !bytes.Equal(firstData, secondData) || !sameBacking(firstData, secondData) {
			t.Fatal("subscribers did not receive the same retained bytes")
		}
		direct, err := json.Marshal(b.observeRecord(rec))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(firstData, direct) {
			t.Fatalf("shared bytes = %q, direct marshal = %q", firstData, direct)
		}
	})
}

func TestLiveEventCachePublication(t *testing.T) {
	t.Run("single subscriber encodes directly", func(t *testing.T) {
		b := NewBuffer(4)
		var observes atomic.Int64
		b.SetModelObserver(func(full bool, _ ...[]*Record) any {
			if !full {
				observes.Add(1)
			}
			return nil
		})
		sub := b.SubscribeLive()
		defer b.UnsubscribeLive(sub)

		rec := record("solo")
		b.PublishLive("begin", rec)
		begin := <-sub
		if begin.cache != nil {
			t.Fatal("one live subscriber received a shared begin cache")
		}
		if _, err := b.encodeLiveEvent(begin); err != nil {
			t.Fatal(err)
		}
		if _, err := b.encodeLiveEvent(begin); err != nil {
			t.Fatal(err)
		}
		if got := observes.Load(); got != 2 {
			t.Fatalf("observer calls = %d, want 2 (nil cache must encode independently)", got)
		}

		b.PublishLive("update", rec)
		if update := <-sub; update.cache != nil {
			t.Fatal("one live subscriber received a shared update cache")
		}
		b.Record(rec)
		if end := <-sub; end.cache != nil {
			t.Fatal("one live subscriber received a shared end cache")
		}
	})

	t.Run("two subscribers share begin update and end caches", func(t *testing.T) {
		b := NewBuffer(4)
		firstSub := b.SubscribeLive()
		secondSub := b.SubscribeLive()
		defer b.UnsubscribeLive(firstSub)
		defer b.UnsubscribeLive(secondSub)

		rec := record("shared")
		b.PublishLive("begin", rec)
		beginFirst, beginSecond := <-firstSub, <-secondSub
		if beginFirst.cache == nil || beginFirst.cache != beginSecond.cache {
			t.Fatalf("begin caches = %p / %p, want one shared cache", beginFirst.cache, beginSecond.cache)
		}

		b.PublishLive("update", rec)
		updateFirst, updateSecond := <-firstSub, <-secondSub
		if updateFirst.cache == nil || updateFirst.cache != updateSecond.cache {
			t.Fatalf("update caches = %p / %p, want one shared cache", updateFirst.cache, updateSecond.cache)
		}

		b.Record(rec)
		endFirst, endSecond := <-firstSub, <-secondSub
		if endFirst.cache == nil || endFirst.cache != endSecond.cache {
			t.Fatalf("end caches = %p / %p, want one shared cache", endFirst.cache, endSecond.cache)
		}

		if beginFirst.cache == updateFirst.cache || updateFirst.cache == endFirst.cache {
			t.Fatal("cache leaked across lifecycle events")
		}
	})
}

// TestSharedEventBytesMatchDirectMarshal pins exact bytes: a shared event must
// encode the same observer JSON as the uncached direct path for records,
// begin/update and end, and every subscriber must receive that one payload.
func TestSharedEventBytesMatchDirectMarshal(t *testing.T) {
	b := NewBuffer(8)
	b.SetModelObserver(func(full bool, _ ...[]*Record) any {
		if full {
			return map[string]string{"revision": "test"}
		}
		return map[string]any{"model": "canonical", "revision": "test"}
	})
	ringA, ringB := b.Subscribe(), b.Subscribe()
	liveA, liveB := b.SubscribeLive(), b.SubscribeLive()
	defer b.Unsubscribe(ringA)
	defer b.Unsubscribe(ringB)
	defer b.UnsubscribeLive(liveA)
	defer b.UnsubscribeLive(liveB)

	start := time.UnixMilli(1_800_000_000_000)
	final := &Record{
		ID: "final", Provider: "fixture", Model: "raw-model", StatusCode: 200,
		Start: start, End: start.Add(1500 * time.Millisecond), TTFTMs: 250,
		Attempts: []RetryAttempt{{StatusCode: 429, ErrorType: "rate_limit_error"}},
	}
	b.Record(final)
	finalA, finalB := <-ringA, <-ringB
	sharedFinalA, err := b.encodeRingEvent(finalA)
	if err != nil {
		t.Fatal(err)
	}
	sharedFinalB, err := b.encodeRingEvent(finalB)
	if err != nil {
		t.Fatal(err)
	}
	directFinal, err := json.Marshal(b.observeRecord(final))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sharedFinalA, directFinal) {
		t.Fatalf("record bytes = %q, direct = %q", sharedFinalA, directFinal)
	}
	if !sameBacking(sharedFinalA, sharedFinalB) {
		t.Fatal("record subscribers did not share the retained bytes")
	}

	pending := &Record{
		ID: "pending", Provider: "fixture", Model: "raw-model", Stream: true,
		Start: start, StatusCode: 200,
	}
	b.PublishLive("begin", pending)
	beginA, beginB := <-liveA, <-liveB
	sharedBeginA, err := b.encodeLiveEvent(beginA)
	if err != nil {
		t.Fatal(err)
	}
	sharedBeginB, err := b.encodeLiveEvent(beginB)
	if err != nil {
		t.Fatal(err)
	}
	directBegin, err := json.Marshal(b.observeLive(beginA))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sharedBeginA, directBegin) {
		t.Fatalf("begin bytes = %q, direct = %q", sharedBeginA, directBegin)
	}
	if !sameBacking(sharedBeginA, sharedBeginB) {
		t.Fatal("begin subscribers did not share the retained bytes")
	}

	pending.Attempts = append(pending.Attempts, RetryAttempt{StatusCode: 503, ErrorType: "provider_error"})
	b.PublishLive("update", pending)
	updateA, updateB := <-liveA, <-liveB
	sharedUpdateA, err := b.encodeLiveEvent(updateA)
	if err != nil {
		t.Fatal(err)
	}
	sharedUpdateB, err := b.encodeLiveEvent(updateB)
	if err != nil {
		t.Fatal(err)
	}
	directUpdate, err := json.Marshal(b.observeLive(updateA))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sharedUpdateA, directUpdate) {
		t.Fatalf("update bytes = %q, direct = %q", sharedUpdateA, directUpdate)
	}
	if !sameBacking(sharedUpdateA, sharedUpdateB) {
		t.Fatal("update subscribers did not share the retained bytes")
	}

	b.Record(pending)
	endA, endB := <-liveA, <-liveB
	sharedEndA, err := b.encodeLiveEvent(endA)
	if err != nil {
		t.Fatal(err)
	}
	sharedEndB, err := b.encodeLiveEvent(endB)
	if err != nil {
		t.Fatal(err)
	}
	directEnd, err := json.Marshal(b.observeLive(endA))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(sharedEndA, directEnd) {
		t.Fatalf("end bytes = %q, direct = %q", sharedEndA, directEnd)
	}
	if !sameBacking(sharedEndA, sharedEndB) {
		t.Fatal("end subscribers did not share the retained bytes")
	}
}

// TestRingEventConcurrentEncodeOnce races two consumers on one shared event:
// the cache must invoke the observer and marshal once, and both must receive
// the same backing bytes.
func TestRingEventConcurrentEncodeOnce(t *testing.T) {
	b := NewBuffer(4)
	var observes atomic.Int64
	b.SetModelObserver(func(full bool, _ ...[]*Record) any {
		if !full {
			observes.Add(1)
		}
		return map[string]string{"model": "canonical"}
	})
	firstSub := b.Subscribe()
	secondSub := b.Subscribe()
	defer b.Unsubscribe(firstSub)
	defer b.Unsubscribe(secondSub)

	b.Record(record("concurrent"))
	firstEv, secondEv := <-firstSub, <-secondSub
	if firstEv.cache == nil || firstEv.cache != secondEv.cache {
		t.Fatalf("event caches = %p / %p, want one shared cache", firstEv.cache, secondEv.cache)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	var firstData, secondData []byte
	var firstErr, secondErr error
	wg.Go(func() {
		<-start
		firstData, firstErr = b.encodeRingEvent(firstEv)
	})
	wg.Go(func() {
		<-start
		secondData, secondErr = b.encodeRingEvent(secondEv)
	})
	close(start)
	wg.Wait()

	if firstErr != nil || secondErr != nil {
		t.Fatalf("concurrent encode errors: %v / %v", firstErr, secondErr)
	}
	if got := observes.Load(); got != 1 {
		t.Fatalf("observer calls = %d, want 1", got)
	}
	if !bytes.Equal(firstData, secondData) || !sameBacking(firstData, secondData) {
		t.Fatal("concurrent consumers did not share one encoded payload")
	}
}

// TestHandleStreamSharedRecordEncodesOnce drives two attached dashboard
// streams through one finalized record: both must emit the same frame payload
// while the observer and marshal path runs once.
func TestHandleStreamSharedRecordEncodesOnce(t *testing.T) {
	b := NewBuffer(8)
	var observes atomic.Int64
	b.SetModelObserver(func(full bool, _ ...[]*Record) any {
		if full {
			return map[string]string{"revision": "test"}
		}
		observes.Add(1)
		return map[string]string{"model": "canonical"}
	})
	type streamTab struct {
		w      *countingWriter
		cancel context.CancelFunc
		done   chan struct{}
	}
	open := func() *streamTab {
		ctx, cancel := context.WithCancel(t.Context())
		req := httptest.NewRequest(http.MethodGet, "/metrics/live/stream", nil).WithContext(ctx)
		w := &countingWriter{ResponseRecorder: httptest.NewRecorder()}
		done := make(chan struct{})
		go func() {
			b.HandleStream(w, req)
			close(done)
		}()
		return &streamTab{w: w, cancel: cancel, done: done}
	}
	first, second := open(), open()
	waitFor(t, func() bool { return first.w.n.Load() >= 1 && second.w.n.Load() >= 1 }, "both snapshot writes")

	b.Record(record("shared"))
	waitFor(t, func() bool { return first.w.n.Load() >= 2 && second.w.n.Load() >= 2 }, "both record batches")

	first.cancel()
	second.cancel()
	for _, tab := range []*streamTab{first, second} {
		select {
		case <-tab.done:
		case <-time.After(2 * time.Second):
			t.Fatal("HandleStream did not return after cancel")
		}
	}

	if got := observes.Load(); got != 1 {
		t.Fatalf("shared record event encoded %d times, want 1", got)
	}
	firstData := recordFrameData(t, first.w.Body.String())
	secondData := recordFrameData(t, second.w.Body.String())
	if firstData != secondData {
		t.Fatalf("subscribers received different shared payloads:\nfirst:  %s\nsecond: %s", firstData, secondData)
	}
	var decoded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(firstData), &decoded); err != nil {
		t.Fatalf("shared record payload is not JSON: %v\n%s", err, firstData)
	}
	if decoded.ID != "shared" {
		t.Fatalf("record id = %q, want shared", decoded.ID)
	}
}

// TestHandleStreamSharedEncodeFailureClosesBoth drives the same shared event
// through an unmarshalable observer result: one encode failure is retained in
// the event cache, so both subscribers close and resync instead of one
// re-encoding a different snapshot.
func TestHandleStreamSharedEncodeFailureClosesBoth(t *testing.T) {
	b := NewBuffer(8)
	var observes atomic.Int64
	b.SetModelObserver(func(full bool, _ ...[]*Record) any {
		if full {
			return map[string]string{"revision": "test"}
		}
		observes.Add(1)
		return func() {}
	})
	type streamTab struct {
		w      *countingWriter
		cancel context.CancelFunc
		done   chan struct{}
	}
	open := func() *streamTab {
		ctx, cancel := context.WithCancel(t.Context())
		req := httptest.NewRequest(http.MethodGet, "/metrics/live/stream", nil).WithContext(ctx)
		w := &countingWriter{ResponseRecorder: httptest.NewRecorder()}
		done := make(chan struct{})
		go func() {
			b.HandleStream(w, req)
			close(done)
		}()
		return &streamTab{w: w, cancel: cancel, done: done}
	}
	first, second := open(), open()
	waitFor(t, func() bool { return first.w.n.Load() >= 1 && second.w.n.Load() >= 1 }, "both snapshot writes")

	b.Record(record("unmarshalable"))
	for _, tab := range []*streamTab{first, second} {
		select {
		case <-tab.done:
		case <-time.After(2 * time.Second):
			t.Fatal("stream kept running after a shared unmarshalable record event")
		}
	}
	first.cancel()
	second.cancel()
	if got := observes.Load(); got != 1 {
		t.Fatalf("unmarshalable event observed %d times, want 1 retained failure", got)
	}
	for _, tab := range []*streamTab{first, second} {
		if strings.Contains(tab.w.Body.String(), "event: record") {
			t.Fatal("unmarshalable record frame was emitted")
		}
	}
}

func recordFrameData(t *testing.T, stream string) string {
	t.Helper()
	const marker = "event: record\ndata: "
	i := strings.Index(stream, marker)
	if i < 0 {
		t.Fatalf("no record frame in stream:\n%s", stream)
	}
	data := stream[i+len(marker):]
	if j := strings.Index(data, "\n\n"); j >= 0 {
		data = data[:j]
	}
	return data
}
