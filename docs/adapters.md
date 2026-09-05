# Optional adapters and token refresh

The normal OpenAI-compatible relay is separate from optional protocol bridges.
Select an adapter with `X-Proxy-Format`; do not infer one from a provider label.
See [protocol](protocol.md) for routing/auth headers and
[operations](operations.md#known-limits) for current limitations.

## Anthropic-compatible Messages

`anthropic` translates common OpenAI chat requests/responses: message history,
system text, tools/results, selected parameters, usage and streaming events.
It is not complete preservation of every provider-specific field. The caller
must supply the native endpoint's correct auth/version/path headers; the
[protocol example](protocol.md#url-and-auth-examples) demonstrates the mechanism.

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

A lost park cold-starts from provided history. The quality-retry path may re-ask
an empty continuation; exhausted/invalid continuations surface errors rather
than silently completing. This is not exactly-once execution, checkpoint
persistence, or a guarantee that an upstream keeps a parked stream alive.

Output usage comes from validated native counters. Input may use checkpoint
context occupancy or a documented estimate when unavailable; unreported cache
and reasoning splits are not invented. Fused model spellings are decomposed by
the existing protocol owner, not copied as raw native parameters.

Model discovery uses the native unary usable-models service and returns the
constructed OpenAI list. Context metadata is not guessed from a static price or
capability table. Discovery budgets cover this path as well as optional enrichment.

Owners: [proxy/cursor_bidi.go](../internal/proxy/cursor_bidi.go),
[proxy/cursor_store.go](../internal/proxy/cursor_store.go),
[format/cursor_run.go](../internal/format/cursor_run.go), and neighboring
format/protocol tests. Keep protocol constants distinct from configurable
provider headers; no unverified first-party fingerprint values belong in public
defaults.

## Token refresh

With `auto_token_refresh` enabled, a request may supply its own
`X-Proxy-Refresh-Token`. The proxy can exchange an expiring JWT for supported
mechanisms; it stores neither a credential database nor reusable refresh state.
The JWT expiry check is not signature authentication.

Two response behaviors are explicit:

| Mechanism | Inference behavior |
| --- | --- |
| Handback (xAI OAuth) | Return 401 with `token_expired` and fresh `X-Proxy-Access-Token` / `X-Proxy-Refresh-Token`; no inference is sent. The client adopts both and retries. |
| In-place (Cursor exchange) | Use the refreshed access token for this request and return the fresh pair in those response headers. |

Model discovery refreshes transparently instead of requiring the handback
workflow. Missing refresh tokens, unknown mechanisms, non-JWT keys or disabled
refresh leave ordinary forwarding unchanged. Exchange failure falls back to the
original upstream credential, allowing the provider to decide authentication.

The optional [Cursor login](../scripts/cursor-login.sh) and
[Grok login](../scripts/grok-login.sh) helpers are interactive first-login tools,
not proxy startup dependencies. They print credentials and do not establish a
secure storage location for the client. Treat terminal output and returned token
headers as secrets; never paste them into issues or commit them.

The implementation lives in [tokenrefresh.go](../internal/proxy/tokenrefresh.go).
Provider/account behavior can change; historic successful observations are not
compatibility or authorization guarantees.
