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
| `--page-size` | `MILLIVOLT_MCP_PAGE_SIZE` | `50` | Default page for the record and capture listings. |
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

## Tools

`describe` is the one to call first. Without the live schema and the column
semantics below, correct SQL is guesswork.

| Tool | Purpose |
| --- | --- |
| `describe` | Live SQLite schema, the `requests`/`request_debug`/`meta` column reference, the exact health predicates, the dimension and status vocabularies, the live known client/provider/model names, the indexes, and every result limit. |
| `query` | One bounded `SELECT`, with a client-side row cap that truncates with an explicit marker. |
| `explore` | Faceted breakdown over all matching history by one dimension, with at most 24 groups. |
| `chart` | Server-authoritative time series, at most 31 clock-aligned buckets. |
| `records` | One durable newest-first log page, with the paired `before_ms`/`before_id` cursor. |
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
what was withheld and how to continue. Nothing is ever silently shortened.

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

**Stopping does not delete.** Stopping a session, or all of them, ends future
captures only. There is no delete endpoint, and a stopped session's documents
stay readable by record id until `debug_capture_ttl` expires. Only a backup
taken beforehand can remove them sooner.

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
3. **The reviewed count must match a real preview.** `reviewed_count` must equal
   what `purge_preview` reported for the SAME filter, and it is re-checked
   immediately before deleting. A deletion therefore cannot be authorized
   against a preview of a different row set than the one being removed.

Always run `purge_preview` first and pass its count back. The count itself can
move with traffic; the filter is what is fixed.

Deleted history cannot be recovered from this API. The deletion preserves newer
completions and in-flight work, does not reset live error-storm protection, and
never touches stored capture documents.

`set_config` is the other consequential call, and it is a revision-checked
partial patch: `values` carries only the keys to change, the whole result is
validated before the file is written, CLI-overridden keys are stripped, and the
response names the keys that need a restart. A supplied list or map replaces
that whole field, so read the current document first for structured keys such as
`providers` or `model_rules`. A failed reload still leaves the file written, and
the tool output says so explicitly.

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
and covers the error shapes, setup validation, credential confidentiality,
truncation markers, the pagination rule, and the purge guards. The live
integration test starts a private instance through
[scripts/dev.sh](../scripts/dev.sh) on its own port and scratch database,
drives every tool through a real MCP session, and runs a full capture cycle
against neutral loopback traffic:

```sh
MILLIVOLT_MCP_INTEGRATION=1 scripts/check.sh go test -count=1 ./internal/mcp
```
