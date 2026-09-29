# MCP server

`cmd/mcp` (built as `millivolt-mcp`), the proxy binary's `millivolt mcp`
subcommand, and the proxy's own `/mcp` endpoint expose millivolt's operator and
observability API to an LLM, using the official
[Model Context Protocol Go SDK](https://github.com/modelcontextprotocol/go-sdk).
They are clients of a running proxy: they never start, stop or restart the proxy
process. Their operator tools can reconfigure a running proxy, but only through
the same credential and the same routes the dashboard uses.

There are two placements, serving the same 22 tools from one implementation:

- URL-capable clients point at the proxy itself and supply only the key, with
  no local process and no `MILLIVOLT_MCP_PROXY_URL`.
- Clients that cannot speak URLs launch one of the two stdio binaries.

The `/mcp` endpoint is always on in the proxy process (like `/metrics`; no flag
or configuration key enables it), operator-gated, and Bearer-only. It forwards
the caller's own `Authorization: Bearer` credential on its internal calls, so
the dashboard session cookie is refused there.

## Setup

The stdio placements use the surface below; the URL placement above needs only
the `Authorization` header. Every stdio parameter comes from a flag and an
environment variable, flags winning. An empty environment value counts as
unset, so an empty container interpolation falls through to the default instead
of becoming a broken value.

| Flag | Environment variable | Default | Meaning |
| --- | --- | --- | --- |
| `--proxy-url` | `MILLIVOLT_MCP_PROXY_URL` | (required) | Proxy origin, for example `http://127.0.0.1:8081`. A trailing slash is normalized away; a path, query, fragment or embedded credential is refused, because every operator route lives at the root. |
| `--operator-token` | `MILLIVOLT_MCP_OPERATOR_TOKEN` | (required) | The proxy's own `MILLIVOLT_OPERATOR_TOKEN`. |
| `--query-max-rows` | `MILLIVOLT_MCP_QUERY_MAX_ROWS` | `200` | Row cap applied to a `query` result. |
| `--query-max-bytes` | `MILLIVOLT_MCP_QUERY_MAX_BYTES` | `131072` | Encoded-size cap on one `query` result. Whole rows are kept until the budget runs out, so a clamped result is still valid JSON. |
| `--page-size` | `MILLIVOLT_MCP_PAGE_SIZE` | `50` | Default page for the record and capture listings. |
| `--capture-max-bytes` | `MILLIVOLT_MCP_CAPTURE_MAX_BYTES` | `262144` | Document size above which a capture is withheld whole instead of being returned. |
| `--query-timeout` | `MILLIVOLT_MCP_QUERY_TIMEOUT` | `2m` | Bound on a full-history chart or explorer read. |
| `--timeout` | `MILLIVOLT_MCP_TIMEOUT` | `30s` | Bound on every call to the proxy. |

A malformed environment override fails setup rather than silently using the
default, and a missing credential, a token whose length is outside the accepted
band, or a malformed origin exit before any transport exists.

The token is presented as `Authorization: Bearer <value>` on every request, and
only there. The session cookie the proxy mints on a successful Bearer is never
used: it would silently outlive a rotated credential. The token is never placed
in a URL or a log line, and failure text returned to the model is scrubbed
before it is shown (see [Errors](#errors)).

The token length band matches the proxy's own boot check (16 to 512
characters). A credential the proxy would refuse to arm with is not a credential
this server can use.

The credential is never a flag default. `flag.PrintDefaults` renders
`(default "...")` for any flag whose default is not its zero value, and it runs
on `-h`, on `--help` and on every flag parse error, so a credential registered as
a default would be printed in plaintext to stderr, which an MCP host does not
capture. The flag carries an empty default and the value is resolved from the
environment afterwards, decided by which flags were actually named: an explicit
empty `--operator-token` stays a deliberate (and invalid) choice rather than
silently falling through to the environment.

`--proxy-url` follows the same rule even though it is not secret: an
environment value can carry userinfo, so it is not a flag default, it is
resolved after parsing, and the validation error names the failure without
restating the value.

Redirects are refused outright. No operator route redirects, and `net/http` strips
`Authorization` only when the hostname changes, so a same-host different-port or
subdomain `307`/`308` would replay both the credential and the full request body
to the redirect target.

### URL placement

A URL-capable client (an editor agent, a hosted connector, an MCP-capable CLI)
needs no binary and no environment: it points at the proxy's `/mcp` endpoint
and presents the operator credential in an `Authorization` header, the same
credential the dashboard uses.

```json
{
  "mcpServers": {
    "millivolt": {
      "url": "http://127.0.0.1:8080/mcp",
      "headers": {
        "Authorization": "Bearer the same value the proxy was started with"
      }
    }
  }
}
```

A remote client uses the reverse-proxied HTTPS origin from
[the reverse-proxy guide](reverse-proxy.md), with `/mcp` appended:

```json
{
  "mcpServers": {
    "millivolt": {
      "url": "https://your-hostname/mcp",
      "headers": {
        "Authorization": "Bearer the same value the proxy was started with"
      }
    }
  }
}
```

The header is the whole setup: the endpoint reads no environment, starts no
process, and retains no session or credential after the request. Every request
is independently authenticated, so a missing or wrong Bearer is denied before
any tool runs, and a request presenting only the dashboard session cookie is
refused with 403, because the caller's own credential is what the internal
calls carry. `/mcp/` look-alikes are reserved: an unregistered path answers 404
and is never forwarded to inference. The published Compose port is
loopback-only, so it is not a remote origin; remote clients terminate TLS at
the ingress as in the [reverse-proxy guide](reverse-proxy.md).

The transport is stateless and request/response only, and its limits are part
of choosing a client:

- `GET` and `DELETE` answer `405` with `Allow: POST`. There is no standalone
  SSE stream and no session to delete.
- There is no session id: the endpoint never sends `Mcp-Session-Id` and nothing
  outlives the request. No session cookie is minted here either, unlike
  `/admin`, `/metrics` and `/dash`, so an MCP client's cookie jar gains no
  dashboard credential; a client must not expect or send a session id.
- SSE resumption is not offered: no event store exists, so `Last-Event-ID`
  replay is not available.
- The server never sends requests to the client: there is no sampling and no
  elicitation, so a client that waits for a server-initiated request waits
  forever.
- Every request must accept both types, for example
  `Accept: application/json, text/event-stream`. A wildcard that covers both
  (`*/*`, `application/*, text/*`) is accepted too; an `Accept` missing either
  type, or one absent altogether, answers `400`.
- The accepted protocol versions are the bundled SDK's full set, newest first:
  `2026-07-28`, `2025-11-25`, `2025-06-18`, `2025-03-26` and `2024-11-05`.
  This build does not narrow them. A classic `initialize` is answered with the
  requested version when it is one of the legacy versions (`2025-11-25` and
  older, verified live) and with `2025-11-25` for any other requested version;
  `2026-07-28` is served only through the SDK's per-request metadata form.
- The SDK's localhost DNS-rebinding check is deliberately off. It rejects a
  loopback connection whose `Host` is not loopback, and the documented
  reverse-proxied placement preserves the external `Host` while dialing the
  proxy over loopback, so leaving it on would reject that placement. The
  endpoint is Bearer-only and the operator gate re-validates the credential on
  every request, which is the stronger check for an authenticated endpoint.

### Stdio placement and setup

Both stdio entrypoints run the same server and resolve the setup surface above
identically; they differ only in the invocation name they report in usage and
diagnostics. A leading positional `mcp` is reserved: it used to fall through the
proxy's argument parser and start the proxy anyway, and it now starts the MCP
server instead.

Build one of the two binaries; every placement below sets the same two
environment variables, either in the MCP client's `env` block or in the
environment it inherits:

```sh
go build -o millivolt-mcp ./cmd/mcp
go build -o millivolt ./cmd/proxy
```

- `MILLIVOLT_MCP_PROXY_URL` is whatever address reaches the proxy from where
  the MCP server runs. A native install uses loopback
  (`http://127.0.0.1:8080`) or the reverse-proxied HTTPS origin from
  [the reverse-proxy guide](reverse-proxy.md). Inside the proxy container it is
  always `http://127.0.0.1:8080`, the container's own loopback.
- `MILLIVOLT_MCP_OPERATOR_TOKEN` is the proxy's own `MILLIVOLT_OPERATOR_TOKEN`.

A native client points `command` at `millivolt-mcp`; for the proxy binary, use
its path as `command` and add `"args": ["mcp"]` before `env`:

```json
{
  "mcpServers": {
    "millivolt": {
      "command": "/path/to/millivolt-mcp",
      "env": {
        "MILLIVOLT_MCP_PROXY_URL": "http://127.0.0.1:8080",
        "MILLIVOLT_MCP_OPERATOR_TOKEN": "the same value the proxy was started with"
      }
    }
  }
}
```

The published image contains the proxy binary, so the same server also runs
inside the proxy container and reaches it over the container's own loopback.
`docker exec` needs local Docker access to the machine running the container,
which a remote client usually does not have; the standalone binary is the
alternative. The container is the bundled Compose container, named `millivolt`
(a plain `docker run` needs `--name millivolt`):

```sh
export MILLIVOLT_MCP_OPERATOR_TOKEN='the same value the proxy was started with'
docker exec -i -e MILLIVOLT_MCP_OPERATOR_TOKEN -e MILLIVOLT_MCP_PROXY_URL=http://127.0.0.1:8080 millivolt /millivolt mcp
```

`-e MILLIVOLT_MCP_OPERATOR_TOKEN` names the variable without a value, so the
credential is forwarded from the caller's environment and never enters argv or
the container's configuration; the exported name must match the forwarded name
exactly. The matching client configuration:

```json
{
  "mcpServers": {
    "millivolt": {
      "command": "docker",
      "args": ["exec", "-i", "-e", "MILLIVOLT_MCP_OPERATOR_TOKEN", "-e", "MILLIVOLT_MCP_PROXY_URL=http://127.0.0.1:8080", "millivolt", "/millivolt", "mcp"],
      "env": {
        "MILLIVOLT_MCP_OPERATOR_TOKEN": "the same value the proxy was started with"
      }
    }
  }
}
```

A remote client builds the standalone binary where it runs and targets a
reachable origin: the reverse-proxied HTTPS origin from
[the reverse-proxy guide](reverse-proxy.md), or an address the operator has
deliberately made reachable from the client (for example the proxy container's
name or host on a shared Docker network). The Compose port is published on
loopback only (`127.0.0.1`), so the published host port is not a remote origin.

## Tools

`describe` is the one to call first. Without the live schema and the column
semantics below, correct SQL is guesswork.

| Tool | Purpose |
| --- | --- |
| `describe` | Live SQLite schema, the complete `requests`/`request_debug`/`meta` column reference, the exact health predicates, a value vocabulary for every filter dimension, the live known client/provider/model and tool names, the indexes, the complete result-limit set (query rows and bytes, the row ceiling, log page band, default page size, explorer group and chart bucket caps, Prometheus line cap and capture byte budget), and the working SQL idioms for this build. |
| `query` | One bounded `SELECT`, clamped on both rows and encoded size, truncating with an explicit marker. The only tool that can express a time range. |
| `values` | The distinct values one dimension has in durable history, most frequent first, so a filter value is discovered rather than guessed. |
| `explore` | Faceted breakdown over all matching history by one dimension, with at most 24 groups. No time window. |
| `chart` | Server-authoritative time series, at most 31 clock-aligned buckets. Cannot group. |
| `records` | One durable newest-first log page, with the paired `before_ms`/`before_id` cursor and the page's `model_canon` map. |
| `snapshot` | The dashboard's live snapshot: sequence, feed, counters, storage signal, recent records. |
| `prometheus` | The text exposition, which covers the ring rather than durable since-inception totals. |
| `audit_status` | Capture sessions, their scope and live capture counts, the scope vocabularies, and the storage prerequisite. |
| `audit_start` | Start or edit one capture session. Requires `confirm: "start capture"` and refuses when durable storage is off. |
| `audit_stop` | Stop one session by id, or every session with `stop_all: "stop every capture session"`. |
| `audit_captures_list` | Page the requests that have a stored capture, through a bounded `SELECT` over `requests WHERE debug = 1`. |
| `audit_capture_get` | Read one capture document by request record id. |
| `operator_state` | Pause, limits, quota/storm and restart status in one call. |
| `set_pause` | Create, edit or resume pause holds. |
| `set_throttle` | Set or clear one provider's budgets. |
| `resume_quota` | Close one provider's open retry-mode quota gate. |
| `set_config` | Revision-checked partial configuration patch. |
| `config_get` | The live configuration document: values, running overrides (the keys the process pins outside the file, which `set_config` strips), the canonical usage and model metadata field names, defaults, per-key schema, restart-required set and the revision a patch must echo. |
| `reload_config` | Re-read the configuration file. |
| `purge_preview` | Count what a filter would delete, without deleting. |
| `purge` | Delete matching history permanently. |

The routes are a closed set owned beside the HTTP client, so no tool can name
an arbitrary path: nothing reaches the proxy's transparent inference
catch-all, and no restore-adjacent route is exposed. The HTTP endpoint
dispatches those same calls in process to the proxy's own gated handler, so
they never leave the process and the gate re-validates the forwarded Bearer.

### Filters

`explore`, `chart`, `records` and the explorer facets share one grammar, owned in
one place. Filters are `dim:id`, repeated, split on the FIRST colon, AND-ed
across dimensions and OR-ed within one dimension. `dim` is one of `client`,
`provider`, `model`, `conversation`, `key`, `status`, `time`, `tool`, `error`.

`status` is a single selector: an exact HTTP status code (`0` to `9999`) or a
live in-flight class (`streaming`, `paused`, `throttled`, `pending`). It is not
the explorer's status-class vocabulary, which travels as `status:2xx`.

An unknown dimension, malformed filter or unknown status is refused before the
request is sent.

### Pagination

`records` and `audit_captures_list` page newest-first with a paired
`(before_ms, before_id)` cursor. Both fields or neither.

The proxy scans a bounded number of rows per call, so a page can be short or
empty while its `more` flag is still true. `exhausted` is true when the cursor
did not advance, and also when `more` is false: treat it as "stop paging". A
non-advancing cursor means the scan budget cannot reach the next row, and
treating it as "keep going" is an endless loop; an empty page with an advanced
cursor is instead the normal shape of "the budget ran out before a match", so
page again.

Deep history needs this rather than `OFFSET`, which degrades on every page.

### Volume

A model cannot read ten thousand rows. Every listing defaults to a small page,
and every tool truncates with an explicit marker (`truncation.marker`) naming
what was withheld and the way to continue for that tool, because only some of
them have a cursor.

What is *not* claimed: the explorer caps groups at 24 and the chart caps buckets
at 31 SERVER-SIDE, so those drops happen inside the proxy and those tools'
`truncation.truncated` is `false` when groups were dropped. A model that trusts
that flag alone will read a short breakdown as a complete one.

### What each tool cannot do

These are the limits that otherwise send a model down the wrong path. They are
also in the tool descriptions, which is where a model reads them.

- `describe` first. Without the live schema, the column meanings, the health
  predicates and the value vocabularies, correct SQL is guesswork.
- **`explore` has no time window.** It always folds all history. It is also
  single-dimension: it cannot cross-tabulate provider by model. For either, use
  `query` with a `WHERE` on `started_at` and a `GROUP BY`.
- **`chart` is the only windowed tool, and it cannot group.** A window broken
  down by a dimension is a `query`.
- **`query` is the only tool that can express a time range.** There is no
  `unixnow()` in this SQLite build, so a relative window is
  `strftime('%s','now')*1000`, for example
  `WHERE started_at >= strftime('%s','now')*1000 - 3600000` for the last hour.
- An **unknown filter value is an empty result, not an error.** A misspelled
  client, provider, model, tool, conversation or key produces a confident zero.
  Call `values` before filtering on one of those. Model filters take the
  **canonical** name the proxy folds from `model_rules` (which is what the
  `values` tool returns), never a raw stored spelling.
- `time` is a **daypart bucket**, not a duration: `time:24h` matches nothing. The
  buckets are `night`, `work`, `evening` and `weekend`.
- `status` takes the proxy's **status classes**, not HTTP shorthand: `2xx`,
  `cancel` (499), `4xx`, `5xx` and `err` (anything outside those, including
  3xx and below 200). There is no `3xx` class of its own.

## Audit semantics

Audit is the proxy's debug-capture subsystem.

**Capture requires durable storage.** With `db_path` empty the proxy accepts a
session, reports success, and stores nothing; every read-back is a 404.
`audit_start` and `audit_captures_list` therefore check the storage signal first
and refuse with that explanation, and `audit_status` reports it. Starting a
session that can never capture would be a silent no-op, which is worse than a
refusal.

**Scope is explicit.** An empty dimension matches anything; non-empty dimensions
are AND-ed and names within one dimension are OR-ed. There is no unrestricted
all-traffic scope, and there is no conversation dimension. Model names use the
proxy's native model normalization, so use the `known_models` values rather than
guessing spellings. A scope overlapping a live session is rejected (`409`)
rather than silently replacing it.

**Starting requires a confirmation.** `audit_start` takes
`confirm: "start capture"` because capture stores full request and response
bodies.

**Stopping does not delete, but a purge does.** Stopping a session, or all of
them, ends future captures only: a stopped session's documents stay readable by
record id until `debug_capture_ttl` expires. There is no capture-specific delete
endpoint, but a `purge` deletes the stored capture document of every request it
matches, in the same transaction as the request row. A filter that reaches a
captured request takes its evidence with it. Only a backup taken beforehand
restores it.

**A scope name must be known when the vocabulary is.** `audit_start` checks
every client, provider and model against the vocabulary `audit_status` reports.
When that vocabulary is non-empty, an unknown name is refused with the known
values offered, because a session scoped to a name that matches nothing would
report `enabled: true` and capture nothing. An empty vocabulary is the
exception: a proxy that has seen nothing knows no names, so refusing every name
would make the tool unusable rather than safer and the check is skipped. A
vocabulary that cannot be read is not a skip: the start is refused, because the
guard cannot be applied at all.

**Read-back is by record id.** `audit_capture_get` takes the REQUEST RECORD id
(`requests.id`), not the session id. There is no listing endpoint, so
`audit_captures_list` discovers those ids by querying `requests WHERE debug = 1`
and reports that it does so. Captured documents are sensitive: credentials are
redacted from headers, but body content is not sanitized.

**Sessions survive a restart.** The proxy restores them from its own state, so a
start list may already contain sessions this server did not create.

## The destructive call

`purge` permanently deletes matching request history. The proxy has NO
server-side confirmation, NO confirm flag and NO two-step protocol. The operator
credential and this tool's own guards are the entire gate.

Three guards apply together, and each alone would be insufficient:

1. **The filter must constrain at least one field.** The bodyless request that
   deletes ALL history is never sent by this tool; an empty filter is refused
   before any request.
2. **The confirmation must be an exact phrase.**
   `confirmation: "permanently delete the matching millivolt history"`. It is
   also a required tool argument, so a call that omits it is rejected by the
   schema before the handler runs.
3. **`preview_token` must be the token `purge_preview` issued for THIS filter.**
   It is an opaque HMAC-bound token over the canonicalized filter, the count it
   authorized and an expiry, so it cannot be forged, cannot be reused for another
   filter, and expires. A bare count could not do this: it is only a number, so
   two different filters matching the same number of rows authorized each other,
   and a count obtained from anywhere - a `query`, a guess, a preview of a
   different filter - satisfied the comparison. That was a live bypass, not a
   theoretical one.

Always run `purge_preview` first and pass its `preview_token` back verbatim.

### What the guard does not prevent

Stated plainly, because an operator needs to know the size of the remaining
window:

- **The re-check and the delete are two HTTP requests with no shared
  transaction.** The count is re-checked immediately before deleting, and a
  deletion is refused when it no longer matches. But rows committed between that
  re-check and the delete are still removed without having been previewed. The
  preview authorizes a *filter* and a *count*; it is not a lock on the row set.
- **A filter that reaches a captured request deletes its capture document too.**
  The preview and the deletion both say so in their output.

Deleted history cannot be recovered from this API. The deletion preserves newer
completions and in-flight work and does not reset live error-storm protection.

`set_config` is the other consequential call, and it is a revision-checked
partial patch: `values` carries only the keys to change, the whole result is
validated before the file is written, CLI-overridden keys are stripped, and the
response names the keys that need a restart. A supplied list or map replaces
that whole field, so call `config_get` first for structured keys such as
`providers` or `model_rules`, and echo the revision it returns.

A failed reload is reported as a save, not as an error. The proxy writes the file
and only then answers 500 with the reload failure in the body, so that response
is decoded as the committed document it is: `saved` is `true` and `error` carries
the reload message. Reporting it as a tool error would tell a model that a
mutation that succeeded had failed, and its retry would then fail with 409.

`reload_config` re-reads the configuration file. It is the one state-changing
route the dashboard never calls. The operator token itself is process-bound and
is not re-read.

## Errors

Every failure is a tool result the model can read and correct, carrying the
proxy's own `.error` text and the status: `401` (credential wrong or missing),
`403` (the proxy's operator plane is unarmed), `404`, `409` (overlap or stale
revision), `413` (a result limit), and `429` with its `Retry-After`. An
unreachable proxy, a timeout, and a body that is not the expected document are
reported the same way. The `.error` field is read regardless of `Content-Type`,
because the operator routes are inconsistent and some answer `text/plain` with
a JSON body.

The returned text is scrubbed of the operator credential first, because a
reflecting endpoint could otherwise put the `Authorization` value into its own
failure message and hand it to the model. The scrub removes the literal token,
any contiguous 8-byte fragment of it (so a credential split across two JSON
string fields leaks no usable piece), percent escapes in any hex case
(including full-byte nested forms), the query `+` form of a space, and
JSON-escaped forms, including backslash-doubled text and surrogate pairs. Each
form is decoded to a fixed point before matching: an encoding is either fully
seen or the excerpt fails closed. Text that still decodes after three escape
applications is not published partly decoded: the whole excerpt is replaced by
`[redacted]` instead. That bound applies regardless of whether the credential
appears in the body: a heavily re-encoded failure with no credential in it
still loses its diagnostics, because a credential could be hidden past the
bound. The scan is bounded to the region that can still reach the excerpt, and
the raw bytes it reads are capped; a body whose whitespace collapse does not
reach the excerpt target inside that cap fails closed to `[redacted]` too, for
the same reason. The excerpt itself is whitespace-collapsed and truncated to
4 KiB before the ` [truncated]` marker is appended, so the model-visible text
runs up to the 12-byte marker past 4 KiB. Redaction runs before the truncation,
so a credential straddling the boundary cannot survive as a fragment.

## Try it against a disposable instance

The standalone binary is not in the published image; use `millivolt mcp` inside
a running container, or build the standalone binary from source. Run either
locally against a disposable instance with the repository's lifecycle owner,
never the main instance on `:8080`:

```sh
(
  set -e
  trap 'scripts/dev.sh stop' EXIT
  export MILLIVOLT_OPERATOR_TOKEN='dev-instance-credential'
  scripts/dev.sh
  MILLIVOLT_MCP_PROXY_URL=http://127.0.0.1:8081 \
    MILLIVOLT_MCP_OPERATOR_TOKEN="$MILLIVOLT_OPERATOR_TOKEN" \
    go run ./cmd/mcp
)
```

## Security

This server holds an operator credential, which is the whole dashboard plane:
pause, limits, configuration, restart, purge. Treat its configuration as
sensitive, keep it out of shared repositories, and prefer an environment
variable over a command-line flag so the credential does not land in a process
listing. See [Security](../SECURITY.md).

## Verification

The unit suite uses a fake proxy that pins the exact method, path, query and
`Authorization` header of every tool, maps canned responses onto typed output,
and covers the error shapes, setup validation, truncation markers, the pagination
rule and the purge guards. The paging rule has one owner shared with
`audit_captures_list`, so the two listings cannot disagree. Two of its tests run
outside that harness on purpose:

- the credential test **builds the binary and runs it**, asserting the token is
  absent from stdout and stderr for `-h`, `--help`, an unknown flag, a bad flag
  value and a bare start, in both the flag and environment forms. An in-process
  check could not have caught that defect, because the leak went to the process's
  own stderr. The same cases run through `millivolt mcp` on the built proxy
  binary and each must carry the `millivolt mcp:` invocation prefix on stderr,
  so a silent fall-through to the proxy fails the test instead of passing an
  absence-only assertion. The setup-and-usage test pins the MCP usage itself:
  help prints `Usage of millivolt mcp:` with `-operator-token` and without the
  proxy's `print-config`, and a tokenless start fails with
  `millivolt mcp: setup:`. Both tests run the built binary under a deadline in
  a scratch working directory whose config pins any fall-through to an
  ephemeral loopback port, so a dispatch regression fails promptly and cannot
  bind `:8080` or write into the repository.
- the redirect test stands up a second listener as the redirect target and
  asserts it received nothing, so "we refused the redirect" is an observation
  rather than an assertion about an error string.

The live integration test starts a private instance through
[scripts/dev.sh](../scripts/dev.sh) on a port and scratch database of its own, so
two concurrent runs cannot collide, drives the tools through a real MCP session,
and runs a full capture cycle against neutral loopback traffic:

```sh
MILLIVOLT_MCP_INTEGRATION=1 scripts/check.sh go test -count=1 ./internal/mcp/...
```

It runs in CI as part of the isolated browser-fixtures step, which already has a
dev instance and an operator token; it is not part of the core source-and-unit
job, because that job should not have to start a second proxy.

It drives every registered tool against its own disposable instance,
including the state-changing ones: `prometheus`, `set_pause` (a hold scoped to
the fixture client, resumed at once), `set_throttle` (a limit set and then
cleared), `resume_quota`, `set_config` and `reload_config` against the scratch
config copy, a full capture cycle (`audit_status`, `audit_start`,
`audit_captures_list`, `audit_capture_get`, `audit_stop`), `config_get`,
`values`, and the purge path: the missing confirmation, the wrong phrase, an
unauthorized token and a token issued for a different filter each refuse with
the row count unchanged, and then an authorized purge deletes the fixture rows.
A successful purge and the config write are safe here because the instance, its
database and its config copy all belong to the test and are discarded.

The same run drives the proxy's own `/mcp` endpoint with a real streamable
HTTP client: `initialize`, `tools/list` (22 tools), and `describe` and
`records` returning the same documents the stdio entrypoint returns, plus the
auth boundary (401 without a credential, 401 for a wrong Bearer from the shared
gate, 403 for a cookie-only request or a wrong Bearer beside an admitted
cookie) and the reserved `/mcp/` look-alike answering 404, never inference.
