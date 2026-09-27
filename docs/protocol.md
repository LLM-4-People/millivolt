# Protocol and client integration

The client-facing interface is OpenAI-compatible; the upstream-facing protocol
can differ. Per-request headers select the destination, credentials and optional
native adapter. The [compatibility matrix](adapters.md#client-and-upstream-compatibility)
maps the default OpenAI-compatible relay and supported native translations.
No endpoint/API-key registry is required. Read
[Security](../SECURITY.md): routing credentials do not authorize operator access,
and the whole dashboard plane is gated by `MILLIVOLT_OPERATOR_TOKEN`
([operator access](operations.md#operator-access)); only `/healthz`,
`/favicon.ico` (and the other origin-root brand/PWA files) and the relay stay open.

## Endpoint behavior

The dashboard, embedded assets, `/admin/*` actions, `/healthz` and registered
`/metrics/*`
routes belong to millivolt. Their methods and meanings are listed in
[operations](operations.md#operator-and-data-routes). `GET /v1/models` and
`GET /models` use the discovery path described below. Other paths reach the
relay catch-all and require valid routing headers; the client chooses the
upstream endpoint and model.

Forwarding a body is not the same as understanding every field in it. The normal
relay does not rewrite an unfamiliar JSON shape just to observe it, but usage,
first-token timing and quality classification depend on recognized formats.
Native translation is explicit and narrower than arbitrary HTTP forwarding.
There is no automatic provider failover or model-selection registry.

## Routing headers

Only `X-Proxy-Base-URL` is required by the proxy. The upstream may require a key.
Routing validation and request construction live in
[request.go](../internal/proxy/request.go).

| Request header | Meaning |
| --- | --- |
| `X-Proxy-Base-URL` | Absolute HTTP(S) upstream base; no userinfo, query or fragment. |
| `X-Proxy-Key` | Upstream key; takes precedence over `Authorization`. Without it, a Bearer credential or the supplied Authorization value is used. No credential is also allowed. |
| `X-Proxy-Auth-Header` | Key's upstream header; defaults to `Authorization`. |
| `X-Proxy-Auth-Prefix` | Prefix before the key; defaults to `Bearer `. A present empty header selects no prefix. |
| `X-Proxy-Path` | HTTP relay: replace the incoming path before joining it to the base URL. Cursor uses its [native path rule](adapters.md#cursor-connect-bridge); model discovery ignores this header. |
| `X-Proxy-Query` | HTTP relay: nonempty query override; otherwise preserve the incoming query. Cursor and model discovery ignore both. |
| `X-Proxy-Headers` | HTTP-relay header overrides as a JSON object of string arrays, e.g. `{"x-example-version":["2026-01"]}`. Not applied to model discovery or the Cursor bridge. |
| `X-Proxy-Timeout-Ms` | Positive milliseconds. HTTP relay: a fresh headers-and-body deadline per attempt, excluding queue/hold/backoff waits. Cursor applies it only to send header waits; model discovery uses its separate configured budget. |
| `X-Proxy-Format` | Upstream format: `openai` (default relay), `anthropic`, or `cursor`. The client-facing interface remains OpenAI-compatible; see [supported combinations](adapters.md#client-and-upstream-compatibility). |
| `X-Proxy-Client` | Observability/client label; otherwise inferred from client SDK/User-Agent metadata. |
| `X-Proxy-Session` | Explicit conversation ID; otherwise a configured tracked body param or automatic grouping applies. |
| `X-Proxy-Parent-Session` | Direct declared parent; requires an explicit own session on the same request. |
| `X-Proxy-Max-Concurrency` | Positive provider-plus-key scheduler-group cap. Each admission updates the shared group's policy; later admissions, including headerless ones using config defaults, can replace it. Not the persistent provider-wide control. |
| `X-Proxy-Limit-Concurrency` | Sticky provider-wide in-flight cap. |
| `X-Proxy-Limit-Requests` | Sticky provider-wide request rate as `COUNT/WINDOW`. |
| `X-Proxy-Limit-Tokens` | Sticky provider-wide token rate as `COUNT/WINDOW`. |
| `X-Proxy-Refresh-Token` | Opt-in stateless expired-token refresh for supported mechanisms. See [refresh](adapters.md#token-refresh). |

One caller setting `X-Proxy-Limit-*` affects every key/client of that provider.
Absent headers preserve limits; `0`, `off`, `none` or `unlimited` clear them.
Windows accept Go durations from 1 second through 24 hours, plus `1d`.
Malformed limit values reject the request without applying any of its limit
headers. The Limits menu shows the policy and its source. Rates use continuously
refilled budgets, not strict fixed-window quotas; see
[Pause and Limits](operations.md#pause-and-limits) for charging and burst behavior.

Supported proxy-control headers never forward upstream. Injected names must be
valid, unique case-insensitively, and not reserved control/hop-by-hop/Host names;
values must be valid HTTP field values. Ordinary hop-by-hop and
Connection-nominated headers are removed. Precedence depends on the request path:

| Path | Header precedence, lowest to highest |
| --- | --- |
| HTTP relay, including Anthropic translation | Forwarded values → extracted authentication → configured provider headers → explicit injection → matching request overrides. Configured/injected authentication can override the extracted key. |
| HTTP model discovery | Constructed authentication → configured provider headers. Explicit injection and request overrides are not applied. |
| Cursor run/model discovery | Configured headers → matching request overrides (runs only) → required protocol/framing headers and extracted authentication when a key is present. Explicit injection is not applied. |

The provider label is server-derived from the base host's registrable domain;
IPs retain their authority/port, and nonregistrable hostnames remain hostnames.
Optional provider aliases apply afterward. There is no supported
`X-Proxy-Provider` override.

## URL and auth examples

For the HTTP relay, a base ending in `/v1` and request path
`/v1/chat/completions` do not produce
`/v1/v1/chat/completions`: the join removes the shared leading segment.
A path override still joins to the selected base. Use `X-Proxy-Query` rather
than embedding a query in the base URL.

For a native Anthropic-compatible endpoint, use its documented base/model/key
and set translation plus the appropriate upstream auth/path/version headers:

```sh
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "X-Proxy-Base-URL: $NATIVE_BASE_URL" \
  -H "X-Proxy-Key: $NATIVE_API_KEY" \
  -H 'X-Proxy-Auth-Header: x-api-key' \
  -H 'X-Proxy-Auth-Prefix;' \
  -H 'X-Proxy-Path: /messages' \
  -H 'X-Proxy-Format: anthropic' \
  -H "X-Proxy-Headers: {\"anthropic-version\":[\"$NATIVE_API_VERSION\"]}" \
  -H 'Content-Type: application/json' \
  -d "{\"model\":\"$NATIVE_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}"
```

The example assumes JSON-safe model/version values. The semicolon in
`-H 'X-Proxy-Auth-Prefix;'` is important: curl sends a present empty header.
`-H 'X-Proxy-Auth-Prefix:'` instead omits it, leaving the proxy's Bearer default.
See the [official curl header guide](https://everything.curl.dev/http/modify/headers.html).
An unauthenticated local upstream needs no dummy API key.

## Relay, retries and model discovery

Ordinary requests preserve original body bytes, but the request is buffered up
to `max_request_bytes` before metadata extraction and sending. Larger requests
are rejected with HTTP 413 without an upstream send. A decoded output cap
above 1,000,000 tokens is rejected with HTTP 400 the same way, before any
upstream send and at both trust boundaries - the original body's decode and
the re-decode of a translated body - for either spelling (`max_tokens` or
`max_completion_tokens`). Invalid JSON is
passed through on the nontranslated path. The proxy can retry transient
transport failures, 429 and 5xx before returning the final response, and
retryable in-band error events on an open stream before any generation
content was relayed. Durable
quota/billing 429s follow `quota_pause_mode` instead of the transient retry
ladder: `off` (default) surfaces them immediately; `retry` parks the
triggering request and every new request to the same OpenAI-wire provider
behind a recovery gate until the first re-send resolves 2xx, with the
consecutive-success threshold from `quota_pause_recovery_successes` (a
provider `Retry-After` on the quota response paces the next probe like
any other 429); `manual` installs an indefinite provider pause that only
an operator resumes. Quota re-sends never burn the transient retry budget. Provider
retry hints and operator holds can substantially extend total wall time. With
`suppress_client_retries` the proxy declares itself the client's sole retry
authority: every error response the relay returns then carries
`x-should-retry: false`, the official OpenAI SDK convention (python, node,
go and ruby honor it from source), which overrides the SDKs'
408/409/429/5xx auto-retry defaults and any `Retry-After`, including a
provider hint that would otherwise relay verbatim. No response exists to
carry the directive on transport-level failures, clients that ignore the
header (the Vercel AI SDK / opencode stack, whose OpenAI adapter classifies an
in-band error by the error's `code` and `type` alone, never its message, and so
cannot be steered by a header) keep their own policy, and mid-stream SSE errors
never auto-retry in official SDKs and cannot carry new headers. Success
responses are untouched, and the setting hot-reloads for new responses.

SSE pacing may insert comment keepalives. Those are comment LINES only, never a
terminated event: an SSE comment is ignored by a parser and does not end the
event in progress, while a blank line dispatches it - so a keepalive landing
between the data lines of one event must not append that blank line, or the
client is handed a document that is not finished and every conforming SDK raises
on it. A known queue/hold wait may commit HTTP
200 before the final upstream outcome, so a subsequent failure must be signaled
in-band. Bounded terminal/quality handling can replace a degenerate completion
with an error. Non-streaming quality retries may make another upstream request;
a stream truncated before any generation content was relayed is re-sent on the
same committed connection, while a truncation after relayed content surfaces
the in-band error for the client to retry. A provider error event streamed
in-band on the 200 is retried the same way while nothing content-bearing was
relayed: the error classes the OpenAI API documents as retryable (500
`server_error`, the 503 overloaded/unavailable family, request timeouts, and
the equivalent Anthropic/gateway spellings) are dropped before the wire and
the request re-sent, so the client never sees the failure. The
`retryable_error_classes` setting extends that vocabulary with operator-supplied
type or code strings for gateway-specific spellings (durable quota/billing
classes are rejected at load); rate limits in-band
(no `Retry-After` exists there), durable request/billing classes, unknown
types, Responses-API feeds once any of their events was relayed (per-response
sequence numbers), and errors after relayed contract bytes relay verbatim for
the client to handle. Mid-thinking rescues
(`thinking_retries`) extend the re-send to requests that die or end without an
answer during the reasoning phase: a truncation or upstream read error after
reasoning-only output, a cleanly finished reasoning-only completion (the
withheld terminal region is replaced by the fresh attempt), and a retryable
in-band error that arrived after reasoning-only output. The fresh stream
appends behind the already-relayed reasoning, which is auxiliary display text;
by that budget, answer content, tool calls, refusals, `finish_reason: length`
(the client's own token cap) and Responses-API feeds (per-response
sequence numbers) are never rescued. An exhausted budget surfaces the
`reasoning_only` class as a provider error (`upstream_error`) on both
surfaces: in-band on the streaming connection, and as an HTTP 502 error
envelope for non-streaming requests.

### Atomic tool calls

`atomic_tool_calls` (default false, so a deployment's wire is unchanged until
an operator opts in) makes a streamed tool call atomic: the generic OpenAI-wire
relay withholds every tool-call-bearing event - a `tool_calls` delta, a
deprecated `function_call` delta, or an ambiguous opener such as a `tool_calls`
key whose value continues on the next `data:` line - until the stream's
terminator, then releases it verbatim. Translated Anthropic streams, the Cursor
bridge and Responses-API feeds keep their own framing and never reach it.

It exists because no client generation waits for the model to say it finished.
A streamed tool call is committed on the client's side, and every generation
checked commits at or before the **end of the response body**, never at
`finish_reason`. Both Vercel AI SDK generations were read in their published
packages: the 4.x adapter (`@ai-sdk/openai-compatible@2.x`, the `ai@6`
generation) keeps every accumulated call in `hasFinished: false` - the flag is
never assigned anywhere in the package - so its end-of-stream `flush()`
finalizes all of them; the 5.x tracker (`@ai-sdk/provider-utils@5.0.49`, behind
both `ai@7` and `@ai-sdk/openai@4.x`) calls `finishToolCall` from `flush()` and
nowhere else, with a source comment stating that tool calls must not finalize
before the stream ends. So a stream that never terminates - or whose body simply
ends - hands the client a tool call the model never finished, and a client that
executes it runs a tool from a truncated response. Verified in the installed
packages, not inferred: see the class-level tests in
[internal/sse](../internal/sse/analyzer.go) (`ToolCallRegionOpened`).

`@ai-sdk/openai@4.x` adds no protection of its own for a stream that never
signals `finish_reason`: its `finishReason` starts as `unified: 'other'` and its
`flush()` enqueues that value unchanged, so such a turn finishes as a **silent
success** rather than an error. The official `openai` SDKs differ again: they
throw on an in-band error chunk, which is why the relay does not rely on any one
client's cooperation, and `openai-node` 7.x is the one client that dispatches a
pending SSE event at EOF (`SSEDecoder.flush()`, with the comment "servers
sometimes omit the trailing blank line"), where every other client generation
discards it - so the same partial bytes are a hard `SyntaxError` for
`openai-node` 7.x users and invisible to everyone else. No client SDK offers a
way to discard bytes it already received: there is no `Last-Event-ID`
resumption, no stream reset and no idempotency key on chat completions, which
is why the guarantee has to be made before the bytes are written. Both
mechanisms are verified in the published packages, not inferred: see the
class-level tests in [internal/sse](../internal/sse/analyzer.go)
(`ToolCallRegionOpened`).

Tool-call accounting has a single owner: the set of distinct tool calls seen on
the stream, keyed by the entry's **array index** - which is how the official
`openai` SDKs (node, python, go) key a streamed call, and therefore the
majority of clients. Two entries at different indices are two calls even when a
gateway hands them the same id, and an argument-only continuation at an index
already seen is the same call. An entry whose `id` is empty or absent is
therefore still a call, which matters because a gateway that omits ids used to
make the relay classify such a stream as the `empty_tool_call` void and replace
a real tool call with that (retryable) error. An entry the provider gave no
index to falls back to its id; an entry with neither contributes nothing, being
an argument-only continuation indistinguishable from a repeat.

That matches three of the four client families, and the fourth diverges in a way
worth stating: `@ai-sdk/provider-utils` 5.x looks a call up by **id first**
(`toolCallsById`, then by index, then the latest call), so two entries that
share an id at different indices are ONE call to `ai@7` with the second entry's
arguments **concatenated** onto the first, where the `openai` SDKs see two. The
relay's count therefore under-reports what such a client executes, and
deliberately keeps reporting what the wire actually carried rather than what one
client family would reconstruct. Measured: two calls sharing one id are
`ToolCalls: 2` on the relay and two executed calls for the `openai` SDKs, but a
single `ai@7` call with merged arguments. Nothing in the proxy can reconcile
that - the two families genuinely disagree about what the stream contained - so
the divergence is documented rather than papered over.

Two producers feed that set and never decode the same bytes twice. The
reassembled SSE event is the owner of every byte its bounded buffer kept: a
`tool_calls` array is legal SSE spread across several `data:` lines of one
event, and a per-line count alone would miss every entry, which would make the
relay relay a complete tool call and simultaneously classify the stream as the
`empty_tool_call` degenerate class. The raw `data:` line is the other, and only
for a line the event buffer had to drop: the buffer caps one event at 1 MiB, so
a tool-call frame whose line is larger is invisible to the event and still
counted from the line itself. Every occurrence of a tool-call key within one
event is scanned, not just the first, because a gateway that drops the blank
line between two tool-call frames legally merges them into a single event. Both
sides are bounded and linear: the tracked set is a fixed-size index bitset plus
a bounded id fallback capped at 1024 distinct calls, registration is O(1), and
a value arriving past the cap is not decoded at all. That bound is what keeps
the proxy-stream path cheap, since `max_concurrent` defaults to unlimited and
nothing else caps one stream's accounting.

The trade, which is why it is opt-in. A provider that ends the body with
neither `finish_reason` nor `[DONE]` - a real shape, see
`metrics.ClassifyOutcome` - truncates, and the withheld tool call is then
dropped with the rest of the truncation rather than relayed. That is safer for
execution but loses a tool call such a provider completed. This holds whatever
the feed's shape: a `tool_calls` delta in a stream the chat-completions
classifier does not recognize is still withheld and still dropped, and its
truncation is surfaced in-band, because a region that is withheld could not also
be released without defeating the setting. Tool-call delivery also stops being
progressive: the client sees the whole call at the terminator.

The region a tool call opens is bounded by the relay's `toolCallHoldMax`
(1 MiB), not the 64 KiB terminal cap - tool-call arguments are provider-authored
and routinely dwarf a terminal region. The region opens on whatever byte run
first revealed the tool call, and a byte run counts as revealing one wherever it
sits: the relay's newline-free line guardrail splits a data line longer than
1 MiB, and every fragment after the first begins mid-JSON with no `data:`
prefix, so the call is found by a scan of the raw run rather than of a data
line. A trailing partial line is the same case. When those bytes are already
going to the wire - the setting is off, or the line's head was relayed before the
call was revealed - the relay records that fact instead, and no rescue may
follow it. Past the bound the region **commits**: the bytes reach the client,
the relay records that as a fact of the wire, and from then on no rescue may
append a fresh attempt behind them and no synthetic outcome may replace them.

A committed region is not a silent success, though - the finished stream's class
is surfaced in-band *after* the committed bytes, and the record carries it, so a
stream that failed says so. Two classes of commit are distinguished, because
they are not equally untrustworthy. A commit of a region that holds a **tool
call** may hide a call the analyzer never saw whole, so only the truncation
survives it: that class asserts merely that the stream stopped, which no
committed region can change, while every other degenerate class is an inference
from counts the commit is exactly what makes unreliable. A commit of a region
with **no tool call in it** keeps its class. The bound is a size guard, not
evidence of health, and a provider-controlled usage blob is enough to cross it
on its own: measured, a stream that produced nothing but a `finish_reason` chunk
and a 65,536-byte usage payload recorded `IsError: false` with an empty
`ErrorCode` purely by being large. Nothing about a tool-call-free region makes
the analyzer's view of it untrustworthy - its bytes were the bytes it saw - so
the class is surfaced in-band after them and carried by the record. A tool call
arriving *after* the terminator is likewise relayed verbatim, because the region
is already released and such a stream is out of contract regardless.

A withheld region is never released by anything that ends the stream early. A
transport read error, a client that goes away, and this process shutting the
stream down all prove nothing about whether the model finished the call, so a
region holding one is dropped and the failure is surfaced in its place: the
read failure in-band (with the record's own class, so a provider that stated a
failure in-band keeps that statement), and nothing at all into a socket that is
already gone. A region with no tool call in it keeps byte preservation, which is
the pre-feature behavior. A close this process initiated is not the client's own
abort either, so it carries its own class: `503` + `server_shutdown` rather than
the flow-event `499`, which would report a request the proxy failed as a clean
one.

Memory: a 1 MiB withhold region retains about **3 MiB per in-flight stream**,
not 1. Three buffers are live at that size and all three scale with the region:
the region's own bytes, the relay's accumulating line buffer, and the analyzer's
per-event buffer. Measured under forced GC with `runtime.MemStats.HeapAlloc`:
one in-flight 1 MiB region reads 3.4 MiB total, and the marginal cost is 3.01
MiB per stream at 64 streams (193 MiB) and again at 256 (772 MiB) - flat across
region sizes and read granularity, so 1,000 concurrent streams is about 3 GiB.
There is no default ceiling on that product: `max_concurrent` defaults to 0,
meaning **unlimited**, and `max_conns_per_host` does not bound it either - Go's
HTTP/2 client never reads that field, so against any HTTPS upstream that
negotiates h2 through ALPN (every real provider) the transport multiplexes every
stream onto one connection and the effective ceiling is whatever the provider
advertises. Measured: with `MaxConnsPerHost` set to 1, a TLS+h2 upstream still
ran 4,000 concurrent requests, because Go's own per-connection stream limit is
1,000. Size `max_concurrent` deliberately before enabling this on a busy
deployment.

Every rescue budget still guarantees the client received no completion-contract
bytes, which is what makes a re-send invisible. That guarantee is a property of
the wire, not of the analyzer's bookkeeping: the relay refuses every rescue once
a withhold region has committed **or** once a tool call has been written that it
could not withhold, so a re-send can never land behind bytes the client already
holds even when the analyzer never saw a whole call in them. That second
condition is the conservative direction at the default setting too, where a tool
call is relayed eagerly: a stream whose in-flight call the analyzer could not
count is a stream a fresh attempt would merge arguments into, so it is surfaced
as a truncation instead.
Note what the record therefore does not show: `RetryAttempt` carries no usage,
tokens or cost, so an absorbed attempt's spend is invisible, `TTFT` is measured
from the final attempt and so excludes any queueing and backoff, and
`FinalizeRecord` can zero `answer_tokens` when the winning attempt ends in
`finish_reason: tool_calls` without content.

`Record.ToolCalls` is the provider's behavior, not the wire's, and it errs in
both directions. It can exceed what was delivered: a truncated stream whose tool
call was withheld records `ToolCalls: 1` while the client received zero tool-call
deltas. It can also under-count: a tool-call frame split at the relay's
newline-free line guardrail belongs to neither producer, so the record reports no
call for a call the client did receive - the count follows what could be
observed, and inventing one would be worse. A stream in that state is never
classified as a void either, since a call in flight disproves "the model
declared a tool and never called it".

Neither transparency nor exactly-once upstream execution is unconditional.

Before relay or translation, the upstream request is subject to the configured
`request_overrides` (see [request overrides](operations.md#request-overrides)):
matching rules set, replace or remove upstream headers as the last header
stage, and their body fields set `max_tokens` / `max_completion_tokens` on
OpenAI-wire and translated Anthropic targets, fail closed. A configured
`sub_conversations` strip also removes tracked body fields before relay
([tracked body params](#tracked-body-params)). Only the upstream request
changes; the client-visible contract is unchanged.

### Scheduling and timing

The provider-plus-key group controls simultaneous upstream sends and its waiting
queue. The separate provider-wide Limits policy combines all keys and clients.
Queue capacity and wait limits can reject work locally; the configured
`queue_retry_after` duration (for example `2s`, never a bare integer) supplies
the caller's retry hint. Operator-hold time is
excluded from the ordinary queue-wait limit. A hold's own queue cap still applies.

For the HTTP relay, `upstream_timeout` limits waiting for response headers,
not the streaming body.
`X-Proxy-Timeout-Ms` instead gives each upstream send a fresh deadline covering
headers and body. Neither is a total end-to-end deadline across queueing, holds,
retries and backoff; client cancellation is the way to stop the whole request.
The [Cursor bridge](adapters.md#cursor-connect-bridge) instead applies the tighter
configured/per-request timeout only until send headers arrive, without a body
deadline on its parked run. Model discovery ignores the per-request timeout
and uses `models_discovery_timeout` for the whole discovery operation.
Provider Retry-After/reset hints are a floor on the wait, not a replacement
for adaptive exponential backoff. A long hint is not clamped by the ordinary
adaptive cap and can therefore outlast it. A short hint does not reset the
doubling, so a 1s "retry shortly" cannot re-hammer a struggling upstream.

A finalized request retains its absorbed attempts and final outcome in one
record. Every attempt - absorbed or final - stores the same upstream-response
information: the provider request id, server, processing time, echoed model,
rate-limit state and the redacted response headers. A provider-side request id
therefore matches to the exact upstream attempt that produced it, including
failures the client never saw. Transport failures have no HTTP response, so
their attempt entries leave those fields empty. Recovered upstream failures can
count as errors even when the final status succeeds. A final or retried 429
is a separate affected-request signal.
A client that disconnects before a final outcome is recorded as status 499
with `client_disconnected`; a plain 499 is a flow event, but one carrying a
structured error (the provider's in-band stream failure the client aborted
around) counts as the upstream error it observed.
See [metric interpretation](operations.md#dashboard-data-and-request-inspection)
for duration, first-token latency and throughput.

### Error storm protection

Enable `storm_enabled` in Settings to add shared provider/model protection to
existing admission and retry pacing. Requests wait in the proxy with the caller
connected; this is not a client SDK queue. The proxy keeps the existing bounded
request bytes and credentials only for the request lifetime. Queues are in
memory, cannot survive a restart, and stop waiting when the caller cancels.

Each exact recorded model name within its resolved provider has a rolling
`storm_window`. A model qualifies when its sampled attempts meet
`storm_min_samples` and its selected-failure percentage reaches
`storm_error_percent`. Provider-wide protection requires **every active model**
to qualify, as well as the aggregate sample/rate threshold. Only models receiving
requests within that same window are active; an idle model or an unused model in
a provider catalog cannot affect that decision. This scope combines credentials
and clients. Dashboard model-display rewrites do not merge storm scopes.

The detector samples successful upstream attempts and selected failures, counting
absorbed retries separately. Incident details also count distinct logical
requests with a selected failure in the window; retries move the same request's
last-failure membership instead of adding another request. Unselected statuses,
non-retryable HTTP account/auth rejections, recognized durable quota envelopes,
caller cancellations and fired per-send deadlines are excluded - quota outcomes
never become storm evidence, even while a quota gate is open. A selected 429
is a storm failure sample but remains a separate rate-limit signal in dashboard
request health. Configurable response/stream failure detection observes a 2xx
attempt once when its relay completes, or before an existing quality or thinking
rescue re-send. It never adds a replay after meaningful output or native tool
execution.

An open scope holds new sends until its cooldown expires, then admits one shared
probe. Failed probes multiply the cooldown, with configurable jitter and a cap;
valid larger upstream retry hints remain effective. Recovery needs the configured
number of consecutive successful probes. The existing provider-plus-key queues,
operator holds and provider Limits still apply. The HTTP relay checks storm
readiness before acquiring key concurrency, then claims a probe only at the
actual send boundary. Readiness never reserves a probe while waiting for a key
slot. Native parked runs remain resumable; fresh Cursor HTTP
handshakes use the shared native send owner and can retry before a run starts.

The quota pause (`quota_pause_mode: retry`) is a separate provider-wide gate in
the same scheduler: the first durable quota/billing 429 from an OpenAI-wire
provider opens it immediately (no sample thresholds), the triggering request
absorbs the 429 and re-sends per the same recovery cooldowns, and every new
request to the provider parks at admission. The first send that resolves 2xx
closes the gate after `quota_pause_recovery_successes` probes and the queue
drains; a provider `Retry-After` on the quota 429 is a floor on the next probe,
bounded by the shared hint ceiling. Parked sends obey `storm_max_queue` and
`storm_max_wait`; beyond them the proxy answers HTTP 429 locally. The gate
appears in the storm banner as a quota incident with an operator **Resume now**
action; a manual-mode hold appears on the pause surface instead. The gate is
in-memory: a restart drops it and the provider's next durable quota 429 re-arms
it. Manual-mode holds persist like every operator pause.

The ordinary HTTP retry budget remains `max_retries`. Selected failures during
an active storm may use up to `storm_max_retries` additional attempts across the
logical request, including quality and thinking re-sends. Cursor retains its single-handshake
behavior when protection is off and uses only the additional storm budget for
failed fresh handshakes. Exhaustion returns the final upstream failure. Retrying
inference can repeat upstream execution or billing even when no output arrived;
this feature does not provide exactly-once processing. See the
[HTTP retry constraints](https://www.rfc-editor.org/rfc/rfc9110.html#section-9.2.2).

`storm_max_queue` bounds callers waiting at storm gates across the process;
ordinary scheduler queues have their own limits. `storm_max_wait` bounds each
storm wait, not total request wall time. Scope capacity and oversized scope
identifiers also fail closed. Local storm admission rejection returns HTTP 429
with `queue_retry_after`; a streaming connection already committed by keepalive
comments receives an in-band error instead. Successful retries remain one
finalized request record with its absorbed attempts and accumulated queue wait.

### Model discovery

`GET /v1/models` and `GET /models` are constructed discovery services, not
ordinary inference records. The proxy normalizes recognized model lists,
translates native formats, completes supported pagination, and optionally
enriches missing fields. Unrecognized successful list bodies remain verbatim;
primary discovery budget/errors fail explicitly, while optional enrichment can
fall back to the complete base list. Discovery can apply limit headers without
consuming an inference slot. Its time/byte/page limits are configuration-owned.

Discovery ignores `X-Proxy-Path`, `X-Proxy-Query` and the incoming query. HTTP
discovery constructs `/v1/models` using the shared base join; Anthropic pagination
constructs its own query. Cursor uses its native unary model service. A primary
upstream non-200 response, including a redirect, becomes a 502 discovery error.

The ordinary HTTP relay returns upstream redirects rather than following them.
Forwarding also changes transport headers and may decompress an upstream gzip response;
do not compare transport framing as a byte-identical contract.

## Main and sub-conversations

### Automatic grouping

An explicit `X-Proxy-Session` takes precedence, then a configured tracked body
param (see [tracked body params](#tracked-body-params)). Without either, the
tracker partitions requests by client label and credential hash, then compares
total user/assistant/tool turn counts against its open conversations. A
nonexpired conversation with the greatest previous count no larger than the new
count is the nearest match.
A reset or idle gap can start another conversation, and the configured open-session
cap evicts the oldest tracked conversation in that partition.

This is a heuristic for clients that send conversation history, not a content
comparison or a universal agent identity. Parallel tasks with similar turn counts
can be ambiguous. Tracker state is process-local, so use explicit session IDs
when predictable continuity matters and keep the client/key namespace stable.
Automatic grouping never establishes a parent/child relationship.

### Tracked body params

The request body is client data, and one field can double as the conversation
identity. When `sub_conversations` names the request's classified client, the
tracked field the client already sends (exemplar: opencode's `promptCacheKey`)
supplies that request's identity in the `k:` namespace: behind an explicit
`X-Proxy-Session`, ahead of automatic grouping. Params match only top-level
body fields, checked in order. The first present field decides: a present
value that is not a JSON string counts as absent and the next field is
checked, while a present string that cannot be decoded or fails the identity
rules drops the identity for the request - no later field is consulted. The
body is relayed unchanged unless the entry sets `strip`, which removes the
entry's configured fields from the top level of the relayed upstream body. A
request translated to the Anthropic wire gains a top-level `prompt_cache_key`
carrying the tracked value. An absent or dropped identity is never a client
error. A tracked identity never declares a parent: `X-Proxy-Parent-Session`
still requires its explicit session pair, and the session wins identity. The
identity rules, name grammar, caps and the settings surface live in
[operations](operations.md#sub-conversation-tracking).

### Declaring a parent

A parent sends `X-Proxy-Session: main-task`. Its child sends its own
`X-Proxy-Session: worker-1` plus `X-Proxy-Parent-Session: main-task`, using the
same client label and API key. Send IDs without the stored `s:` prefix.

When declaring a parent, both headers must occur once, contain nonempty valid
UTF-8 without controls, differ from each other, and fit 512 bytes after trimming.
Self-parent/invalid declarations return 400. The parent header is proxy-only;
it neither edits the body nor instructs an upstream to create a child agent.

The server resolves observed relationships within
`(client, key_hash, conversation_id)`, including history outside the selected
view. The explorer reports main/sub/unresolved counts and role/parent metadata.
A missing parent is **not observed**, not an invented main. Conflicts, cycles or
ambiguous reused IDs remain unresolved. “Main” means no parent declared in
observed history, not independently verified orchestration.

Parent links preserve the full representable client/key namespace. Conversation
filters remain exact leaves, never descendant filters. A changed credential can
change the namespace. Historical parentage cannot be inferred from absent data,
message similarity, or ordinary conversation-continuity fields.
