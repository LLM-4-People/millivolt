package metrics

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// sseHeartbeat keeps idle SSE connections alive through proxies that drop
// silent connections. Internal cadence, not user-tunable.
const sseHeartbeat = 30 * time.Second

// metricsAllow is RFC 9110 Allow for ring-buffer JSON/SSE/Prometheus.
const metricsAllow = "GET"

func rejectUnlessGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet {
		return true
	}
	w.Header().Set("Allow", metricsAllow)
	http.Error(w, `{"error":"GET only"}`, http.StatusMethodNotAllowed)
	return false
}

// cursorParam resolves the resume cursor for a live-feed request - the ONE
// gate both /metrics/bootstrap and the SSE stream share. The `?feed=` pin is
// checked FIRST (deny by default: a foreign feed is a full snapshot -
// sequence numbers are meaningless across restarts, so a stale-but-in-range
// cursor must never be trusted), then the `Last-Event-ID` header (the
// browser echoes the newest consumed id on reconnect) wins over `?since=`
// (the boot-time snapshot position).
func cursorParam(r *http.Request, feed string) int64 {
	if f := r.URL.Query().Get("feed"); f != "" && f != feed {
		return 0
	}
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		since, _ := strconv.ParseInt(v, 10, 64) // malformed → 0 = full snapshot
		return since
	}
	if v := r.URL.Query().Get("since"); v != "" {
		since, _ := strconv.ParseInt(v, 10, 64) // malformed → 0 = full snapshot
		return since
	}
	return 0
}

// SnapshotRequest validates the feed pin and reads the snapshot under one
// lock. A purge between those steps must never label a partial old-epoch delta
// with a new feed ID. Bootstrap and SSE share this atomic trust boundary.
func (b *Buffer) SnapshotRequest(r *http.Request) Snapshot {
	b.mu.RLock()
	snap := b.snapshotSinceLocked(cursorParam(r, b.feedID))
	b.mu.RUnlock()
	sortPendingRecords(snap.InFlightRecords)
	return snap
}

// HandleStream serves live updates over SSE at /metrics/live/stream. It pushes
// each new record as it arrives, plus a heartbeat every 30s - the dashboard's
// instant-refresh channel (event-driven) beside the tick-resumed bootstrap.
//
// Resume protocol (standard EventSource replay): every `record` event carries
// an `id:` = its feed sequence number. On (re)connect the browser sends the
// last received id in the `Last-Event-ID` header; the initial `snapshot`
// event then carries ONLY the records appended after that id - a reconnect
// never re-sends the whole ring. A stale id (ring eviction, proxy restart, or
// reset) degrades to a full snapshot, so a client can never silently miss
// records. A fresh page that already holds a snapshot (the dashboard boot's
// /metrics/bootstrap payload) passes the same cursor as `?since=` (plus its
// `?feed=`), so its stream also opens with a small delta instead of a full
// ring replay; the header wins over the query param when both are present -
// the browser echoes the newest consumed id there on reconnect.
// Subscribe-before-snapshot closes the gap race: a record appended
// between the two is delivered both in the snapshot and freshly from the
// channel - the client upserts by record id, so the duplicate is harmless.
func (b *Buffer) HandleStream(w http.ResponseWriter, r *http.Request) {
	if !rejectUnlessGet(w, r) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// The dashboard is same-origin. Do not grant unrelated browser origins
	// read access to private metrics; non-browser callers still need network
	// access controls because this endpoint does not provide authentication.

	// Capture invalidation BEFORE subscribing/snapshotting: a purge or restart
	// during the initial write must not strand this stream on a newer channel.
	feedDone := b.feedChan()
	// Subscribe FIRST so nothing that arrives while the snapshot is being
	// computed can fall into the gap (duplicates are upsert-idempotent).
	sub := b.Subscribe()
	defer b.Unsubscribe(sub)
	live := b.SubscribeLive()
	defer b.UnsubscribeLive(live)

	snapshot := b.SnapshotRequest(r)
	snap, err := json.Marshal(ObserveSnapshot(snapshot, b.observerModels(true, snapshot.Records, snapshot.InFlightRecords)))
	if err != nil {
		http.Error(w, `{"error":"serialize"}`, http.StatusInternalServerError)
		return
	}
	if _, err := w.Write([]byte("event: snapshot\ndata: " + string(snap) + "\n\n")); err != nil {
		return
	}
	flusher.Flush()

	ctx := r.Context()
	// A purge sends an immediate invalidation before closing, avoiding the
	// browser's reconnect delay. A restart's unchanged epoch simply closes.
	defer func() {
		if feed := b.FeedID(); feed != snapshot.FeedID && ctx.Err() == nil {
			data, _ := json.Marshal(map[string]string{"feed_id": feed})
			if _, err := w.Write([]byte("event: reset\ndata: " + string(data) + "\n\n")); err == nil {
				flusher.Flush()
			}
		}
	}()
	ticker := time.NewTicker(sseHeartbeat)
	defer ticker.Stop()

	for {
		select {
		case <-feedDone:
			return
		default:
		}
		select {
		case ev, ok := <-sub:
			if !ok {
				return // overflow: reconnect before accepting any later cursor
			}
			if ev.feedID != snapshot.FeedID {
				continue // a pre-purge publication delayed until after subscribe
			}
			// A *Record marshals cleanly (scalar fields); on the unreachable
			// failure, skip this event rather than kill the stream. The id:
			// field is the feed cursor - the browser echos it back as
			// Last-Event-ID on reconnect.
			data, err := json.Marshal(b.observeRecord(ev.Record))
			if err != nil {
				continue
			}
			if _, err := w.Write([]byte("id: " + strconv.FormatInt(ev.Seq, 10) + "\nevent: record\ndata: " + string(data) + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case liveEv, ok := <-live:
			if !ok {
				return // overflow: the next snapshot restores pending state
			}
			if liveEv.feedID != snapshot.FeedID {
				continue
			}
			// Lifecycle event (begin/end). The Record marshals cleanly.
			data, err := json.Marshal(b.observeLive(liveEv))
			if err != nil {
				continue
			}
			if _, err := w.Write([]byte("event: " + liveEv.Phase + "\ndata: " + string(data) + "\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-feedDone:
			return
		case <-ctx.Done():
			return
		}
	}
}
