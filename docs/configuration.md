# Configuration

How the configuration surface is owned, found and edited: the generated example,
the settings schema, request overrides and sub-conversation tracking.

> [!WARNING]
> `provider_aliases` is a durable history rename, and a `request_overrides`
> header value is operator-supplied and stamped onto a request whose
> destination the caller chooses with `X-Proxy-Base-URL`. Back up before an
> intentional rename, and review both sections before enabling them.

## Configuration and CLI

[proxy.example.yaml](../proxy.example.yaml) is generated from the canonical
`config.Example()` and schema documentation. `Example()` starts with neutral
`config.Default()` values and adds the enabled
[Grok, OpenCode Zen and Cursor compatibility profiles](adapters.md#bundled-compatibility-profiles)
and the opencode sub-conversation tracking exemplar (the `promptCacheKey`
tracked param, strip on). It is not mutable repository state. Source
deployments copy it to ignored local `proxy.yaml`; images bundle their own copy
for fresh config volumes. Do not start Settings against the committed example.

The proxy's flags are listed once, with their registration defaults and usage
text, in the [settings reference](settings.md#proxy-cli-flags). The effective
`-listen` value is validated after every override, and a handoff child rewrites
`-pid-file` with its own pid on boot so a supervisor keeps tracking the live
process. The three output-only flags are mutually exclusive and exit without
loading local config, opening a database or starting runtime services.

Precedence is built-in defaults, then the YAML file, then CLI overrides.
A missing file loads defaults; verify the path rather than assuming a typo
created a new empty overlay. On startup, unknown keys, unknown nested fields and
extra YAML documents are dropped and the file is rewritten without them. An
invalid recognized value fails startup without rewriting the file, and an
unparseable file is moved to `PATH.invalid` and replaced with the documented
defaults, except an ambiguous duplicate top-level key, which fails closed and
stays in place. Because replacing it with defaults would silently widen the
bind and drop the origin allowlist, an unparseable file first has `listen` and
`allowed_base_urls` salvaged from it block by block and applied over the
defaults; both `(unparseable yaml)` and `<key> (salvaged)` are reported in the
boot log and `last_reload`. A named key with no usable value, or a salvaged
value the config rejects, fails startup with the original file still in place.
Reload rejects an invalid document without rewriting it.
Settings and restore merge treat a missing file as defaults, while
an invalid or unreadable document fails closed and is reported instead of
substituting a default. Healthcheck is stricter: a named file that cannot be
read fails the probe, and only an empty `-config` reaches the built-in
defaults. Backup capture is stricter: a missing config file is an
error because there is nothing to archive. Settings POST still rejects invalid
inputs. CLI-overridden fields cannot be saved from Settings. Healthcheck loads
whatever file it is given read-only and never rewrites it.

To regenerate the public example and the settings reference after changing
their canonical owners:

```sh
go run ./cmd/proxy -print-example-config > proxy.example.yaml
go run ./cmd/docgen -write
```

The shared checks verify generated-file consistency. Built-in defaults remain
neutral; compatibility-profile values belong only in `config.Example()`, never
consumer fallbacks or private credentials. Optional mappings are mechanisms,
not provider registrations, for example:

```yaml
providers:
  neutral.example:
    cost_keys: [billing.charge_usd]
    usage_keys:
      input_tokens: prompt_toks
```

Use the derived provider label as the map key. The example file and Settings
schema document other maps, model rules and header templates; do not maintain
a second complete list of defaults here.

### Finding a setting

The generated [settings reference](settings.md) is the complete field list:
every setting with its type, default, accepted values and hot-reload or restart
behavior, grouped by the same categories as the Settings sheet.
[proxy.example.yaml](../proxy.example.yaml) remains the commented deployment
example. `GET /admin/config` exposes the same field/category metadata alongside
saved values, neutral defaults, CLI overrides, the process-effective snapshot
and the current revision. Its `last_reload` section reports the most recent
configuration application: `ok`, `error` (empty when ok), `dropped_keys`,
`restart_required` and the unix-millisecond `at` timestamp.
Settings searches labels, keys and help across categories.
Durations and byte sizes use a magnitude plus unit (`2s`, `32MiB`) in YAML and
Settings; do not write a bare number and assume seconds. A bare integer is still
accepted as a count of bytes. Integer counts (retries, percents, tokens) stay
unitless numbers with the unit in the schema label when it is not obvious.

| Settings categories | Surface |
| --- | --- |
| Server, Request, Upstream, Queue & retry, Error storm protection | The proxy's traffic surfaces, in that order. |
| Quota pause, Conversations, Format translation | Provider reaction, grouping, and cross-protocol translation. |
| Storage, Backup, MCP, Dashboard | Durable state, its archives, the LLM tool endpoint and the live view. |
| Models, Providers, Request overrides | Grouping rules, upstream mappings and scoped rewrites. |

The category labels, their help and their membership are owned by
`schema.Categories()` and `Schema()` over `GET /admin/config`; the generated
example carries the same sections with each field's type, range and default.
The rows above are names only, not a second copy of either.

The destructive controls sit in their category and each confirms first: a
rebuild and restart is confirmed and single-flight, starting a capture through
MCP needs its `confirm` phrase and stopping every session confirms separately,
a database delete confirms in the console (and over MCP needs the exact phrase
plus the preview token for that filter), and a restore inspects before its
confirmed apply.

### Guardrails that are not settings

Every numeric guard is classified when it is added. A value becomes a setting
when an operator can reasonably need a different number for legitimate traffic.
It stays an internal constant when changing it independently would break an
invariant, encode a protocol maximum, or only tune an internal heuristic.

| Family | Examples | Why it stays internal |
| --- | --- | --- |
| Protocol maxima | Chat `stop` list of 4 | The wire defines the practical maximum; a longer list is clamped to it with an approximation warning. |
| Derived frame bounds | `NativeStreamFrameMax(eventMax)`, `ResponsesStreamFrameMaxBytes`, the proxy's translated frame buffer | Computed from the accepted source-event and retained-byte bounds plus the JSON escape expansion. A separate value could under-size a scanner, turning a valid frame into a translation error, or over-size it and defeat the retention bound. |
| Recursion and canonicalization budgets | Anthropic nested block depth 16, tool-argument canonical depth 64, separator budget, canonical byte cap | They select a safe fallback (the raw-byte fingerprint) or cap decoder recursion. They are algorithm facts, not wire acceptance. |
| Diagnostic echo bounds | 128-byte warning echo, 512-byte identity and provider-text retention, header echo caps | A request-sized diagnostic or identifier must never multiply across a stream or a stored row. Raising them cannot admit a request that is otherwise rejected; it only grows logs and frames. |
| Record retention | Retained tool names (64), tool-call ids (1024), path/method/model/error caps, capture header caps | They bound the ring, pending snapshots and durable rows. Counts still count past the bound; only identities stop being retained. |
| Metrics-only truncation | Non-streaming choice cap, SSE analyzer event buffer | The non-streaming choice cap never changes relayed bytes. The SSE analyzer cap can affect classification for events split across that bound, as the protocol limits note; raising it cannot admit a request that is otherwise rejected. |
| Cursor run and store budgets | Parked run count, per-run pending call/byte caps, blob store caps, aggregate store budget | They bound per-run and process memory for a bridge whose counts come from live tool batches, not operator intent. The per-run and aggregate caps are coupled. |
| Internal heuristic thresholds | Quality retry spool, error-body capture, discovery output allowance, storage sweep batch sizes, log scan and chart/explorer budgets | They decide whether an internal heuristic runs or how much work one pass may do. Changing them changes diagnostics or performance, not what a client may send or receive. |

The operator-facing bounds are settings: `request_tool_calls_max`,
`stream_tool_calls_max`, `translated_response_max_bytes`,
`models_discovery_max_entries`, `non_stream_capture_max`,
`stream_terminal_hold_max`, `stream_event_max_bytes`, `token_response_max` and
`upstream_response_header_max`. `non_stream_capture_max` trades accounting
accuracy for memory and never changes relayed bytes. A new guard must be
classified in this table when it is added, and a guard that becomes
operator-facing moves to the settings schema instead of keeping a parallel
constant.

### Saving settings safely

Settings collects a draft until Apply. Revert reloads the saved document, and
closing a dirty sheet keeps it open until Apply or Revert; an open dirty draft
also asks the browser to confirm a tab close or reload. The server validates the
whole result and compares its revision before writing. A stale tab receives 409;
reload/review its values instead of resubmitting an old full form. The sheet
renders closed vocabularies as selects from the schema's `choices` and checks
duration/byte text against the schema's numeric band before the POST; the server
remains the authority. With no writable config the field controls are disabled,
not only the Apply action.

API saves overlay supplied top-level keys on the saved config. Omitted keys
remain unchanged, but a supplied map/list replaces that entire field. For
example, `providers`, `provider_aliases` and `model_rules` are not recursively
patched. Read the current document and preserve wanted entries when editing a
structured field. Explicitly empty collections clear those fields.

The response separates saved values from the effective snapshot, reports
restart-required keys and carries the same `last_reload` section as GET; the
Settings sheet renders it as its last-apply row with apply time, failure,
dropped-key count and restart notice. A successful file write can still be
followed by a reload failure; `saved:true` means the file changed even when
the response reports an error. Do not assume rollback. The `writable`
indicator means a config path is configured, not that filesystem permissions
or every future write are guaranteed.
See [persistence](containers.md#persistent-state-and-configuration) and
[reload behavior](containers.md#reload-restart-and-shutdown) before changing startup-bound fields.

### Provider aliases and model grouping

`provider_aliases` merges provider labels in stored history at boot/reload and
rekeys new requests. All aliases in one map commit as a single transaction, so
a trigger or constraint failure on a later alias leaves every earlier label
unchanged. It rejects self-maps and alias chains. This is a durable rename, not
a temporary view: removing the alias does not reconstruct original labels.
Existing in-memory ring records can retain their previous label until they age
out or the process restarts. Back up before an intentional history rename.

`model_rules` instead controls display/grouping. The list is capped at 64 rules.
Its ordered exact, pattern and lowercase steps keep stored model spellings
unchanged; an empty rule list groups by those raw spellings. The shipped
pipeline folds case, strips one or more leading `vendor/` namespaces and
trailing `:tag` deployments, strips a trailing run of serving decorations, and
unifies `.` with `-` between digits. A decoration is what a provider adds
around the same weights: a quantization or serving format (`-fp4`, `-nvfp4`,
`-bf16`, `-int8`, `-q4_k_m`, `-gptq`, `-awq`, `-gguf`), a service tier
(`-fast`, `-speed`, `-value`, `-free`), a release channel (`-preview`,
`-latest`, `-beta`, `-alpha`, `-stable`, `-experimental`), a reasoning or
effort word (`-thinking`, `-reasoning`, `-xhigh`, `-high`, `-medium`, `-low`,
`-minimal`), or a build stamp (`-1017`, `-20250929`). Model tiers (`-flash`,
`-pro`, `-mini`, `-ultra`, `-lite`, `-nano`, `-turbo`, `-max`), fine-tunes
(`-abliterated`, `-rl`), specifications (`-coder`, `-base`, `-chat`, `-it`)
and parameter counts stay distinct. Add an exact rule above the shipped
pipeline to split a group, or one it cannot recognize to merge the spelling.
The Settings editor supports reordering, parking and previewing rules. Raw
export/delete predicates do not silently expand to a display group. Debug's
native-model normalization is separate from these rules.

### Request overrides

`request_overrides` is an ordered rule list that rewrites the upstream request
before relay; the default empty list leaves every request untouched. Each rule
has a scope and at least one action: `headers` sets or replaces upstream
headers, `remove_headers` deletes them, and `body` sets the output-token
ceilings `max_tokens` / `max_completion_tokens` (1..1000000) on a Chat
Completions or Anthropic client body before translation. A Responses client
body is not rewritten by the body action because its cap is
`max_output_tokens`; header and removal rules still apply. A Chat client whose
target is the Responses wire has the rewritten cap translated to
`max_output_tokens`. Both override spellings address an Anthropic body's single
`max_tokens` field, with `max_completion_tokens` winning when both are set. The
list is capped at 64 rules and validated like every
other setting: a rule needs at least one scope field and one action, duplicate
scope triples are rejected, and one header name cannot be both set and removed
in the same rule.

Scope matching is exact leaf. A rule applies when every non-empty scope field
equals the request's value exactly: `client` against the classified client
label (`X-Proxy-Client` when sent, except a label that only repeats the user
agent's product name, which records the fuller user-agent identity; otherwise
inferred from SDK/user-agent metadata), `provider` against the canonical
label after `provider_aliases`,
and `model` against the recorded model id (for Cursor requests the base id the
dashboard records, not the fused spelling). An empty scope field matches
anything. There is no model-rule canonicalization, regex or wildcard broadening.
All matching rules apply in list order, and for the same header or body field
the later rule wins.

The client label is a claim, not an authenticated identity. The caller chooses
it, and the proxy never verifies it, so a request presenting another client's
label is treated as that client here and by every other client-scoped feature
(sub-conversations, Debug sessions, pause holds, the Cursor parked-run scope).
See [client identity](../SECURITY.md#client-identity-is-a-claim-not-an-authentication)
for what that means for a listener reachable by parties you do not trust. What
it cannot do is move a credential: credential-class header names are refused at
load and skipped at send, so the exposure is the policy attached to the label,
not the secret behind it.

Header rules are the operator's last word on the upstream wire: the resolved
merge applies after client-forwarded values, configured provider headers and
the client's `X-Proxy-Headers` map, so a matching override wins over all of
them. Only the proxy-owned transport-mode names `accept-encoding` and `range`
are stripped from the final upstream request before send, whichever source set
them, so the transport keeps its own gzip negotiation and never sends a partial
range; the client header maps (`X-Proxy-Headers` and an
`X-Proxy-Auth-Header` naming one) also reject them with a 400 before any send.

The request-override list refuses two further header classes at load. Protocol
and framing names (`host`, `content-type`,
`content-length`, `transfer-encoding`, `te`, `connection`, `keep-alive`,
`proxy-authenticate`, `proxy-connection`, `trailer`, `upgrade`) and the whole
`x-proxy-` control prefix belong to the proxy and the transport. Credential
names belong to whoever issued the secret: `authorization`, `cookie`,
`proxy-authorization`, and every name the shared credential vocabulary
classifies. That vocabulary judges the header's *role*, not a list of vendor
spellings: `auth`/`authorization`, `api-key`, `secret`, `password`,
`credential`, `bearer`, `jwt`, `signature`, `cookie`, `key` and `token` are all
credential-class wherever they appear, so `x-auth-key`, `x-amz-credential`,
`x-functions-key` and `x-subscription-key` are refused along with `x-api-key`.
Names that mention a key or a token but carry an identifier or a measurement
stay admitted: `x-key-id`, `idempotency-key`, `cache-key`, `x-prompt-tokens`,
`x-token-count`, and the `x-ratelimit-*` and `anthropic-ratelimit-*` families.
That breadth is deliberate. An override
value is operator-supplied and is stamped onto a request whose destination the
caller chooses with `X-Proxy-Base-URL`, so a secret in any unclassified header
name would be delivered to that caller's host; vendor spellings cannot be
enumerated in advance, which is why the check keys on role rather than a list. The same vocabulary drives capture and debug
redaction, so a stored capture never keeps a header the wire carried. At
runtime the target's configured auth header is skipped as well, so a rule that
predates this check, or a future grammar slip, still cannot move a credential.
The client header maps reject `content-length` before any send so the transport
keeps ownership of message framing. Header
names are trimmed and canonicalized
case-insensitively, and values must be non-empty single-line header values.

Provider `headers` refuse neither class above. They are applied after the
extracted credential is placed, so a credential name (including the target's
own auth header) or an `x-proxy-` name is legal there and takes effect over the
forwarded and extracted value. Configured provider values are still stripped
for `accept-encoding` and `range` at the final request, exactly like override
values.

The body rewrite is fail closed. It applies to Chat Completions or Anthropic
client bodies before translation, whatever the target (a Responses client body
is not rewritten); when the body is not a JSON object, or the rewritten
result would exceed `max_request_bytes`, nothing is written: the original
bytes are used unchanged by the rewrite stage and are still translated when
translation is active, with one process log line recording the reason. Only a
passthrough relays them verbatim. A successful rewrite replaces the body for
the request's whole life, so every relay attempt, absorbed retry and quality
re-send reuses it, and the recorded request cap is re-stamped from the
effective body. A rule sets only the body
fields it names, and the re-read pair keeps the wire's documented precedence -
`max_completion_tokens` wins over `max_tokens` - so a rule setting only
`max_tokens` cannot raise a request whose client already sent
`max_completion_tokens`: set both fields to the same value to override either
spelling. Cursor targets carry no
token ceilings on their wire, so the body section is skipped for them while
header rules still apply at the native send.

Overrides apply to inference sends only, never model discovery or token
refresh. The list hot-reloads: changed rules apply to new requests. The
Settings editor mirrors the server's checks and rejections live for each
rule, marks empty scope fields as matching any request, and blocks Apply
on the first invalid rule.

To raise the output budget for one provider, either write:

```yaml
request_overrides:
  - provider: nano-gpt.com
    body:
      max_completion_tokens: 65536
```

or open Settings, select **Request overrides**, pick **raise the output budget
for a provider** from the add-a-rule menu, and fill the provider scope; the
other starter templates seed an empty rule and an upstream header for a
client.

### Sub-conversation tracking

`sub_conversations` gives requests from one client a conversation identity of
their own, taken from a value the client already sends in its request body
(exemplar: opencode's `promptCacheKey`). The default empty list leaves tracking
fully off, and the feature is deny-by-default per client: an entry applies only
when its `client` equals the request's classified client label exactly (the same
leaf the request-overrides `client` scope matches; no wildcard, no case
folding), and a request from any other client is never inspected, leaving its
bytes and grouping untouched.

Each entry lists 1..4 exact JSON field names, matched only at the request
body's top level and checked in order; the first present field decides. A
present value that is not a JSON string counts as absent and the next field is
checked, while a present string that cannot be decoded or fails the identity
rules drops the identity for the request - no later field is consulted. A
value is kept only when it is non-empty after trimming, valid UTF-8, free of
control bytes and at most 512 bytes, the same bound explicit session
declarations satisfy. The UTF-8 judgment runs on the value's raw bytes
before JSON decoding: the standard decoder coerces invalid UTF-8 inside
strings to U+FFFD, so a raw invalid-UTF-8 sequence drops the identity while
an escaped lone surrogate, whose bytes are ASCII, is kept and decodes to
U+FFFD. The drop is conservative: the value's raw spelling is
scanned inside a bounded window, so a value whose raw span exceeds the window
(escape sequences spend more raw bytes than they decode to, and trimmable
edges count before trimming) is dropped even when its trimmed form would
pass. An absent or dropped identity is never a client error: on the
passthrough wire the body is payload, and the request keeps automatic
grouping. A translated target still requires the whole request body to be
valid UTF-8, as [protocol](protocol.md#relay-retries-and-model-discovery)
describes.

The kept value groups the request under a `k:` conversation identity.
Precedence is the explicit `X-Proxy-Session` header first, then the tracked
value, then automatic client+key grouping; a request carrying both header and
tracked value keeps the header identity. `X-Proxy-Parent-Session` still owns
parent linkage, and declaring a parent still requires the explicit session
pair, so a tracked identity never carries a parent. In the dashboard a `k:` id
is an ordinary conversation id: lineage, filters, saved views and the request
drawer read it like any other.

`strip: true` removes every configured field of the entry that is present at
the body's top level from the client body before relay or translation,
including a Responses client body; the default `false` forwards them
unchanged. Unlike the [request overrides](#request-overrides) body fields,
strip is not limited to Chat Completions or Anthropic inbound. The removal
rides the same body-rewrite engine: one document decode when both features
match one request, the original bytes used unchanged by the rewrite stage when
the body is not a JSON object or the rewrite would exceed
`max_request_bytes`, and cursor targets skipping the body section. Without
`strip`, the client's spelling reaches the upstream wire on a same-protocol
passthrough; a translated body carries the value in `prompt_cache_key` or drops
it.

When a tracked value was extracted and the request translates to the Anthropic
wire, the translated body gains a top-level `prompt_cache_key` carrying the
value. This is a proxy tracking extension, not an Anthropic cache field:
Anthropic caching uses `cache_control`, and an upstream that rejects unknown
top-level fields will reject the injected request. Injection follows extraction
alone: `strip` governs the tracked client-body fields, never the injected
field.

Validation runs at the load boundary for YAML and Settings saves alike: at most
16 entries, at most 4 params per entry, param names one JSON key segment each
(trimmed, non-empty, at most 64 bytes, printable ASCII, no quote, backslash or
control bytes), and duplicate clients or duplicate params within one entry are
rejected. The list hot-reloads: changed entries apply to new requests.

The Settings editor lives in the Conversations category and mirrors every check
live per entry card: classified-client autocomplete from observed clients,
ordered param rows with add/remove gated at the caps, the strip toggle, a count
line against the entry cap, the server's exact rejection wording, and an Apply
blocked on the first invalid card. To track opencode's prompt cache key and
strip it from the upstream body, write:

```yaml
sub_conversations:
  - client: "opencode"
    params:
      - "promptCacheKey"
    strip: true
```
