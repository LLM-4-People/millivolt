# Routes and debugging

This page covers the operator and data routes, access control, pause and limits,
error storm and quota protection, debug and preview capture, export and deletion,
restart, and the dashboard data and request inspector.

> [!WARNING]
> A genuinely bodyless `POST /admin/purge` deletes the whole history, and a
> restore applies data before the config phase; both are irreversible without a
> backup. Exports, debug captures and retained headers can carry sensitive
> content.

## Operator and data routes

The routes below belong to millivolt. They are operator-gated unless the row
is marked open; the canonical open-path list is in
[operator access](#operator-access). Read handlers enforce their supported
methods; wrong-method requests must not fall through to inference. The
namespace is standardized: `/metrics/*` and dashboard routes are the protected
read plane, and `/admin/*` is the operator action plane; the credential gates
both.

| Route | Action / important contract |
| --- | --- |
| `GET /healthz` | Unauthenticated liveness probe for Docker HEALTHCHECK and load balancers. Reports process/HTTP liveness only, no storage depth. Trailing-slash look-alikes (`/healthz/...`) are reserved and answer 404, never inference. |
| `GET /readyz` | Unauthenticated readiness probe. `200` when the durable pipeline can persist; `503` with a reason when the writer is wedged or a flush failed since boot. Compose uses it through `-readycheck`. Trailing-slash look-alikes are reserved and answer 404, never inference. |
| `GET /favicon.ico` | Unauthenticated ICO brand mark. Browsers fetch this without Authorization; it is registered on the mux so it never reaches the inference catch-all. |
| `GET /favicon.svg`, `/apple-touch-icon.png`, `/icon-*.png` | Ungated SVG/PNG icons for tabs, home screens and the web app manifest. |
| `GET /manifest.webmanifest` | Ungated web app manifest (name, standalone display, 192/512 icons). |
| `GET /sw.js` | Ungated service worker. Precaches brand icons/manifest; cache-first only for a `/dash/` URL carrying exactly the stamped version, network-first with cache fallback for unversioned or stale `/dash/` URLs and brand paths. Navigations to `/` are fetched live (never cached: login vs bootstrap) with a static offline fallback. Its never-cache list is generated at load from the same canonical namespace table that drives the gate and the reserved routes, so the three cannot drift; it never intercepts `/`, `/index.html`, `/healthz`, `/readyz`, `/models`, `/sw.js`, `/admin/*`, `/session/*`, `/metrics/*`, `/mcp/*` or `/v1`. |
| `POST /session` | The one open operator route: exchanges the credential for the session cookie the dashboard's live feed needs. Throttled like every gated route. Lives outside `/admin` so an external authentication layer can front the admin plane without gating millivolt's own login. |
| `GET/POST /admin/config` | Schema/file/effective state; save `{revision,values}`. Stale revision returns 409, and so does a save while a restore holds the config transaction (its upload is client-paced). Saved-but-reload-failed is explicitly reported. GET and save responses carry a `last_reload` section: `ok`, `error`, `dropped_keys`, `restart_required`, `at`. GET returns provider header values verbatim, because the settings sheet must round-trip them on save; redaction applies to the model-visible `config_get` tool and the operator CLI instead (see [cli](cli.md) and [mcp](mcp.md)), where `--reveal` is the opt-in. Posting a redacted document back is safe: a credential header whose value is the marker keeps the loaded secret, while a posted real value replaces it. The same applies to `set_config`, so a model can read and re-post without erasing credentials. |
| `POST /admin/reload` | Re-read config and report restart-required keys. Answers 409 while a restore holds the config transaction. |
| `GET/POST /admin/restart` | Status / rebuild. `GET ?watch=1` streams progress; concurrent starts are rejected, and so is a start while a config transaction (reload, Settings save, backup restore) holds the reload lock: `409` names the operation. |
| `GET/POST /admin/pause` | Inspect/add/edit/resume holds. POST requires `paused`; optional ID targets one hold. GET reports `queued` (admission and send-gate waiters, the populations an operator drain can release) and `storm_waiters` (requests parked by the storm/quota gate before admission, which only the gate opening or a quota resume frees). |
| `GET/POST /admin/quota` | Inspect storm state (shared with the banner) / `POST {"provider","resume":true}` closes a retry-mode quota gate; parked sends resume immediately. |
| `GET/POST /admin/debug` | Inspect/add/edit/stop capture sessions. POST requires `enabled`; starting a session requires durable storage (503 `durable storage is disabled` otherwise); optional ID targets one session. |
| `GET/POST /admin/throttle` | Inspect and edit limits. A policy is named by its opaque `id` or by its selector. Scope dimensions `providers` (the singular `provider` field is still accepted), `models`, `clients`, `key_hashes` and `conversations` intersect; an empty dimension matches any value. An optional schedule uses `days` (0 = Sunday), `start_minute` and `end_minute` in server-local time; equal minutes mean all day and a start after the end spans midnight. POST accepts `concurrency`, `requests`/`request_window`, `tokens`/`token_window`, `anti_burst` and `ramp_duration`; supplied limits merge. A count requires its own window only when the policy has no stored window for that dimension: a fresh count without one is refused, while a count may raise an already windowed dimension alone. A count of `0` clears that dimension and its window. A window without a count is a refused no-op unless a count is already stored. `clear:true` drops the whole policy. Anti-burst and its ramp require a single-provider policy without a schedule. GET also returns `known_providers`, `known_clients` and `known_models` for selector pickers. |
| `POST /admin/queue/drain` | Release parked requests without waiting for their limit, pause hold or retry window, covering both the admission queue and the send gate. Requires exactly one scope: `all:true`, `provider`, `model` or `client`. Released requests answer with a retryable 503, never a provider error; a request already exchanging bytes with the provider finishes normally. |
| `GET /admin/debug/capture?id=` | Load an unexpired durable debug sidecar; absent/no-store returns 404. `&download=1` serves the same document as a gzip artifact named `millivolt-debug-<captured_at>.json.gz`, falling back to the record id when `captured_at` does not decode. |
| `POST /admin/purge/count` | Preview the same deletion/export predicate; traffic can change the count afterward. A genuinely bodyless POST counts the whole set, mirroring the bodyless delete; otherwise the body is `{"filter":{...}}`. Filters are body-only, so any query key is rejected with 400 before the body is read. |
| `POST /admin/purge` | Delete matching finalized records. A genuinely bodyless POST deletes everything: that is the deliberate full-history command, it answers `200` rather than `404`, and any query key must be moved into a body instead. Otherwise the body is `{"filter":{...},"reviewed_count":N}`: an empty/invalid filter is rejected, and a supplied reviewed count is counted again inside the delete transaction, refusing with 409 when the row set moved. `status_code=0`, `debug=0` and `has_error=0` mean "no constraint" on that dimension, not a match for zero or false. The response is `{"deleted":N,"buffer_removed":N}`. Filters are body-only: any query key is rejected with 400 before the body is read, so `?provider=...` can never broaden the no-body full delete. |
| `GET /admin/backup` | Download the self-checked `.mvb` Settings backup archive of config and/or database members. `?config=1` and `?database=1` select members, defaulting to both when no key is given; an explicit empty selection is an error. The query grammar is strict: parse errors, repeated or unknown keys, and malformed values all return 400. |
| `POST /admin/restore` | Restore an uploaded `.mvb` Settings backup archive, validated before anything is applied. `?inspect=1` previews the archive (created time, member sizes, the config values plus their changed-key lists against defaults and the live config, and the database request span when a live store stages the member) without applying anything. A store-less preview still reports the member's presence and decoded size with `inspectable:false` and the reason. `?config=1` and `?database=1` select members as on backup; `?config_mode=` and `?database_mode=` take merge or replace and default to replace when omitted; an explicit empty or whitespace value is rejected as malformed. Success reports the `restart_required` keys. The upload is exempt from `request_body_read_timeout` (the idle bound still applies) and an expired read deadline answers 408. The query grammar is strict: parse errors, repeated or unknown keys, and malformed values all return 400. |
| `GET /metrics/export` | Download all or exactly filtered finalized records as a gzip artifact named `millivolt-logs-<timestamp>.json.gz`; gunzip it for the JSON array. `status_code=0` and `debug=0` mean "no constraint" on that dimension. A debug capture rides as a sibling `debug_capture` member only for a row that has a stored sidecar, so debug bodies appear only while durable storage is live; a ring-only export never carries them, and debug bodies stay sensitive either way. |
| `GET /metrics/query?q=` | Restricted SELECT with timeout/output limits; 503 when durable storage is disabled. Not a hostile-query sandbox. |
| `GET /metrics/bootstrap` | Dashboard state and full/incremental recent-record snapshot. |
| `GET /metrics/live/stream` | Replayable finalized SSE feed plus ephemeral pending lifecycle/reset events. |
| `GET /metrics/agg/chart`, `/explorer`, `/log` | Scoped history chart, faceted explorer, and durable log paging. The Requests-card `s=` selector accepts an exact code or a live in-flight class on all three, but only the explorer folds pending rows: the chart answers a live class with `400 invalid scope` (its buckets are finalized-only, so an accepted class could only read as "no traffic"), and the log keeps its standing empty page so the archive pager does not stop. |
| `GET /metrics/prometheus` | Prometheus exposition over the in-memory ring, not durable since-inception dashboard totals. |
| `GET /metrics/pprof/` | Standard Go profiling handlers behind the same credential as every `/metrics` route: the index listing plus the `profile` (CPU), `heap`, `allocs`, `goroutine`, `block`, `mutex` and `threadcreate` profiles at `/metrics/pprof/<name>`. Unregistered subpaths answer the reserved namespace 404, never inference. See the capture workflow below the route table. |
| `POST /mcp` | Streamable HTTP MCP endpoint, operator-gated like `/metrics` and enabled by default: `mcp_enabled: false` removes it from the process (the route stays reserved, answering 404 and never inference; restart to apply). Serves the 22 MCP tools. Bearer-only: the gate admits the session cookie on gated paths, but this endpoint refuses a cookie-only request (`403`) and forwards the caller's own Bearer on its in-process calls to the routes above, so no session cookie or per-request state is retained between requests; the admitted credential keys an in-memory cache of its SDK server object (never logged or persisted) so repeated calls do not rebuild it. Stateless: `GET`/`DELETE` answer `405`. `/mcp/...` look-alikes are reserved and answer `404`, never inference, and a reserved-namespace spelling whose separator is written single-encoded (`%2f` or `%5c`, any hex case) is refused the same way. A double-encoded separator (`%252f`) is not a reserved-namespace spelling: it decodes to a literal `%2f` inside one segment, so it can reach neither the MCP handler nor a reserved namespace and is ordinary inference traffic. |

Pause/Debug/Limits successes may include a persistence warning: runtime state
was applied, but saving it failed. Do not retry as though the mutation rolled
back. With storage disabled, Pause and Limits keep their memory-only state
intentionally. Debug is the exception: starting a session answers 503 because
with no store every matched request would retain bodies for a sidecar that
could never be fetched.

The profiling subtree is operator-gated like every `/metrics` route: an
unarmed plane answers 403, a wrong credential 401, and a reserved-namespace
spelling with an encoded separator answers the reserved 404. `go tool pprof`
cannot send an `Authorization` header, so fetch the profile with curl and
analyze the file against a locally built binary. Against the dev instance
([`scripts/dev.sh`](../scripts/dev.sh), loopback `127.0.0.1:8081`):

```sh
go build -o /tmp/millivolt-profiling ./cmd/proxy
curl -fsS -H "Authorization: Bearer $MILLIVOLT_OPERATOR_TOKEN" \
  -o /tmp/cpu.pprof 'http://127.0.0.1:8081/metrics/pprof/profile?seconds=30'
go tool pprof /tmp/millivolt-profiling /tmp/cpu.pprof
```

The `seconds=N` parameter on `/metrics/pprof/profile` is the capture window:
that one request stays open for the whole window while samples accumulate. The
valid range is 1 to `response_write_timeout` (600 by default); an absent or
unparseable value uses 30 and a larger one is lowered to the bound, so a
capture never outlives the delivery contract unnoticed. A
process runs one CPU profile at a time, so a second concurrent `profile`
request fails with 500 until the first finishes. On `/metrics/pprof/heap`,
`gc=1` forces a garbage collection cycle before the sample.

The proxy's own `/mcp` endpoint serves this whole surface to an LLM, using the
same credential and the same routes. It adds no route of its own and dispatches
each tool call in process through the same gate, so its forwarded Bearer is
re-validated exactly like an external request. For its irreversible call, the
operator credential and the tool's own guards are the entire gate: an empty
purge filter, a purge without the exact confirmation phrase, and a purge
without the `purge_preview` token issued for that same filter are each refused.
The preview token expires 15 minutes after it is issued, so a purge more than
15 minutes after its preview needs a fresh one.
It also refuses a capture session on a proxy without durable storage, and an
unknown name in a capture scope or a throttle provider is offered the known
values instead. The throttle provider check fails open when the proxy's
vocabulary is empty or cannot be read; the capture scope check accepts any name
when the vocabulary is empty but refuses when the vocabulary cannot be read.
It never follows a redirect, and it presents the credential only in the
`Authorization` header. See the [MCP server guide](mcp.md).

### Operator access

`MILLIVOLT_OPERATOR_TOKEN` is the single operator credential for the whole
dashboard. It is read once at boot from the process environment, never from
proxy.yaml, so it cannot leak through Settings, `-print-config`,
`/admin/config` output, snapshots or logs.

The gate is deny by default and lives in one chokepoint in front of every
route. This is the canonical open-path list; [Security](../SECURITY.md) and the
[reverse-proxy guide](reverse-proxy.md) summarize it instead of maintaining a
second list.

- Open: `GET /healthz`, `GET /readyz`, the `POST /session` login handshake (the
  one open operator route), origin-root brand/PWA files (`/favicon.ico`, icons,
  `/manifest.webmanifest`, `/sw.js`) and transparent inference. Provider
  credentials ride the same header name and are never inspected by the gate.
- Gated: the dashboard HTML and `/dash/*` assets, every `/metrics/*` surface
  and every `/admin/*` route, including unclassified future admin paths and
  unsupported methods, plus the `/mcp` endpoint and its reserved `/mcp/*`
  look-alikes. A gated MCP request also needs its credential as a Bearer
  header: the session cookie alone is refused there (`403`), because the
  caller's credential is forwarded on the endpoint's internal calls.

Present the credential as `Authorization: Bearer <value>`. A valid Bearer
request on a dashboard, admin, metrics or dash route also mints a session
cookie, because the dashboard's live feed uses EventSource, which cannot send
Authorization headers. The `/mcp` endpoint deliberately returns no cookie: its
caller forwards the credential on the endpoint's internal calls. The cookie is
HttpOnly (script never reads it), SameSite Strict, scoped to 12 hours, and
signed with a key derived from the operator token, so a container restart with
the same credential keeps the session. Rotating the token invalidates outstanding
cookies. The cookie carries `Secure` only when the request arrived over TLS or
any `X-Forwarded-Proto` token names a secure scheme (`https` or `wss`). The
header is a list: the canonical NGINX sample overwrites it with this server's
`$scheme`, and an ingress that appends its own scheme after the client's
`http` must not downgrade the cookie - hence any token, not the first. A
loopback plain-HTTP dashboard keeps working
while a TLS presentation gets the flag.

Every response to a secure presentation also carries
`Strict-Transport-Security: max-age=15552000` (180 days), set once at the
outermost server wrapper from the same secure-presentation test. There is no
`includeSubDomains`, because millivolt cannot know whether sibling or parent
hostnames are TLS-capable. A plain-HTTP response carries no header at all
(browsers ignore it there), so the dev instance is unaffected. The lifespan is
deliberately under a quarter: moving the dashboard off TLS becomes reachable
again within that window instead of pinning a browser for a year.
Do not expose the listener over plaintext networks anyway. The login page
served for unauthenticated dashboard visits exchanges the entered value for
that cookie through `POST /session` and works without JavaScript; API
clients can call the same endpoint or simply send the Bearer header on every
request. An expired cookie re-prompts in the dashboard or reappears as the
login page on navigation.
Unregistered `/admin/*`, `/session/*`, `/metrics/*` and `/mcp/*` paths are reserved: they
answer 404 and are never forwarded upstream. The gate also refuses 404 when an
owned-namespace path is written with a percent-encoded separator (`%2f` or
`%5c`, any hex case), a duplicated separator (leading or interior) or a dot
segment: the mux cannot route it to the reserved subtree unchanged, so it
would otherwise fall through to inference. A double-encoded separator
(`%252f`) is not a reserved-namespace spelling: it decodes to a literal `%2f`
inside one segment, matches no reserved route, and is ordinary inference
traffic.

Failed authentications are throttled per source IP. The immediate hop is the
only trusted one, so the throttle key is the rightmost `X-Forwarded-For`
element (the address that hop appended) and `RemoteAddr` when the header is
absent; a client can prepend anything, and an unparseable element is skipped.
The shipped [NGINX example](../deploy/nginx.conf) resolves its own peer with
`real_ip` first, so the rightmost element is the real client. A directly
exposed instance lets a client choose its own key, which only ever relaxes the
throttle, never the credential decision: after five rejected credentials the IP
is locked out with a window that doubles from 10 seconds to a 15 minute
ceiling. The throttle counts only
presented-but-wrong credentials, so an expired-cookie dashboard polling in a
background tab cannot lock its own operator out, and a verified credential is
always admission: presenting the correct Bearer during a lockout clears the
budget, because the lockout exists to rate-limit guessing, not to lock the
operator out of their own dashboard. The map is hard-capped at 4096 source
IPs with idle expiry, so spoofed-address floods cannot grow it; reaching the
cap is logged once per episode, because from then on a new source evicts a
tracked one. Crossing the five-failure threshold is logged once per source. A lockout
response is `429` with a coarse rounded `Retry-After` and a generic JSON body,
except `GET /` (and `/index.html`): the sign-in page is still served, read-only,
with the same `Retry-After`, the form intact and the lockout named in its
notice slot, because the form is the only route back to a credential and `POST
/session` keeps working through the lockout.

With the variable unset or empty the deny-all state holds: the whole dashboard
returns 403, the open `POST /session` handshake cannot mint a cookie, and only
`/healthz` and `/readyz`, brand/PWA files and inference respond. A nonempty
value shorter than 16
or longer than 512 bytes fails the boot instead of silently weakening
the gate. The credential is process bound: config reload does not re-read it,
so rotating the value requires a real process restart (a supervisor restart
or a fresh deployment; the dashboard's restart handoff inherits the parent
environment, so it keeps the old credential by design). The dashboard asks
for the value once per browser tab, keeps it in sessionStorage, attaches it
to gated calls only and drops it as soon as the server rejects it.
Non-browser callers send the header directly, for example
`curl -H "Authorization: Bearer $MILLIVOLT_OPERATOR_TOKEN" -XPOST .../admin/purge`.
Environment variables are readable by same-user processes through `/proc`,
so run the proxy under a dedicated account.

### Pause and Limits

Pause creates holds; it does not disconnect already-running sends. Matching new
sends and retries wait until the hold expires or is resumed. Choose all traffic,
any combination of the five scope dimensions, or clients that were unseen when a
new-clients hold was created. "New" does not mean new requests or new
conversations.

A hold scopes on the same five dimensions a limit does: providers, models,
clients, upstream key hashes and conversations. Values inside one dimension are
alternatives, an unset dimension matches any value, and set dimensions
intersect - the same rule limits use, because the scheduler matches both against
one request identity. When named clients and new are combined, either client
condition may match.

A hold has its own duration and queue cap; the cap defaults to `max_queue_size`
when the hold does not set one. Excess waiting work can be refused;
an indefinite hold needs an explicit resume. Existing holds can be edited in
place or resumed individually, while Resume all clears all holds. Overlapping
scopes are rejected instead of silently replacing a different hold.

The HTTP shape is closed. A resume carries only `paused:false` and an optional
`id`; supplying a scope, duration or queue cap beside it is a 400, because the
proxy has no scope-selective resume and accepting one would clear more holds
than the caller named. `all:true` cannot be combined with `new` or any scope
dimension; the global hold already covers them. Named clients plus `new`
remains a valid union. An edit restates the dimensions it supplies and preserves
the ones it omits, so supplying an explicit empty array is how a dimension is
cleared. These are enforced by the handler, not only by the MCP and CLI
wrappers.

The known client/provider/model vocabularies (the pause and capture pickers,
and the snapshot a new-clients hold compares against) are bounded at 4096 names
per dimension. Client labels and model ids come from the request, so they are
evictable: past the cap the least recently observed name makes room for the
newest, so a client that keeps sending requests outlives a burst of one-shot
labels that arrived after it. Names an operator documents - a hold scope, a debug session, a limit policy
(including one installed from a request's `X-Limit-*` headers or restored at
boot) - are pinned and are never evicted by later traffic, so a flood
of distinct labels cannot hide the client you scoped a hold to. Pins are
derived from the documents that exist, not accumulated: resuming a hold or
ending a debug session releases its names back to ordinary eviction. When a
full vocabulary is entirely pinned, a new observed name is dropped rather than
displacing operator intent, and a scope that cannot be listed is named in the
log instead of disappearing silently.

A hold scopes on the caller's claimed client label, so it separates your own
tools from each other, not trusted callers from untrusted ones; a caller that
sends another client's label is parked, or not parked, as that client. See
[client identity](../SECURITY.md#client-identity-is-a-claim-not-an-authentication).

Limits applies to an intersection of conditions: providers, models, clients,
key hashes and conversations. Values inside one condition are alternatives; an
empty condition matches any value, so a rule with no condition covers every
request. A request must satisfy every non-empty condition, and every matching
policy charges its own lease, so the tightest cap wins. A provider-only rule
keeps the per-key concurrency and fairness behavior below; adding any other
condition narrows the same machinery without changing it. Conditions select the
request's recorded identity, so models match the request spelling, keys match
the SHA-256 digest, and conversations match the selected leaf.

A rule may also carry a recurring schedule: `days` (0 = Sunday), `start_minute`
and `end_minute` in the server's local timezone. Equal minutes mean all day, and
a start after the end spans midnight, with the after-midnight portion belonging
to the previous day. An empty schedule is always active. Outside a rule's window
it does not charge, settle or queue, and a request that matches no other rule
proceeds normally.

A policy's identity is the digest of its normalized conditions and schedule.
Editing either one therefore creates a new identity, and two rules cannot
silently share counters. Provider-only policies keep the version 1 persisted
shape and still accept the singular `provider` field; a rule with any other
condition persists its selector arrays. `clear:true` removes the whole policy
and cannot be combined with a limit dimension: the handler refuses the
ambiguity instead of dropping the supplied value and reporting success.

Choose concurrency, requests per window, tokens per window and the optional
anti-burst ramp. The console shows whether a policy came from the console or a
request header, its schedule, and available budget and in-flight/queued state.
Queued counts every request the scheduler parks for that policy - admission
waits and send-gate waits behind a 429/5xx pacing window alike - not only work
blocked by the configured cap. Send-gate waits are paced by the provider's retry
hint and the retry budget; `max_queue_size` and `max_queue_wait` bound only the
admission FIFO. Provider admission is fair across keys: a release or refill
rotates across the groups actually waiting on the shared provider gate
(concurrency or the request/token bucket), granting one request per group per
pass. A group parked only on its own pacing window, hold, anti-burst ramp or
per-key cap is outside that fair set and does not delay another key's fast
path. A release settles the gate and drains once, and the drain stops as soon as
a grant refills a saturated provider concurrency cap. Header semantics are in
[protocol](protocol.md#routing-headers). Persisted policy does not mean the live
scheduler's counters and in-flight requests survive a restart.

**Drain queue** releases every request the scheduler has parked, or only those
whose recorded provider, model or client matches the one named in the console,
without waiting for a limit window, pause hold or retry pacing to open. The
selector reads each parked request's own identity, so it also releases work
parked by a hold, by pacing, or by a provider without a limit row. The overview
shows the total parked count. Each released request answers with a retryable 503
and is recorded as `queue_drained`, never as a provider error; a request already
exchanging bytes with the provider finishes normally. The API also accepts the
same three scopes for scripted drains.

Anti-burst is opt-in and scoped to each provider+key group. It is available
only on a single-provider rule without a schedule, because a ramp is defined
over the provider-plus-key groups the scheduler owns; the server rejects the
pair on any other rule. With `anti_burst` enabled and a concurrency target of at
least 2, a group starts at one concurrent request and gains allowance linearly
until it reaches the rule's `concurrency` target after `ramp_duration`. It is
inert unless all three are set: the flag, a target of at least 2, and a duration
above zero.
`ramp_duration` accepts the same 1s..24h window grammar as request and token
windows, including `1d`; empty, `0` or `0s` turns it off. Requests beyond the
current allowance stay in the existing per-key FIFO. A fully idle group whose
idle period reaches `ramp_duration` starts a new epoch on its next grant, while a
shorter idle gap continues the existing one. Enabling the ramp while a group
already has in-flight work anchors its epoch at the current occupancy, so
running streams are not stalled. Changing the flag, duration or concurrency
target resets every group's epoch. Header updates change only the three cap
dimensions and preserve the stored anti-burst pair. Clearing the last active cap
drops the whole policy, ramp included. The limit composer carries the checkbox
and the ramp chips; applying sends both, and a form whose caps are all zero
sends the pair off rather than an invalid on-ramp.

Operator limit edits persist synchronously and report a warning when the store
rejects the write. Header-driven limit changes apply in memory immediately and
are persisted by one coalescing background writer: a burst of changes becomes
the latest whole-policy document, with bounded retry and backoff after a store
failure. A closed store is not retried - the generation stays dirty and the
writer stops - while a busy one keeps the ladder. That document also carries
the anti-burst pair, and one written before
those fields restores them off. A crash can therefore lose the most recent
header-driven changes, and a failed save never rejects inference. Graceful
shutdown and restart flush the latest policy before storage closes or the
restarted process opens the database. Provider labels are capped at 255 bytes
and the persisted throttle document at 8 MiB; normal provider labels derived
from base URLs remain below both bounds.

Admission is bounded end to end. `max_inflight_requests` is the process-wide
inference ceiling (default 10000), `max_inflight_body_bytes` its buffered
body budget (default 256 MiB; admission charges the allocation hint, the
declared length bounded by `max_request_bytes` and the read hint, and reconciles
it against the bytes actually read, so a short body refunds the difference and a
chunked one is charged as it arrives), and `max_inflight_response_bytes` its
budget
for buffered upstream response bodies (default 256 MiB): whole-document
translation of non-streaming responses, the non-streaming 200 quality spool
(at most 1 MiB plus one 32 KiB chunk), and model discovery. A request-slot
refusal is answered with `503` and `Retry-After` before the body is read. The
body budget charges the allocation hint at admission and reconciles it against
the bytes actually read; when the hint cannot be covered the slot is still
taken, and the first read decides without retaining the upload. The
per-request cap still owns the status: a
refused request drains at most the remaining `max_request_bytes` window so an
over-cap body answers the definitive `413` instead of a retryable `503`, while
a body that fits the cap answers `503`. Request-body reads must make progress within
`request_body_idle_timeout` and finish within the absolute
`request_body_read_timeout`, so a stalled peer releases its reservation
without cutting off a slow but progressing upload. The `POST /admin/restore`
upload is exempt from the absolute bound only: a full database archive can
legitimately outlast it on a slow link, while the idle bound still cuts a
client that stops sending, and an expired deadline answers `408` with its own
message instead of the archive-cap `413`. Buffered whole-document
response reads use `whole_document_read_timeout` when the send has no deadline
and `whole_document_read_max` as the ceiling even when `X-Proxy-Timeout-Ms` is
longer, so a slow or stalled body releases its response-buffer reservation; a
shorter caller deadline wins and configuration validation preserves the
`models_discovery_timeout` budget below the ceiling. The three admission
budgets accept `0` = unlimited and hot-reload.
`max_queue_size` (default 1024
per group) and `max_queue_wait` (default 10m) bound a held or saturated
provider's parked work; both accept explicit `0` = unlimited. Independent
per-key/header concurrency remains unlimited by default. The scheduler retains
at most 100000 distinct provider+key groups, evicting the least-recently-idle
group (no in-flight or queued work) to admit a new key at the bound; a new key
is refused only when every retained group has active work, so a table full of
active groups still fails closed. Recent pacing/backoff state for evicted
groups is kept in a separate 4096-entry bounded LRU. The scheduler also
retains at most 4096 header-created provider limits, refusing new ones past
that bound rather than growing process memory without limit. A new request
body has the `request_body_read_timeout` budget, SSE writes have the
`sse_write_progress_timeout` per-event progress bound, and an upstream body
that stops sending for `upstream_stream_idle_timeout` is canceled.

Request budgets are strict sliding windows: at most the configured count is
admitted in any trailing window of the configured length, so an idle provider
banks nothing and a busy one cannot spend future allowance as a present burst.
Charging the window is what makes the limit match a provider that counts
requests per rolling or fixed minute; a refilling budget could emit the whole
count in one instant and then refill through the same span, which is how a
`100 per minute` setting reached a provider's `101/100`. Each admitted request
is charged once, at proxy admission, not again for each absorbed upstream retry.
A count above 65536 is enforced by even spacing at the same ceiling instead of
one timestamp per slot, so the memory a window holds stays bounded. Token
budgets remain refilling buckets: prompt tokens are reserved from the captured
characters divided by four and then settled against final reported usage, and a
reservation larger than the whole capacity may proceed while credit is
positive, leaving debt that refill or settlement repays. The client's output
ceiling (`max_tokens` and its spellings) is deliberately not reserved: it is a
ceiling, not expected usage, and reserving it would turn the token budget into
a worst-case concurrency cap. A token budget therefore also sets the sustained
average rate: 500K tokens per 6h refills at about 23 tokens/s, so once the
initial budget is spent, later requests wait for refill unless actual usage
settles lower than the reservation.

### Error storm protection

Open Settings, select **Error storm protection**, and turn on **Enable
protection**. It is opt-in in both neutral defaults and the deployment example.
Choose the rolling window, minimum samples and failure threshold, then select
HTTP failure statuses, transient transport failures and response/stream failures.
Provider protection requires all models receiving requests in that same window
to meet the thresholds individually. A single affected model is held on its own
while other active models remain healthy. If only one model is active, that
model can establish a provider-wide incident. Provider and model protection can
also be enabled independently.

Configure initial/maximum recovery delays, multiplier, jitter, recovery successes,
additional retries, queue capacity, per-wait timeout and retained scope capacity
in that category. The generated [example](../proxy.example.yaml) supplies every
type, range and default. Existing Queue & retry limits and provider Limits still
apply; neither `storm_max_wait` nor a per-send deadline is a total request timeout.
Use a client timeout appropriate to the desired end-to-end wait.

Each clickable banner identifies its current scope and safe failure
category (HTTP status, transport error or response error), percentage/count of
failed upstream attempts, detection window, queued callers and next probe or
recovery progress. Open an incident to inspect the distinct requests that
encountered a selected failure, failed attempts, total sampled attempts, error
category, queue count and next retry time. Provider entries also show affected
versus active models. Distinct request counts follow each request's latest selected failure in the
rolling window, so repeated retries do not inflate that count. The percentage
still describes upstream attempts. These are process-local observations, refreshed
through the normal dashboard bootstrap/poll cadence; they are not chart history or the
Errors chart's distinct-request percentage. Scope counts can overlap where a
caller waits on both provider and model protection. Hide the banner independently
without disabling protection.

Changing a storm policy resets its samples/incidents and wakes waiters to
re-evaluate the new policy. Banner visibility alone preserves detector state.
A reset does not cancel work already sent or erase request history. Restart
starts detection afresh, and queued requests do not persist. History deletion
does not reset live outage protection.

Storage stays outside the detector: fixed 64 time buckets retain counters per
scope, with oldest-edge precision within approximately `storm_window / 64`.
`storm_max_scopes` includes provider aggregates and exact provider/model scopes;
stale unreferenced healthy scopes can be evicted, while active or referenced
scopes remain protected. Scope identifiers over the internal 512-byte retention
bound and exhausted scope capacity reject admission rather than silently bypass
protection. These bounds are not a total heap ceiling: waiting callers retain
bounded request bodies, and existing ordinary queues remain separately sized.
The [protocol guide](protocol.md#error-storm-protection) defines retry safety,
quota exclusions, native handshakes and client-facing failures.

### Quota pause

Open Settings, select **Quota pause**, and choose the reaction to a 429 whose
error envelope is a durable account condition - `insufficient_quota`,
`insufficient_credits`, spend/hard limits, the recognized numeric billing codes
(the built-in classification is fixed and `quota_error_classes` may add
spellings; only the reaction is configurable):

- **off** (default): the 429 surfaces to the client immediately. Nothing is
  parked or retried.
- **retry**: a provider-wide recovery gate opens on the first such 429 from an
  OpenAI-wire provider. The triggering request absorbs the 429 and re-sends
  per the error storm recovery delays; every new request to the provider
  parks on the gate (bounded by `storm_max_queue` and `storm_max_wait`); the
  gate closes after `quota_pause_recovery_successes` consecutive successful
  probes (the default 1 closes on the first 2xx) and the queue drains. A provider
  `Retry-After` on the quota 429 paces the next probe like any other 429.
- **manual**: an indefinite provider-scoped pause (an operator hold labeled
  with the quota class) is created automatically; every request, triggering
  one included, queues until an operator resumes it on the pause surface.
  When a pre-existing hold already covers the provider, no duplicate is
  created: the triggering request queues under that hold only if it parks
  the triggering client too, and a hold that parks other clients only lets
  the triggering request surface the 429 immediately.

The retry-mode gate shares the storm scheduler's parking, probes and pacing,
so storm recovery settings configure it without storm protection being
enabled. It surfaces in the storm banner as a quota incident with a
**Resume now** action (`POST /admin/quota`); manual-mode holds appear on the
pause surface with their reason and resume like any hold, and they derive their
provider pin through the same commit owner as a manual pause so the provider
stays offered in the scope pickers. A quota 429 never
arms the rate-limit pacing trip (the rate-limit health series still counts the
429 itself), and quota re-sends never burn the transient
`max_retries` budget. They are bounded by `storm_max_wait` instead, measured
from the request's first park: when it expires the client receives the
canonical retryable `error storm protection: ...` 429 rather than waiting on a
gate that never opens, and the provider stays parked for the next request.
The retry gate is in-memory - a restart drops it and
the provider's next durable quota 429 re-arms it - while manual-mode holds
persist like every operator pause. In-band occurrences (the same envelope
inside a 200 stream) arm the pause the same way; that request already
answered, so it is never parked. Translated Anthropic-format and Cursor
upstreams keep the off behavior in every mode. The built-in vocabulary is
fixed; `quota_error_classes` may add a gateway-specific spelling as a durable
quota class (it may never move a built-in spelling between classes, and it
re-reads stored history through the same vocabulary).

### Debug and preview capture

Debug selects clients, providers and/or models and starts a capture session for
a chosen duration or until stopped. At least one dimension is required; selected
dimensions intersect, with alternatives inside each dimension. It does not have
an unrestricted "all" switch. Sessions can be edited or stopped individually;
Stop all ends every active session. A stop carries only `enabled:false` and an
optional session `id`; a scope or duration beside a stop is a 400 rather than a
silent stop-everything. Conflicting scopes are rejected.

Debug model matching uses the native-model normalization in
[debug.go](../internal/proxy/debug.go), separate from display-only model rules.
Do not assume that every model spelling is an independent exact scope.

The session timer selects future captures. `debug_capture_ttl` separately
controls retained capture lifetime after completion, and
`debug_capture_max_bytes` bounds one capture document: retained headers are
charged first and the remainder is split between the request and the
client-facing response body. Oversized captures are marked truncated. `debug_store_max_bytes`
(default 512 MiB) caps the whole capture sidecar; once the next write would
exceed it, new captures are skipped and TTL expiry frees capacity again.
Capture writes are counted in bytes, not characters. The request drawer loads an
available sidecar only for a captured request; expired/absent captures are not
recreated from ordinary metrics. Completion is published before the sidecar is
encoded and saved, so a slow sidecar write delays neither the live end nor
durable accounting. A drawer fetch that follows the live end waits for that
request's in-flight save instead of answering a transient 404; a disconnected
client stops waiting.

Starting a capture requires durable storage. With `db_path` empty,
`enabled:true` without an ID answers 503 `durable storage is disabled` and
creates no session. A store-less process
also has no persisted session document to restore, so it never starts with an
enabled session; if one were present in memory, the request path still refuses
to build a capture tap, so no headers or bodies are retained for an unfetchable
sidecar. Editing or stopping a named existing session remains available.

Retained request headers use the same policy as recorded response headers:
names are visited in sorted order for the budget, each value is capped at 4 KiB
and marked with `…` when cut, at most 16 values are kept per name, ordinary
headers share a 16 KiB total budget per header map, a header name is capped at
512 bytes, and at most 256 names are retained per map. A longer name or an extra
name is omitted without a marker, exactly like a value beyond the budget.
Credential headers (the
configured auth header, `Authorization`, cookies, and token-, API-key- or
secret-bearing names) become `[REDACTED]` before any truncation, so a cut
value is never a credential prefix. Headers or values beyond the budget are
omitted without a marker; the capture schema has no header-truncation field.
The capture JSON is encoded without HTML escaping, so `<`, `>` and `&` appear
literally while JSON control escapes remain.

The drawer's Debug section also carries a Download button. It fetches the same
capture with `download=1` and saves a gzip artifact named
`millivolt-debug-<captured_at>.json.gz`, using the record id in the name when
the capture does not carry a readable `captured_at`. Captured request and
response bodies render through the same bounded code surface as record
previews, with wrap, copy and expand controls; the Download button remains the
whole-capture path. The capture cards also surface its schema and captured-at
time, conversation and key correlation, first-answer timing, usage, cost and
attempt count.

`capture_body_preview` is a different opt-in feature: it retains short prompt/
response previews in the normal record without starting a Debug session. The
two features also retain different request bytes: on the passthrough wire
the record's prompt preview is the client's original body, captured at the
metadata decode before `request_overrides` or the sub-conversations strip
rewrites it, while a debug capture's request body is the rewritten upstream
body sent after any rewrite. A translated Chat Completions or Anthropic target
re-decodes the translated body for its prompt preview; a translated Responses
target keeps the client-body prompt preview, because the generated item list is
not re-decoded for preview. The response preview follows the client-facing
response content on every target. Both features can retain sensitive content
even when credential headers are redacted.
Exports/backups can outlive the configured retention period. See
[Security](../SECURITY.md#upstream-destinations-and-credentials).

### Export and deletion

The operator console's export section downloads finalized request records as
JSON, not the process's stderr log and not a complete administrative audit
trail. With durable storage it reads the database; otherwise it uses available
ring records. Open export, choose either all history or matching records, and
review the previewed count before downloading. Provider, model, client and
conversation merge the server's known vocabularies with whatever the live record
ring holds, so a quiet proxy can still filter by what has run since it started,
and each of them also accepts a typed value. Status is the one list-only
dimension, because a status code is not a name anyone can type reliably. A
debug-only export can include retained capture sidecars.

A debug capture is written beside its record as a `debug_capture` member, and
only when the row still has a stored sidecar. The artifact therefore carries
debug bodies only while durable storage is live: a ring-record export, or a
sidecar already reclaimed by the capture retention sweep, has the record alone.
The bodies remain sensitive either way.

The export predicate has no constraint for `status_code=0` and `debug=0`; both
are off rather than a match for the zero status or the false flag. A
genuinely bodyless `POST /admin/purge` deletes everything in one call, which is
why a mistyped query key is refused instead of silently ignored.

The download is always a gzip artifact named `millivolt-logs-<timestamp>.json.gz`;
gunzip it to read the JSON array of records. Compression is applied at the
export writer itself, so every export shape (filtered, all or debug-only,
from the ring or durable storage) downloads compressed the same way. This is a
breaking change for scripts that consumed the plain JSON download: there is
no uncompressed fallback, so scripted consumers must decompress the artifact
first.

A durable export holds one read snapshot for the whole stream, so the server
bounds it with an internal total budget. If the budget expires after bytes were
written, the download is aborted mid-artifact instead of completing as a
successful gzip: the saved file is partial and must be discarded. A caller
cancellation or shorter caller deadline is reported separately from this
server budget.

In the dashboard, both database deletions live in Settings, in the storage
category, and nowhere else. Delete matching uses the same filter/count owner
as the export, and its scope is the chosen filter; Delete everything wipes
the whole database. Deletion requires confirmation in the dashboard; it does
not happen when the sheet opens. A selected filter makes a read-only
`POST /admin/purge/count` preview; the count first commits the writer's queued
batch into the read view, so a just-finalized request cannot read as empty.
That barrier is bounded by the query timeout: a wedged writer lets the preview
answer from the committed snapshot instead of spinning the sheet forever, and
the delete's transactional re-check stays the guard. Neither the caller's
deadline nor a client cancel fails the preview, even when it expires before the
barrier does: each step runs detached on its own query budget, so a short
dashboard deadline answers the same as an unbounded one. A
ring-only deployment removes through the boundary the count saw, so a record
that finalizes during the delete stays behind instead of disappearing
uncounted. Current traffic can still change the count
before the action, but the UI retains the previewed age cutoff rather than
silently moving it, and sends the rendered count with the delete so the
proxy re-checks it inside the delete transaction and refuses a moved row
set with 409 instead of deleting unpredicted rows. A 409 drops the sheet's
preview and re-counts, so the operator confirms the new set instead of
repeating the refusal. The filter selections reset every time the sheet
refills, so a stale invisible filter never arms a destructive default.
Both actions are irreversible without a backup.

Every purge, backup and restore writes one audit line to the process log with
its terminal outcome, before the response is written: the action, the rendered
selection (`filter="all"` for a bodyless full delete, `filter="unparsed"` while
the body or query has not been accepted, and `members="unparsed"` likewise for
a backup/restore), the previewed count when
one was supplied, and the committed `deleted` / `buffer_removed` counts, or the
error. Even a rejection at the method gate or the query grammar, before the
selection exists, leaves a line. Each value is quoted with Go string escaping,
so a label carrying a newline or a quote stays one token on one physical line. A
lost or truncated response therefore stays diagnosable, and a
retry can be compared against what was actually attempted. These lines are the
destructive-action trail; they are not a general request log.

Export/delete filters are exact raw record fields, not display-name model
canonicalization. The API also supports conversation/error-type/time filters
beyond the console's common choices. See the shared
[filter decoder](../cmd/proxy/log.go) and
[PurgeFilter](../internal/storage/purge.go). Empty-body full deletion is
deliberate and destructive; never use malformed or empty JSON as an all-records
command. The count preview applies the same empty-filter rule, so `{}` answers
400 on both routes rather than counting the full set for an ambiguous body; a
genuinely bodyless count previews the whole set, exactly like the bodyless
delete.
The delete fence preserves newer completions and in-flight work. A completed
purge, and an expired debug-capture sweep, checkpoint-truncates the WAL after
the committed delete: `secure_delete` zeroes freed cells in the pages SQLite
writes, and the checkpoint removes the pre-delete frames that would otherwise
keep the old page images readable in the `-wal` sidecar. A reader holding an
older snapshot can defer the checkpoint; the writer's near-term tick retries it
while the reader is gone, and a clean shutdown makes the final attempt after the
read pool is closed.

### Settings and Restart

Settings only reads configuration when opened. Its search, category rail and
structured editors work on a local draft; Apply uses the
[revision-checked save contract](configuration.md#saving-settings-safely).

The console's system section reads restart availability and status when opened.
The restart action is disabled while a restart is in progress or rebuilding is
unavailable. Progress reports actual build/drain/flush/handoff phases; it is not
a simulated timer. After a successful handoff, reconnect/bootstrap refreshes
history, aggregates, operator state and config. Changed embedded frontend assets
trigger a page reload through that same refresh owner. The
[lifecycle section](containers.md#reload-restart-and-shutdown) distinguishes source rebuilds
from container replacement and ordinary shutdown.

## Dashboard data and request inspection

The KPI band is global. Explorer selections and the request status filter scope
the timeline and request log; selecting a card adds a filter and entity links
pivot within that shared route. Browser back/forward restores hash-based scopes.
Chart preset, timeframe, percentile and series visibility persist in browser storage.
These saved views are local browser state, not shared server settings.

The In-flight tile separates work that is sending from work the scheduler is
parking. Its summary names the two totals that fit the narrowest tile - active
and queued - and the composition rail under the value splits every live state
(sending, pending, operator-paused, throttled) by color; hovering the rail, or
focusing it with assistive technology, reads the exact per-state counts. A
queued request is one the proxy has not sent upstream yet: it can be waiting in
the admission queue (per-key concurrency, pacing, a provider cap or an operator
hold) or in the retry send gate behind a 429/5xx pacing window. The request
table marks the same state with the `throttled` pill, and the status selector
can filter on it.

Explorer dimensions are Providers, Models, Client, Conversations, Tools, Time,
Status, Errors and Keys. Their meanings are server-owned: model grouping and
time-bucket membership are not browser guesses. Conversations can show declared
parents and main/sub/unresolved counts; read the
[lineage contract](protocol.md#main-and-sub-conversations) before interpreting them.
Tools and error signatures can each have multiple memberships per request.
Tool cards count affected requests for N and share, with request-level card
metrics counted once per affected request; their calls KPI separately sums
request ToolCalls, so repeated same-name calls do not inflate the request count.
A request using several tools contributes to each tool card. Error/429 health
badges and every gallery share use distinct affected requests.

The [visual guide](dashboard.md) covers every timeline and its controls. Overview
is the summary: it carries no plot, and its tiles take the whole card (requests
over the blended token volume, tokens in/out/cached with their shares, spend
with the blended per-Mtok price, the errors / 429 pair, and average latency /
speed over every measured request with each metric's low-high sample range);
Traffic compares requests/errors; Tokens separates available input/output/reasoning/cache
usage and tracks the cache-hit share of input on a percentage axis; Speed + latency
plots throughput on its own axis with TTFT and the decode window (first token to
last, the span decode speed measures) sharing a duration axis; Errors compares
error count/rate; Cost places reported USD spend beside request volume. One
percentile selection controls the timing series (the Overview keeps only the
timeframe control). Period readouts are computed from period samples, not
averages of bucket percentiles. Insufficient samples
remain unavailable. All time means the available retained history, not deleted,
dropped or never-observed requests, and the timeframe control never offers a
window wider than it. The chart endpoint takes either a minute `window` (or
`all`) or an explicit `from_ms`/`to_ms` pair for calendar ranges; each payload
reports the resolved right edge `to_ms` and the unscoped `data_from_ms` extent
the control gates on.

### Request rows and details

The log starts with recent rows and pages older durable records as you scroll.
Live rows show in-flight/queue/retry progress; final rows retain their outcomes.
Retry disclosure opens absorbed attempts under the original request, not another
inference record. The attempts log keeps the newest `max_retries` plus
`storm_max_retries` entries (at least one): a durable quota wait re-sends
without spending either budget, so the tail is what explains a long parked
request.
Click the detail arrow or a non-link row cell to open its
drawer; previous/next controls navigate requests without changing the scope.

The inspector presents available facts in four classified groups. Request covers
identity/status/times, time bucket, wire sizes, client/runtime, provider metadata,
model and captured parameters. Content covers conversation and parent ids, turn
and size composition, and prompt/response previews. Usage covers token splits,
generated tokens, timing, throughput, cost, transport decomposition and tools.
Diagnostics covers queue/rate limits, retries, in-band upstream responses,
retained headers and Debug. A summary readout carries the status, route, identity
and first numbers, and a final upstream failure renders as an error alert above
the groups. Only the selected group is shown; card headers collapse, and the
active group and collapse choices survive live updates.

A typed 429 is flow control, so its reason, code and message stay in the
rate-limit card instead of the error alert; a rate limit carried by a non-429
status is a final failure and renders the alert like any other.
An in-band upstream response that arrived after a committed success is disclosed
in Diagnostics rather than counted as a retry. Live pause and throttle state
appear as explicit rows while a request is in flight.

Copy actions cover the values that matter (identifiers, paths, models, headers,
error detail and captured bodies), field-list cards and the header's JSON
action. One clipboard path reports success or denial in a polite status line.
Long captured bodies use a bounded code surface with line count, wrap, copy and
expand controls; a long single line scrolls inside that surface instead of
widening the inspector. Narrow viewports get the full-width inspector with keys
stacked above values and touch-sized copy and close targets.

Recognized parameters include sampling and output limits, stop/logprob controls,
reasoning/verbosity, response format, tool choice and streaming flags. Explicit
zero/false parameters remain distinct from absent ones. Ordinary HTTP relay
metadata does not infer missing parameter defaults. Adapters differ: Anthropic
metadata comes from the translated body, including its configured output-limit
default, and Cursor stores the base model ID rather than its fused spelling. See
[adapters](adapters.md) before treating these fields as an exact representation
of the caller's original request.

Opening ordinary details does not fetch private request content. Prompt/response
previews appear only if captured, and Debug sidecars use their separate opt-in
and retention contract. Even ordinary labels, error text and headers can be
sensitive; do not publish request drawers from private history without review.

### Reading timing, cost and health

Request duration starts after upload and includes metadata work, queue/hold/retry
waits and response handling. TTFT is measured from the upstream attempt's own
send start to the first token (reasoning or answer), so every retry has its own
TTFT. The value excludes proxy scheduler queueing, request parsing and retry
backoff, but it does span the upstream transport window from request handoff
(dial, TLS, connection-pool wait, header wait). A request that produced no
token, one that never sent an upstream attempt of its own (a parked cursor
resume), and any non-streamed response have no TTFT: the log shows a
placeholder, not 0 ms, and aggregates skip it. Rows written before
0.3.34 stored `ttft_ms` anchored at request start (upload, queueing and retry
backoff included) and were zeroed once by the boot repair, so they report TTFT
as absent; their `started_at` and `first_token_at` timestamps remain, keeping
`pre_token_ms` and decode windows usable. TPS is the single
generation-speed measure: (generation tokens - 1) over the first-token to
last-token window. Average TTFT and average TPS are the arithmetic means of
those per-request samples. TPS is absent for fewer than two tokens, a
non-positive window, or a response that did not stream (no token timestamps).
The compact surfaces (the KPI band and the Overview timing tile) abbreviate
this unit as `t/s`; roomier readouts spell it `tok/s`.
The numerator assumes the first observed content chunk carries one token; a provider that bundles several tokens into that chunk can
still overstate the rate. These are provider-attempt measurements, not claims
about total user wait or system-wide throughput.

Money stays USD in storage and APIs. The shared UI formatter uses cents below
$1, including fractional cents, and dollars for larger magnitudes. Usage/cost
comes from recognized upstream fields or configured field mappings; unavailable
reporting and a displayed zero do not establish a zero bill. Reasoning/cache
splits depend on what the upstream actually reports. When a stream carries no
usage object, or carries one whose output count is zero, the recorded output
token count is restored from the streamed chunk count, which includes
tool-argument fragments; the count is an estimate, not a provider report. Chat
tool calls are deduplicated by delta index while the retained index set holds
up to 1,024 distinct indexes. Once the set is full a further index cannot be
retained, so each of its fragments counts again; a hostile stream of repeated
indexes can overstate the tool-call count without growing memory past the
bound.

Errors count affected requests with a genuine failure, including recovered
upstream failures. Rate limits count requests that saw a rate-limit class on
the final, a retried or an in-band attempt: an HTTP 429, a 429 whose status
line a committed 200 could not relay, or a provider spelling the classifier
recognizes (such as `tier_limit_exceeded` on a 400); each request counts once.
The two signals can overlap: a bare 429 is not an error, but a non-429 status
that carries a rate-limit class failed the request and counts in both.
Client-local
cancellation is likewise distinct from an upstream failure - except a 499 whose
record carries a structured error type: that observed a failure before the
client left, either the provider's in-band stream failure (typically an error
event the client received and aborted around) or a truncated stream whose
completion marker never arrived, independent of which relay write failed. It
counts as the upstream error it is. Zero
explorer health badges are hidden independently. Error-dimension card
headlines count failure events (final plus absorbed), while card N counts
distinct affected requests. The gallery share denominator `error_total` is the
union of affected requests represented by the error cards, so a card's share
is N / `error_total` and can never exceed 100%. This denominator includes
recovered quality failures whose final status is not an error (for example an
absorbed empty-completion attempt), counting each affected request once even
when no request ends in an error status. For every other dimension
`error_total` remains the canonical affected-request error count. Do not treat
every displayed error number as the same metric.

The explorer gallery renders at most 24 cards. Its fold tracks exact
per-value accumulators for at most 8192 distinct values in the active
dimension; later values are combined into a synthetic `other` card with
aggregate count, cost, token, tool and health fields, competing for the 24
cards by that count. The synthetic card carries `synthetic: true` on the wire,
renders as `other (grouped)`, and is not filterable. A real value named `other`
keeps its own exact card and filter; `other` is a display bucket only when the
card is synthetic. Rail distinct-value counts, footer scope totals and
conversation summaries still cover the whole filtered history, so card counts
need not add up to them.

### Snapshots, live feeds and monitoring

The HTML includes the canonical bootstrap state so the first paint need not wait
for a second state request. Live SSE carries pending progress and finalization;
KPI/operator polling and configured aggregate refresh keep derived surfaces current.
Reconnects use sequence/feed identity and fall back to full snapshots when replay
cannot be trusted. The footer distinguishes live/offline, active holds/Debug,
process-local storage drops, totals degraded by a failed startup history scan,
and a degraded operator-state restore. The last one is the difference between
"nothing is held" and "a saved hold or limit document could not be read": the
served `pause` and `throttle` snapshots carry `restore_degraded`, so a truncated
document does not boot as a healthy, unpaused proxy with its saved rules silently
gone. The flag stays true for the life of the process; a restart that reads a
document which decodes is what clears it. Each key holds one value, so the next
accepted operator write replaces the unreadable document rather than preserving
it: capture the bytes first if they are needed as evidence. An offline
indicator alone does not prove inference is down. None of these mechanisms makes
initial history preload instantaneous.

For integrations, use the documented read routes rather than scraping the DOM.
Bootstrap/live data is a recent observation window; scoped aggregate routes query
available history; JSON export provides finalized records; restricted SQL queries
durable data within configured time/output limits. The API is in initial
development, so pin and verify the selected revision for integrations.

Prometheus is a bounded-ring view. Its request/token/error totals are gauges that
can shrink after eviction, purge or restart, not monotonic lifetime counters;
do not apply counter-only `rate()`/`increase()` assumptions to them. Its TTFT
quantiles use the ring's sample contract, which differs from dashboard R7 history
percentiles. Metric names and units are exposed with HELP/TYPE metadata at
`/metrics/prometheus`; [prometheus.go](../internal/metrics/prometheus.go) owns them.
An unchanged finalized ring revision serves the exact last exposition without
another ring copy, aggregate or TTFT sort; any ring content transition rebuilds.

### Operator recipes

The gated query route (`GET /metrics/query?q=`, above) and the [MCP](mcp.md)
`query` tool both take exactly one `SELECT` with no statement separator, and
results are bounded by `storage_query_max_rows` and
`storage_query_max_bytes`; an over-budget result is an error on the route, so
keep wide scans behind a `started_at` window or a `LIMIT`. The statement gate
also denies the read-only capabilities SQLite exposes outside ordinary tables
and columns: `ATTACH`, `DETACH`, `PRAGMA`, `LOAD_EXTENSION`, the
`sqlite_dbpage` and `dbstat` table-valued functions, the whole result-bearing
`pragma_*` family and the `readfile`/`writefile` functions. Those would read
raw pages, page statistics or the absolute database path, which is not part of
this console's surface.

The denial is lexical over the raw query text, string literals included, so a
keyword spelled inside a literal is refused too: `SELECT 'attach' AS x` and
`SELECT 'pragma_table_info'` both fail. A name only has to contain the bare
word at an ASCII boundary; `attachment` and `attach_x` pass. The driver
executes a multi-statement string wholesale, returning the rows of the last
statement, so both rules are load-bearing: without the `;` refusal a second
`ATTACH` statement would run behind a `SELECT` prefix the prefix check accepts,
and the keyword scan alone would not see it. Column meanings
come from the MCP `describe` tool. `started_at` is Unix milliseconds: a
relative window is `started_at >= strftime('%s','now')*1000 - 3600000`, and
calendar math divides by 1000 and applies SQLite's `unixepoch` modifier,
with `localtime` for the host's zone. The recipes below earn their keep in
the derived shapes no dashboard surface computes: the outlier ratio, the
write/read ratio, the latency decompositions. Their ingredient aggregates
are conceded up front, the way the decode window already is: the explorer
sums per-group counts and cache reads (its `N` and `Cache` fields) and a
per-group blended $/Mtok (its `cost_per_mtok` KPI, the same cost-reporting
in+out rule), and the chart's dec series samples the shared decode window
(`last_token_at - first_token_at`) that recipe 1 decomposes latency
around. Several of the columns a recipe reads do overlap the aggregate
scans: `ttft_ms`, `reasoning_tokens`, `status_code`, `attempts`,
`output_tokens` and `cache_read_tokens` all appear in the dashboard's scan
lists. The recipes' private reading set, which no aggregate scan reads, is
`queue_wait_ms`, `first_answer_at`, `retry_after_ms`,
`cache_write_tokens`, the `chars_*` columns, `total_tokens`,
`req_max_tokens`, `duration_ms` and `retries`. Each recipe is a
question, the SQL that answers it, and how to read the result.

**Where did the time go on a slow request?**

```sql
SELECT started_at, duration_ms, queue_wait_ms, retries,
       first_token_at - started_at AS pre_token_ms,
       ttft_ms,
       last_token_at - first_token_at AS decode_ms,
       started_at + duration_ms - last_token_at AS tail_ms
FROM requests
WHERE started_at >= strftime('%s','now')*1000 - 3600000
  AND first_token_at > 0
ORDER BY duration_ms DESC
LIMIT 20
```

`pre_token_ms` is the wait the caller felt, including queue and retry waits;
`ttft_ms` is measured from the adopted upstream attempt's send start, so a
retried request shows a small ttft beside a large `pre_token_ms`. Rows written
before 0.3.34 carry a start-anchored `ttft_ms` and were zeroed once by the
boot repair, so their `ttft_ms` reads 0 (absent) while `pre_token_ms` and
`decode_ms` stay usable from the stored timestamps. `decode_ms` is the
streaming window and `tail_ms` what remains after the last token, where a
millisecond of rounding noise is normal. A request that produced no token, or
was not streamed, has nothing to decompose, hence the `first_token_at > 0` gate.

**How long did a model think before answering?**

```sql
SELECT started_at, model, reasoning_tokens,
       first_answer_at - first_token_at AS thinking_ms
FROM requests
WHERE started_at >= strftime('%s','now')*1000 - 3600000
  AND first_answer_at > 0
ORDER BY thinking_ms DESC
LIMIT 20
```

`thinking_ms` spans the first token of any kind, reasoning included, to the
first answer token; 0 means the first token already was the answer.
`first_answer_at` is streaming-only on the generic relay; cursor-format
non-streaming requests set it (the bridge synthesizes each delta and tracks
it via cursorTrackDelta), and such rows legitimately enter the gate because
`first_token_at` is stamped by the same helper. Tool-call-only streams keep
0 and stay outside the gate.

**How long does an agent idle between conversation turns?**

```sql
SELECT conversation_id, started_at,
       started_at - LAG(started_at + duration_ms)
         OVER (PARTITION BY conversation_id ORDER BY started_at) AS idle_ms
FROM requests
WHERE started_at >= strftime('%s','now')*1000 - 86400000
  AND conversation_id != ''
ORDER BY conversation_id, started_at
```

`idle_ms` is the gap between one request's end and the next request's start
inside the same conversation; the first turn has no predecessor and stays
null. Order by `idle_ms DESC` instead to surface the biggest stalls first.

**What retried before the client saw a response?**

```sql
SELECT r.started_at, r.id, r.retries, r.status_code AS final_status,
       j.value->>'status_code' AS attempt_status,
       j.value->>'at' AS attempt_at,
       j.value->>'retry_after_ms' AS retry_after_ms
FROM requests r, json_each(r.attempts) j
WHERE r.started_at >= strftime('%s','now')*1000 - 86400000
  AND json_array_length(r.attempts) > 0
  AND COALESCE(json_extract(j.value,'$.in_band'),0)=0
ORDER BY r.started_at, j.key
```

One row per absorbed retry, oldest first. The `in_band` filter excludes a 429
recorded behind a committed 200: that entry is health evidence, not a retry.
The `json_array_length` gate is required: `attempts` defaults to the JSON
literal `null`, and `json_each` over that literal still yields a row. `retry_after_ms` appears (nonzero)
when the provider sent a `Retry-After` header or a ratelimit-reset header
(`x-ratelimit-reset-requests`/`-tokens`, `anthropic-ratelimit-requests-reset`,
`anthropic-ratelimit-tokens-reset`), bounded by the proxy's hint ceiling.

**Did a conversation's cache writes pay for themselves?**

```sql
SELECT conversation_id, COUNT(*) AS requests,
       SUM(cache_write_tokens) AS cache_written,
       SUM(cache_read_tokens) AS cache_read,
       CASE WHEN SUM(cache_read_tokens) > 0
            THEN ROUND(1.0 * SUM(cache_write_tokens) / SUM(cache_read_tokens), 2)
       END AS write_per_read
FROM requests
WHERE conversation_id != ''
GROUP BY conversation_id
ORDER BY cache_written DESC, cache_read DESC
LIMIT 20
```

`write_per_read` at or below 1 means the reads repaid the writes; reads with
no writes ride a cache somebody else warmed. A conversation with writes and
no reads paid for a prompt that never came back.

**When does traffic arrive?**

```sql
SELECT strftime('%w', started_at/1000, 'unixepoch', 'localtime') AS weekday,
       strftime('%H', started_at/1000, 'unixepoch', 'localtime') AS hour,
       COUNT(*) AS requests, SUM(cost) AS cost,
       ROUND(AVG(duration_ms), 1) AS avg_ms
FROM requests
GROUP BY weekday, hour
ORDER BY weekday, hour
```

`weekday` is `0` (Sunday) through `6` and `hour` is two digits in the host's
local zone; drop `localtime` for UTC. Cost is USD, summed as stored.

**How far does the token estimate drift?**

```sql
SELECT model, COUNT(*) AS requests,
       ROUND(AVG((chars_system + chars_user + chars_assistant + chars_tool) / 4), 1) AS avg_chars_est,
       ROUND(AVG(input_tokens), 1) AS avg_input,
       ROUND(AVG(1.0 * input_tokens /
            ((chars_system + chars_user + chars_assistant + chars_tool) / 4)), 3) AS input_vs_chars4,
       ROUND(AVG(1.0 * total_tokens /
            ((chars_system + chars_user + chars_assistant + chars_tool) / 4 + req_max_tokens)), 3) AS total_vs_est
FROM requests
WHERE input_tokens > 0
  AND chars_system + chars_user + chars_assistant + chars_tool > 0
GROUP BY model
ORDER BY input_vs_chars4 DESC
```

The scheduler reserves prompt chars/4 plus `req_max_tokens` when the client
sent one. `input_vs_chars4` compares real input tokens with the chars/4
share, and `total_vs_est` compares settled total tokens with the full
reservation. A ratio far from 1 on a well-represented model means the
reservation systematically over- or under-reserves for it.

**Which requests paid far above their model's blended rate?**

```sql
SELECT o.model, o.id, o.started_at,
       ROUND(1e6 * o.cost / (o.input_tokens + o.output_tokens), 2) AS per_mtok,
       ROUND(1e6 * a.cost_sum / a.tok_sum, 2) AS model_avg_per_mtok,
       ROUND((1e6 * o.cost / (o.input_tokens + o.output_tokens)) /
             (1e6 * a.cost_sum / a.tok_sum), 2) AS ratio
FROM requests o
JOIN (SELECT model, SUM(cost) AS cost_sum,
             SUM(input_tokens + output_tokens) AS tok_sum
      FROM requests WHERE cost > 0 GROUP BY model) a ON a.model = o.model
WHERE o.cost > 0 AND o.input_tokens + o.output_tokens > 0
ORDER BY ratio DESC
LIMIT 10
```

`per_mtok` is the request's own rate over its in+out tokens, and `ratio`
scales it against the model's blended average, computed over cost-reporting
requests only so unpriced traffic cannot dilute it. A ratio far above 1
marks expensive outliers such as cache misses or long-tail pricing; flip the
sort to find the bargains. SQLite has no `median()`, which is why the
blended average plus a sorted ratio stands in for it.

