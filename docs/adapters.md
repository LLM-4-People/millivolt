# Upstream compatibility, adapters and token refresh

The client-facing OpenAI-compatible interface is separate from the upstream
protocol. Select an adapter with `X-Proxy-Format`; do not infer one from a
provider label.
See [protocol](protocol.md) for routing/auth headers and
[operations](operations.md#known-limits) for current limitations.

## Client and upstream compatibility

Your client connects to millivolt using an OpenAI-compatible API. millivolt
then relays or translates the request according to the selected upstream format:

| Client → millivolt | millivolt → upstream | `X-Proxy-Format` |
| --- | --- | --- |
| OpenAI-compatible requests | OpenAI, xAI/Grok and other hosted or local OpenAI-compatible APIs | `openai` (default) |
| Supported OpenAI Chat Completions | [Anthropic-compatible Messages](#anthropic-compatible-messages) | `anthropic` |
| Supported OpenAI Chat Completions with text and tools | [Cursor agent.v1 Connect/protobuf](#cursor-connect-bridge) | `cursor` |

xAI/Grok uses the ordinary OpenAI-compatible relay, not a separate native
adapter. Other providers and local model servers can use that same path when
they expose a compatible API; there is no fixed provider registry. Native
translation is limited to the implemented adapters and their supported fields,
not every OpenAI endpoint or arbitrary provider protocol. Translated responses
and stream events are returned in OpenAI-compatible shapes.

Clients must support custom routing headers or middleware that adds them.
For native formats, supply the adapter selection and the upstream's required
authentication, version and path headers; the
[Anthropic example](protocol.md#url-and-auth-examples) and
[Cursor login setup](#cursor-login) show these choices. Provider profiles enrich
headers and metadata but do not select an adapter or grant provider access.

## Bundled compatibility profiles

The generated [configuration example](../proxy.example.yaml) enables Grok and
Cursor profiles. `config.Example()` in [internal/config](../internal/config)
owns their exact mappings and header values; built-in `config.Default()` remains
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

## Anthropic-compatible Messages

`anthropic` translates common OpenAI chat requests/responses: message history,
system text, tools/results, selected parameters, usage and streaming events.
It is not complete preservation of every provider-specific field. The caller
must supply the native endpoint's correct auth/version/path headers; the
[protocol example](protocol.md#url-and-auth-examples) demonstrates the mechanism.

Stored parameter and conversation-composition metadata describe the translated
upstream body, not an exact copy of the original request. If the caller omits
both output-limit fields, translation supplies `anthropic_default_max_tokens`,
which can therefore appear in request details as a captured value.

Non-streaming translation buffers the full response before committing status;
a translation failure can therefore return a real error status. Streaming
translation emits OpenAI-shaped events and observes original-source custom cost
privately before final accounting. Cache-inclusive native usage conversion has
one owner; never add reasoning twice when it is already part of output.

A valid native stop marker closes the turn. Missing completion/error state must
not be converted into a fabricated clean finish. Legitimate empty/refusal/native
tool turns are not permission to execute upstream tools again. General native
multi-line event assembly still has limits; fixture coverage is not a blanket
protocol-conformance claim.

Native model discovery translates and completes supported paginated lists under
the shared discovery time/byte/page budget. Optional enrichment fills missing
fields without replacing upstream-owned values.

Owners: [format/anthropic.go](../internal/format/anthropic.go),
[format/models.go](../internal/format/models.go),
[proxy/relay.go](../internal/proxy/relay.go), and
[proxy/models.go](../internal/proxy/models.go).

## Cursor Connect bridge

`cursor` is a text/tool bridge from OpenAI chat to the native agent.v1
bidirectional Connect/protobuf protocol. It is not a general multimodal bridge
or a promise of a stable public provider API. Check the provider's current
access terms and supported integration options before using account credentials;
the project does not establish permission on your behalf.

The run owns a bidirectional HTTP/2 transport, framed messages, heartbeat,
context/KV replies and one ordered event queue. Malformed known nested messages,
invalid usage or error terminal frames fail explicitly. A validated turn-ended
marker plus EOF remains a supported completion convention; bare EOF without
completion is not a clean success.

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
than silently completing. This is not exactly-once execution, checkpoint
persistence, or a guarantee that an upstream keeps a parked stream alive.

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
