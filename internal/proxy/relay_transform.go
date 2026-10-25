package proxy

import (
	"bufio"
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

// translationErrorCode is the stable in-band code for a streaming translation
// failure after HTTP 200 was committed. The client receives the shared
// upstream_error envelope; the translator's own error text stays in the
// record, never on the wire.
const translationErrorCode = "translation_error"

// translationErrorMessage is the non-sensitive client-facing text for a
// failed streaming translation. It never echoes the translator's error or
// provider payloads.
const translationErrorMessage = "stream translation failed"

// translatedStreamWriter observes the translator's writes into the pipe
// without buffering the stream: it records whether the inbound protocol's
// terminal frame was written and whether an in-band error frame was already
// forwarded. transformResponse uses that state after the pipe fails so the
// client receives a terminal signal exactly once.
type translatedStreamWriter struct {
	dst        io.Writer
	terminal   func([]byte) bool
	done       bool
	errorFrame bool
}

func (t *translatedStreamWriter) Write(p []byte) (int, error) {
	n, err := t.dst.Write(p)
	if err != nil || n != len(p) {
		return n, err
	}
	// The translator emits each event as one complete Write through
	// sse.EmitFrame, so these comparisons observe frame boundaries without
	// retaining any bytes.
	if t.terminal != nil && t.terminal(p) {
		t.done = true
		return n, nil
	}
	if !t.errorFrame && frameIsError(p) {
		t.errorFrame = true
	}
	return n, nil
}

// closeFailedTranslation appends the terminal frames a failed translation
// pipe did not deliver. The status line and any translated content are
// already on the wire, so a protocol-native error frame (unless the
// translator already forwarded a provider error) followed by that protocol's
// terminal event is the only remaining signal. A failed write means the client is gone: markClientGone
// keeps the cancellation classification and no further writes are attempted.
func closeFailedTranslation(w http.ResponseWriter, flusher http.Flusher, rec *metrics.Record, inbound ir.Protocol, forwardedError bool) {
	if !forwardedError {
		if err := writeProtocolError(w, rec, inbound, rec.ID, sse.TypeUpstreamError, translationErrorCode, translationErrorMessage); err != nil {
			markClientGone(rec)
			return
		}
	}
	if err := writeProtocolTerminal(w, rec, inbound, rec.ID, rec.Model, sse.TypeUpstreamError, translationErrorCode, translationErrorMessage); err != nil {
		markClientGone(rec)
		return
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// transformResponse relays a format-translated upstream response, encoding it
// in the client's protocol on the fly. This handles only the STREAMING branch (the caller routes non-streaming responses through
// serveNonStreaming, which buffers the translation before the status line -
// transformation errors there carry a real error status instead of a committed
// 200). A streaming translation failure after the 200 was committed is closed
// in-band with one protocol-native error frame and terminal event, so the
// client never sees an incomplete stream.
func (s *Server) transformResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, rec *metrics.Record, t *target, stream bool) {
	if stream || isEventStream(resp.Header.Get("Content-Type")) {
		var flusher http.Flusher
		if f, ok := w.(http.Flusher); ok {
			flusher = f
		}
		// Stream translation: convert Anthropic SSE -> OpenAI SSE, while
		// feeding the transformed stream through the metrics analyzer.
		if t.translate {
			pr, pw := io.Pipe()
			tracked := &translatedStreamWriter{dst: pw, terminal: terminalFrameFor(t.inbound)}
			costKeys := s.costKeysFor(rec.Provider)
			// The source analyzer owns provider usage/cost keys; the
			// translated wire is normalized and must not be read with them.
			// Seed its wire so a zero-event source (empty body, comments or
			// pings) still classifies as the routed protocol's truncation
			// instead of returning an empty outcome and losing the canonical
			// upstream_error/truncated stamp.
			sourceAnalyzer := s.analyzerFor(rec)
			sourceAnalyzer.SetWire(t.wire())
			var sourceRec metrics.Record
			var sourceCost float64
			var sourceCostReported bool
			var translationErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				// Propagate a translation/scan failure to the reader as a pipe
				// error instead of faking a clean EOF (CloseWithError(nil) ==
				// Close). The goroutine must NOT touch rec - the handler
				// finalizes the record right here, so the reader side
				// (streamBodyTranslated) classifies the surfaced error
				// (translation failure vs client disconnect) itself. The
				// tracking writer records the translator's terminal frames so
				// the handler can close an incomplete stream exactly once.
				translationErr = providerformat.TranslateStreamBetween(t.inbound, t.outbound, tracked, resp.Body, nil, func(eventType string, payload []byte) {
					sourceAnalyzer.FeedEvent(eventType, payload, time.Now())
					if cost, ok := metrics.ExtractCost(payload, costKeys); ok {
						sourceCost, sourceCostReported = cost, true
					}
				}, providerformat.StreamOptions{IncludeUsage: t.streamIncludeUsage, FallbackModel: rec.Model})
				pw.CloseWithError(translationErr)
			}()
			// Re-run the SSE analyzer over the transformed stream.
			resetTurnMetrics(rec)
			s.streamBodyTranslated(ctx, w, pr, rec, flusher, t.inbound)
			// Close both ends of upstream work on early client failure, then
			// join before reading source accounting or finalizing the record.
			pr.Close()
			resp.Body.Close()
			<-done
			// The pipe delivered every frame the translator produced. A
			// failure without [DONE] would leave the client waiting forever,
			// so close the committed stream with the one terminal pair the
			// client understands. The synthetic frame is not fed back through
			// the analyzer: the record keeps markStreamErr's translation
			// classification rather than being relabeled a provider error.
			if translationErr != nil && !tracked.done && !rec.ClientDisconnected {
				closeFailedTranslation(w, flusher, rec, t.inbound, tracked.errorFrame)
			}
			// Source accounting is authoritative for translated streams. The
			// translated analyzer refused provider overrides; stamp what the
			// source actually reported after the client turn is finalized.
			fillRecord(&sourceAnalyzer, &sourceRec)
			// The source observer owns the provider error class. The
			// client-wire analyzer sees the target encoder's normalized error
			// (api_error, provider_error) and loses the source code, so the
			// first real source error wins over that view. A truncation is
			// synthesized by the translator inside TranslateStreamBetween and
			// therefore never reaches the source analyzer as an error event;
			// stamp the canonical truncation unless a genuine translator
			// failure already owns the record. Healthy streams are unchanged.
			if sourceRec.ErrorType != "" {
				rec.ErrorType = sourceRec.ErrorType
				rec.ErrorCode = sourceRec.ErrorCode
				rec.ErrorMsg = sourceRec.ErrorMsg
			} else if translationErr == nil && sourceAnalyzer.OutcomeCode() == metrics.CodeTruncated {
				rec.ErrorType = sse.TypeUpstreamError
				rec.ErrorCode = metrics.CodeTruncated
				rec.ErrorMsg = metrics.DegenerateMessage(metrics.CodeTruncated)
			}
			// A provider-executed item has no target block: every encoder drops
			// it with a warning, so the translated analyzer counts zero. The
			// source observer owns the activity and its retained names.
			if sourceRec.ToolCalls > rec.ToolCalls {
				rec.ToolCalls = sourceRec.ToolCalls
				if len(sourceRec.ToolNames) > 0 {
					rec.ToolNames = append([]string(nil), sourceRec.ToolNames...)
				}
			}
			// Invalid accounting is sticky: the source observer may have
			// captured only the usage object that failed validation (or a
			// later partial one), and copying it would restore a refundable
			// count for a turn already marked unavailable.
			if sourceRec.UsageInvalid {
				invalidateUsage(rec, metrics.ErrMetricRange)
			}
			if sourceAnalyzer.UsageReported() && !rec.UsageInvalid {
				rec.Usage = sourceRec.Usage
				// Provider usage also owns the generation count: the
				// translated analyzer's chunk-derived fallback (tool-arg
				// chunks excluded) must not survive real accounting.
				rec.GenTokens = sourceRec.GenTokens
			}
			if sourceCostReported {
				rec.Cost = sourceCost
			} else if sourceRec.Cost != 0 {
				rec.Cost = sourceRec.Cost
			}
			return
		}
		// Note: format "cursor" never reaches here - it is served by the
		// dedicated bidirectional handler (serveCursorBidi), bypassing
		// doWithRetry/transformResponse entirely.
		if _, err := io.Copy(disconnectWriter{w, rec}, resp.Body); err != nil {
			markStreamErr(ctx, rec, err)
		}
		return
	}
}

// streamBodyTranslated is like streamBody but for a pre-transformed SSE stream
// that does not need line splitting (the transformer emits complete frames).
// A scanner error here is the transformer's failure propagated through the
// pipe: a genuine translation/upstream error - unless the client disconnected,
// in which case the loop above already exited via a failed write to w.
func (s *Server) streamBodyTranslated(ctx context.Context, w http.ResponseWriter, body io.Reader, rec *metrics.Record, flusher http.Flusher, inbound ir.Protocol) {
	if flusher == nil {
		if _, err := io.Copy(disconnectWriter{w, rec}, body); err != nil {
			markStreamErr(ctx, rec, err)
		}
		return
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, sseScannerBufInit), translatedFrameBufMax)
	a := s.translatedAnalyzer()
	// Frames are accumulated until their blank-line terminator, then written
	// and flushed once: the transformer always emits "data: ...\n\n", so this
	// is one Write and one Flush per event instead of two of each, and the
	// buffer is reused across events with no per-line allocation.
	var frame []byte
	isTerminal := terminalFrameFor(inbound)
	terminal := false
	writeFrame := func() bool {
		if len(frame) == 0 {
			return true
		}
		// Capture the upstream-arrival timestamp before the client write:
		// a slow or backpressured client must not stretch TTFT or the
		// decode window. The direct relay path anchors its timestamps the
		// same way at the read site.
		now := time.Now()
		if _, err := writeCounted(w, rec, frame); err != nil {
			markClientGone(rec)
			// An in-band provider error that already reached the client is
			// the turn's outcome even though the following write failed; the
			// native streamBody twin fills it in clientGone. Without this the
			// 499 erases the provider classification.
			if a.HasInBandError() {
				fillRecord(&a, rec)
			}
			return false
		}
		// Feed the analyzer from the bytes actually delivered, in line order.
		rest := frame
		for len(rest) > 0 {
			line := rest
			if i := bytes.IndexByte(rest, '\n'); i >= 0 {
				line, rest = rest[:i], rest[i+1:]
			} else {
				rest = nil
			}
			a.Feed(line, now)
		}
		terminal = isTerminal(frame)
		frame = frame[:0]
		flusher.Flush()
		return true
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			frame = append(frame, '\n')
			if !writeFrame() {
				return
			}
			if terminal {
				// The terminal [DONE] resolved the stream. Stop reading so
				// post-terminal debris cannot surface as a translation error
				// on a completed record; the caller closes the upstream body
				// and joins the translator.
				fillRecord(&a, rec)
				return
			}
			continue
		}
		frame = append(frame, line...)
		frame = append(frame, '\n')
	}
	if !writeFrame() {
		return
	}
	// A scanner error is the transformer's failure arriving through the pipe
	// (CloseWithError): classify it here, in the handler, so the classification
	// stays race-free with the deferred record finalize. markStreamErr keeps a
	// client-disconnect-induced pipe close out of the error fields.
	if err := scanner.Err(); err != nil {
		markStreamErr(ctx, rec, err)
	}
	fillRecord(&a, rec)
	if err := scanner.Err(); errors.Is(err, metrics.ErrMetricRange) {
		invalidateUsage(rec, err)
	}
}

// fillRecord is the one stream-side bridge from analyzer accounting to the
// record. The analyzer owns the source-usage validity signal because it sees
// sum overflows that a later chunk fallback would otherwise hide; invalidation
// runs after Fill so it clears any partial document Fill published.
func fillRecord(a *sse.Analyzer, rec *metrics.Record) {
	a.Fill(rec)
	if a.UsageInvalid() {
		invalidateUsage(rec, metrics.ErrMetricRange)
	}
}

// streamRescue classifies why streamBody wants the caller to transparently
// re-send the SAME request on the same committed SSE connection. Every class
// guarantees the client received no completion-contract bytes (no answer
// content, no tool call, no refusal), so the fresh stream's frames append
// cleanly; each class is booked as an absorbed retry attempt (Retries +
// Attempts, the dashboard's correction flag) and the re-send re-enters the
// ordinary admission path (operator holds, storm, pacing).
type streamRescue int

const (
	// rescueNone: the stream finished and its outcome is final - finalize.
	rescueNone streamRescue = iota
	// rescueEmptyTruncation: the stream hit clean EOF with no terminal
	// marker while the client had received no content-bearing frame at all
	// (only role/keepalive). The quality_retries budget owns this class.
	rescueEmptyTruncation
	// rescueReasoningTruncation: the stream died mid-thinking - clean EOF or
	// a genuine upstream read error, still no terminal marker - after the
	// client received reasoning deltas but nothing answer-shaped. The
	// thinking_retries budget owns this class.
	rescueReasoningTruncation
	// rescueReasoningStop: the stream terminated cleanly (stop, absent, or
	// interruption finish_reason) after reasoning-only output, and the
	// terminal region is still WITHHELD by the hold, so nothing signed the
	// void on the wire; the re-send replaces the withheld finish. The
	// thinking_retries budget owns this class (metrics.CodeReasoningOnly).
	rescueReasoningStop
	// rescueUpstreamError: the provider signaled failure in-band (an error
	// event streamed inside the 200) in a retryable server-availability
	// class, while nothing content-bearing had reached the client - the
	// error frame itself was dropped before the wire, so the re-send is
	// invisible. The quality_retries budget owns this class;
	// thinking_retries owns it when only reasoning was relayed. The
	// upstreamErrorRescue return carries the provider's error envelope for
	// the absorbed attempt's log entry.
	rescueUpstreamError
)

// upstreamErrorRescue carries the provider's in-band error envelope of an
// absorbed rescueUpstreamError attempt to the re-send loop's bookkeeping
// (the attempt log shows the provider's own type/code/message - what the
// error explorer groups it under). reasoning selects the budget exactly like
// the truncation twins: reasoning frames already relayed are auxiliary
// display text, so the thinking budget owns the rescue.
type upstreamErrorRescue struct {
	typ, code, msg string
	reasoning      bool
}

// endsWithUnframedDataLine reports whether b ends with a complete SSE data
// line and no blank separator after it. Framing the next data line as a new
// event prevents two payloads from concatenating into one event no spec
// parser can dispatch.
func endsWithUnframedDataLine(b []byte) bool {
	if len(b) == 0 || b[len(b)-1] != '\n' {
		return false
	}
	end := len(b) - 1
	start := bytes.LastIndexByte(b[:end], '\n') + 1
	line := bytes.TrimRight(b[start:end], "\r")
	if len(bytes.TrimSpace(line)) == 0 {
		return false
	}
	_, ok := sse.DataPayload(line)
	return ok
}
