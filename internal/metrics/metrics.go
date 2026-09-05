package metrics

import (
	"bytes"
	"encoding/json"
	"time"
	"unicode/utf8"
)

// Usage is the normalized token/cache accounting for a single request. Fields
// are populated from the provider's usage payload (OpenAI `usage` object or
// Anthropic `message_start`/`message_delta`) and fall back to a running
// content-chunk count when the stream is interrupted before the usage chunk
// arrives.
type Usage struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`
	CacheWrite      int64 `json:"cache_write_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// RetryAttempt is one transparently-absorbed upstream response - a 429 or 5xx
// the proxy retried before returning a final answer to the client. It carries
// the status and the provider's error detail so the proxy's audit log shows
// every failure even though the client only ever saw the successful outcome.
type RetryAttempt struct {
	StatusCode   int       `json:"status_code"`
	ErrorType    string    `json:"error_type,omitempty"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMsg     string    `json:"error_msg,omitempty"`
	RetryAfterMs int       `json:"retry_after_ms,omitempty"`
	At           time.Time `json:"at"`
}

// Record is the per-request observability record. Every field is captured at
// the proxy boundary and carries no request/response payload.
type Record struct {
	ID        string `json:"id"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	KeyHash   string `json:"key_hash"` // sha256 of the upstream key, never the key itself
	UserAgent string `json:"user_agent"`
	Client    string `json:"client"` // friendly app/SDK classification (e.g. "desktop-client", "openai-python")
	Stream    bool   `json:"stream"`

	// ConversationID groups requests that belong to the same conversation/task.
	// Set from X-Proxy-Session when the client provides one, else auto-derived
	// by the proxy from turn-count monotonicity (see conversation.go).
	ConversationID string `json:"conversation_id,omitempty"`
	// ParentConversationID is the client's explicit direct-parent declaration.
	// Relationships are resolved within Client+KeyHash, never by this ID alone.
	// Empty means no declaration; response continuity does not imply parentage.
	ParentConversationID string `json:"parent_conversation_id,omitempty"`

	// Client identification & connection metadata, captured automatically.
	ClientIP   string `json:"client_ip"`             // remote address (host part only)
	Path       string `json:"path"`                  // request path (e.g. /v1/chat/completions)
	Method     string `json:"method"`                // HTTP method
	ClientLang string `json:"client_lang,omitempty"` // x-stainless-lang / parsed from UA

	// ClientMeta is extra identity and routing config the client sent
	// (X-Stainless-* SDK headers, X-Proxy-Format / timeout / per-request
	// max-concurrency). Never keys, never message content. Empty is omitted.
	ClientMeta ClientMeta `json:"client_meta,omitempty"`

	// Provider-side metadata captured from the response (headers + body).
	ProviderRequestID string `json:"provider_request_id,omitempty"` // x-request-id / anthropic-request-id
	ProviderServer    string `json:"provider_server,omitempty"`     // Server header
	ProviderModel     string `json:"provider_model,omitempty"`      // model echoed by the provider (may differ from requested)
	ProcessingMs      int    `json:"processing_ms"`                 // provider-reported server processing time (x-openai-processing-ms / anthropic-...)
	RetryAfterMs      int    `json:"retry_after_ms,omitempty"`      // Retry-After hint on 429

	StatusCode int       `json:"status_code"`
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`

	// FirstTokenAt / LastTokenAt are the wall-clock times of the first and last
	// content-bearing token, used to derive TTFT and decode TPS.
	FirstTokenAt time.Time `json:"first_token_at,omitempty"`
	LastTokenAt  time.Time `json:"last_token_at,omitempty"`

	// FinalAttemptAt is when the upstream attempt that produced the returned
	// response began. On a request with transparent retries, TTFT is measured
	// from this point (the successful attempt) rather than from Start, so the
	// dashboard shows the provider's real responsiveness, not the retry
	// backlog. Zero when there were no retries (TTFT falls back to Start).
	FinalAttemptAt time.Time `json:"final_attempt_at,omitempty"`

	FinishReason       string   `json:"finish_reason,omitempty"`
	ErrorType          string   `json:"error_type,omitempty"`
	ErrorMsg           string   `json:"error_msg,omitempty"`
	ErrorCode          string   `json:"error_code,omitempty"`
	ToolCalls          int      `json:"tool_calls"`
	ToolNames          []string `json:"tool_names,omitempty"`
	Usage              Usage    `json:"usage"`
	ClientDisconnected bool     `json:"client_disconnected"`

	// Rate-limit headers captured from upstream responses.
	RateLimitRemaining int `json:"rate_limit_remaining,omitempty"`
	RateLimitLimit     int `json:"rate_limit_limit,omitempty"`

	// Paused is a live-only flag: the request is queued because of the
	// operator hold. Never persisted (the store insert list omits it).
	Paused bool `json:"paused,omitempty"`

	// Throttled is a live-only flag: the request is queued on a provider
	// concurrency / requests-per-window / tokens-per-window cap. Never persisted.
	Throttled bool `json:"throttled,omitempty"`

	// Debug is set when an operator debug session matched this request.
	// The full capture (bodies, headers) lives in the request_debug sidecar,
	// never on this record - the ring/SSE carry only this flag + session id.
	Debug          bool   `json:"debug,omitempty"`
	DebugSessionID string `json:"debug_session_id,omitempty"`

	// Queueing/scheduler metadata.
	QueueWaitMs int64 `json:"queue_wait_ms"`          // time spent in the scheduler queue before sending
	Retries     int   `json:"retries"`                // number of transparent upstream retries (429/5xx)
	RateLimited bool  `json:"rate_limited,omitempty"` // scheduler wait or 429/503 pacing; HasRateLimit owns the exact 429 predicate
	// Attempts records each transparently-retried upstream response (the ones
	// the client never saw) so the proxy logs the full history, not just the
	// final outcome. Bounded by max_retries.
	Attempts []RetryAttempt `json:"attempts,omitempty"`

	// Derived fields populated at record time for the dashboard/storage.
	TTFTMs     int64   `json:"ttft_ms"`
	OverallTPS float64 `json:"overall_tps"`
	DecodeTPS  float64 `json:"gen_tps"`
	DurationMs int64   `json:"duration_ms"`
	Cost       float64 `json:"cost,omitempty"`

	// Reasoning ("thinking") tokens captured separately. For reasoning models
	// (o-series, DeepSeek R1, Claude extended thinking), reasoning tokens are
	// invisible thinking; answer tokens are the visible response. We also track
	// the time when the first answer (non-reasoning) token arrives separately.
	AnswerTokens  int64     `json:"answer_tokens"` // completion_tokens - reasoning_tokens
	FirstAnswerAt time.Time `json:"first_answer_at,omitempty"`

	// HadAnswerContent records whether the response carried answer text content
	// (role:"assistant" content parts), independent of timing - set for BOTH
	// streaming (a content chunk arrived) and non-streaming (the JSON message
	// had non-empty content) paths. FinalizeRecord uses it to tell a
	// tool-call-ONLY response (no content → completion_tokens are all tool-call
	// arguments, not generation) from a mixed content+tool_calls response.
	HadAnswerContent bool `json:"had_answer_content,omitempty"`

	// GenTokens is the count of GENERATION tokens (reasoning + answer content)
	// with tool-call argument tokens excluded. It is the numerator for DecodeTPS
	// (gen_tps): providers count tool-call argument tokens in completion_tokens
	// (OpenAI does - a content:null tool_calls response still bills them), which
	// would otherwise inflate the generation rate. When the provider reports a
	// usage blob, GenTokens = reasoning + answer (== OutputTokens, since the
	// proxy has no tool-token sub-breakdown and tool args are only separable in
	// the chunk-count fallback); when it doesn't, GenTokens = content+reasoning
	// chunks (tool-arg chunks excluded). See Fill / FinalizeRecord.
	GenTokens int64 `json:"gen_tokens,omitempty"`

	// LLM request parameters parsed from the request body. These are the fields
	// that define the request shape; they carry no message content.
	ReqMaxTokens    *int     `json:"req_max_tokens,omitempty"`
	ReqTemperature  *float64 `json:"req_temperature,omitempty"`
	ReqTopP         *float64 `json:"req_top_p,omitempty"`
	ReqToolsCount   int      `json:"req_tools_count"`
	ReqToolChoice   string   `json:"req_tool_choice,omitempty"`
	ReqStreamOpts   bool     `json:"req_stream_opts,omitempty"`
	ReqN            *int     `json:"req_n,omitempty"`
	ReqStop         int      `json:"req_stop,omitempty"`     // number of stop sequences
	ReqLogprobs     bool     `json:"req_logprobs,omitempty"` // whether logprobs requested
	ReqPresencePen  *float64 `json:"req_presence_pen,omitempty"`
	ReqFrequencyPen *float64 `json:"req_frequency_pen,omitempty"`

	// Additional non-content request parameters, captured so the dashboard can
	// show the full request shape without storing any message content.
	ReqResponseFormat string `json:"req_response_format,omitempty"` // text / json_object / json_schema
	ReqSeed           *int64 `json:"req_seed,omitempty"`
	ReqParallelTools  *bool  `json:"req_parallel_tools,omitempty"`
	ReqLogitBias      int    `json:"req_logit_bias,omitempty"`    // number of biased token ids
	ReqTopLogprobs    *int   `json:"req_top_logprobs,omitempty"`  // top-logprobs count requested
	ReqServiceTier    string `json:"req_service_tier,omitempty"`  // auto / flex / priority ...
	ReqThinking       bool   `json:"req_thinking,omitempty"`      // anthropic extended-thinking requested
	ReqMetadataKeys   int    `json:"req_metadata_keys,omitempty"` // number of metadata entries
	// ReqReasoningEffort / ReqVerbosity are OpenAI request-config tokens
	// (reasoning_effort, or reasoning.effort; verbosity). Shape only.
	ReqReasoningEffort string `json:"req_reasoning_effort,omitempty"`
	ReqVerbosity       string `json:"req_verbosity,omitempty"`

	// Optional content previews. Populated only when capture_body_preview is
	// enabled; off by default to avoid storing prompts in metrics storage.
	// Both are capped at PreviewMaxBytes via TruncatePreview.
	PromptPreview   string `json:"prompt_preview,omitempty"`
	ResponsePreview string `json:"response_preview,omitempty"`

	// Turn breakdown parsed from the request body (0 for non-chat endpoints).
	TurnsUser      int `json:"turns_user"`
	TurnsAssistant int `json:"turns_assistant"`
	TurnsTool      int `json:"turns_tool"`

	// Prompt composition parsed from the request body. Chars* are per-role
	// text character counts (system = role:"system"/"developer" messages plus
	// Anthropic's top-level "system" field) - sizes only, never content.
	// Images/Attachments count multimodal content parts across all messages
	// (an attachment is any non-image, non-text part: file/document/audio/
	// video/embedded tool result). LastTurnRole is the role of the final
	// message - what the model is actually responding to.
	CharsSystem    int    `json:"chars_system"`
	CharsUser      int    `json:"chars_user"`
	CharsAssistant int    `json:"chars_assistant"`
	CharsTool      int    `json:"chars_tool"`
	Images         int    `json:"images"`
	Attachments    int    `json:"attachments"`
	LastTurnRole   string `json:"last_turn_role,omitempty"`

	// Upstream response headers captured at the proxy (empty for errors pre-headers).
	ResponseHeaders map[string][]string `json:"response_headers,omitempty"`
}

// ClientMeta is allowlisted client identity and proxy-routing config captured
// at the request boundary. All fields are optional; Empty reports a zero value.
type ClientMeta struct {
	OS         string `json:"os,omitempty"`
	Arch       string `json:"arch,omitempty"`
	Runtime    string `json:"runtime,omitempty"`
	RuntimeVer string `json:"runtime_ver,omitempty"`
	PkgVer     string `json:"pkg_ver,omitempty"`
	Format     string `json:"format,omitempty"`          // X-Proxy-Format
	TimeoutMs  int    `json:"timeout_ms,omitempty"`      // X-Proxy-Timeout-Ms
	MaxConc    int    `json:"max_concurrency,omitempty"` // X-Proxy-Max-Concurrency (per-request)
}

// Empty reports whether m carries any captured fact.
func (m ClientMeta) Empty() bool { return m == (ClientMeta{}) }

// OverallThroughput returns tokens per second computed from the full request
// lifetime (start → end). This is the stable, always-correct metric.
func (r *Record) OverallThroughput() float64 {
	d := r.End.Sub(r.Start)
	if d <= 0 || r.Usage.OutputTokens == 0 {
		return 0
	}
	return float64(r.Usage.OutputTokens) / d.Seconds()
}

// GenThroughput returns generation tokens per second over the generation
// window - from the FIRST token (reasoning or answer, whichever arrives first)
// to the LAST token. The numerator is GenTokens (reasoning + answer content,
// tool-call argument tokens excluded), so a tool-call-heavy request doesn't
// show an inflated rate. The window ends at the last token, not at End (which
// includes trailing usage/[DONE] handling), so it measures actual generation.
// Returns 0 when there were no generation tokens or the window is degenerate.
func (r *Record) GenThroughput() float64 {
	if r.FirstTokenAt.IsZero() || r.GenTokens == 0 {
		return 0
	}
	end := r.LastTokenAt
	if end.IsZero() || !end.After(r.FirstTokenAt) {
		// Single-token (or unmeasured last-token) responses have no measurable
		// generation window; fall back to the request end so the rate is still
		// meaningful rather than 0.
		end = r.End
	}
	d := end.Sub(r.FirstTokenAt)
	if d <= 0 {
		return 0
	}
	return float64(r.GenTokens) / d.Seconds()
}

// IsError is the single canonical definition of "this request saw an upstream
// failure". A request counts when the upstream produced a genuine failure -
// a 5xx response or a structured error - at ANY point: as the final outcome
// OR as an absorbed retry attempt (the upstream still failed, even if a later
// retry recovered). A 429 is flow control, not an error, and never counts -
// neither as the final status nor as an absorbed attempt; the same holds for
// 499: the LOCAL client closed the connection (its own cancellation - a relay
// write failed on broken pipe), which is recorded as ClientDisconnected, never
// as an upstream failure. (503 is retried as a rate limit but is still a
// server error, so it counts.)
//
// StatusClientClosedRequest is the status recorded when the LOCAL client
// disconnected before the proxy delivered a final response (nginx's "client
// closed request" convention). It is a flow event like 429: IsError treats it
// as a non-error (unless an absorbed 5xx attempt was seen), so a user's own
// cancellation never inflates the error rate.
const StatusClientClosedRequest = 499

// Use it everywhere instead of re-deriving the predicate so the live counter,
// the aggregates, and the dashboard never disagree. The purge SQL predicate
// (storage.store.go's HasError clause) must stay semantically identical to
// this function.
func (r *Record) IsError() bool {
	// A final 429 (rate limit) or 499 (local client closed the connection
	// before a final outcome) is flow control/own-abort, not an error - even
	// though the provider's error body may classify the 429 (ErrorType =
	// rate_limit_error/provider_error). Rate limiting is surfaced separately
	// through HasRateLimit; client disconnects via ClientDisconnected.
	if r.StatusCode == 429 || r.StatusCode == StatusClientClosedRequest {
		// Still honor an absorbed 5xx from an earlier attempt (a request that
		// saw a genuine server failure before the client gave up on it).
		for _, a := range r.Attempts {
			if a.StatusCode >= 500 {
				return true
			}
		}
		return false
	}
	// A structured error classified by the proxy (queue_full,
	// upstream_unreachable, stream_read_error, transform_error, or the
	// provider's parsed error type) is always a genuine failure.
	if r.ErrorType != "" {
		return true
	}
	// Final HTTP status: any error status (429/499 already excluded above).
	if r.StatusCode >= 400 {
		return true
	}
	// Absorbed retry attempts: a 5xx that a later retry masked still counts as
	// an upstream failure (a 429 attempt is flow control and does not).
	for _, a := range r.Attempts {
		if a.StatusCode >= 500 {
			return true
		}
	}
	return false
}

// HasRateLimit reports whether this request received HTTP 429, either as its
// final response or a retried attempt. It counts an affected request once,
// not its attempts. RateLimited is broader scheduler metadata (queue waits
// and 503 pacing) and intentionally does not define this observer metric.
func (r *Record) HasRateLimit() bool {
	if r.StatusCode == 429 {
		return true
	}
	for _, attempt := range r.Attempts {
		if attempt.StatusCode == 429 {
			return true
		}
	}
	return false
}

// PreviewMaxBytes bounds stored content previews (prompt/response). Content
// previews exist only when capture_body_preview is enabled.
const PreviewMaxBytes = 120

// TruncatePreview is the single canonical preview truncation: it caps s at
// PreviewMaxBytes, backing off to a rune boundary so the ellipsis never
// follows a split multi-byte UTF-8 sequence.
func TruncatePreview(s string) string {
	if len(s) <= PreviewMaxBytes {
		return s
	}
	cut := PreviewMaxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// HasErrorKey reports whether b contains a top-level-candidate "error" key,
// tolerating optional whitespace before the colon (`"error":` and `"error" :`).
// It is a cheap byte-scan GATE used to decide whether to run the full
// ParseErrorEnvelope decode - not a parse itself. Both the SSE streaming
// analyzer and the non-streaming body path share this single gate so the two
// in-band-error detectors can never drift apart. The authoritative decode
// (which only reads the top-level key, so a nested or string-literal "error"
// never false-positives) happens in ParseErrorEnvelope.
func HasErrorKey(b []byte) bool {
	// Keep normal error-free chunks on the vectorized negative path. A key
	// spelling of ASCII "error" can only hide a letter through a Unicode escape.
	if !bytes.Contains(b, []byte(`"error"`)) && !bytes.Contains(b, []byte(`\u`)) {
		return false
	}
	return JSONKey(b, "error") != nil
}

// ParseErrorEnvelope parses a provider error payload's top-level "error" key
// into (type, code, message), provider-agnostic across the common wire shapes:
//
//	{"error":{"message":"...","type":"...","code":"..."}}     OpenAI / gateways
//	{"type":"error","error":{"type":"overloaded_error",...}}   Anthropic
//	{"error":"message"}                                        plain-string
//
// The code may be a string (OpenAI) or a bare number (OpenRouter/LiteLLM);
// both are accepted. It returns ("", "", "") when the payload is decodable but
// carries a null/empty error value (a decoy key, not a failure) - that lets
// callers treat null envelopes as success while still catching real ones. For
// genuinely unparseable JSON it also returns ("", "", ""); callers that must
// still classify such bodies keep their own bounded raw-prefix fallback
// (proxy's parseErrorBody). Message is bounded via TruncatePreview.
func ParseErrorEnvelope(b []byte) (typ, code, msg string) {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 {
		return "", "", ""
	}
	var probe struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return "", "", ""
	}
	raw := bytes.TrimSpace(probe.Error)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", "", ""
	}
	switch raw[0] {
	case '"':
		_ = json.Unmarshal(raw, &msg)
	case '{':
		var e struct {
			Type    string          `json:"type"`
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			return "", "", ""
		}
		typ, msg = e.Type, e.Message
		if c := bytes.TrimSpace(e.Code); len(c) > 0 && !bytes.Equal(c, []byte("null")) {
			if c[0] == '"' {
				_ = json.Unmarshal(c, &code)
			} else {
				code = string(c) // numeric code, kept verbatim
			}
		}
	default:
		// A number/bool/array under "error" is not a failure we can classify.
		return "", "", ""
	}
	if typ == "" && code == "" && msg == "" {
		return "", "", "" // empty object/string: a decoy, not a failure
	}
	if typ == "" {
		typ = "provider_error"
	}
	return typ, code, TruncatePreview(msg)
}

// TTFT returns the time to first token, or 0 if the response produced no
// tokens (e.g. a pure error or empty completion). It is measured from the
// final (returned) upstream attempt's start when retries occurred - so a
// request that absorbed retries shows the successful attempt's real TTFT, not
// the accumulated retry delay. Duration still reflects total wall time.
func (r *Record) TTFT() time.Duration {
	if r.FirstTokenAt.IsZero() {
		return 0
	}
	start := r.Start
	if !r.FinalAttemptAt.IsZero() {
		start = r.FinalAttemptAt
	}
	return r.FirstTokenAt.Sub(start)
}

// Recorder is the sink for per-request records. Implementations must be safe
// for concurrent use and must never block the proxy's hot path (the recorder
// is expected to do O(1) work; any durable storage drains asynchronously).
type Recorder interface {
	Record(*Record)
}

// Counters holds live, process-wide tallies exposed to the dashboard.
type Counters struct {
	InFlight int64 `json:"in_flight"`
	TotalReq int64 `json:"total_requests"`
	TotalErr int64 `json:"total_errors"`
}
