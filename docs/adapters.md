# Upstream compatibility, adapters and token refresh

The client-facing OpenAI-compatible interface is separate from the upstream
protocol. Select an adapter with `X-Proxy-Format`; do not infer one from a
provider label.
See [protocol](protocol.md) for routing/auth headers and
[operations](operations.md#known-limits) for current limitations.

## Client and upstream compatibility

millivolt accepts three client wire families and translates between any of
them and the supported upstream families. The client protocol is selected by
the request path, or explicitly by `X-Proxy-In-Format`; the upstream protocol
is selected by `X-Proxy-Format`. Translation runs only when both are known and
differ. An absent `X-Proxy-Format` never enters the IR, and an explicit format
equal to the client protocol also stays passthrough. Passthrough is not an
unconditional byte-identity promise: configured `request_overrides`, a
`sub_conversations` strip, or ensured tools can still rewrite the upstream
body before it is sent (see
[request overrides](operations.md#request-overrides)). Without any of those
configured, the body bytes are relayed unchanged.

| Client path | Client protocol | `X-Proxy-Format` | Upstream protocol |
| --- | --- | --- | --- |
| `POST /v1/chat/completions` | OpenAI Chat Completions | `openai` or absent | OpenAI Chat Completions, passthrough when both are Chat |
| `POST /v1/chat/completions` | OpenAI Chat Completions | `openai-responses` | OpenAI Responses |
| `POST /v1/chat/completions` | OpenAI Chat Completions | `anthropic` | Anthropic Messages |
| `POST /v1/chat/completions` | OpenAI Chat Completions | `cursor` | Cursor agent.v1 |
| `POST /v1/responses` | OpenAI Responses | `openai` | OpenAI Chat Completions |
| `POST /v1/responses` | OpenAI Responses | `openai-responses` or absent | OpenAI Responses, passthrough when both are Responses |
| `POST /v1/responses` | OpenAI Responses | `anthropic` | Anthropic Messages |
| `POST /v1/messages` | Anthropic Messages | `openai` | OpenAI Chat Completions |
| `POST /v1/messages` | Anthropic Messages | `openai-responses` | OpenAI Responses |
| `POST /v1/messages` | Anthropic Messages | `anthropic` or absent | Anthropic Messages, passthrough when both are Anthropic |

xAI/Grok uses the ordinary OpenAI-compatible relay, not a separate native
adapter. Other providers and local model servers can use that same path when
they expose a compatible API; there is no fixed provider registry. Native
translation covers the fields listed under
[cross-protocol translation](#cross-protocol-translation), not every endpoint
or arbitrary provider protocol.

Clients must support custom routing headers or middleware that adds them.
For native formats, supply the adapter selection and the upstream's required
authentication, version and path headers; the
[Anthropic example](protocol.md#url-and-auth-examples) and
[Cursor login setup](#cursor-login) show these choices. Provider profiles enrich
headers, metadata and ensured tool signatures but do not select an adapter or
grant provider access.

## Cross-protocol translation

Cross-protocol translation goes through one canonical request, response and
stream model in [format/ir](../internal/format/ir/ir.go). Each wire family has
one decoder and one encoder; the IR is the single source of truth for roles,
content blocks, tool calls and results, reasoning artifacts, stop reasons and
usage. Same-protocol requests never enter the IR: they relay unchanged unless
a configured request override, sub-conversation strip or ensured-tools rule
rewrites the upstream body first.

Carried wherever the target protocol represents them: text, images (URL,
base64 or file id), documents, audio where the target accepts it,
system/developer/user/assistant turns, function tools, tool choice, parallel
tool use, tool calls and results, structured outputs, reasoning effort and
summary, stop sequences, sampling parameters, metadata, usage and cache-token
accounting.

A malformed Chat `stop` value is rejected before the upstream send on a
translated request. Absent, null, a string and an array of strings are the
accepted shapes; any other type returns 400 instead of silently dropping the
stop filter. An array element that is null or not a string is also rejected:
Go would otherwise decode it as an empty stop sequence. The same element rule
applies to Anthropic `stop_sequences`. Native Chat passthrough is not decoded:
the body is relayed unchanged, so `stop: 5` reaches the upstream and its
response is returned. An empty string and an empty array still mean no stops.

Protocol-only artifacts are preserved for the same family and reported when a
target cannot consume them:

- Anthropic thinking and redacted-thinking blocks keep their opaque signatures.
  A Responses reasoning item has no Anthropic signature, so translating it to
  Anthropic emits a `thinking` block with an empty `signature`. The proxy keeps
  the reasoning text and does not invent a signature; resending that block is a
  client choice and the target may reject it.
- OpenAI Responses reasoning items keep `encrypted_content` and item ids.
  A cross-family encrypted reasoning artifact is never replayed into another
  provider's field. A non-streaming surface drops it with a warning; a stream
  has no warning sink and drops it silently.
- A Responses reasoning item exposes two independent text channels: the
  summary parts and the content reasoning text. A translated stream emits both
  as they arrive, so text the source places in both channels appears in two
  target reasoning blocks. The proxy does not deduplicate across channels: a
  native client accumulating the source observes the same two channels, and
  merging them would corrupt genuinely distinct summary and content text.
- Provider-executed blocks such as server tool use and web search results have
  no cross-protocol equivalent and are dropped with a warning.
- A Responses item `phase` is a wire hint with no target equivalent. A
  non-streaming request or response records one `input.phase` or `output.phase`
  warning, and a translated stream records one `output.phase` warning on its
  terminal. The phase text never reaches the client wire.
- A canonical output block with no Responses content part, such as a file or
  audio block carried from Anthropic, is dropped from the output with one
  `output.<type>` warning. Text around the dropped block is preserved.
- Responses provider-executed tool calls in the shared tool-item vocabulary
  (`custom_tool_call`, `local_shell_call`, `mcp_call`, `computer_call`,
  `tool_search_call` and their peers) are tool activity: they count toward a
  `tool_calls` finish even though no cross-protocol content block exists for
  them. Provider-executed generation artifacts (`program`, `program_output`,
  `image_generation_call`, `code_interpreter_call`) are output presence
  instead: a completed turn that carries only one of them is not an empty
  completion, while its finish stays the source's stop rather than
  `tool_calls`, because the item is generated source or media rather than a
  model tool invocation. Extending the tool vocabulary waits for a
  demonstrated client need.
- `previous_response_id`, `item_reference`, `background`, `conversation` and
  prompt templates require provider-side state and are rejected when the
  target cannot honor them. `store: true` is honored by Chat Completions and
  Responses targets and rejected by Anthropic. A Chat or Anthropic request
  translated to a Responses target sets `store: false` when the source never
  chose storage, because the Responses default retains data for at least 30
  days while the source protocol has no such opt-in and the proxy has no
  retrieval path. Explicit source values are preserved. A Responses source's
  implicit storage cannot be honored on Chat or Anthropic, and no response id
  is returned for later retrieval.
- Every system or developer message before the first emitted conversation
  turn, together with the Responses instructions, becomes the top-level
  Anthropic `system` prompt. A mid-conversation system run stays in `messages`
  only after an emitted user turn and before an assistant turn or the end; any
  other position is lifted to the prompt with one approximation warning. A
  later developer turn has no Anthropic role and is lifted to the prompt with
  an approximation warning, which reports the positional change.
- An Anthropic target drops leading assistant turns because the first message
  must use the user role, reporting one `messages.leading_assistant` warning.
  A tool result that answered a dropped leading call is dropped with one
  `messages.orphan_tool_result` warning, so that result never reaches the
  wire. A source-supplied result that matches no call in the source history is
  content, not an artifact of the drop: it passes through unchanged as a
  `tool_result` even though no preceding `tool_use` exists, with no warning.
  Pairing happens before the drop: a result that names a dropped call stays
  paired to it and is dropped with that call rather than retargeted to a later
  same-name call, because the name may answer the dropped call and retargeting
  could answer the wrong tool. A system or developer turn before
  the first emitted turn is still lifted to the top-level prompt even when a
  dropped assistant or an empty user turn preceded it.
- A non-clean non-streaming terminal can carry a missing, null, whitespace or
  partial tool-call argument value the source never finished: a Chat finish
  other than `stop`, `tool_calls` or `function_call`, or a Responses
  `incomplete` or `cancelled` terminal, including a `completed` response that
  still carries an incomplete reason. The proxy drops only the unusable call
  with one `tool_calls.arguments` or `function_call.arguments` warning and
  keeps the text, refusal and source finish. A Responses `failed` terminal is not covered
  by that rule: it returns the provider error envelope and discards the output,
  so no block or warning is produced. A clean terminal still maps an omitted,
  null or whitespace value to the empty object, but fails closed on a partial
  or otherwise invalid value. Structural failures such as a missing name or id
  remain fatal on every terminal, even when the arguments are also unusable.
- A streamed Responses `function_call` that omits `call_id` uses its item id as
  the target tool id. When both identities are absent, the block starts with a
  stable `call_proxy_<output index>` id instead of an empty id, so Chat does not
  write an empty tool call and Anthropic does not reject the block. An
  announcement that carries both identities accepts either spelling on the
  terminal item. A terminal item that uses the other identity kind, an item id
  after a call-id announcement or the reverse, matches a unique open announced
  call with the same name. A terminal entry that repeats an announced call
  whose block already closed is skipped before open matching when its identity
  and name agree; an id-less entry also requires its effective argument value
  to agree. An id-less repeat is skipped before its array position is resolved,
  so a closed call announced at a foreign position cannot claim a different
  open call. Formatting differences and object key order do not affect that
  agreement, but numeric literals are compared as written, so `1` and `1.0`
  do not agree. A value above the canonicalization size, nesting or
  member-separator bounds keeps the byte-exact rule, as does a partial or
  otherwise invalid value, so formatting and key-order differences in such a
  value do not agree. A named terminal item matches only an open announced call
  with the same name; several same-name candidates are disambiguated only by
  the announcement at the terminal position, and no same-name candidate starts
  a new call instead of taking a differently named open call. An id-less
  terminal item then matches a unique open announced call with the same name,
  then the announcement at its own position among same-name candidates, then a
  lone or first still-open id-less announced call in output order, so a
  reordered terminal array completes the announced block rather than
  synthesizing a second call. If no open call matches, an id-less entry whose
  name and effective argument value agree with a closed id-less call is
  skipped as that call's repeat. An unannounced id-less call at a colliding
  announced position cannot be distinguished from a repeat and is dropped
  rather than assigned to a different call. An identified terminal call also
  matches only a same-name candidate whose identity kind is compatible: a
  candidate keyed by the same identity kind must carry that identity, while a
  cross-kind candidate stays eligible under the mixed-spelling rule. Position
  alone never moves arguments to a conflicting identity. The non-streaming
  decoder still rejects a call with both identities absent.
- A legacy Chat `message.function_call` is treated as answer-shaped for retry
  safety because the deprecated shape can represent either a tool invocation or
  a final answer, and it is not counted as a tool call. A Chat source that uses
  only that shape therefore records no tool-call count even though the call
  translates.
- Anthropic `metadata.user_id` is limited to 512 characters. A longer Chat or
  Responses `user` value is rejected before any upstream send rather than
  truncated, because truncation can collide two distinct identifiers.
  Chat/Responses `service_tier: priority` maps to Anthropic `speed: fast` with
  an approximation warning; `fast` maps without one.
- An Anthropic document `title` is limited to 500 characters. A longer source
  filename is dropped with a `document.title` warning while the document bytes
  still translate; strict mode rejects the loss before any upstream send.
- A response metadata map has no field on the Anthropic Message object, so a
  non-streaming translated response drops it with a warning. A translated
  Anthropic stream cannot report response-side warnings after its status line
  is committed.
- An image or document inside an Anthropic assistant turn is schema-valid
  because assistant content shares the message content type. The proxy keeps
  it and reports an `assistant.image` or `assistant.document` approximation
  warning; runtime acceptance of assistant media is not verified. User media
  is carried without that warning.
- A Responses `input_file` with inline `file_data` but no media-type
  declaration has one inferred from a data-URL prefix, a `.pdf` or common
  plain-text filename extension, or a base64 `%PDF-` signature. Unrecognized
  bytes are dropped with one warning while a file id or URL on the same part
  survives; a part with no remaining payload is dropped. Chat file parts keep
  their existing PDF-only default.
- A Chat response carries one refusal field, so multiple IR refusal parts are
  concatenated in block order. An empty part contributes nothing and never
  clears earlier text.
- A translated non-streaming response assumes a schema-valid source identity.
  A source that omits `id` or `model` yields an empty string on the target
  rather than an invented identity. The Responses envelope carries protocol
  defaults for request-echo fields (`parallel_tool_calls: true`, `tools: []`,
  `tool_choice: "auto"`, null `instructions`/`temperature`/`top_p`,
  `store: false`); a translated response cannot echo the source request's
  parameters without threading the request IR into the encoder.
- A Responses output tool-call `call_id` has no schema length bound, so a
  provider id longer than 64 characters is relayed unchanged. If the client
  replays that call as request history, the Responses request boundary enforces
  its 1..64 character reference rule and rejects the request with 400. The
  proxy never shortens or drops the output id, because that would break the
  correlation the client already received.
- Responses `include` options other than `message.output_text.logprobs` and a
  non-string Chat metadata value have no target equivalent and are dropped
  with a warning. The logprobs include maps to the target's request field, but
  translated responses do not carry provider logprob details.
- Deprecated Chat `functions` and `function_call` are converted to the
  canonical tool list and tool choice; a legacy per-assistant `function_call`
  pairs with its `role: function` reply by function name. Chat `prediction`,
  audio output, non-text `modalities`, and `web_search_options` have no target
  equivalent and are dropped with a warning.
- A Chat assistant tool call without an id gets a generated id and reports the
  recorded approximation. Pairing keeps the kind of reference. A non-empty
  `tool_call_id` answers the unanswered call with that exact id, so an explicit
  id is never shadowed by another call's function name; when no id matches, a
  generated call may still be addressed by the function name its client supplied
  in place of the missing id. A message `name` (with no `tool_call_id`) addresses
  a unique generated call. A reply with neither resolves only while exactly one
  generated call is unresolved and no other call is unresolved. Any reference
  matching more than one unresolved call fails closed, and an explicit call is
  never answered by its name alone.
- A `json_schema` response format must carry its `schema` object to translate;
  a schema-less `json_schema` is rejected with 400 because a cross-protocol
  target has no meaningful mapping for one. A native same-protocol Chat
  request is relayed unchanged, so the passthrough path still forwards it.
- A non-function tool type and malformed tool arguments are rejected before any
  upstream send.

`translation_loss` selects the policy. `warn` (default) drops the optional
fields this proxy models but the target cannot represent, and reports each one
in `X-Proxy-Warnings`. Provider-specific extension fields outside that modeled
vocabulary are not inspected or reported; the passthrough path relays them
unchanged. `reject` fails the request with 400 naming the fields before any
upstream send,
and it refuses every recorded loss, including a documented approximation such
as a clamped temperature, a moved developer message, a generated tool-call id
or a `json_object` response format, not only a field the target cannot
represent at all. The header is proxy-owned: a provider response header of the same name
is never forwarded. Its whole value is bounded at 1024 bytes; an over-cap list
is cut at the byte budget, possibly inside the final field, and ends with
`; truncated`. Response-side warnings
are never fatal; a non-streaming response reports them in `X-Proxy-Warnings`,
while a streaming response cannot add a header after the status line is
committed and drops them. The field is described under
[settings](operations.md#finding-a-setting).

A provider error envelope that arrives inside HTTP 200 is translated too, but
the client status becomes 502: a translated client would read 200 as a
successful completion. A non-200 translated error body is buffered before the
client sees it, at most 64 KiB, under `whole_document_read_timeout` (default 2
minutes), so a provider that holds an error stream open can delay the client
until that bound expires. Pre-commit failures (routing, request validation,
translation, or an oversized translated body) answer in the client protocol's
own error envelope. Non-streaming translation reads the complete upstream 200
body and the translated document under a fixed 32 MiB cap each; exceeding
either fails closed with 502 `response_too_large` before any client bytes. The
cap is an internal safety constant, not a configurable limit.

Streaming translation is event-driven and bounded. Each target encoder owns its
protocol's ordering invariants: Anthropic opens a block before any delta and
closes it before `message_delta`; Responses emits `response.created`, per-item
lifecycle events and one terminal event; Chat emits the role chunk, deltas, one
`finish_reason` chunk and one `[DONE]`. The official Anthropic event flow closes
every content block before `message_delta`. A nonconforming source that sends a
content delta after `message_delta` is not given a runtime special case, because
the terminal contract and the target encoder ordering still own the turn. A truncated upstream is closed with the
client protocol's own error event, never a fabricated clean finish. The
Responses encoder can emit `response.cancelled` for a canonical cancelled
finish. The current `ResponseStreamEvent` union lists completed, failed and
incomplete terminals, while the `Response` status enum includes cancelled; the
event is kept as a forward-compatible extension and a strict client may treat
it as unknown. Synthetic Responses error and terminal frames emitted on a
committed socket after an accepted non-completion status restart
`sequence_number` at 0 and 1; relayed events before that keep their original
sequence. The degenerate terminal-region substitution emits the same error plus
`response.failed` pair at 0 and 1 even when earlier streamed events carried
higher numbers, because the relay does not retain the provider's counter. Both
reset cases are documented contract gaps, not reproduced client failures; the
terminal event is final for the turn.

Streaming citations are not translated. A Responses
`response.output_text.annotation.added` event is not decoded, and a streamed
target does not emit citations or annotations; non-streaming responses do carry
url citations. A citation stream event is a separate feature, not a silent
claim of streaming parity.

A non-streaming translated Responses message item always carries
`status: "completed"`. The response-level `status` owns lifecycle and can be
`incomplete` or `failed` even when individual items completed; a truncated
tool turn is reported there, not by rewriting an item's status. In a
translated stream, `response.output_item.added` carries `status:
"in_progress"` and the closing `response.output_item.done` carries
`"completed"`; the terminal response status still owns the turn lifecycle.

Parallel tool calls translated to Anthropic open their indexed content blocks
concurrently (for example start 0, start 1, delta 0, delta 1, stop 0, stop 1).
The blocks carry the index the streaming contract requires, and SDKs accumulate
by index; a strict assumption of sequential block lifecycles would need a
separate encoder design, not a per-request special case. A streamed Responses
tool call is validated by its terminal class. A completed terminal fails closed
on a partial function-call value. Incomplete and cancelled terminals do the
same, because both are normal turn boundaries and an executable empty object
must never close a call the model did not finish. On those two terminals a call
with missing, null or whitespace arguments is also rejected unless the source
explicitly sent `{}`. When validation fails, the target abort closes non-tool blocks but
leaves an open tool block unclosed: Anthropic emits no `content_block_stop` over
empty input, and Responses emits no done function-call item with empty
arguments. The proxy error frame terminates the stream, so a client that keeps
reading cannot execute an empty call. A failed, errored or truncated terminal
instead drops only the unparseable call and keeps the provider error or
truncation. An already-announced call on one of those three terminals can still
close with `{}` because its block already reached the client; Chat reports a
cancelled finish as `stop`, so that accepted limit is not distinguishable
there. A Chat `[DONE]`
without a preceding non-null finish reason is a clean transport terminator but
an empty finish: a Responses target receives `response.incomplete` with
`incomplete_details: null`, and the record reports `finish=other`. That
degenerate classification is deliberate rather than a fabricated completion.

## Bundled compatibility profiles

The generated [configuration example](../proxy.example.yaml) enables the Grok,
OpenCode Zen and Cursor profiles and the opencode sub-conversation tracking
exemplar (the `promptCacheKey` tracked param, strip on). `config.Example()` in
[internal/config](../internal/config) owns the exact profile mappings, header
values and that tracking entry; built-in `config.Default()` remains
provider-neutral. The image installs the example into fresh config volumes.
Existing saved configurations are not silently replaced.

These are operator-selected compatibility snapshots, not verified current public
API fingerprints or permission to use an account service. They contain no keys
and do not choose the upstream URL or protocol adapter for a request. Review
them for your integration and remove or adjust any unwanted profile in Settings.

The Grok profile supplies client-identity/request-ID headers and model-modality
enrichment through the model discovery endpoint. Model `version` is not context
length, and the profile does not invent that mapping. Standard usage detection
and USD-tick scaling already handle recognized cost fields; custom `cost_keys`
must name USD values rather than bypassing that scaling. Streaming cost requires
the upstream to report usage; missing cost is not evidence of a free request.
See xAI's [model reference](https://docs.x.ai/developers/rest-api-reference/inference/models)
and [cost tracking](https://docs.x.ai/developers/cost-tracking).

The Cursor profile supplies CLI compatibility, allowed-tool and request-ID
headers. Its ghost-mode field is a supplied wire preference, not a privacy or
retention guarantee. Cursor's [public authentication documentation](https://cursor.com/docs/cli/reference/authentication)
does not establish a stable public contract for this native bridge or its
compatibility headers. Keep authorization and current service support separate
from a successful local fixture test.

The OpenCode Zen profile supplies the opencode CLI's client identity on the
ordinary OpenAI-compatible relay: the CLI's composite `User-Agent`,
`x-opencode-client` and `x-opencode-project`, plus the CLI's per-conversation
`x-opencode-request` and `x-opencode-session` headers, which the
`{{opencode-msg-id}}` and `{{opencode-ses-id}}` templates mint fresh per
request in the CLI's 26-character identifier format (provider headers
override client-forwarded values; the ids carry no session state, so
replacing a genuine client's own ids is functionally neutral). The profile
also sets `ensure_tools: [bash, read]`. A check on 2026-09-24 found zen's
free tier gated on that wire signature rather than on TLS, header order or
credentials: with the id headers in shape and a chat `tools` array containing
the `bash` and `read` tool names, the gated free models served even under
the anonymous `public` bearer. `ensure_tools` injects missing names as inert
stubs. When the client sent no tools of its own, an absent, null, or `auto`
`tool_choice` is forced to `none`, while an explicit `none` stays; a
`required`, named, or malformed choice skips injection entirely, because
forcing `none` would contradict the client and injecting would let the model
call an undeclared stub. When the client already declares at least one tool,
the configured names are added to that list and an explicit `required` or
named `tool_choice` stays in force. A body that already satisfies the
signature is relayed unchanged, apart from any configured request override or
sub-conversation strip that runs before it. The stubs are Chat-shaped, so they
are injected only into an OpenAI Chat client body on a Chat outbound wire; an
Anthropic, Responses, Cursor, or arbitrary body is never rewritten.
The same check found the signature also requires a streaming request
(`"stream": true`): non-streaming requests to the gated free models get the
provider's `FreeTierError` 403, so clients of those models must stream (the
CLI always does). That observation is about that day, not an entitlement: the
profile is not an account, key or permission, and there is no zen login
helper. Zen's models are listed in
[OpenCode's Zen documentation](https://opencode.ai/docs/zen/).

Every profile is editable operator configuration, in the private config file
or in Settings under Providers (each card edits the field maps, ensured tool
names and upstream headers, with hot reload): changing a signature tool set
or a header value is an edit, never a code change. To reproduce another
first-party client's signature for a different provider, work from evidence
top down: capture the client's own requests against a local logging upstream,
strip one signal at a time against the real endpoint (TLS ClientHello, header
order and values, body fields, credentials) until only the gate remains, then
express the surviving identity as a provider profile - static headers
directly, per-request dynamic values as `{{uuid4}}`, `{{platform}}` or a new
mint placeholder in the single template expander, and required tool names as
`ensure_tools`. The zen profile above is one worked example of that recipe,
not a hardcoded mechanism.

## Anthropic-compatible Messages

`anthropic` works in both directions: as an upstream selector for a Chat,
Responses or Anthropic client, and as the inbound protocol for
`POST /v1/messages`. Translation covers message history, system text,
images, tools and results, thinking blocks, cache control, selected parameters,
usage and streaming events. Fields with no target representation follow the
[translation loss policy](#cross-protocol-translation). A configured
[sub-conversation tracking](operations.md#sub-conversation-tracking) value is
injected as a top-level `prompt_cache_key`. That field is a proxy tracking
extension, not an Anthropic cache control: Anthropic caching uses
`cache_control`, and the injection is only safe for upstreams that tolerate
unknown top-level fields. The client's own spelling of the tracked field is
otherwise dropped by the allowlist. The caller
must supply the native endpoint's correct auth/version/path headers; the
[protocol example](protocol.md#url-and-auth-examples) demonstrates the mechanism.

Stored parameter and conversation-composition metadata describe the translated
upstream body, not an exact copy of the original request. For a native or
translated Anthropic body, the recorded stop count, tool choice, parallel-tool
flag, reasoning effort, response format and service tier come from the
Anthropic spellings (`stop_sequences`, the `tool_choice` type/name and
`disable_parallel_tool_use`, `output_config`, and `speed`) when the Chat
spelling is absent. A body that combines the Responses `max_output_tokens` spelling with a Chat
`max_tokens` or `max_completion_tokens` spelling is rejected with HTTP 400
before translation or any upstream send. If the caller omits
all three output-limit spellings (`max_tokens`, `max_completion_tokens` and
`max_output_tokens`), translation supplies `anthropic_default_max_tokens`,
which can therefore appear in request details as a captured value. When a
request supplies both Chat spellings, translation prefers
`max_completion_tokens`; the proxy's request decode applies the same precedence
when capturing the cap, so the captured value always agrees with the one the
translated body carries upstream.

A translated Chat body always spells its output cap `max_tokens`: it has the
widest OpenAI-compatible support. OpenAI documents `max_tokens` as deprecated
and its o-series reasoning models require `max_completion_tokens` instead. A
native Chat client may send either spelling, and the decoder prefers
`max_completion_tokens`, but a translated Chat request still carries
`max_tokens` and can therefore be refused by a target that accepts only the
newer spelling.

An Anthropic target request forwards `temperature` (clamped to 0..1), `top_p`
and `top_k` for older and compatible upstreams. The current Anthropic
`MessageCreateParamsBase` request type no longer lists those sampling fields,
so a strict current endpoint may reject them. The forwarding is kept as a
compatibility choice rather than silently dropping client values; revisit it
if a live current endpoint is verified to reject them.

Non-streaming translation buffers the full response before committing status;
a translation failure can therefore return a real error status. A translated
stream is emitted in the client's protocol, not as OpenAI-shaped events, and it
observes original-source custom cost privately before final accounting.
Cache-inclusive native usage conversion has one owner; never add reasoning
twice when it is already part of output.

A valid native stop marker closes the turn. Missing completion/error state must
not be converted into a fabricated clean finish. Legitimate empty/refusal/native
tool turns are not permission to execute upstream tools again. General native
multi-line event assembly still has limits; fixture coverage is not a blanket
protocol-conformance claim.

Native model discovery translates and completes supported paginated lists under
the shared discovery time/byte/page budget. Optional enrichment fills missing
fields without replacing upstream-owned values; it draws from the same
discovery entry budget and degrades to the base list when the remainder cannot
hold its metadata source.

Owners: [format/anthropic.go](../internal/format/anthropic.go),
[format/models.go](../internal/format/models.go),
[proxy/relay.go](../internal/proxy/relay.go), and
[proxy/models.go](../internal/proxy/models.go).

## Cursor Connect bridge

`cursor` is a text/tool bridge from OpenAI chat to the native agent.v1
bidirectional Connect/protobuf protocol. It currently requires an OpenAI Chat
client: a Responses or Anthropic client targeting `cursor` is rejected with a
clear 400 instead of receiving the wrong response shape. It is not a general multimodal bridge
or a promise of a stable public provider API. Check the provider's current
access terms and supported integration options before using account credentials;
the project does not establish permission on your behalf.

The run owns a bidirectional HTTP/2 transport, framed messages, heartbeat,
context/KV replies and one ordered event queue. Malformed known nested messages,
invalid usage or error terminal frames fail explicitly. A validated turn-ended
marker plus EOF remains a supported completion convention; bare EOF without
completion is not a clean success. The event queue carries an internal retained
byte budget and the pending tool-call map internal count and payload byte caps;
the KV blob store carries per-run byte and entry caps. The parked-run store
enforces aggregate budgets across all parked runs: retained pending plus queued
event bytes, retained blob bytes, and blob entries. It holds one per-run
pending-plus-queued allowance free for the run being actively driven, and it
rejects admission or growth that would cross an aggregate bound. The entry caps
bound unique zero-length blobs that the byte cap cannot charge. Exceeding a run
or aggregate parked bound fails closed with a stable descriptive error instead
of retaining unbounded memory; an over-cap history fails the request and a
refused park cold-starts the next turn. None of these bounds is
operator-tunable.

When the upstream requests a client tool, the proxy returns OpenAI tool calls
and parks the native stream. A later matching tool-result tail can resume that
same stream. Full route/auth/model/client scope prevents cross-identity reuse.
Registration, expiration and close share the run-store owner; client cancellation
ends active work, while a legitimate parked run lives until its configured TTL,
upstream close, capacity eviction or process restart.

For each new send, the tighter of `upstream_timeout` and
`X-Proxy-Timeout-Ms` bounds waiting for response headers. That wait deadline is
removed after headers; it is not an overall body deadline. Resuming an existing
parked stream does not open another send. The default path is the native Run
RPC; an explicit `X-Proxy-Path` is appended to the base with a leading slash,
without the HTTP relay's shared-segment deduplication. Query overrides and the
incoming query are not used.

A lost park cold-starts from provided history. The quality-retry path may re-ask
an empty continuation; exhausted/invalid continuations surface errors rather
than silently completing. Cursor's wire reports one combined output total with
no answer/reasoning split, so the generic mid-thinking rescue
(`thinking_retries`) does not apply here: a Cursor turn that produced nothing
is the existing empty-turn re-ask's business. This is not exactly-once
execution, checkpoint persistence, or a guarantee that an upstream keeps a
parked stream alive.

Output usage comes from validated native counters. Input may use checkpoint
context occupancy or a documented estimate when unavailable; unreported cache
and reasoning splits are not invented. Fused model spellings are decomposed by
the existing protocol owner, not copied as raw native parameters. Stored model
metadata uses the canonical base ID, not the caller's fused spelling.

Model discovery uses the native unary usable-models service and returns the
constructed OpenAI list. Context metadata is not guessed from a static price or
capability table. Discovery budgets cover this path as well as optional enrichment.

Owners: [proxy/cursor_bidi.go](../internal/proxy/cursor_bidi.go),
[proxy/cursor_store.go](../internal/proxy/cursor_store.go),
[format/cursor_run.go](../internal/format/cursor_run.go), and neighboring
format/protocol tests. Keep protocol constants distinct from the configurable
[compatibility profile](#bundled-compatibility-profiles); built-in defaults remain
neutral and the example does not establish provider support.

### Cursor service and protobuf versions

The service name, transport version and codec are different layers, not two
protobuf implementations running together:

| Layer | What millivolt uses |
| --- | --- |
| Cursor service | `agent.v1.AgentService`: bidirectional `Run` and unary `GetUsableModels`. The `v1` is part of the service namespace. |
| Connect transport | `Connect-Protocol-Version: 1`. Run uses framed `application/connect+proto`; model discovery uses unframed `application/proto`. |
| Protobuf codec | The limited handwritten encoder/decoder in [proto.go](../internal/format/proto.go), using the adapter's mapped fields. There is no Go protobuf APIv1 or APIv2 dependency. |

The [Connect specification](https://connectrpc.com/docs/protocol/) defines its
version header separately from protobuf. Likewise, Go's
[APIv1/APIv2 distinction](https://go.dev/blog/protobuf-apiv2) and Buf's
[JavaScript runtime v2](https://buf.build/blog/protobuf-es-v2) are library versions,
not instructions to change `agent.v1` to `agent.v2` or protobuf syntax to proto2.
The small codec avoids a generated-runtime dependency, but requires maintaining
explicit field mappings and does not promise general protobuf conformance.

A read-only check on 2026-09-06 of the installed Cursor CLI bundle
`2026.08.11-e8db854` found `agent.v1` descriptors with proto3 semantics and
`@bufbuild/protobuf` **1.10.1**, not v2. This is evidence about that inspected
client artifact, not a claim about every client release or the current server.
No live login or inference was used for this check.

## First-login helpers

[grok-login.sh](../scripts/grok-login.sh) and
[cursor-login.sh](../scripts/cursor-login.sh) obtain initial account credentials
through interactive browser approval. Run them on a trusted Linux host or
workstation, not inside the minimal proxy image. Neither a running millivolt
instance nor the provider's CLI/desktop application is required by these scripts.
Account eligibility, permitted integrations and model access remain the provider's
decision; successful authentication does not establish subscription entitlement
or a free inference allowance.

The scripts do not write files or update client configuration. They print secret
credential headers to standard output, and approval links/progress to standard
error. Keep the terminal private: do not use shell tracing (`bash -x`), CI logs,
screenshots or shared output redirection. Review a downloaded script before
running it. These helpers have no command-line option parser; configure them
with the environment variables below, not `--help` or invented flags.

### Prerequisites and obtaining the scripts

Both need Bash, `curl`, `jq` and standard Linux command-line utilities. Cursor
also needs `openssl` and a curl build with HTTP/2 support; check that
`curl --version` lists `HTTP2` in Features, as described in the
[curl HTTP/2 guide](https://everything.curl.dev/http/versions/http2.html).
`xdg-open` is optional: without it, open
the printed approval URL yourself, including from another browser-capable device.

Source checkouts already contain both helpers under `scripts/`. Image-only
deployments can download just the desired helper on the host, without cloning
or rebuilding millivolt:

```sh
curl -fsSL https://raw.githubusercontent.com/LLM-4-People/millivolt/main/scripts/grok-login.sh -o grok-login.sh
curl -fsSL https://raw.githubusercontent.com/LLM-4-People/millivolt/main/scripts/cursor-login.sh -o cursor-login.sh
```

Use a new directory so downloads do not overwrite existing files. For a pinned
installation, replace `main` in those URLs with the reviewed commit or release
tag. Standalone downloads use `bash ./grok-login.sh` or `bash ./cursor-login.sh`
instead of the checkout paths below. Do not pipe a download directly into Bash.

### Grok login

From a source checkout:

```sh
bash scripts/grok-login.sh
```

Open the printed verification URL, check the account and approve the displayed
device code. The helper polls for approval and prints `Authorization: Bearer`
with the access token, `X-Proxy-Refresh-Token` when supplied by the provider, and
`X-Proxy-Base-URL: https://api.x.ai/v1`. This is an account-login token flow, not
creation of a pay-as-you-go `xai-` API key. It does not select a model or add a
native-format header; the ordinary OpenAI-compatible relay is used.

For SSH or a headless terminal, suppress automatic browser opening:

```sh
NO_OPEN=1 bash scripts/grok-login.sh
```

### Cursor login

From a source checkout:

```sh
bash scripts/cursor-login.sh
```

Open the printed login link and approve the account sign-in. The helper creates
a PKCE challenge, requests HTTP/2 when polling the account endpoint, and prints the access
token, optional refresh token, upstream base URL and `X-Proxy-Format: cursor`.
Use the printed format header to select the [Cursor bridge](#cursor-connect-bridge);
a provider profile by itself does not enable translation.

The same headless switch is supported:

```sh
NO_OPEN=1 bash scripts/cursor-login.sh
```

### Configure the client after approval

Keep the client's API base pointed at **millivolt**, such as
`http://127.0.0.1:8080/v1`, using your actual host/port or protected ingress URL.
The helper's `X-Proxy-Base-URL` instead identifies the upstream; do not replace
the client's millivolt URL with it.

| Printed value | Client configuration |
| --- | --- |
| Access token after `Authorization: Bearer ` | Put only the token in an SDK's API-key field, or use the complete printed Authorization header if configuring headers directly. Do not add `Bearer` twice. |
| `X-Proxy-Base-URL` | Add as a custom request header. |
| `X-Proxy-Refresh-Token`, when present | Add as a custom request header if using [automatic refresh](#token-refresh); the client must retain rotated credentials returned by the proxy. |
| `X-Proxy-Format: cursor` | Add for the Cursor helper only. |

Select a model your account is authorized to use. Keep tokens in private client
configuration or a secret store, not `proxy.example.yaml` or a Git-tracked file.
Millivolt does not register these credentials globally. If your ingress consumes
Authorization, follow the [separate upstream-key setup](reverse-proxy.md#credentials-and-access-control).

### Helper environment overrides

These affect the login process only, not server Settings or the saved client.
Use only origins and OAuth registrations you trust: they receive login material.
Defaults remain owned by each script.

| Variable | Helper | Meaning and default |
| --- | --- | --- |
| `NO_OPEN` | Both | Any nonempty value suppresses `xdg-open`; unset or empty permits it. The URL is still printed. |
| `GROK_LOGIN_TIMEOUT` | Grok | Approval-poll budget in seconds; `600`. Use a positive integer. |
| `XAI_AUTH_BASE` | Grok | Login origin; `https://auth.x.ai`. Does not change the printed inference base. |
| `XAI_OAUTH_CLIENT_ID` | Grok | OAuth registration; defaults to the client ID shared with the proxy's xAI refresh implementation. |
| `CURSOR_LOGIN_TIMEOUT` | Cursor | Approval-poll budget in seconds; `300`. Use a positive integer. |
| `CURSOR_API_BASE` | Cursor | Poll origin and printed inference base; `https://api2.cursor.sh`. |
| `CURSOR_LOGIN_BASE` | Cursor | Browser login origin; `https://www.cursor.com`. |

Timeouts are polling budgets, not strict end-to-end deadlines: a current HTTP
operation or sleep can extend elapsed time. Changing Grok's auth origin/client
ID does not change millivolt's xAI refresh endpoint/registration, so a custom login
can be incompatible with renewal. Cursor refresh targets the request's base URL
and still requires a recognized refresh mechanism. These overrides do not
establish support for arbitrary identity providers or compatible service clones.

### Login troubleshooting

| Symptom | What to check |
| --- | --- |
| Required tool not found | Install the named dependency on the host running the helper, not in the proxy image. |
| No browser opens | Open the printed URL manually. `NO_OPEN=1` deliberately disables automatic opening. |
| Cursor reports unsupported HTTP/2 or keeps waiting | Check `curl --version` for HTTP2 support and inspect its diagnostics. The helper keeps polling non-200 responses until its budget expires. |
| Approval times out, expires or is denied | Verify the account/browser approval, then rerun for a fresh login challenge. Never reuse an expired code. |
| No refresh token is printed | The helper warns and omits that header. Millivolt cannot renew without it; obtain another login when the access token expires. |
| Login succeeds but inference returns 401/403 | Check client headers, upstream base, account permissions and model availability. Login alone does not prove inference access. |
| Grok or Cursor repeatedly returns `token_expired` | The client must adopt the returned access token and any rotated refresh token, then retry. Repeating the old credentials does not complete handback. |
| Refresh is rejected | Recheck provider access and perform a new interactive login. Do not publish token responses or credential headers for troubleshooting. |

## Token refresh

With `auto_token_refresh` enabled, a request may supply its own
`X-Proxy-Refresh-Token`. The proxy can exchange an expiring JWT for supported
mechanisms; it stores neither a credential database nor reusable refresh state.
The JWT expiry check is not signature authentication.

**Grok and Cursor use the same inference handback behavior.** A successful
exchange returns HTTP 401 with error type `authentication_error` and code
`token_expired`. The error object includes `access_token` and, when the
exchange returned a different refresh token, `refresh_token`. The same
values are in the error message. No inference is sent or recorded for that
handback. The client stores the returned access token, adopts any returned
refresh token, and retries the original request.

Starting with **0.2.0**, Cursor no longer refreshes in place on inference requests.
Clients that previously relied on that behavior must now handle the same 401
handback as Grok. Starting with **0.3.1**, credentials are in that error body,
not `X-Proxy-Access-Token` / `X-Proxy-Refresh-Token` response headers. Update
client handling before upgrading; a generic client that only reads those
headers cannot complete automatic renewal. Retry only after adopting the
fresh access token, not by blindly replaying an old key.

Model discovery refreshes transparently instead of requiring the handback
workflow, but does not return updated tokens to the client.
Missing refresh tokens, unknown mechanisms, non-JWT keys or disabled
refresh leave ordinary forwarding unchanged. Exchange failure falls back to the
original upstream credential, allowing the provider to decide authentication.

Use the [first-login helpers](#first-login-helpers) to obtain initial credentials.
They are not proxy startup dependencies. Treat terminal output and returned tokens
as secrets; never paste them into issues or commit them. Adopt a returned
refresh token when present; if a successful exchange omits it, including when
the provider echoed the same refresh token, retain the existing refresh token
rather than replacing it with an empty value.

The implementation lives in [tokenrefresh.go](../internal/proxy/tokenrefresh.go).
Provider/account behavior can change; historic successful observations are not
compatibility or authorization guarantees.
