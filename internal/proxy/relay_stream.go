package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/format/ir"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/sse"
)

// streamBody forwards an SSE body, preserving bytes exactly, and flushes after
// each event boundary so TTFT == upstream TTFT. Alongside the copy it scans
// for token timestamps and the final usage chunk using an SSE analyzer.
//
// The relay withholds the stream's terminal region - everything from the
// first event carrying a non-null finish_reason or a [DONE] - until the
// stream ends, then releases it verbatim. When the analyzer classifies the
// finished stream as degenerate (metrics.ClassifyOutcome: truncated /
// tool_calls-without-calls / empty stop / reasoning-only stop), the held
// region is replaced with an in-band OpenAI error chunk the client treats as
// a retryable failure. The withholding is what makes that work: SDKs treat
// the finish chunk as the stream's end, so an error written after it would be
// ignored. The hold is bounded by terminalHoldMax (past it the stream
// commits to verbatim relay - no degenerate stream has a large "terminal"
// region), and every non-held byte is written through immediately, so the hot
// path only ever reserves one event's worth of bytes.
//
// Transparent re-sends: while a rescue budget remains, a truncation that
// relayed nothing content-bearing (allowTruncationRetry, the quality
// budget) or a mid-thinking death that relayed only reasoning
// (allowThinkingRescue, the thinking budget) returns the matching
// rescueReasoning* class instead of surfacing an error: the caller owns the
// re-send and the attempt bookkeeping. A reasoning-only clean stop is equally
// rescuable (rescueReasoningStop) because its terminal region never left the
// hold. A provider error event streamed in-band on the 200 is the same
// contract from the other side: while nothing content-bearing was relayed
// the frame is still unwritten, so a retryable server-availability class
// (retryableUpstreamErrorClass, the OpenAI retry semantics) is dropped and
// rescueUpstreamError re-sends - the multi-line flush discovery, a frame
// after opened content, a non-retryable class, or a spent budget falls back
// to verbatim relay exactly as before. The returned rescue is only ever
// reported in those no-contract-bytes states - never after answer content,
// tool calls, a refusal, an in-band error that reached the wire, or a read
// error caused by our own context (client gone or the per-send deadline).
// With the budget spent (rescueNone), the truncation or degenerate class
// surfaces in-band exactly as before: finish_reason length is a terminator
// and never rescuable. uerr receives the provider's envelope for a
// rescueUpstreamError return (the caller's attempt bookkeeping); it is
// untouched for every other outcome.
func (s *Server) streamBody(ctx context.Context, w http.ResponseWriter, body io.Reader, rec *metrics.Record, allowTruncationRetry, allowThinkingRescue bool, uerr *upstreamErrorRescue, wire ir.Protocol) (rescue streamRescue) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		if _, err := io.Copy(disconnectWriter{w, rec}, body); err != nil {
			markStreamErr(ctx, rec, err)
		}
		return rescueNone
	}
	var (
		lineBuf bytes.Buffer // partial line + current chunk bytes not yet consumed
		out     bytes.Buffer // released bytes awaiting one client write
		hold    []byte       // bytes withheld from the terminal-candidate event onward
		holding bool
		// wireDataOpen records that the last bytes written to the client end
		// with a data line and no blank separator. It survives writeOut's
		// buffer reset, so a hold opened after an earlier flush still knows to
		// start a new event.
		wireDataOpen bool
		// eventName and eventBody buffer one Responses terminal-named event
		// until its blank-line boundary. The name alone cannot decide hold or
		// release: a multi-line data payload may only become a terminal
		// document once reassembled, and a name that contradicts that document
		// must be dropped together with it, never relayed ahead of a synthetic
		// failure. Non-terminal events keep the ordinary line path.
		eventName []byte
		eventBody []byte
		// pendingName is the terminal event name for an event that could not
		// be buffered as a unit: a late name after the event's data lines, or
		// a buffered event released verbatim at the hold bound. It classifies
		// the reassembled document at the real blank-line boundary.
		pendingName string
		// eventHasData records that the current SSE event already produced a
		// data: line, so a terminal name arriving for it belongs to the same
		// event and must keep its original position.
		eventHasData bool
		// deferredFinish keeps a terminal document's hold until its SSE
		// boundary: the event may still name itself after its data line, and
		// resolving at the data line would drop that late name.
		deferredFinish bool
		buf            [4096]byte
		// holdAborted marks that the hold was abandoned (its bytes flushed on
		// the terminalHoldMax overflow): the stream then COMMITS to verbatim
		// relay - no degenerate classification may substitute anything (a
		// stream with a >64KiB terminal region is not one of the degenerate
		// classes, and the client already saw the finish-reason bytes).
		holdAborted bool
		// rescueRequested carries the rescue decision out of finishHold
		// (which returns only client-gone).
		rescueRequested streamRescue
	)
	a := s.analyzerFor(rec)
	a.SetWire(wire)
	// boundaryDrops reports whether finishHold will replace this attempt's
	// terminal region with the in-band degenerate error instead of writing the
	// held bytes. The tail-drop decision and finishHold share this predicate so
	// delivery and record accounting cannot drift.
	boundaryDrops := func(code string) bool {
		return code != "" && !a.HasInBandError() && !rec.ClientDisconnected &&
			a.ChatLike() && !holdAborted
	}
	// appendHeld frames one complete line into the withheld region. A second
	// data line in the same event would concatenate onto the first payload
	// (the SSE client joins data lines with a newline), so start a new event
	// before it. A field line stays with the data line it introduces.
	appendHeld := func(line []byte) {
		if _, isData := sse.DataPayload(line); isData && endsWithUnframedDataLine(hold) {
			hold = append(hold, '\n')
		}
		hold = append(hold, line...)
		hold = append(hold, '\n')
	}
	// lastLineIsData reports whether the next data line would join an open
	// event's existing data line, whether that line is still buffered in out
	// or was already flushed to the client.
	lastLineIsData := func() bool {
		if out.Len() > 0 {
			return endsWithUnframedDataLine(out.Bytes())
		}
		return wireDataOpen
	}
	// clientGone records a write-detected abort: the disconnect stamps
	// the 499, and the analyzer's in-band error - when one reached the
	// wire - still fills the record, so the abort counts like its 200
	// twin exactly as the read-detected paths do. Every caller returns
	// immediately, so the fill runs at most once; a stream without an
	// in-band error keeps the plain disconnect accounting.
	clientGone := func() {
		markClientGone(rec)
		if a.HasInBandError() {
			fillRecord(&a, rec)
		}
	}
	writeOut := func() bool {
		if out.Len() == 0 {
			return true
		}
		o := out.Bytes()
		if _, werr := writeCounted(w, rec, o); werr != nil {
			clientGone()
			return false
		}
		wireDataOpen = endsWithUnframedDataLine(o)
		if bytes.IndexByte(o, '\n') >= 0 {
			flusher.Flush()
		}
		out.Reset()
		return true
	}
	// writeRescueBoundary closes the client's pending SSE event before a rescue
	// return or the degenerate terminal error. Complete lines already in out are
	// written; the trailing partial line was never received by the client and is
	// dropped. One newline ends the open event (and is a harmless blank
	// separator between complete events), so the next data line cannot join an
	// existing payload. The withheld terminal region is discarded with the
	// attempt or replaced by the degenerate error.
	writeRescueBoundary := func() bool {
		o := out.Bytes()
		if n := bytes.LastIndexByte(o, '\n'); n >= 0 {
			o = o[:n+1]
		} else {
			o = nil
		}
		if len(o) > 0 {
			if _, werr := writeCounted(w, rec, o); werr != nil {
				clientGone()
				return false
			}
		}
		out.Reset()
		hold = hold[:0]
		holding = false
		if _, werr := writeCounted(w, rec, []byte("\n")); werr != nil {
			clientGone()
			return false
		}
		wireDataOpen = false
		flusher.Flush()
		return true
	}
	// finishHold resolves the withheld terminal region: classify the stream and
	// either release the held bytes verbatim or replace them with the in-band
	// degenerate error - never stacked on top of a provider-sent in-band error,
	// and never written to a client already gone. Returns false when the client
	// disappeared. Called the moment the outcome is decided: at a [DONE] /
	// Responses-terminal line (everything that can arrive has arrived - no need
	// to wait for EOF / a stalled upstream to close the connection), and at
	// clean EOF for streams that end without those markers. A rescuable
	// reasoning-only stop with budget remaining instead records
	// rescueRequested: the held region never reached the client, so the caller
	// re-sends and the fresh attempt's own terminal region takes its place.
	finishHold := func() bool {
		if ctx.Err() == context.Canceled {
			// The request context only cancels when the LOCAL client is gone
			// (markStreamErr's classification) - and this resolution is
			// reachable with the client already gone: the upstream FIN can
			// beat the transport's own cancellation, handing the read loop a
			// CLEAN EOF for the abort. With out and hold drained no write
			// remains to observe the dead socket, and the only wire contact
			// left here is flusher.Flush, whose error net/http discards - so
			// the disconnect is stamped here (the write-detected abort's
			// exact accounting, in-band error kept) instead of finalizing the
			// abort away as a clean 200.
			clientGone()
			return false
		}
		code := a.OutcomeCode()
		// The stop rescue requires the terminal region still WITHHELD: once a
		// finishHold call released it the outcome was already signed on the
		// wire, and no re-send may append behind it. RetryableReasoningStop is
		// the analyzer's wire-safety gate (no refusal/function_call bytes, not
		// a Responses-shaped feed). Close any pending event before returning;
		// the held terminal bytes are discarded instead of written.
		if code != "" && !a.HasInBandError() && !rec.ClientDisconnected && a.ChatLike() &&
			holding && code == metrics.CodeReasoningOnly && allowThinkingRescue && a.RetryableReasoningStop() {
			if !writeRescueBoundary() {
				return false
			}
			rescueRequested = rescueReasoningStop
			return true
		}
		// A degenerate error is appended as a new SSE event. Close the
		// pending event first: a complete data line in out still needs its
		// blank separator, and a trailing partial line was never received and
		// must be dropped. Without this boundary the error becomes a second
		// data line in the open payload, or concatenates on the same line.
		degenerate := boundaryDrops(code)
		if degenerate {
			if !writeRescueBoundary() {
				return false
			}
		} else if !writeOut() {
			return false
		}
		if holdAborted {
			// Commit-to-verbatim mode: the overflow already released the
			// terminal bytes - nothing may be substituted, ever. Bytes held
			// after the overflow (a later marker opened a fresh hold) are
			// still part of the verbatim stream.
			if len(hold) > 0 {
				if _, werr := writeCounted(w, rec, hold); werr != nil {
					clientGone()
					return false
				}
				wireDataOpen = endsWithUnframedDataLine(hold)
				hold = hold[:0]
			}
			holding = false
			flusher.Flush()
			return true
		}
		if code != "" && !a.HasInBandError() && !rec.ClientDisconnected {
			if !a.ChatLike() {
				// A foreign wire such as Anthropic Messages has its own
				// terminal contract but no OpenAI degenerate substitution:
				// record the truncation without writing OpenAI frames into it.
				rec.ErrorType = sse.TypeUpstreamError
				rec.ErrorCode = code
				rec.ErrorMsg = metrics.DegenerateMessage(code)
			} else {
				if a.ResponsesFeed() {
					emitDegenerateResponses(w, &a, time.Now(), code, rec)
				} else {
					emitDegenerateSSE(w, &a, time.Now(), code, rec)
				}
			}
		} else if len(hold) > 0 {
			// A released terminal region is a complete SSE event: close the
			// final line and add the blank boundary a spec parser needs at
			// EOF. A held [DONE] or full terminal document already ends with
			// a blank separator and is unchanged.
			if !bytes.HasSuffix(hold, []byte("\n\n")) {
				if !bytes.HasSuffix(hold, []byte("\n")) {
					hold = append(hold, '\n')
				}
				hold = append(hold, '\n')
			}
			if _, werr := writeCounted(w, rec, hold); werr != nil {
				clientGone()
				return false
			}
			wireDataOpen = endsWithUnframedDataLine(hold)
		}
		hold = hold[:0]
		holding = false
		// A healthy turn whose last complete data line never got its blank
		// separator must still close the client's SSE event at EOF; otherwise
		// a spec parser discards the final event. Degenerate paths already
		// wrote their own boundary above.
		if code == "" && wireDataOpen {
			if _, werr := writeCounted(w, rec, []byte("\n")); werr != nil {
				clientGone()
				return false
			}
			wireDataOpen = false
		}
		flusher.Flush()
		return true
	}
	// processEventBuffer resolves one complete terminal-named event. The SSE
	// name is kept only when it agrees with the document (or the document has
	// no type and the name is its only classifier); a contradicting name is
	// dropped so the client never receives an incomplete label over completed
	// data. A terminal document is held and resolved atomically with its
	// complete bytes. Returns true when the caller must leave streamBody;
	// rescueRequested then carries the re-send decision.
	processEventBuffer := func(now time.Time) bool {
		name := string(sse.EventName(eventName))
		data := sse.EventData(eventBody)
		typ, keepName := sse.ResolveResponsesTerminal(name, data)
		if typ != "" {
			a.FeedEvent(typ, data, now)
			event := make([]byte, 0, len(eventName)+len(eventBody))
			if keepName {
				event = append(event, eventName...)
			}
			event = append(event, eventBody...)
			holding = true
			hold = append(hold, event...)
			eventName = eventName[:0]
			eventBody = eventBody[:0]
			if !writeOut() {
				return true
			}
			if !finishHold() {
				return true
			}
			if rescueRequested != rescueNone {
				return true
			}
			fillRecord(&a, rec)
			return true
		}
		// The name announced a terminal event but the document is not one:
		// drop the name and relay the data as ordinary bytes. The analyzer
		// still sees the complete event exactly once.
		if len(data) > 0 {
			a.FeedEvent("", data, now)
		}
		out.Write(eventBody)
		eventName = eventName[:0]
		eventBody = eventBody[:0]
		return false
	}
	// abandonEventBuffer releases a buffered terminal-named event verbatim
	// when it outgrows the hold bound. Nothing may be substituted after that
	// commit, exactly like a hold overflow. The buffered prefix stays one
	// event for the analyzer: its reassembled data is preserved for the real
	// blank-line boundary, and the event name is remembered so a type-less
	// document can still be classified there. No artificial blank line is
	// fed at the overflow point - that would split the document mid-event.
	//
	// Known malformed-input residual (F5-7): after this commit, later bytes
	// of the same malformed event continue through the ordinary verbatim
	// relay. A delta, [DONE] or response.failed that follows an over-bound
	// terminal is therefore relayed after the completion, while the record
	// keeps the finish the terminal already signed. The normal path stops
	// reading post-terminal debris, but here the terminal was never held as
	// a unit and the relay has committed to byte preservation. Stopping or
	// withholding the tail would require rejecting bytes the client's SSE
	// parser may still accept, so this residual is documented rather than
	// changed.
	abandonEventBuffer := func(now time.Time) {
		raw := make([]byte, 0, len(eventName)+len(eventBody))
		raw = append(raw, eventName...)
		raw = append(raw, eventBody...)
		out.Write(raw)
		pendingName = string(sse.EventName(eventName))
		eventHasData = true
		a.FeedBufferedEvent(raw, now)
		eventName = eventName[:0]
		eventBody = eventBody[:0]
		holdAborted = true
	}
	for {
		n, err := body.Read(buf[:])
		if n > 0 {
			now := time.Now()
			lineBuf.Write(buf[:n])
			for {
				b := lineBuf.Bytes()
				idx := bytes.IndexByte(b, '\n')
				if idx < 0 {
					break
				}
				line := b[:idx]
				name := sse.EventName(line)
				terminalName := name != "" && sse.ResponsesTerminalEventName(name)
				// A terminal name that follows this event's data lines belongs
				// to the same event. Relay it in its original position and
				// classify the reassembled document at the blank boundary.
				if terminalName && len(eventName) == 0 && (holding || eventHasData) {
					pendingName = name
				}
				// Buffer a terminal-named event whole when its name opens the
				// event. Its document may only become parseable after several
				// data: lines, and its name must be kept or dropped together
				// with the document.
				if terminalName && len(eventName) == 0 && !holding && !eventHasData {
					eventName = append(eventName[:0], line...)
					eventName = append(eventName, '\n')
					lineBuf.Next(idx + 1)
					continue
				}
				if len(eventName) > 0 {
					if len(eventName)+len(eventBody)+len(line)+2 > terminalHoldMax {
						// Past the bound, commit to verbatim relay: feed and
						// release every buffered line, and never substitute.
						eventBody = append(eventBody, line...)
						eventBody = append(eventBody, '\n')
						abandonEventBuffer(now)
						lineBuf.Next(idx + 1)
						continue
					}
					eventBody = append(eventBody, line...)
					eventBody = append(eventBody, '\n')
					lineBuf.Next(idx + 1)
					if len(bytes.TrimSpace(line)) != 0 {
						continue
					}
					if processEventBuffer(now) {
						return rescueRequested
					}
					eventHasData = false
					if !writeOut() {
						return rescueNone
					}
					continue
				}
				blank := len(bytes.TrimSpace(line)) == 0
				prev := a.Terminated()
				// A [DONE] on a Responses feed is not its terminal marker, but
				// the line must still be withheld: a zero-event Responses body
				// replaces it with one response.failed at the end. Other wires
				// relay it as the foreign line it is.
				heldDone := sse.DoneLine(line) && a.ResponsesFeed()
				if blank {
					eventHasData = false
					if pendingName != "" {
						name := pendingName
						pendingName = ""
						a.FlushEventWithName(name, now)
					} else {
						a.Feed(line, now)
					}
					if !prev && a.Terminated() && !holding {
						// The boundary completed a terminal document that was
						// never buffered as a unit (multi-line data or a late
						// event name). Relay the boundary, resolve the record,
						// and stop reading post-terminal debris.
						out.Write(line)
						out.WriteByte('\n')
						if !writeOut() {
							return rescueNone
						}
						fillRecord(&a, rec)
						return rescueNone
					}
				} else {
					if _, isData := sse.DataPayload(line); isData {
						eventHasData = true
					}
					a.Feed(line, now)
				}
				switch {
				case holding:
					appendHeld(line)
					if sse.TerminalLine(line) && (a.DoneTerminates() || !sse.DoneLine(line)) {
						deferredFinish = true
					}
					if sse.DoneLine(line) && a.DoneTerminates() {
						// [DONE] ends the stream at the line itself; emit the
						// blank line its SSE block would have carried.
						hold = append(hold, '\n')
						if !finishHold() {
							return rescueNone
						}
						if rescueRequested != rescueNone {
							return rescueRequested
						}
						fillRecord(&a, rec)
						return rescueNone
					}
					// A Responses/Anthropic terminal document stays held until
					// its blank-line boundary so a late event name can follow
					// in its original position. A standalone error event is a
					// terminal statement of its own and must not wait for EOF:
					// a provider may keep the connection open after it. This
					// release is scoped to an observed in-band error, so a
					// Chat finish-reason hold still waits for usage/[DONE].
					if blank && (deferredFinish || (!prev && a.Terminated()) || a.HasInBandError()) {
						if !finishHold() {
							return rescueNone
						}
						if rescueRequested != rescueNone {
							return rescueRequested
						}
						fillRecord(&a, rec)
						return rescueNone
					}
				case !prev && (a.Terminated() || heldDone) && !blank:
					// This line opened the terminal region: withhold it (and
					// everything after) until the outcome is decided. A data
					// line already flushed in out is its own event; separate
					// it before the held event starts.
					holding = true
					if lastLineIsData() {
						hold = append(hold, '\n')
					}
					appendHeld(line)
					if sse.DoneLine(line) && a.DoneTerminates() {
						hold = append(hold, '\n')
					}
					if sse.TerminalLine(line) && (a.DoneTerminates() || !sse.DoneLine(line)) {
						deferredFinish = true
					}
					if !writeOut() {
						return rescueNone
					}
					if sse.DoneLine(line) && a.DoneTerminates() {
						if !finishHold() {
							return rescueNone
						}
						if rescueRequested != rescueNone {
							return rescueRequested
						}
						fillRecord(&a, rec)
						return rescueNone
					}
				default:
					// A provider error event that just parsed, still unwritten:
					// a retryable server-availability class with budget left is
					// dropped here (the client never sees this frame) and the
					// caller transparently re-sends - the OpenAI retry semantics
					// the provider asked for ("please retry"), applied at the
					// only wire state where re-sending cannot duplicate anything.
					// Anything else falls through to verbatim relay.
					if typ, code, msg, reasoning := a.RetryableUpstreamError(); typ != "" &&
						s.retryableUpstreamErrorClass(typ, code) && !rec.ClientDisconnected &&
						ctx.Err() == nil &&
						((reasoning && allowThinkingRescue) || (!reasoning && allowTruncationRetry)) {
						// Flush the complete lines the loop already fed to the
						// analyzer (role/keepalive/reasoning) so the wire matches
						// the record before the fresh attempt appends behind them.
						if !writeOut() {
							return rescueNone
						}
						// Close the client's pending SSE event: the dropped
						// frame was its first data line, so field lines of the
						// same event (an Anthropic-style `event: error`) may have
						// been relayed and must not type the fresh attempt's
						// first chunk. A blank line ends that event with no
						// data - spec parsers discard it - and is a no-op
						// between complete events.
						if !writeRescueBoundary() {
							return rescueNone
						}
						uerr.typ, uerr.code, uerr.msg, uerr.reasoning = typ, code, msg, reasoning
						return rescueUpstreamError
					}
					out.Write(line)
					out.WriteByte('\n')
				}
				lineBuf.Next(idx + 1)
			}
			if holding {
				// Retain a partial line for the next read: the hold owns only
				// complete lines already seen by the analyzer. TCP/read chunk
				// boundaries must never discard a terminal usage event.
				if len(hold)+lineBuf.Len() > terminalHoldMax {
					hold = append(hold, lineBuf.Bytes()...)
					lineBuf.Reset()
					if _, werr := writeCounted(w, rec, hold); werr != nil {
						clientGone()
						return rescueNone
					}
					wireDataOpen = endsWithUnframedDataLine(hold)
					flusher.Flush()
					hold = hold[:0]
					holding = false
					holdAborted = true
				}
			} else if len(eventName) > 0 && lineBuf.Len() > sseScannerBufMax {
				// A newline-free giant inside a buffered terminal event:
				// commit its bytes verbatim, exactly like a hold overflow.
				eventBody = append(eventBody, lineBuf.Bytes()...)
				abandonEventBuffer(now)
				lineBuf.Reset()
			} else if lineBuf.Len() > sseScannerBufMax {
				// Safety guardrail: a newline-free (or one giant) stream from a
				// buggy or hostile upstream must not grow lineBuf without bound.
				// A newline-free chunk never enters the line loop above, so the
				// check must live here too; past the cap, feed the accumulated
				// bytes to the analyzer (it only byte-scans, so a fragment is
				// harmless), relay, and reset.
				a.Feed(lineBuf.Bytes(), now)
				out.Write(lineBuf.Bytes())
				lineBuf.Reset()
			}
			if !writeOut() {
				return rescueNone
			}
		}
		if err != nil {
			now := time.Now()
			// A terminal-named event that ended at EOF without its blank-line
			// boundary still has all of its data lines; resolve it now rather
			// than leaving a stale name or dropping an authoritative terminal
			// document behind a synthetic failure.
			if len(eventName) > 0 {
				if lineBuf.Len() > 0 {
					eventBody = append(eventBody, lineBuf.Bytes()...)
					lineBuf.Reset()
				}
				if processEventBuffer(now) {
					return rescueRequested
				}
				if !writeOut() {
					return rescueNone
				}
			}
			// Save any trailing partial line without feeding it to the
			// analyzer yet. The line is never written by a rescue, so its
			// content, tool call and preview state must not veto the pre-content
			// rescues below. An unterminated final line can still carry a
			// terminal marker ([DONE] or a non-null finish_reason); the
			// analyzer's tail classifier owns that grammar. releaseTail feeds
			// and releases it only on the paths that actually deliver it.
			var tail []byte
			if lineBuf.Len() > 0 {
				tail = append(tail, lineBuf.Bytes()...)
				lineBuf.Reset()
			}
			releaseTail := func() {
				if len(tail) == 0 {
					return
				}
				a.Feed(tail, now)
				if holding {
					// Close any pending event, then write the tail as its own
					// event with a blank boundary. Without this framing a
					// finish line and a content tail would form one SSE payload
					// with two data lines, which no spec parser can dispatch at
					// EOF.
					if len(hold) > 0 && !bytes.HasSuffix(hold, []byte("\n\n")) {
						if !bytes.HasSuffix(hold, []byte("\n")) {
							hold = append(hold, '\n')
						}
						hold = append(hold, '\n')
					} else if len(hold) == 0 && lastLineIsData() {
						// The content line was already flushed into its own
						// event. Start the held tail with a separator; a plain
						// if followed by the boundary block would add a third
						// newline.
						hold = append(hold, '\n')
					}
					hold = append(hold, tail...)
					hold = append(hold, '\n', '\n')
				} else {
					if _, isData := sse.DataPayload(tail); isData && lastLineIsData() {
						out.WriteByte('\n')
					}
					out.Write(tail)
				}
				tail = nil
			}
			// An event released at the hold bound may also end at EOF without
			// its blank-line boundary. The buffered prefix and every later
			// data line are already reassembled; apply the remembered event
			// name now so a type-less terminal document still classifies.
			if pendingName != "" {
				name := pendingName
				pendingName = ""
				a.FlushEventWithName(name, now)
			}
			// A reassembled event that ended at EOF without its blank-line
			// boundary may be a complete Responses terminal document (pretty
			// or split across data: lines). Resolve it before classification:
			// the terminal bytes are already on the wire, and without this
			// flush the analyzer reports a truncation and the relay appends a
			// synthetic failure behind a completed turn.
			if !holding {
				a.FlushEventWithName("", now)
			}
			// An unterminated final line can itself carry the terminal marker.
			// Decide before any pre-tail rescue so a complete answer is not
			// re-sent or discarded. The classifier never commits analyzer
			// state; the feed below is the only commit.
			tailTerminates := a.TailTerminates(tail)
			if err != io.EOF {
				// A mid-thinking death by read error (connection reset,
				// unexpected EOF from the upstream) is the same rescuable
				// truncation as clean EOF - but only when only reasoning was
				// relayed, and never when our own context caused the failure:
				// the client is gone (the outer context is canceled), or the
				// per-send deadline fired. The deadline lives on a CHILD
				// context (doWithRetry's withSendTimeout), so the outer ctx
				// stays clean when it kills the read - reject the deadline
				// sentinel itself (and any error wrapping it): a re-send
				// would mint a fresh deadline the client did not ask for.
				if allowThinkingRescue && ctx.Err() == nil && !tailTerminates &&
					!errors.Is(err, context.DeadlineExceeded) &&
					a.RetryableReasoningTruncation() && !rec.ClientDisconnected {
					if !writeRescueBoundary() {
						return rescueNone
					}
					return rescueReasoningTruncation
				}
				// A real read failure is the transport's own error - release
				// the held bytes verbatim (byte preservation) and never
				// substitute a synthetic outcome on top. A terminal tail is
				// still delivered as its own framed event: the non-holding arm
				// would write it without a trailing blank and the client would
				// discard it at EOF.
				markStreamErr(ctx, rec, err)
				if tailTerminates {
					holding = true
				}
				releaseTail()
				if holding {
					if _, werr := writeCounted(w, rec, hold); werr != nil {
						clientGone()
						return rescueNone
					}
					wireDataOpen = endsWithUnframedDataLine(hold)
					hold = hold[:0]
				}
				if !writeOut() {
					return rescueNone
				}
				fillRecord(&a, rec)
				return rescueNone
			}
			// Clean EOF: resolve the hold (truncation is decided here - a
			// stream without any terminal marker). A trailing partial line
			// released into out is written before any substituted chunk by
			// finishHold's writeOut-first ordering.
			//
			// Proxy-side truncation rescue: when a budget remains and the
			// stream died truncated with no completion-contract bytes on the
			// wire, hand the re-send to the caller instead of emitting. The
			// empty class (quality budget) leaves the client only
			// role/keepalive frames (a trailing partial line, if any, is
			// equally empty - content here would have marked the analyzer);
			// the thinking class (thinking budget) may additionally leave
			// reasoning deltas, which are auxiliary display text the fresh
			// stream appends after. The record is filled by the succeeding
			// attempt alone. No truncation can coexist with an open hold:
			// holding implies a seen terminator. A committed verbatim relay
			// (holdAborted) must never be re-sent: the client already saw the
			// oversized bytes.
			if !tailTerminates && allowTruncationRetry && ctx.Err() == nil && !holdAborted && a.RetryableTruncation() && !rec.ClientDisconnected {
				if !writeRescueBoundary() {
					return rescueNone
				}
				return rescueEmptyTruncation
			}
			if !tailTerminates && allowThinkingRescue && ctx.Err() == nil && !holdAborted && a.RetryableReasoningTruncation() && !rec.ClientDisconnected {
				if !writeRescueBoundary() {
					return rescueNone
				}
				return rescueReasoningTruncation
			}
			// No rescue owns this attempt. finishHold either delivers the
			// trailing partial line through its verbatim branch or drops it at
			// the degenerate boundary. Decide that before feeding it: content
			// the boundary drops was never delivered, so it must not set the
			// answer content or first-answer time on the record. The
			// truncation class is skipped only when the analyzer's own tail
			// classifier says the line carries no completion terminal marker:
			// a final Chat [DONE] or non-null finish_reason is fed rather than
			// dropped. A fed tail is delivered only when it supplies a
			// complete answer or tool call; an empty or reasoning-only
			// completion resolves through the degenerate boundary, which
			// withholds or replaces the terminal bytes. The other degenerate
			// classes also run finishHold after the feed because the tail can
			// supply the missing output.
			if tailTerminates {
				// Feed the terminal tail into the withheld region and let
				// finishHold decide healthy, degenerate or reasoning-stop. It
				// is released as its own framed SSE event.
				holding = true
				releaseTail()
			} else {
				preTailCode := a.OutcomeCode()
				tailDropped := preTailCode == metrics.CodeTruncated && boundaryDrops(preTailCode)
				if !tailDropped {
					releaseTail()
				}
			}
			if !finishHold() {
				return rescueNone
			}
			if rescueRequested != rescueNone {
				// A reasoning-only stream whose withheld terminal region was
				// resolved as a rescue at EOF (the finish chunk arrived but
				// no [DONE] ever did).
				return rescueRequested
			}
			fillRecord(&a, rec)
			return rescueNone
		}
	}
}

// emitDegenerateResponses replaces a withheld Responses terminal region with a
// Responses-shaped response.failed event. A Responses client has no [DONE]
// sentinel and no choices, so the Chat substitution would leave it without a
// terminal signal.
func emitDegenerateResponses(w http.ResponseWriter, a *sse.Analyzer, now time.Time, code string, rec *metrics.Record) {
	id := a.ResponseID()
	if id == "" {
		id = rec.ID
	}
	msg := metrics.DegenerateMessage(code)
	// The substitution is proxy-minted, not a provider statement: stamp the
	// local failure before feeding the synthetic frames so the standalone
	// error classifier cannot attribute it to the provider.
	a.SetLocalFailure(code, msg)
	// The free-form error event carries the specific internal code; the
	// response.failed envelope carries the closed ResponseErrorCode enum.
	// Feed the error event first so the analyzer's first-real-error rule keeps
	// the specific code on the record; the enum value is wire-only.
	if payload, frame, err := providerformat.ResponsesSyntheticEvent(
		"error", 0, id, rec.Model, code, msg); err == nil {
		a.Feed(append([]byte("data: "), payload...), now)
		a.Feed(nil, now)
		if _, werr := writeCounted(w, rec, frame); werr != nil {
			markClientGone(rec)
			return
		}
	}
	payload, frame, err := providerformat.ResponsesSyntheticEvent(
		"response.failed", 1, id, rec.Model, code, msg)
	if err != nil {
		return
	}
	a.Feed(append([]byte("data: "), payload...), now)
	if _, werr := writeCounted(w, rec, frame); werr != nil {
		markClientGone(rec)
		return
	}
}

// emitDegenerateSSE writes the in-band OpenAI error chunk + closing [DONE] in
// place of a withheld terminal region, and feeds both lines to the analyzer so
// the record carries the failure exactly like a provider-sent in-band error.
// The code comes from metrics.ClassifyOutcome (the single canonical owner of
// the degenerate classes + their messages). A failed write to the (already
// dying) client still marks client-disconnected on the record.
func emitDegenerateSSE(w http.ResponseWriter, a *sse.Analyzer, now time.Time, code string, rec *metrics.Record) {
	if b, err := sse.ErrorEnvelope("", sse.TypeUpstreamError, code, metrics.DegenerateMessage(code)); err == nil {
		line := sse.DataFrame(b)
		a.Feed(line[:len(line)-2], now)
		// The substituted frames replace a terminal region the client never
		// saw, so writeCounted books the bytes the client received, never the
		// withheld originals.
		if _, werr := writeCounted(w, rec, line); werr != nil {
			markClientGone(rec)
			return
		}
	}
	a.Feed([]byte("data: [DONE]"), now)
	if _, werr := writeCounted(w, rec, []byte(sse.DoneFrame)); werr != nil {
		markClientGone(rec)
	}
}
