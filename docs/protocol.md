# Protocol and client integration

The client-facing interface is OpenAI-compatible; the upstream-facing protocol
can differ. Per-request headers select the destination, credentials and optional
native adapter. The [compatibility matrix](adapters.md#client-and-upstream-compatibility)
maps the default OpenAI-compatible relay and supported native translations.
No endpoint/API-key registry is required. Read
[Security](../SECURITY.md): routing credentials do not authorize operator access,
and the whole dashboard plane is gated by `MILLIVOLT_OPERATOR_TOKEN`
([operator access](operations.md#operator-access)); only `/healthz` and the
relay stay open.

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
| `X-Proxy-Session` | Explicit conversation ID; otherwise automatic grouping applies. |
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
| HTTP relay, including Anthropic translation | Forwarded values → extracted authentication → configured provider headers → explicit injection. Configured/injected authentication can override the extracted key. |
| HTTP model discovery | Constructed authentication → configured provider headers. Explicit injection is not applied. |
| Cursor run/model discovery | Configured headers → required protocol/framing headers and extracted authentication when a key is present. Explicit injection is not applied. |

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
are rejected with HTTP 413 without an upstream send. Invalid JSON is
passed through on the nontranslated path. The proxy can retry transient
transport failures, 429 and 5xx before returning the final response. Durable
quota/billing 429s are not treated as transient. Provider retry hints and
operator holds can substantially extend total wall time.

SSE pacing may insert comment keepalives. A known queue/hold wait may commit HTTP
200 before the final upstream outcome, so a subsequent failure must be signaled
in-band. Bounded terminal/quality handling can replace a degenerate completion
with an error. Non-streaming quality retries may make another upstream request;
neither transparency nor exactly-once upstream execution is unconditional.

### Scheduling and timing

The provider-plus-key group controls simultaneous upstream sends and its waiting
queue. The separate provider-wide Limits policy combines all keys and clients.
Queue capacity and wait limits can reject work locally; the configured
`queue_retry_after` supplies the caller's retry hint. Operator-hold time is
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
Provider Retry-After/reset hints are not clamped by the ordinary adaptive
backoff cap. A long provider hint can therefore outlast that cap.

A finalized request retains its absorbed attempts and final outcome in one
record. Recovered upstream failures can count as errors even when the final
status succeeds. A final or retried 429 is a separate affected-request signal.
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
caller cancellations and fired per-send deadlines are excluded. A selected 429
is a storm failure sample but remains a separate rate-limit signal in dashboard
request health. Configurable response/stream failure detection observes a 2xx
attempt once when its relay completes, or before an existing quality re-ask.
It never adds a replay after meaningful output or native tool execution.

An open scope holds new sends until its cooldown expires, then admits one shared
probe. Failed probes multiply the cooldown, with configurable jitter and a cap;
valid larger upstream retry hints remain effective. Recovery needs the configured
number of consecutive successful probes. The existing provider-plus-key queues,
operator holds and provider Limits still apply. The HTTP relay checks storm
readiness before acquiring key concurrency, then claims a probe only at the
actual send boundary. Readiness never reserves a probe while waiting for a key
slot. Native parked runs remain resumable; fresh Cursor HTTP
handshakes use the shared native send owner and can retry before a run starts.

The ordinary HTTP retry budget remains `max_retries`. Selected failures during
an active storm may use up to `storm_max_retries` additional attempts across the
logical request, including quality re-asks. Cursor retains its single-handshake
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

An explicit `X-Proxy-Session` takes precedence. Without one, the tracker partitions
requests by client label and credential hash, then compares total user/assistant/
tool turn counts against its open conversations. A nonexpired conversation with
the greatest previous count no larger than the new count is the nearest match.
A reset or idle gap can start another conversation, and the configured open-session
cap evicts the oldest tracked conversation in that partition.

This is a heuristic for clients that send conversation history, not a content
comparison or a universal agent identity. Parallel tasks with similar turn counts
can be ambiguous. Tracker state is process-local, so use explicit session IDs
when predictable continuity matters and keep the client/key namespace stable.
Automatic grouping never establishes a parent/child relationship.

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
