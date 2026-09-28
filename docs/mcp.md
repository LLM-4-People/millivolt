# MCP server

`cmd/mcp` exposes millivolt's operator and observability API to an LLM over
stdio, using the official
[Model Context Protocol Go SDK](https://github.com/modelcontextprotocol/go-sdk).
It is a client of a running proxy: it starts, reconfigures and stops nothing.

An MCP client (an editor's agent mode, Claude Desktop, an MCP-capable CLI)
launches the binary and speaks the protocol over its stdin and stdout. Every
tool call becomes an authenticated request to the proxy's
[operator plane](operations.md#operator-and-data-routes), so the dashboard's
credential is the same credential and the same routes. That makes this a
convenience surface, not a second control plane: anything it can do, the
dashboard can do, and anything it refuses, the API refuses too.

## Setup

Every parameter comes from a flag and an environment variable, flags winning.
An empty environment value counts as unset, so an empty container interpolation
falls through to the default instead of becoming a broken value.

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
default, and a missing credential, an out-of-band token, or a malformed origin
exit before any transport exists.

The token is presented as `Authorization: Bearer <value>` on every request, and
only there. The session cookie the proxy mints on a successful Bearer is never
used: it would silently outlive a rotated credential. The token never reaches a
URL, a log line or any tool output.

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

Redirects are refused outright. No operator route redirects, and `net/http` strips
`Authorization` only when the hostname changes, so a same-host different-port or
subdomain `307`/`308` would replay both the credential and the full request body
to the redirect target.

## Tools

`describe` is the one to call first. Without the live schema and the column
semantics below, correct SQL is guesswork.

| Tool | Purpose |
| --- | --- |
| `describe` | Live SQLite schema, the complete `requests`/`request_debug`/`meta` column reference, the exact health predicates, a value vocabulary for every filter dimension, the live known client/provider/model and tool names, the indexes, every result limit, and the working SQL idioms for this build. |
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
| `config_get` | The live configuration document: values, running overrides, defaults, per-key schema, restart-required set and the revision a patch must echo. |
| `reload_config` | Re-read the configuration file. |
| `purge_preview` | Count what a filter would delete, without deleting. |
| `purge` | Delete matching history permanently. |

The routes are a closed set owned beside the HTTP client, so no tool can name
an arbitrary path: nothing reaches the proxy's transparent inference
catch-all, and no restore-adjacent route is exposed.

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
empty while its `more` flag is still true. A cursor that does not advance means
the scan budget is exhausted, so every listing reports `exhausted` explicitly.
Stop when it is true: treating a non-advancing cursor as "keep going" is an
endless loop, and `exhausted` is what prevents it.

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
  Call `values` before filtering on one of those.
- `time` is a **daypart bucket**, not a duration: `time:24h` matches nothing. The
  buckets are `night`, `work`, `evening` and `weekend`.
- `status` takes the proxy's **status classes**, not HTTP shorthand: `2xx`,
  `cancel` (499), `4xx`, `5xx` and `err` (below 200). There is no `3xx` class,
  because a 3xx never reaches a recorded row.

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

**A scope name must be known.** Every client, provider and model named in
`audit_start` must already be in the vocabulary `audit_status` reports. A session
scoped to a name that matches nothing would report `enabled: true` and capture
nothing, so an unknown name is refused with the known values offered.

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

## Example client configuration

### It is a source-only binary

The `Dockerfile` copies `cmd/proxy` and `internal` only, so the published image
contains the proxy and not `millivolt-mcp`. Build it from the repository:

```sh
go build -o millivolt-mcp ./cmd/mcp
```

```json
{
  "mcpServers": {
    "millivolt": {
      "command": "millivolt-mcp",
      "env": {
        "MILLIVOLT_MCP_PROXY_URL": "http://127.0.0.1:8081",
        "MILLIVOLT_MCP_OPERATOR_TOKEN": "the same value the proxy was started with"
      }
    }
  }
}
```

Run it locally against a disposable instance with the repository's lifecycle
owner, never the main instance on `:8080`:

```sh
(
  set -e
  trap 'scripts/dev.sh stop' EXIT
  export MILLIVOLT_OPERATOR_TOKEN='dev-instance-credential'
  scripts/dev.sh
  go run ./cmd/mcp --proxy-url http://127.0.0.1:8081 \
    --operator-token "$MILLIVOLT_OPERATOR_TOKEN"
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
  own stderr.
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

It drives what a query needs, not every tool. `prometheus`, `set_pause`,
`set_throttle`, `resume_quota`, `set_config`, `reload_config` and a successful
`purge` are not exercised against the live instance: several are state-changing,
and a live `purge` or `set_pause` against a fixture is not something a test
should do. What the live pass does drive is the whole read path, `config_get`,
`values`, a full capture cycle, and the purge GUARDS: the missing confirmation,
the wrong phrase, an unauthorized token and a token issued for a different filter
must each refuse, and the row count must be unchanged afterwards.
