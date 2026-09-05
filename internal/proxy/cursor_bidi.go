package proxy

// Cursor agent.v1 bidirectional Run path.
//
// Cursor's chat RPC is full-duplex: the server answers on the same open HTTP/2
// stream while the client keeps writing (KV blob pulls, the request-context
// handshake, heartbeats). The generic doWithRetry path writes a fixed body and
// reads a response - that can't carry a bidi exchange. So cursor requests are
// routed here instead: we translate the OpenAI body into an AgentClientMessage,
// open a full-duplex h2 stream, and drive CursorRun, which writes the run request,
// answers the server's mid-stream callbacks, and emits OpenAI SSE.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"

	"golang.org/x/net/http2"
)

// cursorH2PingTimeout is HTTP/2 ping detection on the Cursor bidi transport
// (half-open parked streams). Not SSE keepalive (that's SSEKeepaliveInterval).
const cursorH2PingTimeout = 15 * time.Second

// errEchoMax bounds an upstream error body echoed into a 502 message.
const errEchoMax = 512

// cursorClientFor returns the shared full-duplex HTTP/2 client used for Cursor
// Run calls, building it on first use. It mirrors the main upstream client's
// pool/timeout tunables where they apply, but uses an x/net/http2.Transport so
// plaintext (h2c) upstreams and explicit full-duplex are supported (stdlib
// net/http can't do client h2c with a custom dialer, and cursor is always h2).
func (s *Server) cursorClientFor() *http.Client {
	s.cursorClientMu.Lock()
	defer s.cursorClientMu.Unlock()
	if s.cursorClient != nil {
		return s.cursorClient
	}
	// TLS is selected by URL scheme, never by the port. Distinct pools also
	// prevent an http:// connection from satisfying an https:// request to
	// the same authority. The TLS transport owns certificate/ALPN validation.
	plain := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, network, addr)
		},
		PingTimeout: cursorH2PingTimeout,
	}
	tlsTransport := &http2.Transport{PingTimeout: cursorH2PingTimeout}
	s.cursorClient = &http.Client{
		Transport:     &cursorSchemeTransport{plain: plain, tls: tlsTransport},
		CheckRedirect: preserveUpstreamRedirect,
	}
	return s.cursorClient
}

type cursorSchemeTransport struct{ plain, tls *http2.Transport }

func (t *cursorSchemeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	switch r.URL.Scheme {
	case "https":
		return t.tls.RoundTrip(r)
	case "http":
		return t.plain.RoundTrip(r)
	default:
		return nil, fmt.Errorf("unsupported upstream scheme %q", r.URL.Scheme)
	}
}

func (t *cursorSchemeTransport) CloseIdleConnections() {
	t.plain.CloseIdleConnections()
	t.tls.CloseIdleConnections()
}

// doCursorUntil runs client.Do on a Background-bound request so a parked
// run outlives this HTTP handler, but cancels that request if waitCtx
// (client cancel / X-Proxy-Timeout-Ms) fires before headers arrive.
func doCursorUntil(client *http.Client, req *http.Request, waitCtx context.Context, cancel context.CancelFunc) (*http.Response, error) {
	if waitCtx == nil {
		return client.Do(req)
	}
	type out struct {
		resp *http.Response
		err  error
	}
	ch := make(chan out, 1)
	go func() {
		resp, err := client.Do(req)
		ch <- out{resp, err}
	}()
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-waitCtx.Done():
		// Prefer a result that already landed (Go select is uniform-random
		// when both cases are ready). Only then cancel the in-flight Do.
		select {
		case r := <-ch:
			return r.resp, r.err
		default:
		}
		cancel()
		r := <-ch
		if r.resp != nil {
			r.resp.Body.Close()
		}
		if r.err != nil {
			return nil, r.err
		}
		return nil, waitCtx.Err()
	}
}

// cursorWaitCtx bounds one bidi send's header wait with the tighter of
// upstream_timeout and the per-request X-Proxy-Timeout-Ms (0 = that source is
// unbounded), without putting a deadline on the Background-bound parked run.
// Like doWithRetry, the deadline anchors at the send: the WaitSend gate above
// it (operator hold, group serialization) runs on the base request context
// and never burns the budget.
func (s *Server) cursorWaitCtx(ctx context.Context, reqTimeout time.Duration) (context.Context, context.CancelFunc) {
	if s == nil || ctx == nil {
		return ctx, func() {}
	}
	d := s.cfg().UpstreamTimeout
	if reqTimeout > 0 && (d <= 0 || reqTimeout < d) {
		d = reqTimeout
	}
	if d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// cursorSendFailure classifies a failed bidi send for both cursor send paths
// (the initial Run and the quality re-ask): paced reports whether the
// provider+key group should FailSend, msg is the public error text. A FIRED
// per-send budget (waitErr == context.DeadlineExceeded - the wait context's
// own deadline, read before its cancel func runs) mirrors the generic path's
// attemptCtx rule: never paced (the client's own deadline is not an upstream
// health signal) and surfaced as the canonical "context deadline exceeded" -
// whether the Do died with waitCtx.Err() itself (the doCursorUntil race
// window, where the raw error is a retryable-class net timeout) or with the
// "context canceled" its cancel produced (the common shape). Any other error
// keeps the transport classification.
func cursorSendFailure(err, waitErr error) (paced bool, msg string) {
	if waitErr == context.DeadlineExceeded {
		return false, "context deadline exceeded"
	}
	return isRetryableTransportErr(err), transportErrText(err)
}

// serveCursorBidi runs one turn of a resumable Cursor Run and streams OpenAI
// SSE to w. Cursor's Run stream pauses on a tool call waiting for the result on
// the SAME stream; to mimic Cursor's agent loop (and avoid a fresh handshake
// per tool turn), the proxy parks that stream and resumes it when the client
// returns the tool result on a later request. Correlation is by tool_call_id
// (the only exact, client-agnostic signal). A request whose tool results match
// no parked run cold-starts a fresh Run with the full history the client sent.
// logCursorRequestShape logs the request's message SHAPE only (roles, content
// lengths, tool_call ids). Oversized messages get a 4-byte sha256 prefix for
// correlation - never content bytes (message content must not reach logs).
func (s *Server) logCursorRequestShape(r *http.Request, body []byte) {
	var in struct {
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &in) != nil {
		return
	}
	parts := make([]string, 0, len(in.Messages))
	for _, m := range in.Messages {
		p := fmt.Sprintf("%s(%d,%s)", m.Role, len(m.Content), shortID(m.ToolCallID))
		if len(m.Content) > 8192 {
			sum := sha256.Sum256(m.Content)
			p += fmt.Sprintf("«%x»", sum[:4])
		}
		parts = append(parts, p)
	}
	log.Printf("cursor-shape session=%s n=%d msgs=%s", shortID(r.Header.Get("X-Session-Id")), len(in.Messages), strings.Join(parts, " "))
}

// shortID renders a tool_call/session id as an 8-char suffix prefix (used in
// shape logs only).
func shortID(id string) string {
	const n = 8
	if len(id) <= n {
		return id
	}
	return "…" + id[len(id)-n:]
}

func (s *Server) serveCursorBidi(ctx context.Context, w http.ResponseWriter, r *http.Request, t *target, key, rawModel string, body []byte, stream bool, rec *metrics.Record, groupKey string, hooks scheduler.WaiterHooks) {
	s.logCursorRequestShape(r, body)
	rr := cursorTurnRender{est: estimateInputTokens(body), includeUsage: requestIncludesUsage(body), model: rec.Model, scope: s.cursorScopeFor(t, key, rawModel, rec.Client)}
	// Does this request carry tool results that match a parked run? If so,
	// resume that stream instead of opening a new one.
	toolResults := extractToolResults(body)
	if ids := toolResultIDs(toolResults); len(ids) > 0 {
		if run, tail := s.cursorRuns.findByToolTail(rr.scope, ids); run != nil {
			if run.Closed() {
				// The parked stream died upstream while waiting (reset/EOF): the
				// run is a zombie and its body pipe is closed - resume would
				// fail with "io: read/write on closed pipe". Treat it like an
				// expired park: cold-start a fresh Run; the history carries
				// everything, and a void outcome is flagged (empty_turn).
				s.cursorRuns.drop(run)
				run.Close()
				log.Printf("cursor-run: COLD-START (parked stream died: %s…)", shortID(ids[len(ids)-1]))
			} else {
				// Only the trailing results are this turn's answers; older ids in the
				// cumulative history were consumed turns ago.
				matched := make(map[string]bool, len(tail))
				for _, id := range tail {
					matched[id] = true
				}
				current := toolResults
				if len(tail) != len(ids) {
					current = nil
					for _, tr := range toolResults {
						if matched[tr.toolCallID] {
							current = append(current, tr)
						}
					}
				}
				log.Printf("cursor-run: RESUME ids=%d of %d", len(current), len(ids))
				// Resumed turns never open an upstream request, so StatusCode is
				// never set by the fresh-run path below. With status 0 the dashboard
				// keeps rendering the row as an in-flight "streaming" row forever.
				rec.StatusCode = http.StatusOK
				// A user message typed mid-turn arrives bundled after the tool
				// results; steer it into the running turn (else it would be dropped).
				s.driveParkedRun(ctx, w, run, current, providerformat.CursorTrailingUserText(body), stream, rec, rr)
				return
			}
		} else {
			log.Printf("cursor-run: COLD-START (trailing id not parked: %s…)", shortID(ids[len(ids)-1]))
		}
		// No live parked run owns these ids - fall through to a fresh Run (the
		// history the client sent already carries the tool call + result).
	}

	// Fresh run: translate the request (structured history blobs incl. any tool
	// call/result turns), open the bidi stream, and drive the turn. Every fresh
	// run mints a NEW conversation_id: a stable id only pairs with the server's
	// own checkpointed state (which a stateless replay doesn't carry) - reusing
	// the client's session id across cold starts left server-side tool state
	// dangling (the no-bridge doc combination; see the null-.map crash class).
	// History travels content-addressed, so recall never depends on the id.
	clientMessage, blobs, resumeRequest, err := providerformat.TranslateCursorRunRequestWithConversation(body, "")
	if err != nil {
		writeClientError(w, rec, "invalid_request_error", err.Error(), http.StatusBadRequest)
		return
	}

	targetURL := cursorRunURL(t)

	// The request body is a pipe we keep open for the whole (possibly
	// multi-request) exchange - it must NOT be closed when this HTTP request
	// returns, because a parked run keeps writing to it on a later request.
	pr, pw := io.Pipe()
	upstreamCtx, upstreamCancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, targetURL, pr)
	if err != nil {
		upstreamCancel()
		pw.Close()
		writeClientError(w, rec, "upstream_unreachable", err.Error(), http.StatusBadGateway)
		return
	}
	// agent.v1 / Connect-RPC headers.
	s.setCursorHeaders(req, t, key)

	// Prime the body with the framed run_request BEFORE Do (the h2 transport
	// won't send headers until it can pull the first body chunk).
	initialFrame := providerformat.AppendConnectEnvelope(nil, clientMessage)
	go func() { _, _ = pw.Write(initialFrame) }()

	own, err := s.scheduler.WaitSend(ctx, groupKey, hooks, false, false)
	if err != nil {
		upstreamCancel()
		pw.Close()
		if r.Context().Err() == context.Canceled {
			rec.StatusCode = metrics.StatusClientClosedRequest
			rec.ClientDisconnected = true
			return
		}
		writeClientError(w, rec, "upstream_unreachable", err.Error(), http.StatusBadGateway)
		return
	}
	sendFailed := false
	ended := false
	endSend := func() {
		if ended {
			return
		}
		ended = true
		s.cursorEndSend(groupKey, own, sendFailed)
	}
	defer endSend()

	wait, cancelWait := s.cursorWaitCtx(ctx, t.timeout)
	resp, err := doCursorUntil(s.cursorClientFor(), req, wait, upstreamCancel)
	// Read the wait ctx's Err before cancelWait runs, so a FIRED budget is
	// captured unmasked (cursorSendFailure keys on it).
	waitErr := wait.Err()
	cancelWait()
	if err != nil {
		upstreamCancel()
		pw.Close()
		// Client cancel vs genuine transport failure - same classification as
		// the doWithRetry error path: the client's own cancellation is 499 +
		// client-disconnected, never an upstream error. (The base request ctx
		// is what distinguishes a client cancel from a fired per-send budget:
		// the wait deadline cancels only the upstream request.)
		if r.Context().Err() == context.Canceled {
			rec.StatusCode = metrics.StatusClientClosedRequest
			rec.ClientDisconnected = true
			return
		}
		paced, msg := cursorSendFailure(err, waitErr)
		sendFailed = paced
		writeClientError(w, rec, "upstream_unreachable", "upstream error: "+msg, http.StatusBadGateway)
		return
	}

	rec.StatusCode = resp.StatusCode
	rec.ResponseHeaders = captureHeaders(resp.Header, t.authHeader)
	rec.ProviderRequestID = firstNonEmpty(
		resp.Header.Get("X-Request-Id"),
		resp.Header.Get("Request-Id"),
	)
	if resp.StatusCode >= 400 {
		// Connect streaming is HTTP 200 + envelopes. A 4xx/5xx means the
		// Run never started - do not drive it as a live bidi body.
		sendFailed = cursorSendFailed(resp)
		captureErrorFromResponse(resp, rec)
		upstreamCancel() // tears down the h2 stream - the transport closes the REAL body (the cursor path never reaches ServeHTTP's body defer)
		pw.Close()
		// resp.Body here is captureErrorFromResponse's NopCloser re-wrap, so
		// this Close is a no-op, kept only so a future Close-propagating wrap
		// cannot leak the stream.
		resp.Body.Close()
		typ, msg := rec.ErrorType, rec.ErrorMsg
		if typ == "" {
			typ = "api_error"
		}
		if msg == "" {
			msg = fmt.Sprintf("upstream HTTP %d", resp.StatusCode)
		}
		writeClientError(w, rec, typ, msg, resp.StatusCode)
		return
	}
	// Headers landed: release the send token before the turn (and any
	// park). A parked resume is not a new send and must not hold the gate.
	endSend()

	// Build the resumable run over the live stream. closeFn cancels the upstream
	// request and closes the body pipe (only when the run truly ends). The
	// heartbeat cadence comes from the live config snapshot (a reload applies
	// to the next run) - the canonical default lives in config.Default().
	run := providerformat.NewCursorRun(pw, resp.Body, blobs, func() {
		upstreamCancel()
		pw.Close()
		resp.Body.Close()
	}, s.cfg().CursorHeartbeatInterval)
	run.Start()

	// The one-shot re-ask: when the resume-action turn voids (0 output), the
	// turn driver may transparently re-drive the SAME request as a fresh user
	// turn (user_message_action) on a NEW upstream run - nothing visible was
	// emitted for the void turn, so the client just sees the re-ask's answer
	// on the same stream. Gated by quality_retries (0 disables) and by the
	// request really being a continuation (a plain fresh turn has nothing to
	// re-ask; its own emptiness is already surfaced).
	var reask func() (*providerformat.CursorRun, error)
	if s.cfg().QualityRetries > 0 && resumeRequest {
		called := false
		reask = func() (*providerformat.CursorRun, error) {
			if called {
				return nil, nil
			}
			called = true
			return s.openCursorRun(ctx, r, t, key, body, true, groupKey, hooks)
		}
	}
	s.driveRun(ctx, w, run, stream, rec, rr, resumeRequest, reask)
}

func cursorRunURL(t *target) string {
	path := t.path
	if path == "" {
		path = providerformat.CursorConnectPath()
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return t.baseURL + path
}

// cursorEndSend releases the send token. A retryable failure trips the
// provider+key group; a success or 4xx/quota/cancel does not.
func (s *Server) cursorEndSend(groupKey string, own, failed bool) {
	if failed {
		s.scheduler.FailSend(groupKey, own)
		return
	}
	if own {
		s.scheduler.EndSend(groupKey, false)
	}
}

// cursorSendFailed reports whether this Cursor HTTP outcome should FailSend.
// Quota 429s and non-retryable 4xx do not trip the group. A peeked 429
// body is re-wrapped onto resp.Body so the caller can still relay it.
func cursorSendFailed(resp *http.Response) bool {
	if resp.StatusCode == http.StatusTooManyRequests {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBodyBytes+1))
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(errBody))
		if len(errBody) <= maxErrBodyBytes {
			typ, code, _ := metrics.ParseErrorEnvelope(errBody)
			return !isNonRetryableQuotaErr(typ, code)
		}
		return true
	}
	return isRetryable(resp.StatusCode)
}

// openCursorRun opens a fresh agent.v1 Run stream for the given request body.
// forceUser selects the re-ask translation (fresh user turn). Errors are
// returned to the caller (never written to the client); the caller classifies
// them - a failed re-ask falls back to surfacing the void.
func (s *Server) openCursorRun(ctx context.Context, r *http.Request, t *target, key string, body []byte, forceUser bool, groupKey string, hooks scheduler.WaiterHooks) (*providerformat.CursorRun, error) {
	var (
		clientMessage []byte
		blobs         *providerformat.KVBlobStore
		err           error
	)
	if forceUser {
		clientMessage, blobs, err = providerformat.TranslateCursorRunRequestFreshUser(body)
	} else {
		clientMessage, blobs, _, err = providerformat.TranslateCursorRunRequestWithConversation(body, "")
	}
	if err != nil {
		return nil, err
	}

	targetURL := cursorRunURL(t)

	pr, pw := io.Pipe()
	upstreamCtx, upstreamCancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(upstreamCtx, http.MethodPost, targetURL, pr)
	if err != nil {
		upstreamCancel()
		pw.Close()
		return nil, err
	}
	s.setCursorHeaders(req, t, key)

	initialFrame := providerformat.AppendConnectEnvelope(nil, clientMessage)
	go func() { _, _ = pw.Write(initialFrame) }()

	// Re-ask is a new send after the voided run already EndSend'd.
	own, err := s.scheduler.WaitSend(ctx, groupKey, hooks, true, false)
	if err != nil {
		upstreamCancel()
		pw.Close()
		return nil, err
	}
	sendFailed := false
	ended := false
	endSend := func() {
		if ended {
			return
		}
		ended = true
		s.cursorEndSend(groupKey, own, sendFailed)
	}
	defer endSend()

	wait, cancelWait := s.cursorWaitCtx(ctx, t.timeout)
	resp, err := doCursorUntil(s.cursorClientFor(), req, wait, upstreamCancel)
	// Same fired-budget rule as the initial send: read the wait ctx's Err
	// before cancelWait so cursorSendFailure sees an unmasked deadline.
	waitErr := wait.Err()
	cancelWait()
	if err != nil {
		upstreamCancel()
		pw.Close()
		sendFailed, _ = cursorSendFailure(err, waitErr)
		return nil, err
	}
	if resp.StatusCode >= 400 {
		sendFailed = cursorSendFailed(resp)
		detail := fmt.Sprintf("cursor run: HTTP %d", resp.StatusCode)
		upstreamCancel()
		pw.Close()
		resp.Body.Close()
		return nil, fmt.Errorf("%s", detail)
	}
	endSend()

	run := providerformat.NewCursorRun(pw, resp.Body, blobs, func() {
		upstreamCancel()
		pw.Close()
		resp.Body.Close()
	}, s.cfg().CursorHeartbeatInterval)
	run.Start()
	return run, nil
}

// setCursorIdentity applies the agent.v1 auth + protocol headers shared by
// the bidi Run path and the unary GetUsableModels path. The identity headers
// (client version/type, ghost mode, request id) are the provider's
// CONFIGURED upstream headers (providers.<label>.headers - the same map the
// generic paths apply); the Connect protocol version and the h2 trailers
// declaration are protocol constants that stay code-owned and cannot be
// overridden.
func (s *Server) setCursorIdentity(req *http.Request, t *target, key string) {
	s.applyProviderHeaders(req.Header, t.provider)
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Te", "trailers")
	if key != "" {
		req.Header.Set(t.authHeader, t.authPrefix+key)
	}
}

// setCursorHeaders applies the agent.v1 passthrough headers to a cursor Run
// request. The enveloped connect+proto content type is protocol (never
// configurable) and is stamped AFTER the configured map so no configured
// value can corrupt the Connect framing.
func (s *Server) setCursorHeaders(req *http.Request, t *target, key string) {
	s.setCursorIdentity(req, t, key)
	req.Header.Set("Content-Type", "application/connect+proto")
}

// driveRun drives a fresh run's first turn and renders the outcome as OpenAI
// SSE (stream=true) or a single JSON completion (stream=false). On a tool call
// it parks the run for a later request to resume. resumeRequest marks a
// continuation (empty resume_action) turn, eligible for void detection; reask
// (optional) opens a fresh-user re-ask run when that turn voids.
func (s *Server) driveRun(ctx context.Context, w http.ResponseWriter, run *providerformat.CursorRun, stream bool, rec *metrics.Record, rr cursorTurnRender, resumeRequest bool, reask func() (*providerformat.CursorRun, error)) {
	id := "cursor-" + providerformat.UUID4()
	if stream {
		s.serveRunStream(ctx, w, run, id, rec, rr, resumeRequest, reask)
		return
	}
	s.serveRunJSON(ctx, w, run, id, rec, rr, resumeRequest, reask)
}

// driveParkedRun resumes a parked run with the tool results from this request.
func (s *Server) driveParkedRun(ctx context.Context, w http.ResponseWriter, run *providerformat.CursorRun, results []cursorToolResult, steerText string, stream bool, rec *metrics.Record, rr cursorTurnRender) {
	id := "cursor-" + providerformat.UUID4()
	if stream {
		s.serveResumeStream(ctx, w, run, results, steerText, id, rec, rr)
		return
	}
	s.serveResumeJSON(ctx, w, run, results, steerText, id, rec, rr)
}

// cursorTrackDelta records timing + answer timestamps for one emitted delta:
// first/last token (any kind), plus first/last ANSWER token (a content delta
// - reasoning chunks never count as the answer). Without these the dashboard
// renders the first-answer cell as "now".
func cursorTrackDelta(rec *metrics.Record, delta map[string]any) {
	now := time.Now()
	if rec.FirstTokenAt.IsZero() {
		rec.FirstTokenAt = now
	}
	rec.LastTokenAt = now
	if c, ok := delta["content"].(string); ok && c != "" {
		if rec.FirstAnswerAt.IsZero() {
			rec.FirstAnswerAt = now
		}
		// Content presence flags the tool-call-only detection (sync'd across
		// finalize paths): a mixed text+tool-call turn keeps its answer tokens.
		rec.HadAnswerContent = true
	}
}

// serveRunStream drives a fresh run's turn, writing OpenAI SSE. When a
// resume-action turn voids (0 output tokens) and a re-ask opener is available,
// the void is absorbed as one transparent re-ask: a NEW run opens with the
// same request re-translated as a fresh user turn, and its answer continues on
// the SAME client stream (the void turn emitted nothing the client could see,
// and no [DONE] separates the two attempts). A void that cannot be re-asked
// (no opener, opener failure, or this WAS the re-ask) surfaces as in-band
// empty_turn.
func (s *Server) serveRunStream(ctx context.Context, w http.ResponseWriter, run *providerformat.CursorRun, id string, rec *metrics.Record, rr cursorTurnRender, resumeRequest bool, reask func() (*providerformat.CursorRun, error)) {
	flusher, _ := w.(http.Flusher)
	writeSSEHeaders(w, rec.StatusCode)
	emit := sseEmitter(w, id, rr.model, flusher)
	if err := emit(map[string]any{"role": "assistant", "content": ""}, nil); err != nil {
		// The opening role chunk already hit a dead socket. Mark the
		// disconnect now: a turn that ends with zero delta events (an empty
		// upstream turn) would otherwise never reach the emitDelta failure
		// path and would record a clean outcome.
		rec.ClientDisconnected = true
	}
	rec.FirstTokenAt = time.Time{}
	rec.LastTokenAt = time.Time{}
	rec.Usage = metrics.Usage{}

	emitDeltas := func(delta map[string]any) error {
		cursorTrackDelta(rec, delta)
		return emit(delta, nil)
	}
	result := run.RunTurn(ctx, emitDeltas)
	if cursorVoidTurn(resumeRequest, result) && reask != nil {
		// Absorb the void: close the voided run and re-drive as a fresh user
		// turn on a new run. The re-ask turn remains void-eligible (a fresh
		// turn that ALSO produced nothing is the same failure surface).
		s.cursorRuns.drop(run)
		run.Close()
		s.absorbCursorVoid(rec)
		next, err := reask()
		if err == nil && next != nil {
			rec.FirstTokenAt = time.Time{}
			rec.LastTokenAt = time.Time{}
			rec.Usage = metrics.Usage{}
			result = next.RunTurn(ctx, emitDeltas)
			s.finishRunTurn(w, next, result, emit, rec, id, rr, true) // re-ask turn: void surfaces
			return
		}
		// Re-ask unavailable (opener failed): surface the void below.
	}
	s.finishRunTurn(w, run, result, emit, rec, id, rr, resumeRequest)
}

// serveResumeStream resumes a parked run with tool results, writing OpenAI SSE.
func (s *Server) serveResumeStream(ctx context.Context, w http.ResponseWriter, run *providerformat.CursorRun, results []cursorToolResult, steerText string, id string, rec *metrics.Record, rr cursorTurnRender) {
	flusher, _ := w.(http.Flusher)
	writeSSEHeaders(w, rec.StatusCode)
	emit := sseEmitter(w, id, rr.model, flusher)
	if err := emit(map[string]any{"role": "assistant", "content": ""}, nil); err != nil {
		rec.ClientDisconnected = true // same zero-delta-turn gap as serveRunStream
	}
	rec.FirstTokenAt = time.Time{}
	rec.LastTokenAt = time.Time{}
	rec.Usage = metrics.Usage{}

	m := map[string]struct {
		Text    string
		IsError bool
	}{}
	for _, tr := range results {
		m[tr.toolCallID] = struct {
			Text    string
			IsError bool
		}{tr.content, false}
	}
	result := run.ResumeTurn(ctx, m, steerText, func(delta map[string]any) error {
		cursorTrackDelta(rec, delta)
		return emit(delta, nil)
	})
	s.finishRunTurn(w, run, result, emit, rec, id, rr, false)
}

// cursorTurnRender carries per-request rendering decisions threaded through the
// turn drivers: the input-token estimate fallback, whether the client requested
// OpenAI usage chunks, and the model id to stamp on chunks.
type cursorTurnRender struct {
	scope        cursorScope
	est          int64
	includeUsage bool
	model        string
}

// requestIncludesUsage reports whether the request asked for usage via
// stream_options.include_usage (the OpenAI contract says the final usage chunk
// appears only then; cumulative-history clients provide it).
func requestIncludesUsage(body []byte) bool {
	var in struct {
		StreamOptions struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	return json.Unmarshal(body, &in) == nil && in.StreamOptions.IncludeUsage
}

// recordCursorTools folds a turn's surfaced tool calls into the metrics record
// (count + unique names) so the dashboard's tool column and the explorer's
// tool dimension see cursor tool calls like any other path.
func recordCursorTools(rec *metrics.Record, calls []providerformat.CursorToolCall) {
	if len(calls) == 0 {
		return
	}
	seen := make(map[string]bool, len(rec.ToolNames)+len(calls))
	for _, n := range rec.ToolNames {
		seen[n] = true
	}
	for _, c := range calls {
		rec.ToolCalls++
		if !seen[c.Name] {
			seen[c.Name] = true
			rec.ToolNames = append(rec.ToolNames, c.Name)
		}
	}
}

// cursorVoidMsg is the single canonical description of the "void": Cursor
// finishing a continuation (empty resume_action) with an empty turn - 0 output
// tokens, finish stop - because there was nothing server-side to resume (the
// parked stream was lost to a restart, TTL expiry, or an upstream stream
// reset). Surfaced on the record, in the in-band SSE error, and in the
// non-streaming error body.
const cursorVoidMsg = "cursor returned an empty turn for a resume-action continuation (0 output tokens, finish stop) - there was nothing to resume upstream"

func cursorVoidTurn(resume bool, r providerformat.TurnResult) bool {
	return resume && r.Outcome == providerformat.TurnFinished && r.Output == 0 && !r.ClientAbort
}

func stampCursorVoid(rec *metrics.Record) {
	rec.ErrorType = "empty_turn"
	rec.ErrorCode = "empty_turn"
	rec.ErrorMsg = cursorVoidMsg
}

// absorbCursorVoid logs a recovered void re-ask onto Attempts without
// stamping the live record (a later successful re-ask must stay a non-error).
func (s *Server) absorbCursorVoid(rec *metrics.Record) {
	rec.Attempts = append(rec.Attempts, metrics.RetryAttempt{
		StatusCode: rec.StatusCode,
		ErrorType:  "empty_turn",
		ErrorMsg:   cursorVoidMsg,
		At:         time.Now(),
	})
	rec.Retries++
	s.publishUpdate(rec)
}

// finishRunTurn renders the outcome (finish chunk + usage + [DONE], or an
// in-band error) and parks or closes the run accordingly. When Cursor's exact
// usage summary hasn't arrived yet (a parked turn ends before the summary
// lands), the input side falls back to the request-payload estimate so a turn
// never reports 0 input tokens. resumeRequest marks an empty-resume-action
// continuation; when such a turn finishes with 0 output tokens (the void) the
// outcome is surfaced as an error to both the client and the dashboard instead
// of a silently empty success.
func (s *Server) finishRunTurn(w http.ResponseWriter, run *providerformat.CursorRun, result providerformat.TurnResult, emit func(map[string]any, any) error, rec *metrics.Record, id string, rr cursorTurnRender, resumeRequest bool) {
	applyCursorUsage(rec, &result, rr.est)
	// A failed post-commit write here means the LOCAL client disconnected
	// mid-turn. Cursor's documented exception: the committed upstream 200
	// stays the record's status - mark the disconnect flag only, never
	// markClientGone's 499 (that is the passthrough paths' contract).
	markGone := func(err error) {
		if err != nil {
			rec.ClientDisconnected = true
		}
	}
	recordCursorTools(rec, result.ToolCalls)
	switch result.Outcome {
	case providerformat.TurnErrored:
		s.cursorRuns.drop(run)
		run.Close()
		if result.ClientAbort {
			// Writing the turn's SSE to the LOCAL client failed (broken pipe:
			// the client disconnected mid-stream). This is the client's own
			// cancellation, NOT an upstream failure: mark it client-disconnected
			// (upstream status, typically 200, stays the status) and stop -
			// no error stamp, no error SSE to the dead socket.
			rec.ClientDisconnected = true
			return
		}
		rec.ErrorType = "stream_read_error"
		rec.ErrorMsg = fmt.Sprint(result.Err)
		markGone(emitErrorSSE(w, id, "stream_read_error", fmt.Sprint(result.Err)))
		return

	case providerformat.TurnParked:
		// Park the run so a later request resumes it with the tool result.
		// (park() fails at capacity → close and let the client cold-start.)
		if !s.cursorRuns.park(rr.scope, run) {
			run.Close()
			log.Printf("cursor-run: park FAILED (capacity) - run closed, client will cold-start")
		} else {
			log.Printf("cursor-run: PARKED %d calls (%s…)", len(result.ToolCalls), shortID(result.ToolCalls[len(result.ToolCalls)-1].CallID))
		}
		rec.FinishReason = "tool_calls"
		prompt := rec.Usage.InputTokens
		eErr := emit(map[string]any{}, "tool_calls")
		markGone(eErr)
		markGone(usageChunk(w, id, rr.model, rr.includeUsage, prompt, result.Output, result.Reasoning))
		_, dErr := io.WriteString(w, "data: [DONE]\n\n")
		markGone(dErr)
		return

	default: // TurnFinished
		s.cursorRuns.drop(run)
		run.Close()
		rec.FinishReason = "stop"
		prompt := rec.Usage.InputTokens
		if cursorVoidTurn(resumeRequest, result) {
			// The void: an empty resume against a conversation the server is not
			// holding. Flag the record (dashboard error row + error dimension)
			// and surface a real failure in-band instead of an empty stop.
			stampCursorVoid(rec)
			obj := map[string]any{"error": map[string]any{
				"message": cursorVoidMsg, "type": "upstream_error", "param": nil, "code": "empty_turn",
			}}
			if b, mErr := json.Marshal(obj); mErr == nil {
				_, wErr := fmt.Fprintf(w, "data: %s\n\n", b)
				markGone(wErr)
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			_, dErr := io.WriteString(w, "data: [DONE]\n\n")
			markGone(dErr)
			return
		}
		eErr := emit(map[string]any{}, "stop")
		markGone(eErr)
		markGone(usageChunk(w, id, rr.model, rr.includeUsage, prompt, result.Output, result.Reasoning))
		_, dErr := io.WriteString(w, "data: [DONE]\n\n")
		markGone(dErr)
		return
	}
}

// usageChunk emits the final usage chunk (OpenAI stream_options shape).
// The returned error is the client-write failure, if any: a cursor stream's
// committed upstream 200 stays the status (the documented exception), so the
// caller - which owns the record - marks the disconnect itself.
func usageChunk(w http.ResponseWriter, id, model string, includeUsage bool, prompt, output, reasoning int64) error {
	if !includeUsage {
		return nil
	}
	counts, err := cursorUsage(prompt, output, reasoning)
	if err != nil {
		return err
	}
	usage := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": output,
		"total_tokens":      counts.TotalTokens,
	}
	if reasoning > 0 {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": reasoning}
	}
	obj := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(),
		"model": model, "choices": []map[string]any{}, "usage": usage,
	}
	if b, err := json.Marshal(obj); err == nil {
		if _, werr := fmt.Fprintf(w, "data: %s\n\n", b); werr != nil {
			return werr
		}
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// serveRunJSON drives a fresh run's turn and writes a single chat.completion.
// Mirrors serveRunStream's re-ask (nothing was written yet, so the retry is
// fully transparent - the client sees only the re-ask's completion).
func (s *Server) serveRunJSON(ctx context.Context, w http.ResponseWriter, run *providerformat.CursorRun, id string, rec *metrics.Record, rr cursorTurnRender, resumeRequest bool, reask func() (*providerformat.CursorRun, error)) {
	var content strings.Builder
	var toolCalls []map[string]any
	accumulate := func(delta map[string]any) error {
		cursorTrackDelta(rec, delta)
		accumulateJSONDelta(&content, &toolCalls, delta)
		return nil
	}
	result := run.RunTurn(ctx, accumulate)
	if cursorVoidTurn(resumeRequest, result) && reask != nil {
		s.cursorRuns.drop(run)
		run.Close()
		s.absorbCursorVoid(rec)
		next, err := reask()
		if err == nil && next != nil {
			rec.FirstTokenAt = time.Time{}
			rec.LastTokenAt = time.Time{}
			rec.Usage = metrics.Usage{}
			content.Reset()
			toolCalls = toolCalls[:0]
			result = next.RunTurn(ctx, accumulate)
			s.writeRunJSON(w, next, result, content.String(), toolCalls, rec, id, rr, true)
			return
		}
	}
	s.writeRunJSON(w, run, result, content.String(), toolCalls, rec, id, rr, resumeRequest)
}

// serveResumeJSON resumes a parked run and writes a single chat.completion.
func (s *Server) serveResumeJSON(ctx context.Context, w http.ResponseWriter, run *providerformat.CursorRun, results []cursorToolResult, steerText string, id string, rec *metrics.Record, rr cursorTurnRender) {
	var content strings.Builder
	var toolCalls []map[string]any
	m := map[string]struct {
		Text    string
		IsError bool
	}{}
	for _, tr := range results {
		m[tr.toolCallID] = struct {
			Text    string
			IsError bool
		}{tr.content, false}
	}
	result := run.ResumeTurn(ctx, m, steerText, func(delta map[string]any) error {
		cursorTrackDelta(rec, delta)
		accumulateJSONDelta(&content, &toolCalls, delta)
		return nil
	})
	s.writeRunJSON(w, run, result, content.String(), toolCalls, rec, id, rr, false)
}

// emitHTTPError writes an HTTP error body with http.Error's exact observable
// bytes - delete a stale Content-Length, Content-Type text/plain, nosniff, the
// status line, then body + "\n" - but returns the client-write error
// http.Error discards. A failed write means the LOCAL client disconnected
// before the error body was delivered; the caller owns the record and marks
// the disconnect per its path's contract.
func emitHTTPError(w http.ResponseWriter, body string, code int) error {
	h := w.Header()
	h.Del("Content-Length")
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, err := fmt.Fprintln(w, body)
	return err
}

// writeRunJSON renders a non-streaming chat.completion from a turn result.
func (s *Server) writeRunJSON(w http.ResponseWriter, run *providerformat.CursorRun, result providerformat.TurnResult, content string, toolCalls []map[string]any, rec *metrics.Record, id string, rr cursorTurnRender, resumeRequest bool) {
	applyCursorUsage(rec, &result, rr.est)
	// A failed post-commit write here means the LOCAL client disconnected
	// before the completion - or the error body below - was delivered.
	// Cursor's documented exception (the same contract as finishRunTurn and
	// the success write at the bottom): the committed upstream 200 stays the
	// record's status - mark the disconnect flag only, never markClientGone's
	// 499 (writeRunJSON only runs past a committed 2xx upstream handshake;
	// a 4xx/5xx Run returns before the turn is ever driven).
	markGone := func(err error) {
		if err != nil {
			rec.ClientDisconnected = true
		}
	}
	recordCursorTools(rec, result.ToolCalls)
	if result.Outcome == providerformat.TurnErrored {
		s.cursorRuns.drop(run)
		run.Close()
		if result.ClientAbort {
			// Same client-cancellation classification as finishRunTurn: not an
			// upstream failure, no 502 to the dead socket.
			rec.ClientDisconnected = true
			return
		}
		rec.ErrorType = "transform_error"
		rec.ErrorMsg = fmt.Sprint(result.Err)
		markGone(emitHTTPError(w, errJSON("api_error", fmt.Sprint(result.Err)), http.StatusBadGateway))
		return
	}
	if result.Outcome == providerformat.TurnParked && !s.cursorRuns.park(rr.scope, run) {
		run.Close()
	} else if result.Outcome == providerformat.TurnFinished {
		s.cursorRuns.drop(run)
		run.Close()
	}

	if cursorVoidTurn(resumeRequest, result) {
		// The void (see finishRunTurn): a non-streaming response has not been
		// written yet, so surface it with a real HTTP error status + body.
		stampCursorVoid(rec)
		rec.FinishReason = "stop"
		markGone(emitHTTPError(w, errJSON("empty_turn", cursorVoidMsg), http.StatusBadGateway))
		return
	}

	finish := "stop"
	if result.Outcome == providerformat.TurnParked {
		finish = "tool_calls"
	}
	rec.FinishReason = finish
	message := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	out := map[string]any{
		"id": id, "object": "chat.completion", "created": time.Now().Unix(),
		"model":   rr.model,
		"choices": []map[string]any{{"index": 0, "message": message, "finish_reason": finish}},
		"usage": map[string]any{
			"prompt_tokens":     rec.Usage.InputTokens,
			"completion_tokens": rec.Usage.OutputTokens,
			"total_tokens":      rec.Usage.TotalTokens,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	// A failed write here means the LOCAL client disconnected before the
	// completion was delivered (markGone, above: flag only, upstream 200 stays).
	markGone(json.NewEncoder(w).Encode(out))
}

// cursorUsage owns checked totals for both record and wire rendering. The
// protocol reports no cache split, so only reported input/output/reasoning
// fields are populated; no counts are invented to fit the numeric range.
func cursorUsage(prompt, output, reasoning int64) (metrics.Usage, error) {
	total, err := metrics.SumCounts(prompt, output)
	if err != nil {
		return metrics.Usage{}, err
	}
	if _, err := metrics.SumCounts(reasoning); err != nil {
		return metrics.Usage{}, err
	}
	return metrics.Usage{InputTokens: prompt, OutputTokens: output, TotalTokens: total, ReasoningTokens: reasoning}, nil
}

// applyCursorUsage validates before either renderer can park or emit success.
// A rejected proto count invalidates the whole sample, including any earlier
// partial count, and the existing error path closes the run.
func applyCursorUsage(rec *metrics.Record, result *providerformat.TurnResult, estimate int64) {
	if errors.Is(result.Err, metrics.ErrMetricRange) {
		invalidateUsage(rec, result.Err)
		result.Outcome = providerformat.TurnErrored
		return
	}
	prompt := result.Prompt
	if prompt == 0 || result.Outcome == providerformat.TurnErrored {
		prompt = estimate
	}
	usage, err := cursorUsage(prompt, result.Output, result.Reasoning)
	if err != nil {
		invalidateUsage(rec, err)
		result.Outcome, result.Err = providerformat.TurnErrored, err
		return
	}
	rec.Usage = usage
}

// serveCursorModels lists the account's usable Cursor models (agent.v1
// GetUsableModels, a unary Connect RPC with a raw application/proto body) and
// renders them in the OpenAI GET /v1/models shape. This lets OpenAI-compatible
// clients discover the usable model ids natively instead of hardcoding them.
func (s *Server) serveCursorModels(d *modelsDiscovery, w http.ResponseWriter, r *http.Request, t *target, key string) {
	targetURL := t.baseURL + "/agent.v1.AgentService/GetUsableModels"
	req, err := http.NewRequestWithContext(d.ctx, http.MethodPost, targetURL,
		bytes.NewReader(providerformat.EncodeGetUsableModelsRequest(nil)))
	if err != nil {
		http.Error(w, errJSON("api_error", err.Error()), http.StatusBadGateway)
		return
	}
	// Unary call: the shared client is fine (it negotiates h2 via ALPN for
	// https upstreams; h2c test servers go through the cursor transport's
	// AllowHTTP dial - but the shared client's UnencryptedHTTP2 also covers it).
	body, err := s.fetchModelsResponse(d, req, t, key)
	if err != nil {
		// Metadata calls never create metrics records.
		writeModelsError(w, err)
		return
	}
	models, err := providerformat.ParseGetUsableModelsResponse(body)
	if err != nil {
		http.Error(w, errJSON("api_error", "decode models: "+err.Error()), http.StatusBadGateway)
		return
	}
	s.emitModelsList(d, w, r, t, key,
		providerformat.CursorModelEntries(models, s.cfg().CursorDefaultContextWindow))
}

// truncateForErr bounds an upstream error body echoed into a 502 message.
func truncateForErr(b []byte) string {
	s := string(b)
	if len(s) > errEchoMax {
		s = s[:errEchoMax] + "…"
	}
	return s
}
