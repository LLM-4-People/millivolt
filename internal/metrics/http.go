package metrics

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/LLM-4-People/millivolt/internal/adminjson"
)

// sseHeartbeat keeps idle SSE connections alive through proxies that drop
// silent connections. Internal cadence, not user-tunable.
const sseHeartbeat = 30 * time.Second

// sseWriteProgressDeadline bounds one dashboard SSE write. A tab that stops
// reading must fail its write and release the handler instead of pinning it
// until process shutdown; every event re-arms the bound.
const sseWriteProgressDeadline = time.Minute

func armSSEWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(sseWriteProgressDeadline))
}

// metricsAllow is RFC 9110 Allow for ring-buffer JSON/SSE/Prometheus.
const metricsAllow = "GET"

// RejectUnlessGet gates a GET-only JSON/SSE/Prometheus endpoint (these
// handlers produce no HEAD bodies; do not reuse a GET/HEAD gate for them).
// A wrong method gets the Allow header and a 405. Shared with the dashboard
// aggregate endpoints (internal/web), which serve the same response shape.
func RejectUnlessGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet {
		return true
	}
	w.Header().Set("Allow", metricsAllow)
	adminjson.WriteError(w, http.StatusMethodNotAllowed, "GET only")
	return false
}

// SetStreamHeaders writes the SSE response header triple shared by every
// server-sent stream the process serves: the dashboard live feed here and
// the cursor bridge's OpenAI SSE (internal/proxy). One owner, so a header
// can never drift between the surfaces. It lives here rather than
// internal/sse because that package already imports this one (the stream
// analyzer); importing it back would be a cycle.
func SetStreamHeaders(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	// The operator gate stages no-store beside the session cookie it mints for
	// a Bearer-authenticated dashboard stream. Preserve that stricter directive
	// instead of downgrading the authenticated response to no-cache; plain and
	// cursor streams keep the SSE default.
	if !hasNoStoreCacheControl(h.Values("Cache-Control")) {
		h.Set("Cache-Control", "no-cache")
	}
	h.Set("Connection", "keep-alive")
}

// hasNoStoreCacheControl reports whether any Cache-Control directive list
// already carries the no-store directive (case-insensitive).
func hasNoStoreCacheControl(values []string) bool {
	for _, value := range values {
		for _, directive := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(directive), "no-store") {
				return true
			}
		}
	}
	return false
}

// cursorParam resolves the resume cursor for a live-feed request - the ONE
// gate both /metrics/bootstrap and the SSE stream share. q is the caller's
// one strict-parsed query (adminjson.StrictQuery; the feed and since
// duplicates are already rejected before this gate runs). The `?feed=` pin is
// checked FIRST (deny by default: a foreign feed is a full snapshot -
// sequence numbers are meaningless across restarts, so a stale-but-in-range
// cursor must never be trusted), then the `Last-Event-ID` header (the
// browser echoes the newest consumed id on reconnect) wins over `?since=`
// (the boot-time snapshot position).
func cursorParam(q url.Values, header http.Header, feed string) int64 {
	if f := q.Get("feed"); f != "" && f != feed {
		return 0
	}
	if v := header.Get("Last-Event-ID"); v != "" {
		since, _ := strconv.ParseInt(v, 10, 64) // malformed → 0 = full snapshot
		return since
	}
	if v := q.Get("since"); v != "" {
		since, _ := strconv.ParseInt(v, 10, 64) // malformed → 0 = full snapshot
		return since
	}
	return 0
}

// SnapshotRequest validates the feed pin and reads the snapshot under one
// lock. A purge between those steps must never label a partial old-epoch delta
// with a new feed ID. Bootstrap and SSE share this atomic trust boundary.
// q is the caller's strict-parsed query and header carries the request's
// Last-Event-ID; both HTTP callers parse once, reject the malformed and
// duplicated feed/since spellings before any stream header or payload is
// staged, and pass the trusted values down here.
func (b *Buffer) SnapshotRequest(q url.Values, header http.Header) Snapshot {
	b.mu.RLock()
	snap := b.snapshotSinceLocked(cursorParam(q, header, b.feedID))
	b.mu.RUnlock()
	sortPendingRecords(snap.InFlightRecords)
	return snap
}

// appendSSEFrame appends one event frame to dst. The snapshot writer uses it
// on an empty destination; the live loop batches many frames into one write so
// a burst costs one syscall, one flush, and one deadline arm instead of one
// per event.
func appendSSEFrame(dst []byte, event string, id int64, hasID bool, data []byte) []byte {
	if hasID {
		dst = append(dst, "id: "...)
		dst = strconv.AppendInt(dst, id, 10)
		dst = append(dst, '\n')
	}
	if event != "" {
		dst = append(dst, "event: "...)
		dst = append(dst, event...)
		dst = append(dst, '\n')
	}
	dst = append(dst, "data: "...)
	dst = append(dst, data...)
	return append(dst, '\n', '\n')
}

// writeSSEFrame streams one event as one write. The JSON payload is already
// allocated, so concatenating prefix+payload+suffix avoids the per-part writes
// (and their deadline re-arms) for the snapshot and the tests.
func writeSSEFrame(w io.Writer, event string, id int64, hasID bool, data []byte) error {
	if rw, ok := w.(http.ResponseWriter); ok {
		armSSEWriteDeadline(rw)
	}
	_, err := w.Write(appendSSEFrame(nil, event, id, hasID, data))
	return err
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
	if !RejectUnlessGet(w, r) {
		return
	}
	// The strict parse runs before any stream header is staged, so a
	// malformed pair answers a plain 400, never an SSE-framed error - a
	// dropped feed pin would otherwise leave a stale cursor trusted on a
	// foreign feed. feed and since are this surface's consumed keys.
	q, err := adminjson.StrictQuery(r)
	if err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, "invalid query")
		return
	}
	if err := adminjson.DuplicateQueryKey(q, "feed", "since"); err != nil {
		adminjson.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	SetStreamHeaders(w.Header())
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

	snapshot := b.SnapshotRequest(q, r.Header)
	feedID := snapshot.FeedID
	snap, err := json.Marshal(ObserveSnapshot(snapshot, b.observerModels(true, snapshot.Records, snapshot.InFlightRecords)))
	if err != nil {
		log.Printf("metrics: encode snapshot: %v", err)
		adminjson.WriteError(w, http.StatusInternalServerError, "serialize")
		return
	}
	// The snapshot's record slices are not needed after this marshal. Clearing
	// them releases every pointer they pin for the life of the connection.
	snapshot = Snapshot{}
	if err := writeSSEFrame(w, "snapshot", 0, false, snap); err != nil {
		return
	}
	flusher.Flush()

	ctx := r.Context()
	// A purge sends an immediate invalidation before closing, avoiding the
	// browser's reconnect delay. A restart's unchanged epoch simply closes.
	defer func() {
		if feed := b.FeedID(); feed != feedID && ctx.Err() == nil {
			data, _ := json.Marshal(map[string]string{"feed_id": feed})
			if _, err := w.Write([]byte("event: reset\ndata: " + string(data) + "\n\n")); err == nil {
				flusher.Flush()
			}
		}
	}()
	ticker := time.NewTicker(sseHeartbeat)
	defer ticker.Stop()

	const sseBatchBytes = 32 << 10
	batch := make([]byte, 0, sseBatchBytes)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		armSSEWriteDeadline(w)
		if _, err := w.Write(batch); err != nil {
			return err
		}
		flusher.Flush()
		batch = batch[:0]
		return nil
	}
	appendEvent := func(event string, id int64, hasID bool, data []byte) error {
		if len(batch) > 0 && len(batch)+len(data)+64 > sseBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = appendSSEFrame(batch, event, id, hasID, data)
		if len(batch) >= sseBatchBytes {
			return flush()
		}
		return nil
	}
	for {
		select {
		case <-feedDone:
			return
		default:
		}
		select {
		case ev, ok := <-sub:
			if !ok {
				_ = flush()
				return // overflow: reconnect before accepting any later cursor
			}
			if ev.feedID != feedID {
				continue // a pre-purge publication delayed until after subscribe
			}
			// A *Record marshals cleanly (scalar fields). If a custom observer
			// ever returns an unmarshalable value, close the stream rather than
			// skipping this seq: the next id would advance Last-Event-ID past
			// a record that can no longer be replayed. Closing forces a resync.
			// Already-encoded record frames are shared across subscribers of the
			// same event when one cache is attached.
			data, err := b.encodeRingEvent(ev)
			if err != nil {
				log.Printf("metrics: encode record event: %v", err)
				return
			}
			if err := appendEvent("record", ev.Seq, true, data); err != nil {
				return
			}
		case liveEv, ok := <-live:
			if !ok {
				_ = flush()
				return // overflow: the next snapshot restores pending state
			}
			if liveEv.feedID != feedID {
				continue
			}
			// Lifecycle event (begin/end). A marshal failure is a stream
			// contract failure, not a skippable event; resync instead. The one
			// encode owner shares already-encoded frames across subscribers of
			// this event.
			data, err := b.encodeLiveEvent(liveEv)
			if err != nil {
				log.Printf("metrics: encode live event: %v", err)
				return
			}
			if err := appendEvent(liveEv.Phase, 0, false, data); err != nil {
				return
			}
		case <-ticker.C:
			if err := flush(); err != nil {
				return
			}
			armSSEWriteDeadline(w)
			if _, err := w.Write([]byte(": heartbeat\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case <-feedDone:
			_ = flush()
			return
		case <-ctx.Done():
			_ = flush()
			return
		}
		// Drain everything already queued into the same write. Bounded by the
		// batch cap, so a burst cannot starve shutdown or unboundedly delay a
		// flush.
	drain:
		for {
			select {
			case ev, ok := <-sub:
				if !ok {
					_ = flush()
					return
				}
				if ev.feedID != feedID {
					continue
				}
				data, err := b.encodeRingEvent(ev)
				if err != nil {
					log.Printf("metrics: encode record event: %v", err)
					return
				}
				if err := appendEvent("record", ev.Seq, true, data); err != nil {
					return
				}
			case liveEv, ok := <-live:
				if !ok {
					_ = flush()
					return
				}
				if liveEv.feedID != feedID {
					continue
				}
				data, err := b.encodeLiveEvent(liveEv)
				if err != nil {
					log.Printf("metrics: encode live event: %v", err)
					return
				}
				if err := appendEvent(liveEv.Phase, 0, false, data); err != nil {
					return
				}
			case <-feedDone:
				_ = flush()
				return
			case <-ctx.Done():
				_ = flush()
				return
			default:
				break drain
			}
		}
		if err := flush(); err != nil {
			return
		}
	}
}
