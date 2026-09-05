package metrics

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func record(id string) *Record {
	return &Record{ID: id, Provider: "p", StatusCode: 200, Start: time.Now()}
}

// payload decodes the live-payload wrapper for cursor assertions.
type payload struct {
	Records     []*Record `json:"records"`
	BufferSize  int       `json:"buffer_size"`
	Seq         int64     `json:"seq"`
	OldestSeq   int64     `json:"oldest_seq"`
	FeedID      string    `json:"feed_id"`
	Incremental bool      `json:"incremental"`
}

func decodePayload(t *testing.T, raw []byte) payload {
	t.Helper()
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("unmarshal payload: %v\n%s", err, raw)
	}
	return p
}

func TestSnapshotSinceCursorSemantics(t *testing.T) {
	b := NewBuffer(3)

	// Two records: seqs 1, 2.
	b.Record(record("a"))
	b.Record(record("b"))

	full, err := b.SnapshotJSONSince(0)
	if err != nil {
		t.Fatal(err)
	}
	p := decodePayload(t, full)
	if p.Seq != 2 || p.OldestSeq != 1 || len(p.Records) != 2 || p.Incremental {
		t.Errorf("no-cursor payload = seq%d oldest%d n%d inc%v, want seq2 oldest1 n2 incremental=false",
			p.Seq, p.OldestSeq, len(p.Records), p.Incremental)
	}
	if p.FeedID == "" {
		t.Error("feed_id empty")
	}

	// In-range cursor: only newer records.
	inc, _ := b.SnapshotJSONSince(1)
	p2 := decodePayload(t, inc)
	if !p2.Incremental || len(p2.Records) != 1 || p2.Records[0].ID != "b" {
		t.Errorf("since=1: inc=%v n=%d ids=%v, want incremental, 1 record (b)", p2.Incremental, len(p2.Records), ids(p2.Records))
	}
	if p2.FeedID != p.FeedID {
		t.Error("feed_id changed across snapshots")
	}

	// Cursor at the tip: empty delta, still incremental.
	tip, _ := b.SnapshotJSONSince(2)
	p3 := decodePayload(t, tip)
	if !p3.Incremental || len(p3.Records) != 0 {
		t.Errorf("since=2: inc=%v n=%d, want incremental, 0 records", p3.Incremental, len(p3.Records))
	}

	// Eviction: ring capacity 3, so seq1 falls out; a stale cursor must
	// degrade to a FULL snapshot, never a silently-missing delta.
	b.Record(record("c"))
	b.Record(record("d"))
	ev, _ := b.SnapshotJSONSince(1)
	p4 := decodePayload(t, ev)
	if p4.Incremental || len(p4.Records) != 3 || p4.OldestSeq != 2 || p4.Seq != 4 {
		t.Errorf("stale since=1: inc=%v n=%d oldest=%d seq=%d, want FULL 3 records oldest2 seq4",
			p4.Incremental, len(p4.Records), p4.OldestSeq, p4.Seq)
	}

	// A cursor AHEAD of the current range (restart/reset) is also full.
	fwd, _ := b.SnapshotJSONSince(99)
	p5 := decodePayload(t, fwd)
	if p5.Incremental || len(p5.Records) != 3 {
		t.Errorf("since=99: inc=%v n=%d, want FULL", p5.Incremental, len(p5.Records))
	}
}

func TestSnapshotSinceSurvivesRemoveWhere(t *testing.T) {
	b := NewBuffer(8)
	b.Record(record("a"))     // seq 1
	b.Record(record("error")) // seq 2
	b.Record(record("b"))     // seq 3

	if n := b.RemoveWhere(func(r *Record) bool { return r.ID == "error" }); n != 1 {
		t.Fatalf("removed = %d, want 1", n)
	}
	// The survivors keep their original sequences (2 is now a gap, not reused);
	// a cursor below 2 must still be in range and see only seq>since records.
	inc, _ := b.SnapshotJSONSince(1)
	p := decodePayload(t, inc)
	if !p.Incremental || len(p.Records) != 1 || p.Records[0].ID != "b" {
		t.Errorf("since=1 after purge: inc=%v n=%d ids=%v, want incremental 1 record (b)",
			p.Incremental, len(p.Records), ids(p.Records))
	}
	if p.OldestSeq != 1 || p.Seq != 3 {
		t.Errorf("cursor range = %d..%d, want 1..3 (kept sequences preserved)", p.OldestSeq, p.Seq)
	}

	// A later append continues the sequence, never reusing the gap.
	b.Record(record("c"))
	all, _ := b.SnapshotJSONSince(0)
	p2 := decodePayload(t, all)
	if p2.Seq != 4 {
		t.Errorf("seq after purge+append = %d, want 4 (nextSeq never rewinds)", p2.Seq)
	}
}

func TestResetKeepsFeedCursorForward(t *testing.T) {
	b := NewBuffer(4)
	b.Record(record("a"))
	before, _ := b.SnapshotJSONSince(0)
	pb := decodePayload(t, before)

	b.Reset()
	b.Record(record("b"))
	after, _ := b.SnapshotJSONSince(0)
	pa := decodePayload(t, after)

	if pa.Seq <= pb.Seq {
		t.Errorf("post-reset seq %d not ahead of pre-reset %d - a stale cursor would silently collide", pa.Seq, pb.Seq)
	}
	// A pre-reset cursor must force a full resync against the wiped ring.
	ev, _ := b.SnapshotJSONSince(pb.Seq)
	pe := decodePayload(t, ev)
	if pe.Incremental {
		t.Errorf("pre-reset cursor yielded incremental on a wiped ring, want full resync")
	}
	if len(pe.Records) != 1 || pe.Records[0].ID != "b" {
		t.Errorf("resync records = %v, want just (b)", ids(pe.Records))
	}
}

// TestCursorParamGate: the ONE cursor gate shared by /metrics/bootstrap and
// the SSE stream. The ?feed= pin is checked FIRST (a foreign feed forces a
// full snapshot even when a Last-Event-ID header rides along - the browser
// auto-reconnect case after a restart: a stale-but-in-range cursor against
// the NEW process's renumbered space must never yield a delta), then the
// header wins over ?since=.
func TestCursorParamGate(t *testing.T) {
	b := NewBuffer(8)
	b.Record(record("a"))
	b.Record(record("b"))
	b.Record(record("c"))
	feed := b.FeedID()

	cases := []struct {
		name, target, lastEventID string
		want                      int64
	}{
		{"no params", "/x", "", 0},
		{"since only", "/?since=2", "", 2},
		{"header wins over since", "/?since=1", "3", 3},
		{"matching feed + since", "/?since=2&feed=" + feed, "", 2},
		{"matching feed + header", "/?since=1&feed=" + feed, "3", 3},
		{"foreign feed forces full (no header)", "/?since=2&feed=dead", "", 0},
		{"foreign feed + header forces full", "/?since=2&feed=dead", "2", 0},
		{"malformed since → full", "/?since=abc", "", 0},
		{"malformed header → full", "/", "abc", 0},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.target, nil)
		if tc.lastEventID != "" {
			req.Header.Set("Last-Event-ID", tc.lastEventID)
		}
		if got := cursorParam(req, b.FeedID()); got != tc.want {
			t.Errorf("%s: CursorParam = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// runStreamCollect runs HandleStream against a caller-canceled context and
// returns everything the SSE stream produced up to cancellation.
func runStreamCollect(t *testing.T, b *Buffer, lastEventID string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/metrics/live/stream", nil)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	return runStreamRequest(t, b, req).Body.String()
}

// All stream fixtures join the handler before inspecting its headers/body.
func runStreamRequest(t *testing.T, b *Buffer, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	req = req.WithContext(ctx)
	w := &countingWriter{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		b.HandleStream(w, req)
		close(done)
	}()
	// The snapshot event is a single Write - wait for it to land instead of
	// a fixed window, then cancel the stream.
	waitFor(t, func() bool { return w.n.Load() >= 1 }, "snapshot write")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleStream did not return after cancel")
	}
	return w.ResponseRecorder
}

func TestHandleStreamNoCrossOriginSharing(t *testing.T) {
	for _, origin := range []string{"", "http://dashboard.example", "https://foreign.example", "null"} {
		t.Run(origin, func(t *testing.T) {
			b := NewBuffer(4)
			b.Record(record("first"))
			b.Record(record("second"))
			req := httptest.NewRequest(http.MethodGet, "http://dashboard.example/metrics/live/stream", nil)
			if origin != "" {
				req.Header.Set("Origin", origin)
			}
			req.Header.Set("Last-Event-ID", "1")
			w := runStreamRequest(t, b, req)
			if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/event-stream" {
				t.Fatalf("stream unavailable: status=%d headers=%v", w.Code, w.Header())
			}
			for _, name := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials"} {
				if w.Header().Get(name) != "" {
					t.Errorf("metrics feed must not grant cross-origin browser reads: %s", name)
				}
			}
			p := extractSnapshotPayload(t, w.Body.String())
			if !p.Incremental || p.Seq != 2 || len(p.Records) != 1 || p.Records[0].ID != "second" {
				t.Fatalf("ordinary replay changed: %+v", p)
			}
		})
	}
}

func TestHandleStreamReplaySinceLastEventID(t *testing.T) {
	b := NewBuffer(8)
	b.Record(record("a"))
	b.Record(record("b"))
	b.Record(record("c"))

	// No cursor: full snapshot event.
	full := runStreamCollect(t, b, "")
	if !strings.Contains(full, "event: snapshot") {
		t.Fatalf("missing snapshot event:\n%s", full)
	}
	p := extractSnapshotPayload(t, full)
	if p.Incremental || len(p.Records) != 3 {
		t.Errorf("no-cursor replay: inc=%v n=%d, want FULL 3", p.Incremental, len(p.Records))
	}

	// Cursor from a previous connection: only newer records replayed.
	delta := runStreamCollect(t, b, "2")
	dp := extractSnapshotPayload(t, delta)
	if !dp.Incremental || len(dp.Records) != 1 || dp.Records[0].ID != "c" {
		t.Errorf("Last-Event-ID=2 replay: inc=%v n=%d ids=%v, want incremental 1 (c)",
			dp.Incremental, len(dp.Records), ids(dp.Records))
	}

	// Stale cursor (record a/b evicted): full resync, never a silent gap.
	small := NewBuffer(2)
	small.Record(record("x"))
	small.Record(record("y"))
	stale := runStreamCollect(t, small, "0") // 0 = no cursor
	_ = stale
	small.Record(record("z")) // evicts x; oldest becomes 2
	stale2 := runStreamCollect(t, small, "1")
	sp := extractSnapshotPayload(t, stale2)
	if sp.Incremental {
		t.Errorf("stale Last-Event-ID=1 against evicted ring: incremental, want full resync")
	}
	if len(sp.Records) != 2 {
		t.Errorf("resync records = %v, want 2 (y, z)", ids(sp.Records))
	}
}

// countingWriter counts Write calls atomically so a test can wait until the
// handler has pushed the expected events before canceling (all body reads
// happen after cancellation, keeping -race clean).
type countingWriter struct {
	*httptest.ResponseRecorder
	n atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n.Add(1)
	return c.ResponseRecorder.Write(p)
}

func TestHandleStreamRecordEventsCarryID(t *testing.T) {
	b := NewBuffer(8)
	b.Record(record("a")) // seq 1
	b.Record(record("b")) // seq 2

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", "/metrics/live/stream", nil).WithContext(ctx)
	w := &countingWriter{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		b.HandleStream(w, req)
		close(done)
	}()

	// Wait for the snapshot write, then drive one frame of EVERY kind
	// mid-stream: a finalized record (ring → "record" event) and the
	// begin/end lifecycle pair (live fan-out only). Without the lifecycle
	// pair the no-id guard below has no lifecycle frames to scan and can
	// never fire - that is how this test's guard once went dead.
	waitFor(t, func() bool { return w.n.Load() >= 1 }, "snapshot write")
	b.Record(record("c"))               // seq 3 → record event (id: 3)
	b.PublishLive("begin", record("d")) // → begin event (no id)
	b.Record(record("d"))               // → end event (no id), plus finalized record
	waitFor(t, func() bool { return w.n.Load() >= 4 }, "record + lifecycle event writes")

	cancel()
	<-done

	out := w.Body.String()
	// The record event must carry an id: field (the feed cursor) so the
	// browser's EventSource echoes it back as Last-Event-ID on reconnect.
	if !strings.Contains(out, "id: 3\nevent: record\n") {
		t.Errorf("record event missing id cursor:\n%s", out)
	}
	if !strings.Contains(out, `"id":"c"`) {
		t.Errorf("record event missing c payload:\n%s", out)
	}
	// Frame-level replay protocol, scanned per frame so the guard actually
	// inspects the lifecycle events this test drives: record frames carry
	// the id cursor; begin/end/update frames never do (they are ephemeral,
	// not replayable).
	sawLifecycle := false
	for _, frame := range strings.Split(out, "\n\n") {
		var event, id string
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				id = strings.TrimPrefix(line, "id: ")
			}
		}
		switch event {
		case "record":
			if id == "" {
				t.Errorf("record frame %q carries no id cursor", frame)
			}
		case "begin", "end", "update":
			sawLifecycle = true
			if id != "" {
				t.Errorf("lifecycle event %s must never carry an id cursor (not replayable): %q", event, frame)
			}
		}
	}
	if !sawLifecycle {
		t.Errorf("test never drove a lifecycle event; the no-id guard scanned nothing:\n%s", out)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func extractSnapshotPayload(t *testing.T, stream string) payload {
	t.Helper()
	const marker = "event: snapshot\ndata: "
	i := strings.Index(stream, marker)
	if i < 0 {
		t.Fatalf("no snapshot event in stream:\n%s", stream)
	}
	data := stream[i+len(marker):]
	if j := strings.Index(data, "\n\n"); j >= 0 {
		data = data[:j]
	}
	var p payload
	if err := json.Unmarshal([]byte(data), &p); err != nil {
		t.Fatalf("snapshot data unparseable: %v\ndata: %s", err, data)
	}
	return p
}

func ids(recs []*Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		if r == nil {
			out[i] = "<nil>"
			continue
		}
		out[i] = r.ID
	}
	return out
}

// CloseFeeds must end in-flight dashboard SSE streams server-side (the
// graceful-restart handoff closes the feeds before draining - they would
// otherwise hold httpSrv.Shutdown open forever), while streams opened AFTER
// the close keep working: CloseFeeds swaps in a fresh channel, so a failed
// handoff that resumes serving never leaves the dashboard dead. Proxied LLM
// streams are untouched - they never subscribe here.
func TestCloseFeedsEndsStream(t *testing.T) {
	b := NewBuffer(4)
	b.Record(record("a"))
	srv := httptest.NewServer(http.HandlerFunc(b.HandleStream))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	head := make([]byte, 256)
	n, err := resp.Body.Read(head)
	if err != nil || !strings.Contains(string(head[:n]), "event: snapshot") {
		t.Fatalf("first read = %q, err %v; want the snapshot event", head[:n], err)
	}

	ended := make(chan struct{})
	go func() { io.Copy(io.Discard, resp.Body); close(ended) }()
	b.CloseFeeds()
	select {
	case <-ended:
	case <-time.After(2 * time.Second):
		t.Fatal("CloseFeeds did not end the SSE stream")
	}

	// A stream opened after the close is live (fresh feed channel) and can
	// be closed the same way.
	resp2, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("stream after CloseFeeds: %v", err)
	}
	defer resp2.Body.Close()
	n, err = resp2.Body.Read(head)
	if err != nil || !strings.Contains(string(head[:n]), "event: snapshot") {
		t.Fatalf("post-close first read = %q, err %v; want a live snapshot", head[:n], err)
	}
	ended2 := make(chan struct{})
	go func() { io.Copy(io.Discard, resp2.Body); close(ended2) }()
	b.CloseFeeds()
	select {
	case <-ended2:
	case <-time.After(2 * time.Second):
		t.Fatal("second CloseFeeds did not end the new stream")
	}
}

// runStreamURL runs HandleStream against an arbitrary target URL (the boot
// path passes ?since=&feed=; reconnects pass Last-Event-ID) and returns the
// SSE body produced up to caller cancellation.
func runStreamURL(t *testing.T, b *Buffer, target string, lastEventID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", target, nil).WithContext(ctx)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	w := &countingWriter{ResponseRecorder: httptest.NewRecorder()}
	done := make(chan struct{})
	go func() {
		b.HandleStream(w, req)
		close(done)
	}()
	// The snapshot event is a single Write - wait for it to land instead of
	// a fixed window, then cancel the stream.
	waitFor(t, func() bool { return w.n.Load() >= 1 }, "snapshot write")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("HandleStream did not return after cancel")
	}
	return w.Body.String()
}

// TestHandleStreamSinceFeedParams: the boot path passes the bootstrap
// snapshot's cursor as ?since= (+ its ?feed= pin) and the stream must open
// with a delta, never a full-ring replay; a foreign feed is a full resync
// (deny by default) and the Last-Event-ID header wins over ?since=.
func TestHandleStreamSinceFeedParams(t *testing.T) {
	b := NewBuffer(8)
	b.Record(record("a"))
	b.Record(record("b"))
	b.Record(record("c"))
	feed := b.FeedID()

	// Matching feed + in-range cursor → incremental delta (records after 2).
	delta := runStreamURL(t, b, "/metrics/live/stream?since=2&feed="+feed, "")
	dp := extractSnapshotPayload(t, delta)
	if !dp.Incremental || len(dp.Records) != 1 || dp.Records[0].ID != "c" {
		t.Errorf("?since=2&feed: inc=%v n=%d ids=%v, want incremental 1 (c)",
			dp.Incremental, len(dp.Records), ids(dp.Records))
	}

	// Foreign feed → full snapshot, never a silently trusted cursor.
	full := runStreamURL(t, b, "/metrics/live/stream?since=2&feed=deadbeef", "")
	fp := extractSnapshotPayload(t, full)
	if fp.Incremental || len(fp.Records) != 3 {
		t.Errorf("?since=2&feed=dead: inc=%v n=%d, want FULL 3 (foreign feed)", fp.Incremental, len(fp.Records))
	}

	// Last-Event-ID header wins over the stale ?since= the boot URL still carries.
	hdr := runStreamURL(t, b, "/metrics/live/stream?since=1&feed="+feed, "3")
	hp := extractSnapshotPayload(t, hdr)
	if !hp.Incremental || len(hp.Records) != 0 {
		t.Errorf("header=3 since=1: inc=%v n=%d, want incremental 0 (header wins)", hp.Incremental, len(hp.Records))
	}

	// The reconnect-after-restart combined case: a foreign ?feed= in the URL
	// (the boot URL the browser still holds) plus a Last-Event-ID header -
	// the feed gate must win and degrade to a full snapshot, never a delta
	// against the new process's renumbered sequence space.
	re := runStreamURL(t, b, "/metrics/live/stream?since=2&feed=dead", "2")
	rep := extractSnapshotPayload(t, re)
	if rep.Incremental || len(rep.Records) != 3 {
		t.Errorf("foreign feed + header: inc=%v n=%d, want FULL 3", rep.Incremental, len(rep.Records))
	}
}

// TestSnapshotLimitCapsFullOnly: SetSnapshotLimit bounds FULL snapshots to
// the newest N records (a cursor-less boot needs only the first log pages;
// older history pages from the store) but must NEVER shrink an incremental
// delta - a capped delta would be a silent miss.
func TestSnapshotLimitCapsFullOnly(t *testing.T) {
	b := NewBuffer(8)
	for _, id := range []string{"a", "b", "c", "d"} {
		b.Record(record(id))
	}
	b.SetSnapshotLimit(2)

	full, err := b.SnapshotJSONSince(0)
	if err != nil {
		t.Fatal(err)
	}
	fp := decodePayload(t, full)
	if fp.Incremental || len(fp.Records) != 2 || fp.Records[0].ID != "c" || fp.Records[1].ID != "d" {
		t.Errorf("capped full: inc=%v n=%d ids=%v, want FULL newest 2 (c,d)",
			fp.Incremental, len(fp.Records), ids(fp.Records))
	}

	// Delta unaffected by the cap.
	inc, _ := b.SnapshotJSONSince(2)
	ip := decodePayload(t, inc)
	if !ip.Incremental || len(ip.Records) != 2 || ip.Records[0].ID != "c" || ip.Records[1].ID != "d" {
		t.Errorf("capped delta: inc=%v n=%d ids=%v, want incremental 2 (c,d) uncapped",
			ip.Incremental, len(ip.Records), ids(ip.Records))
	}

	// Limit 0 = unlimited (memory-only mode).
	b.SetSnapshotLimit(0)
	full2, _ := b.SnapshotJSONSince(0)
	if p := decodePayload(t, full2); p.Incremental || len(p.Records) != 4 {
		t.Errorf("unlimited full: inc=%v n=%d, want FULL 4", p.Incremental, len(p.Records))
	}
}

// TestBootstrapCursorFeedsStream: the cross-surface contract of the ONE
// cursor path - a bootstrap full snapshot's (seq, feed_id) passed to the SSE
// stream as ?since=&feed= must yield exactly the post-cursor records (the
// gap between the two reads is closed, never a silent miss or a replay).
func TestBootstrapCursorFeedsStream(t *testing.T) {
	b := NewBuffer(8)
	b.Record(record("a"))
	b.Record(record("b"))
	b.SetSnapshotLimit(2)

	// Boot: cursor-less bootstrap (capped full snapshot).
	boot, err := b.SnapshotJSONSince(0)
	if err != nil {
		t.Fatal(err)
	}
	bp := decodePayload(t, boot)
	if bp.Incremental || bp.Seq != 2 {
		t.Fatalf("boot payload: inc=%v seq=%d, want full seq2", bp.Incremental, bp.Seq)
	}

	// Traffic lands between bootstrap and stream connect.
	b.Record(record("c"))
	b.Record(record("d"))

	// The page's stream opens with the boot cursor; only the delta replays.
	body := runStreamURL(t, b, "/metrics/live/stream?since="+strconv.FormatInt(bp.Seq, 10)+"&feed="+bp.FeedID, "")
	sp := extractSnapshotPayload(t, body)
	if !sp.Incremental || len(sp.Records) != 2 || sp.Records[0].ID != "c" || sp.Records[1].ID != "d" {
		t.Errorf("post-boot stream: inc=%v n=%d ids=%v, want incremental 2 (c,d)",
			sp.Incremental, len(sp.Records), ids(sp.Records))
	}
	if sp.FeedID != bp.FeedID {
		t.Errorf("stream feed %q != boot feed %q", sp.FeedID, bp.FeedID)
	}
}
