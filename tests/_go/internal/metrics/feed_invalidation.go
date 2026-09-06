package metrics

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentRecordPublicationSequence(t *testing.T) {
	old := runtime.GOMAXPROCS(8)
	defer runtime.GOMAXPROCS(old)
	const writers = 64 // Fits subChanCap: this tests ordering, not overflow.
	for round := range 100 {
		b := NewBuffer(writers)
		ch := b.Subscribe()
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range writers {
			wg.Go(func() {
				<-start
				b.Record(&Record{ID: strconv.Itoa(i), StatusCode: 200})
			})
		}
		close(start)
		wg.Wait()
		for seq := int64(1); seq <= writers; seq++ {
			if ev := <-ch; ev.Seq != seq {
				t.Fatalf("round %d: publication seq %d, want %d; reconnect would skip an unpublished record", round, ev.Seq, seq)
			}
		}
		b.Unsubscribe(ch)
	}
}

func TestLifecycleRevisionOwnsSnapshotGauge(t *testing.T) {
	b := NewBuffer(8)
	ch := b.SubscribeLive()
	defer b.UnsubscribeLive(ch)
	r := &Record{ID: "pending"}
	for i, phase := range []string{"begin", "update", "end"} {
		if phase == "end" {
			b.Record(r)
		} else {
			b.PublishLive(phase, r)
		}
		ev := <-ch
		snap := b.SnapshotSince(0)
		if ev.PendingRevision != uint64(i+1) || snap.PendingRevision != ev.PendingRevision ||
			snap.Counters.InFlight != ev.InFlight || ev.InFlight != int64(len(snap.InFlightRecords)) {
			t.Fatalf("%s revision/gauge mismatch: event=%+v snapshot=%+v", phase, ev, snap)
		}
		// Invalid lifecycle transitions never change the registry or its gauge.
		if phase == "begin" {
			b.PublishLive("begin", r)
		} else if phase == "end" {
			b.PublishLive("update", r)
		}
		if got := b.SnapshotSince(0); got.PendingRevision != snap.PendingRevision || got.Counters != snap.Counters {
			t.Fatalf("duplicate/out-of-order lifecycle changed state: %+v", got)
		}
		select {
		case ev := <-ch:
			t.Fatalf("invalid transition published %+v", ev)
		default:
		}
	}
}

func TestRemovalInvalidatesReplayEpoch(t *testing.T) {
	b := NewBuffer(8)
	b.Record(record("remove"))
	b.Record(record("survivor"))
	old := b.SnapshotSince(0)
	done := b.feedChan()
	b.RemoveWhere(func(r *Record) bool { return r.ID == "remove" })
	select {
	case <-done:
	default:
		t.Fatal("removal did not wake the existing feed")
	}
	r := httptest.NewRequest(http.MethodGet, "/?since=2&feed="+old.FeedID, nil)
	r.Header.Set("Last-Event-ID", "2")
	snap := b.SnapshotRequest(r)
	if snap.FeedID == old.FeedID || snap.Incremental || len(snap.Records) != 1 || snap.Records[0].ID != "survivor" {
		t.Fatalf("old tip cursor must see the full surviving ring: %+v", snap)
	}
	if snap.Seq != old.Seq {
		t.Fatal("removal changed surviving sequence numbers")
	}
	feed := b.FeedID()
	done = b.feedChan()
	b.RemoveWhere(func(*Record) bool { return false })
	if b.FeedID() != feed {
		t.Fatal("ring-only no-op invalidated the feed")
	}
	select {
	case <-done:
		t.Fatal("ring-only no-op closed the feed")
	default:
	}

	// A successful durable delete can affect archive rows absent from the ring.
	b.RemoveThrough(b.RemovalBoundary(), func(*Record) bool { return false })
	if b.FeedID() == feed {
		t.Fatal("durable-only deletion did not invalidate archive views")
	}
}

func TestRemovalEpochPreservesPostBoundaryAndPending(t *testing.T) {
	b := NewBuffer(8)
	b.Record(record("old"))
	boundary := b.RemovalBoundary()
	feed := b.FeedID()
	b.PublishLive("begin", &Record{ID: "pending"})
	b.Record(record("new"))
	b.RemoveThrough(boundary, nil)
	snap := b.SnapshotRequest(httptest.NewRequest(http.MethodGet, "/?since=2&feed="+feed, nil))
	if snap.Incremental || len(snap.Records) != 1 || snap.Records[0].ID != "new" || snap.Counters.TotalReq != 1 {
		t.Fatalf("post-boundary completion was lost: %+v", snap)
	}
	if len(snap.InFlightRecords) != 1 || snap.InFlightRecords[0].ID != "pending" {
		t.Fatal("purge removed an in-flight lifecycle snapshot")
	}
	feed = snap.FeedID
	b.Reset()
	if b.FeedID() == feed || len(b.SnapshotSince(0).InFlightRecords) != 1 {
		t.Fatal("reset must rotate the replay epoch without clearing pending requests")
	}
}

// Block the first socket write, after snapshot capture. Invalidation must use
// the pre-snapshot channel, not whichever channel exists when Write returns.
type initialFeedWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	resume  chan struct{}
	written chan struct{}
}

func (w *initialFeedWriter) Write(p []byte) (int, error) {
	if w.entered != nil {
		close(w.entered)
		<-w.resume
		w.entered = nil
	}
	n, err := w.ResponseRecorder.Write(p)
	if w.written != nil {
		w.written <- struct{}{}
	}
	return n, err
}

func TestStreamInvalidationDuringInitialSnapshot(t *testing.T) {
	b := NewBuffer(8)
	b.Record(record("old"))
	feed := b.FeedID()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &initialFeedWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), resume: make(chan struct{})}
	entered := w.entered
	done := make(chan struct{})
	go func() {
		b.HandleStream(w, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
		close(done)
	}()
	<-entered
	b.Reset()
	b.Record(record("new"))
	close(w.resume)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream missed purge during snapshot write")
	}
	if !strings.Contains(w.Body.String(), "event: reset\ndata: {\"feed_id\":\""+b.FeedID()+"\"}") {
		t.Fatalf("missing immediate reset event: %s", w.Body.String())
	}
	snap := b.SnapshotRequest(httptest.NewRequest(http.MethodGet, "/?feed="+feed+"&since=1", nil))
	if snap.Incremental || len(snap.Records) != 1 || snap.Records[0].ID != "new" {
		t.Fatalf("reopened old URL did not force a fresh full snapshot: %+v", snap)
	}
}

func TestStreamRejectsDelayedPriorEpochPublications(t *testing.T) {
	b := NewBuffer(8)
	oldFeed := b.FeedID()
	b.Reset()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &initialFeedWriter{httptest.NewRecorder(), make(chan struct{}), make(chan struct{}), make(chan struct{}, 8)}
	entered := w.entered
	done := make(chan struct{})
	go func() {
		b.HandleStream(w, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
		close(done)
	}()
	<-entered
	// Record/PublishLive capture their epoch at mutation time, before their
	// nonblocking fanout. A delayed old fanout can reach a new subscriber.
	b.subsMu.Lock()
	for ch := range b.subs {
		ch <- RingEvent{Record: record("deleted-final"), Seq: 1, feedID: oldFeed}
	}
	for ch := range b.liveSubs {
		ch <- &LiveEvent{Phase: "end", Record: record("deleted-end"), feedID: oldFeed}
	}
	b.subsMu.Unlock()
	b.Record(record("new-final"))
	b.PublishLive("begin", &Record{ID: "new-pending"})
	close(w.resume)
	// Both queues must consume the old publication before their new marker.
	for range 3 { // initial snapshot + one current event from each queue
		select {
		case <-w.written:
		case <-time.After(time.Second):
			t.Fatal("stream did not deliver both current-epoch events")
		}
	}
	b.CloseFeeds()
	<-done
	if strings.Contains(w.Body.String(), "deleted-") || strings.Contains(w.Body.String(), "event: reset") {
		t.Fatalf("new stream replayed an old publication or reset on unchanged restart epoch: %s", w.Body.String())
	}
}

func TestSnapshotRequestAtomicallyValidatesEpoch(t *testing.T) {
	b := NewBuffer(8)
	b.Record(record("keep"))
	feed := b.FeedID()
	r := httptest.NewRequest(http.MethodGet, "/?feed="+feed+"&since=1", nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			b.RemoveThrough(b.RemovalBoundary(), func(*Record) bool { return false })
		}
	}()
	for range 100 {
		snap := b.SnapshotRequest(r)
		if snap.FeedID != feed && (snap.Incremental || len(snap.Records) != 1) {
			t.Fatalf("new epoch incorrectly blessed old cursor: %+v", snap)
		}
		if _, err := json.Marshal(snap); err != nil {
			t.Fatal(err)
		}
	}
	<-done
}

func TestFinalizationAndPurgeShareLifecycleBoundary(t *testing.T) {
	b := NewBuffer(8)
	r := record("completed-before-purge")
	b.PublishLive("begin", r)
	b.PublishLive("begin", &Record{ID: "still-pending"})
	live := b.SubscribeLive()
	defer b.UnsubscribeLive(live)
	feed := b.FeedID()
	b.Record(r)
	end := <-live
	if end.Phase != "end" || end.feedID != feed || end.InFlight != 1 {
		t.Fatalf("completion did not own its lifecycle end: %+v", end)
	}
	b.Reset()
	snap := b.SnapshotSince(0)
	if len(snap.Records) != 0 || len(snap.InFlightRecords) != 1 || snap.InFlightRecords[0].ID != "still-pending" {
		t.Fatalf("purged completion returned as pending: %+v", snap)
	}
	if end.feedID == snap.FeedID {
		t.Fatal("delayed completion event was retagged with the new epoch")
	}
	b.Record(record("standalone-final"))
	if b.Counters().InFlight != 1 {
		t.Fatal("standalone recording settled another request's in-flight gauge")
	}
}
