package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LLM-4-People/millivolt/internal/config"
	providerformat "github.com/LLM-4-People/millivolt/internal/format"
	"github.com/LLM-4-People/millivolt/internal/metrics"
	"github.com/LLM-4-People/millivolt/internal/scheduler"
)

// Routing header names. The app describes the upstream per request; nothing
// about providers is persisted in the proxy.
const (
	hdrBaseURL        = "X-Proxy-Base-URL"
	hdrAuthHeader     = "X-Proxy-Auth-Header"
	hdrAuthPrefix     = "X-Proxy-Auth-Prefix"
	hdrPath           = "X-Proxy-Path"
	hdrQuery          = "X-Proxy-Query"
	hdrHeaders        = "X-Proxy-Headers"
	hdrKey            = "X-Proxy-Key"
	hdrRefreshToken   = "X-Proxy-Refresh-Token"
	hdrTimeout        = "X-Proxy-Timeout-Ms"
	hdrFormat         = "X-Proxy-Format"
	hdrMaxConcurrency = "X-Proxy-Max-Concurrency"
	// hdrSession optionally pins a request to an explicit conversation/session
	// id supplied by the client. When absent, the proxy auto-groups requests
	// into conversations (see conversation.go).
	hdrSession = "X-Proxy-Session"
	// hdrParentSession declares a direct parent, never a previous turn.
	hdrParentSession = "X-Proxy-Parent-Session"
	// hdrClient optionally names the calling app for the dashboard's client
	// dimension (falls back to x-stainless-*/User-Agent sniffing).
	hdrClient = "X-Proxy-Client"
)

const (
	// maxErrBodyBytes bounds how much of an upstream error body is read to
	// capture the provider's error detail for metrics; the body itself is
	// still relayed to the client in full. Safety guardrail, not user-tunable.
	maxErrBodyBytes = 64 * 1024
	// sseScannerBufInit / sseScannerBufMax bound one SSE event line when
	// scanning a pre-translated stream (initial buffer / hard cap). Safety
	// guardrail, not user-tunable.
	sseScannerBufInit = 64 * 1024
	sseScannerBufMax  = 1024 * 1024
	// nonStreamCaptureMax bounds the prefix of a non-streaming response body
	// kept for usage/cost parsing. Documents exceeding this capture can have
	// unavailable accounting; their original bytes are still relayed in full.
	// Safety guardrail, not user-tunable.
	nonStreamCaptureMax = 1 << 20 // 1 MiB
	// nonStreamGrowInit / nonStreamChunk size the incremental non-stream
	// relay buffer (initial capture prefix / read chunk). Guardrails, not tunables.
	nonStreamGrowInit = 64 * 1024
	nonStreamChunk    = 32 * 1024
	// maxResponseHeaderBytes is the abusive-upstream header cap on the
	// outbound client. Safety guardrail, not user-tunable.
	maxResponseHeaderBytes = 1 << 20 // 1 MiB
	// qualitySpoolMax bounds the pre-write spool of a non-streaming 200 kept
	// for degenerate-outcome verification. Degenerate bodies are tiny (empty
	// JSON shells); a body past this cap is clearly not degenerate and is
	// committed to verbatim relay. Safety guardrail, not user-tunable.
	qualitySpoolMax = 1 << 20 // 1 MiB
	// terminalHoldMax bounds the bytes withheld at the end of a relayed SSE
	// stream (from the first terminal-candidate event onward) while the relay
	// decides between verbatim release and substituting an in-band degenerate
	// error. Past the cap the hold is abandoned: no terminal region this large
	// belongs to a degenerate stream. Safety guardrail, not user-tunable.
	terminalHoldMax = 64 * 1024
)

// nonRetryableQuotaClasses are provider error type/code strings that, when
// delivered with HTTP 429, denote a DURABLE account/billing condition - quota
// or credits exhausted, spend/hard limits, expired plans. Waiting cannot clear
// them, so the proxy surfaces the response immediately instead of burning its
// retry budget (and pacing the whole provider+key group) on a retry loop.
// Transient rate-limit 429s keep their normal retry/backoff behavior. Exact
// match (after lowercasing) against the canonical envelope parser's type AND
// code fields, never message text: OpenAI insufficient_quota / *_limit_reached
// family, Kimi exceeded_current_quota_error, and Z.AI's numeric business
// codes (1113 balance exhausted, 1309/1311/1314/1315 plan/package limits).
var nonRetryableQuotaClasses = map[string]bool{
	"insufficient_quota":                true,
	"insufficient_credits":              true,
	"credit_balance_exhausted":          true,
	"organization_spend_limit_exceeded": true,
	"project_spend_limit_exceeded":      true,
	"organization_usage_limit_exceeded": true,
	"billing_hard_limit_reached":        true,
	"billing_not_active":                true,
	"exceeded_current_quota_error":      true,
	"1113":                              true,
	"1309":                              true,
	"1311":                              true,
	"1314":                              true,
	"1315":                              true,
}

// isNonRetryableQuotaErr reports whether a 429's structured error type or code
// is a durable account/billing condition that a retry can never clear.
func isNonRetryableQuotaErr(typ, code string) bool {
	return nonRetryableQuotaClasses[strings.ToLower(typ)] || nonRetryableQuotaClasses[strings.ToLower(code)]
}

// target is the resolved upstream destination for one request.
type target struct {
	baseURL     string
	authHeader  string
	authPrefix  string
	path        string
	query       string
	provider    string
	timeout     time.Duration
	format      string
	extraHeader http.Header
}

// livePublisher is the narrow interface for emitting per-request lifecycle
// events (begin/end) to live SSE subscribers. The metrics.Buffer implements it;
// the durable store does NOT (it must persist exactly one finalized record per
// request, so lifecycle events bypass it entirely).
type livePublisher interface {
	PublishLive(phase string, r *metrics.Record)
}

// publishUpdate fans out a mid-flight "update" lifecycle event so the dashboard
// shows progress on an in-flight request (currently: each absorbed retry
// attempt) as it happens, not only after the request completes. Ephemeral - it
// never reaches the ring buffer or the durable store, and the in-flight gauge is
// untouched (the request is still inside its begin→end window). No-op when the
// recorder doesn't publish live events.
func (s *Server) publishUpdate(rec *metrics.Record) {
	if lp, ok := s.rec.(livePublisher); ok {
		lp.PublishLive("update", rec)
	}
}

// Server is the transparent reverse proxy. A single Server shares one
// http.Transport so upstream connections are pooled across all requests.
type Server struct {
	// cfgSnap is an atomic config snapshot (atomic.Pointer[config.Config]) so a
	// config reload never races a per-request read. Each snapshot is an immutable
	// deep copy; reload swaps in a fresh one. Read it via s.cfg().
	cfgSnap atomic.Pointer[config.Config]
	// client is the swappable upstream HTTP client (atomic.Pointer[http.Client]).
	// A reload builds a fresh Transport (new pool, new header timeout) and swaps
	// this pointer; in-flight requests keep the client they started with, so no
	// stream is ever dropped. Read it via s.httpClient().
	client atomic.Pointer[http.Client]
	// cursorClient is a dedicated HTTP/2 full-duplex client for Cursor's
	// bidirectional agent.v1 Run RPC. It's separate from the shared client
	// because the bidi exchange needs an h2 (or h2c) transport that supports
	// writing the request body while reading the streaming response; the shared
	// transport is HTTP/1.1-keep-alive oriented. Built lazily on first cursor
	// request (cursorClientFor), guarded by cursorClientMu.
	cursorClient   *http.Client
	cursorClientMu sync.Mutex
	// cursorRuns holds live, resumable Cursor Run streams keyed so a follow-up
	// request carrying a tool result can find the parked conversation and write
	// the result into the same open upstream stream (mimicking Cursor's agent
	// loop instead of a fresh handshake per tool turn). In-memory only - a
	// parked HTTP/2 stream can't survive a restart anyway.
	cursorRuns *cursorRunStore
	rec        metrics.Recorder
	scheduler  *scheduler.Scheduler
	convos     *ConversationTracker
	pause      pauseRuntime
	throttle   throttleRuntime
	debug      debugRuntime
	// ModelObserver is wired before serving. Debug state uses the same Go
	// canonical-name metadata provider as all other dashboard observers.
	ModelObserver func([]string) any
}

// cfg returns the current config snapshot. Cheap (atomic load); safe to call
// on the hot path. The returned *config.Config is immutable - never mutate it.
func (s *Server) cfg() *config.Config { return s.cfgSnap.Load() }

// storeQueryCtx bounds a durable-store round trip with StorageQueryTimeout
// (the single owner - pause/throttle persist must not invent 5s/2s).
func (s *Server) storeQueryCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.cfg().StorageQueryTimeout)
}

// rejectUnlessGetPost is the RFC 9110 method gate for dashboard JSON
// that is GET+POST (pause, throttle). Mux patterns stay unmethoded so a
// wrong method cannot fall through to the LLM proxy.
func rejectUnlessGetPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodPost {
		return true
	}
	w.Header().Set("Allow", "GET, POST")
	http.Error(w, `{"error":"GET or POST"}`, http.StatusMethodNotAllowed)
	return false
}

// httpClient returns the current upstream client. Each request captures it once
// so an in-flight stream keeps the transport it started on even across a reload.
func (s *Server) httpClient() *http.Client { return s.client.Load() }

// New builds a Server. rec may be nil (uses a Noop recorder). All tunables
// come from cfg (see internal/config); nothing is hardcoded here.
func New(cfg *config.Config, rec metrics.Recorder) *Server {
	if rec == nil {
		rec = metrics.Noop{}
	}
	s := &Server{
		rec:        rec,
		convos:     NewConversationTracker(cfg.ConversationIdleGap, cfg.ConversationMaxOpen),
		cursorRuns: newCursorRunStore(cfg.CursorParkTTL),
		scheduler:  scheduler.New(schedulerOptions(cfg)),
	}
	s.initPause()
	s.cfgSnap.Store(cfg)
	s.client.Store(newUpstreamClient(cfg))
	return s
}

// newUpstreamClient builds the upstream HTTP client/transport from cfg. A
// reload builds a fresh one so pool sizes and the header timeout hot-apply to
// new connections without disturbing in-flight streams.
func newUpstreamClient(cfg *config.Config) *http.Client {
	tr := &http.Transport{
		MaxIdleConns:        cfg.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
		// Long-lived SSE streams hold a connection for the whole generation;
		// size the pool for concurrent streams, not requests/sec.
		MaxConnsPerHost:        cfg.MaxConnsPerHost,
		IdleConnTimeout:        cfg.IdleConnTimeout,
		ResponseHeaderTimeout:  cfg.UpstreamTimeout, // 0 = no header-arrival cap
		MaxResponseHeaderBytes: maxResponseHeaderBytes,
		// Advertise both HTTP/1.1 and HTTP/2. Declaring the set explicitly (not
		// relying on the implicit default) keeps h2 available even if a custom
		// DialContext/TLSClientConfig is ever added - the conservative-disable
		// heuristic (ForceAttemptHTTP2) only governs the nil-Protocols path.
		// HTTPS upstreams that speak h2 (e.g. Cursor's Connect-RPC agent.v1,
		// which REQUIRES h2 for server-streaming) negotiate it via ALPN;
		// everything else keeps HTTP/1.1 keep-alive, the reliable SSE path.
		// UnencryptedHTTP2 (h2c prior-knowledge) lets plaintext http:// test
		// servers and h2c gateways also use h2.
		Protocols: func() *http.Protocols {
			p := new(http.Protocols)
			p.SetHTTP1(true)
			p.SetHTTP2(true)
			p.SetUnencryptedHTTP2(true)
			return p
		}(),
		// Compression left enabled: Go auto-decompresses gzip upstream bodies so
		// the SSE analyzer sees plaintext. We strip Content-Encoding downstream
		// so clients get readable bytes.
	}
	return &http.Client{Transport: tr, CheckRedirect: preserveUpstreamRedirect}
}

// A redirect is an upstream response, not authority to send this request and
// its credentials to another destination. All outbound clients share this
// policy, including constructed metadata and token-exchange requests.
func preserveUpstreamRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// Reload hot-applies a freshly-loaded config with ZERO dropped requests: the
// listener and HTTP server are never touched, so no connection can be dropped.
// It swaps the config snapshot (per-request reads pick it up atomically),
// rebuilds the upstream transport (new pool sizes + header timeout apply to new
// connections; in-flight streams keep the transport they started on), and
// re-applies the scheduler/conversation tunables. Fields that are bound at
// process startup and cannot move (listen address, db path, storage pipeline,
// HTTP server timeouts, history size) are NOT re-applied - they require a
// restart and are the caller's responsibility to flag.
func (s *Server) Reload(cfg *config.Config) {
	// Swap the config snapshot first so subsequent reads see it.
	s.cfgSnap.Store(cfg)
	// Swap the upstream transport for new requests; drain the old one's idle
	// connections (in-flight streams on the old client are unaffected and finish
	// normally - CloseIdleConnections never touches active connections).
	old := s.client.Swap(newUpstreamClient(cfg))
	if old != nil {
		if tr, ok := old.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
	// Re-apply the live-tunable scheduler + conversation settings.
	s.scheduler.UpdateOptions(schedulerOptions(cfg))
	s.convos.UpdateLimits(cfg.ConversationIdleGap, cfg.ConversationMaxOpen)
	s.cursorRuns.UpdateTTL(cfg.CursorParkTTL)
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	t, err := resolveTarget(r, s.cfg())
	if err != nil {
		http.Error(w, errJSON("invalid_request_error", err.Error()), http.StatusBadRequest)
		return
	}
	session, parentSession, err := requestConversation(r.Header)
	if err != nil {
		http.Error(w, errJSON("invalid_request_error", err.Error()), http.StatusBadRequest)
		return
	}

	// Model discovery: a GET to the OpenAI models endpoint is handled per
	// upstream format - passthrough for OpenAI-compatible providers, translated
	// for Anthropic, and served from Cursor's GetUsableModels for cursor - so any
	// provider's models are listable through the standard OpenAI route.
	if err := s.applyThrottleHeaders(r, t.provider, classifyClient(r)); err != nil {
		http.Error(w, errJSON("invalid_request_error", err.Error()), http.StatusBadRequest)
		return
	}

	if r.Method == http.MethodGet && (r.URL.Path == "/v1/models" || r.URL.Path == "/models") {
		// Model discovery keeps its separate transparent-refresh contract for
		// every mechanism: use the refreshed key without returning token headers.
		s.serveModels(w, r, t, extractKey(r))
		return
	}

	body, rec, err := readRequest(r, s.cfg().MaxRequestBytes, s.cfg().CaptureBodyPreview)
	if err != nil {
		status := http.StatusBadRequest
		var overflow *http.MaxBytesError
		if errors.As(err, &overflow) {
			status = http.StatusRequestEntityTooLarge
		}
		http.Error(w, errJSON("invalid_request_error", err.Error()), status)
		return
	}
	stream, model := rec.Stream, rec.Model

	key, ok := s.resolveKey(w, r, t, extractKey(r))
	if !ok {
		return // handback response already written; nothing proxied
	}

	// Optional format translation (e.g. OpenAI -> Anthropic). The default is
	// byte-transparent passthrough. Cursor is excluded here: its Run RPC is
	// bidirectional, so serveCursorBidi translates the body itself into the
	// enveloped run_request (and keeps the history blobs for the KV channel).
	if t.format != "" && t.format != "openai" && t.format != "cursor" {
		translated, err := translateRequest(t.format, body, s.cfg().AnthropicDefaultMaxTokens)
		if err != nil {
			http.Error(w, errJSON("invalid_request_error", err.Error()), http.StatusBadRequest)
			return
		}
		body = translated
		// Parameter/composition metrics describe the translated upstream body,
		// while routing keeps the client's original model and stream choice.
		rec = &metrics.Record{Start: rec.Start}
		parseLLMRequest(body, rec, s.cfg().CaptureBodyPreview)
	}

	// Cursor clients send FUSED display model ids (thinking level + tier baked
	// into the name). Record the canonical base id instead so the dashboard
	// groups a Cursor model with the same model from other providers; the
	// upstream wire encoding still decomposes the raw id itself.
	recModel := model
	if t.format == "cursor" {
		recModel = providerformat.CursorModelBase(model)
	}

	rec.ID = requestID()
	rec.Provider = t.provider
	rec.Model = recModel
	rec.KeyHash = hashKey(key)
	rec.UserAgent = r.UserAgent()
	rec.Client = classifyClient(r)
	rec.ClientIP = remoteIP(r)
	rec.Path = r.URL.Path
	rec.Method = r.Method
	rec.ClientLang = clientLang(r)
	rec.Stream = stream
	fillClientMeta(r, t, rec)
	// Group into a conversation: explicit X-Proxy-Session wins; otherwise
	// auto-group by turn-count monotonicity within the client+key partition.
	{
		totalTurns := rec.TurnsUser + rec.TurnsAssistant + rec.TurnsTool
		rec.ConversationID = s.convos.Assign(rec.Client, rec.KeyHash,
			session, totalTurns, rec.Start)
		if parentSession != "" {
			rec.ParentConversationID = explicitConversationID(parentSession)
		}
	}
	// Announce the request the moment it's accepted so the dashboard shows a
	// live "streaming" row immediately (not only after it completes). This goes
	// ONLY to live SSE subscribers - the durable store sees just the finalized
	// record below, so it still persists exactly one row per request.
	if s.scheduler.HoldsTarget(rec.Client, rec.Provider) {
		rec.Paused = true
	}
	debugTap := s.beginDebugTap(r, t, rec, body)
	if lp, ok := s.rec.(livePublisher); ok {
		lp.PublishLive("begin", rec)
	}
	s.noteClient(rec.Client)
	s.noteProvider(rec.Provider)
	s.noteModel(rec.Model)
	defer func() {
		rec.End = time.Now()
		metrics.FinalizeRecord(rec)
		s.finishDebugTap(debugTap, rec)
		// The live recorder finalizes pending state and emits end atomically
		// with this record, under the same boundary used by log deletion.
		s.rec.Record(rec)
	}()

	state := &stormRequestState{rec: rec}
	ctx := context.WithValue(r.Context(), stormContextKey{}, state)
	defer func() {
		s.finishStormResponse(ctx, false)
	}()

	// Transparent rate-limit queueing: acquire a slot in the provider+key
	// group (FIFO order, concurrency cap, pacing after 429). The client never
	// sees a rate-limit failure unless the queue is full or the wait exceeds
	// the cap. X-Proxy-Timeout-Ms is applied AFTER Acquire: it is the max
	// time to wait for upstream response headers (README), never the hold -
	// and it is anchored as a FRESH deadline at each upstream send inside
	// doWithRetry / serveCursorBidi, so holds, queue waits, backoff, and
	// Retry-After windows never burn the budget.
	groupKey := t.provider + "|" + rec.KeyHash
	maxConc := s.cfg().MaxConcurrent
	if v := r.Header.Get(hdrMaxConcurrency); v != "" {
		// Deny by default: a malformed routing header is rejected, never
		// silently defaulted (same contract as X-Proxy-Timeout-Ms).
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, errJSON("invalid_request_error",
				fmt.Sprintf("invalid %s %q", hdrMaxConcurrency, v)), http.StatusBadRequest)
			return
		}
		maxConc = n
	}
	pacer := newSSEPacer(w, s.cfg().SSEKeepaliveInterval)
	if stream {
		w = pacer
		pacer.Arm()
	}
	if debugTap != nil {
		w = debugTap.wrapWriter(w)
	}
	defer pacer.Stop()
	hooks := scheduler.WaiterHooks{
		Client:    rec.Client,
		Provider:  rec.Provider,
		EstTokens: estimateTokens(rec),
		OnWait: func() {
			if stream {
				pacer.Start()
			}
		},
		OnHold: func() {
			rec.Paused = true
			s.publishUpdate(rec)
		},
		OnUnhold: func() {
			rec.Paused = false
			s.publishUpdate(rec)
		},
		OnThrottle: func() {
			rec.Throttled = true
			s.publishUpdate(rec)
		},
		OnUnthrottle: func() {
			rec.Throttled = false
			s.publishUpdate(rec)
		},
	}
	// Park storm work before per-key concurrency so an affected model does
	// not consume slots while healthy models could run. Cursor parked resumes
	// keep their existing stream; only its fresh-send owner takes this gate.
	if t.format != "cursor" {
		_, err = s.waitStormGate(ctx, hooks, true)
		if err != nil {
			if r.Context().Err() != nil {
				markClientGone(rec)
				return
			}
			s.writeStormQueueError(w, rec, err)
			return
		}
	}
	queueStart := time.Now()
	release, err := s.scheduler.AcquireWith(ctx, groupKey, maxConc, hooks)
	if err != nil {
		if r.Context().Err() == context.Canceled {
			markClientGone(rec)
			return
		}
		rec.ErrorType = "queue_full"
		rec.ErrorMsg = err.Error()
		extra := http.Header{}
		extra.Set("Retry-After", strconv.Itoa(s.cfg().QueueRetryAfter))
		writeClientErrorHdr(w, rec, "rate_limit_error", "proxy queue full or wait exceeded; retry later", http.StatusTooManyRequests, extra)
		return
	}
	rec.QueueWaitMs += time.Since(queueStart).Milliseconds()
	if rec.QueueWaitMs > 0 {
		rec.RateLimited = true
	}
	defer func() {
		release(settleTokens(rec))
	}()

	// Cursor's agent.v1 Run is a bidirectional HTTP/2 stream (the server writes
	// requests back on the same open stream and blocks for replies). That can't
	// ride the write-body-then-read-response doWithRetry path, so it gets a
	// dedicated full-duplex handler. Cursor is excluded from translateRequest
	// (which would error on format cursor); the bidi handler translates to the
	// enveloped run_request.
	if t.format == "cursor" {
		s.serveCursorBidi(ctx, w, r, t, key, model, body, stream, rec, groupKey, hooks)
		return
	}

	resp, cancelSend, err := s.doWithRetry(ctx, groupKey, r, t, key, body, rec, hooks, false)
	if err != nil {
		// Client cancel vs genuine transport failure: when the LOCAL client
		// disconnected (its context canceled) the upstream exchange died with
		// it - that is the client's own cancellation, recorded as 499
		// "client closed request" (never an error, matching the broken-pipe
		// stream classification). A nil request context (e.g. our own request
		// deadline) is a real proxy-side failure: 502 upstream_unreachable.
		if r.Context().Err() == context.Canceled {
			markClientGone(rec)
			return
		}
		if s.writeStormQueueError(w, rec, err) {
			return
		}
		writeClientError(w, rec, "upstream_unreachable", "upstream error: "+transportErrText(err), http.StatusBadGateway)
		return
	}
	// The per-send deadline's cancel is owned here: it must survive until the
	// body relay finishes (the deadline still governs the body phase, exactly
	// like the request-lifetime wrap it replaced). LIFO closes the body
	// first, then cancels.
	defer cancelSend()
	defer resp.Body.Close()

	rec.StatusCode = resp.StatusCode

	// Capture error detail from 4xx/5xx response bodies (read before streaming).
	if resp.StatusCode >= 400 {
		captureErrorFromResponse(resp, rec)
	}

	// Capture upstream response headers for the audit view.
	rec.ResponseHeaders = captureHeaders(resp.Header, t.authHeader)

	// Provider-side metadata: request id, server, processing time, actual model.
	rec.ProviderRequestID = firstNonEmpty(
		resp.Header.Get("X-Request-Id"),
		resp.Header.Get("X-Openai-Request-Id"),
		resp.Header.Get("X-Api-Request-Id"),
		resp.Header.Get("Request-Id"),
	)
	rec.ProviderServer = resp.Header.Get("Server")
	// Provider processing time (OpenAI- and Anthropic-style header names).
	headerIntInto(resp.Header, &rec.ProcessingMs, "X-Openai-Processing-Ms", "anthropic-processing-ms")
	// Provider-reported model (some providers echo the actual model).
	if v := resp.Header.Get("X-Model"); v != "" {
		rec.ProviderModel = v
	}

	// Rate-limit headers (common across OpenAI-compatible providers).
	headerIntInto(resp.Header, &rec.RateLimitRemaining,
		"X-Ratelimit-Remaining-Requests", "anthropic-ratelimit-requests-remaining")
	headerIntInto(resp.Header, &rec.RateLimitLimit,
		"X-Ratelimit-Limit-Requests", "anthropic-ratelimit-requests-limit")

	if stream || isEventStream(resp.Header.Get("Content-Type")) {
		// Streaming: the status line is committed immediately (the client is
		// already consuming events) and the terminal-region hold/substitution
		// happens inside streamBody, after any content already relayed.
		// The idle pacer may already have committed 200 SSE + comment
		// frames (queue / hold / TTFB); WriteHeader is then a no-op.
		// A 4xx/5xx body after that commit cannot change the status - emit
		// it in-band so the SDK sees a retryable error, not raw JSON on 200.
		if pacer.applyUpstream(resp.Header, resp.StatusCode) && resp.StatusCode >= 400 {
			typ, msg := rec.ErrorType, rec.ErrorMsg
			if typ == "" {
				typ = "api_error"
			}
			if msg == "" {
				msg = fmt.Sprintf("upstream HTTP %d", resp.StatusCode)
			}
			if err := emitErrorSSE(w, rec.ID, typ, msg); err != nil {
				// The in-band error hit a dead socket: the client is gone.
				// The decided error status stays on the record (markClientGone
				// stamps 499 only when no error outcome is decided).
				markClientGone(rec)
			}
			return
		}

		// If a format translation was requested, transform the response from
		// the upstream wire format back to OpenAI before relaying.
		if resp.StatusCode != http.StatusOK {
			s.serveNonStreaming(ctx, w, resp, r, t, key, body, rec, groupKey, hooks)
			return
		}
		if t.format != "" && t.format != "openai" {
			s.transformResponse(ctx, w, resp, rec, t.format, stream)
			return
		}

		s.streamBody(ctx, w, resp.Body, rec)
		return
	}

	// Non-streaming: the quality-aware relay owns the status line now - it
	// spools the body BEFORE writing anything so a degenerate 200 can be
	// transparently retried before any byte reaches the client (a client can
	// do nothing with a partial non-streaming body, so the spool costs it
	// nothing).
	s.serveNonStreaming(ctx, w, resp, r, t, key, body, rec, groupKey, hooks)
}
