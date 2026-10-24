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
differ. An absent `X-Proxy-Format` is always a byte-transparent passthrough,
and an explicit format equal to the client protocol is also passthrough.

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
usage. Same-protocol requests never enter the IR: they relay byte-for-byte.

Carried wherever the target protocol represents them: text, images (URL,
base64 or file id), documents, audio where the target accepts it,
system/developer/user/assistant turns, function tools, tool choice, parallel
tool use, tool calls and results, structured outputs, reasoning effort and
summary, stop sequences, sampling parameters, metadata, usage and cache-token
accounting.

Protocol-only artifacts are preserved for the same family and reported when a
target cannot consume them:

- Anthropic thinking and redacted-thinking blocks keep their opaque signatures.
- OpenAI Responses reasoning items keep `encrypted_content` and item ids.
  A cross-family encrypted reasoning artifact is never replayed into another
  provider's field; it is dropped with a warning.
- Provider-executed blocks such as server tool use and web search results have
  no cross-protocol equivalent and are dropped with a warning.
- `previous_response_id`, `item_reference`, `background`, `conversation`,
  `store: true` and prompt templates require provider-side state and are
  rejected when the target cannot honor them.
- Responses `include` options other than `message.output_text.logprobs` and a
  non-string Chat metadata value have no target equivalent and are dropped
  with a warning. The logprobs include maps to the target's request field, but
  translated responses do not carry provider logprob details.
- Deprecated Chat `functions` and `function_call` are converted to the
  canonical tool list and tool choice; a legacy per-assistant `function_call`
  pairs with its `role: function` reply by function name. Chat `prediction`,
  audio output, non-text `modalities`, and `web_search_options` have no target
  equivalent and are dropped with a warning.
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
as a clamped temperature, a moved system message, a generated tool-call id or
a `json_object` response format, not only a field the target cannot represent
at all. The header is proxy-owned: a provider response header of the same name
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
own error envelope.

Streaming translation is event-driven and bounded. Each target encoder owns its
protocol's ordering invariants: Anthropic opens a block before any delta and
closes it before `message_delta`; Responses emits `response.created`, per-item
lifecycle events and one terminal event; Chat emits the role chunk, deltas, one
`finish_reason` chunk and one `[DONE]`. A truncated upstream is closed with the
client protocol's own error event, never a fabricated clean finish.

Streaming citations are not translated. A Responses
`response.output_text.annotation.added` event is not decoded, and a streamed
target does not emit citations or annotations; non-streaming responses do carry
url citations. A citation stream event is a separate feature, not a silent
claim of streaming parity.

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
stubs and adds `tool_choice: none` when the client sent no tools of its own,
so the stubs can never be invoked; a body that already satisfies the
signature relays byte-identical. The stubs are Chat-shaped, so they are
injected only into an OpenAI Chat client body on a Chat outbound wire; an
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
upstream body, not an exact copy of the original request. A body that combines the Responses `max_output_tokens` spelling with a Chat
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
